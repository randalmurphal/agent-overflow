package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/webview2host"
)

// The hosted engine: the Windows/WSL implementation of the driver.go seam.
//
// A page's OPERATIONS are plain CDP — `cdp_page.go` and its siblings drive
// a WebView2 controller exactly as they would a Chrome tab. What differs is
// everything around a page's LIFETIME:
//
//   - the controller lives in the Windows launcher's process, because a
//     WebView2 controller must be a child window of a Win32 window and the
//     backend lives inside a Linux distro. So creating, closing, showing,
//     hiding and positioning one is a `browser:host` DIRECTIVE, and the
//     launcher answers with a report (internal/webview2host);
//   - the CDP endpoint is on the Windows side of the WSL boundary, reached
//     only through the relay tunnel the launcher dials
//     (internal/cdprelay). chromedp attaches through a loopback listener
//     inside the distro, never to an address that arrived over a wire;
//   - a page id is the backend's own handle, minted here and validated
//     against the launcher's vocabulary. The CDP target id is what the
//     `created` report carries back, and it is used for exactly one thing:
//     attaching chromedp;
//   - a download is the launcher's: WebView2 saves it to the Windows
//     user's Downloads folder and the launcher reports its life
//     (hosted_downloads.go). No download crosses CDP.
//
// Policy stays in the Manager, as everywhere else on this seam.

const (
	// hostCreateTimeout bounds the wait for a `created` report. The FIRST
	// create additionally pays for a cold WebView2 environment, which the
	// spike measured in seconds; past this the launcher is wedged or gone,
	// and the tool call must fail rather than hang.
	hostCreateTimeout = 45 * time.Second
	// hostAttachTimeout bounds establishing the CDP browser connection
	// through the tunnel: waiting for the launcher's tunnel to dial back,
	// one /json/version round trip, and the browser-level handshake. It
	// bounds each page's attach on that connection too (startCDPPage).
	hostAttachTimeout = 30 * time.Second
	// hostClearTimeout bounds the wait for a `cleared` report. It is
	// deliberately the largest of the three: the launcher closes every
	// controller, releases the environment, and then retries deleting the
	// user-data folder for up to its own 15s budget while the WebView2
	// browser process lets go of its file handles.
	hostClearTimeout = 45 * time.Second
	// hostClosedFallback bounds how long a page whose CDP target went
	// waits for the launcher's close before the engine retires it itself.
	// The launcher reports a closing page's download ends and then its
	// close on one serial queue, each call bounded at 5 seconds
	// (wsllauncher's notificationBridgeRPCTimeout), and a finished
	// download's apply here waits up to hostedPathTimeout, so the close
	// follows the destroy well within it. Past it the close was lost (the
	// launcher delivers reports once, with no retry), and a page with no
	// target must not stay listed.
	hostClosedFallback = 30 * time.Second
	// hostProfilePrefix keeps a derived profile id inside
	// ValidateProfileID's character class whatever the digest looks like.
	hostProfilePrefix = "ws"
)

// hostRelay is the engine's whole view of the CDP tunnel: one question,
// answered with an address this backend owns.
type hostRelay interface {
	// BrowserWebSocketURL discovers the pane environment's CDP browser
	// endpoint through the tunnel and returns it re-addressed onto the
	// relay's own loopback listener.
	BrowserWebSocketURL(ctx context.Context) (string, error)
}

// hostReport is one launcher answer, delivered to whoever is waiting on
// that page id.
type hostReport struct {
	kind   webview2host.ReportKind
	detail string
}

type hostedEngine struct {
	relay  hostRelay
	send   func(webview2host.Directive)
	events engineEvents
	logf   func(string, ...any)
	// accelerators is the bound-chord set the launcher matches against in
	// its own process. Nil ships an empty set: no chord is taken.
	accelerators func() keybindings.AcceleratorSet

	// The two bounds are fields rather than the constants directly so a
	// test can drive the timeout paths without spending them.
	createTimeout time.Duration
	attachTimeout time.Duration
	clearTimeout  time.Duration
	// ownerWait bounds a paused request's wait for its owner
	// (awaitRequestOwner); zero is requestOwnerWait.
	ownerWait time.Duration
	// closeFallback bounds the wait for the launcher's close of a page
	// whose target went (hostClosedFallback), and closeFallbacks are the
	// pages waiting, each with the timer that retires it at the bound.
	closeFallback  time.Duration
	closeFallbacks map[string]*time.Timer

	// attachMu serializes ensureBrowser so a burst of concurrent creates
	// establishes one browser connection rather than one each.
	attachMu sync.Mutex
	// setups are the page attaches under way, which Interrupt ends.
	setups cdpPageSetups

	// infoOrder numbers the page info reports (engineEvents.PageInfoChanged).
	infoOrder atomic.Uint64

	mu            sync.Mutex
	started       bool
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	attachCancel  context.CancelFunc
	pending       map[string]chan hostReport
	pageByTarget  map[string]string
	targetByPage  map[string]string
	// pageConn is the browser connection each bound page's session runs on
	// (bind), until the page leaves or that connection's retirement claims
	// it (claimLostLocked).
	pageConn map[string]context.Context
	// connectionEnded is a test seam: called by a connection's watcher once
	// the connection ended, before it retires it. Production leaves it nil.
	connectionEnded func()
	shown           map[string]bool
	bounds          map[string]PanePlacement
	// drivers reaches a page's CDP half from its handle, for the one
	// presentation fact the launcher cannot apply: the scale the device
	// metrics override draws a presented page at.
	drivers map[string]*cdpPage
	// ratios caches each page's devicePixelRatio against the pane ratio it
	// was read under: a rect report arrives per changed frame while the pane
	// is dragged, and the page's ratio (the OS scale) only changes when the
	// window changes monitor, which changes the pane's ratio with it.
	ratios map[string]pageRatio
	// scales is the latest-wins queue behind applyViewScale: one CDP override
	// in flight per page, and every scale asked for while it runs collapses
	// into the next one.
	scales map[string]*viewScaleState
	// noFind holds the pages whose runtime has no find API, as the launcher
	// reported before their created report.
	noFind map[string]bool
	// pageProfile is each page's profile id, created or popup, and policies
	// each live profile's navigation policy (profileOptions.Allow), for a
	// popup's profile and the browser-wide policy (hosted_popups.go).
	pageProfile map[string]string
	policies    map[string]func(owners []string, rawURL string) (allowed, resolved bool)
	// popups are the popups the launcher reported whose pages have not
	// gone (hostedPopup), and popupByTarget their pages by CDP target.
	popups        map[string]*hostedPopup
	popupByTarget map[string]string
	// targets is the current connection's record of its targets
	// (cdp_targets.go): the info of a target no page answers to yet, and
	// the target that opened each popup target, as the browser reports it.
	// Each connection has its own, which goes with it.
	targets *cdpTargets
	// downloads is the record of the launcher's downloads
	// (hosted_downloads.go).
	downloads hostedDownloads
	// downloadQuiet overrides hostedDownloadQuiet when set (tests).
	downloadQuiet time.Duration

	// readPageRatio reads a page's own devicePixelRatio; a test replaces it
	// to stand in for the CDP evaluate.
	readPageRatio func(ctx context.Context, p *cdpPage) (float64, error)
	// windowsPath is the backend path's Windows view (`wslpath -w`), and
	// backendPath a Windows path's backend view (`wslpath -u`); a test
	// replaces them.
	windowsPath func(ctx context.Context, path string) (string, error)
	backendPath func(ctx context.Context, path string) (string, error)
	// viewScaleAsked is a test seam called whenever a page is asked for a
	// view scale it was not already asked for. Production leaves it nil.
	viewScaleAsked func(handle string, want viewScale)
}

func newHostedEngine(relay hostRelay, send func(webview2host.Directive), accelerators func() keybindings.AcceleratorSet, events engineEvents) *hostedEngine {
	e := &hostedEngine{
		relay:         relay,
		send:          send,
		events:        events,
		accelerators:  accelerators,
		logf:          log.Printf,
		createTimeout: hostCreateTimeout,
		attachTimeout: hostAttachTimeout,
		clearTimeout:  hostClearTimeout,
		closeFallback: hostClosedFallback,

		pending:      make(map[string]chan hostReport),
		pageByTarget: make(map[string]string),
		targetByPage: make(map[string]string),
		pageConn:     make(map[string]context.Context),
		shown:        make(map[string]bool),
		bounds:       make(map[string]PanePlacement),
		drivers:      make(map[string]*cdpPage),
		ratios:       make(map[string]pageRatio),
		scales:       make(map[string]*viewScaleState),
		noFind:       make(map[string]bool),
		downloads:    newHostedDownloads(),

		pageProfile:   make(map[string]string),
		policies:      make(map[string]func([]string, string) (bool, bool)),
		popups:        make(map[string]*hostedPopup),
		popupByTarget: make(map[string]string),

		closeFallbacks: make(map[string]*time.Timer),

		readPageRatio: func(ctx context.Context, p *cdpPage) (float64, error) { return p.devicePixelRatio(ctx) },
		windowsPath:   windowsPathFor,
		backendPath:   func(ctx context.Context, path string) (string, error) { return runWSLPath(ctx, "-u", path) },
	}
	e.targets = e.newTargets()
	return e
}

// Start has nothing to start, and that is the design rather than a gap.
//
// The launcher builds its WebView2 environment lazily, on the first
// directive it receives, and its tunnel dials back only after that. So the
// CDP connection cannot exist until a `create` has been emitted — which
// happens in NewPage, after Start. Blocking here would wait for a tunnel
// that this call is what unblocks.
func (e *hostedEngine) Start(context.Context) error {
	if e.relay == nil || e.send == nil {
		return errors.New("browser: pane host is not wired")
	}
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()
	// The launcher matches chords itself, so it needs the set before the
	// first page can take focus. Idempotent, like everything else here.
	e.SyncAccelerators()
	return nil
}

// SyncAccelerators is engineAccelerators: ship the current bound set to the
// launcher, which holds it host-wide and gates every page's key events on it.
func (e *hostedEngine) SyncAccelerators() {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	// Before Start there is no page to gate, and a directive would make the
	// launcher build its WebView2 environment for nothing; Start ships the
	// set when the first page is about to exist.
	if e.send == nil || !started {
		return
	}
	var list []keybindings.Accelerator
	if e.accelerators != nil {
		list = e.accelerators().List()
	}
	e.send(webview2host.Directive{Op: webview2host.OpAccelerators, Accelerators: list})
}

var _ engineAccelerators = (*hostedEngine)(nil)

func (e *hostedEngine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started
}

// Interrupt releases a create or an attach that is waiting on a launcher
// which may never answer, so a concurrent shutdown can proceed.
func (e *hostedEngine) Interrupt() {
	e.mu.Lock()
	cancel := e.attachCancel
	pending := make([]chan hostReport, 0, len(e.pending))
	for id, waiter := range e.pending {
		pending = append(pending, waiter)
		delete(e.pending, id)
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, waiter := range pending {
		close(waiter)
	}
	e.setups.interrupt()
}

func (e *hostedEngine) Stop() {
	e.mu.Lock()
	browserCancel, allocCancel := e.browserCancel, e.allocCancel
	e.browserCtx, e.browserCancel, e.allocCancel = nil, nil, nil
	e.started = false
	e.pageByTarget = make(map[string]string)
	e.targetByPage = make(map[string]string)
	e.pageConn = make(map[string]context.Context)
	e.shown = make(map[string]bool)
	e.bounds = make(map[string]PanePlacement)
	e.drivers = make(map[string]*cdpPage)
	e.ratios = make(map[string]pageRatio)
	e.scales = make(map[string]*viewScaleState)
	e.noFind = make(map[string]bool)
	e.pageProfile = make(map[string]string)
	var cancelAttaches []func()
	for _, popup := range e.popups {
		cancelAttaches = append(cancelAttaches, popup.takeAttachCancelLocked())
	}
	e.popups = make(map[string]*hostedPopup)
	e.popupByTarget = make(map[string]string)
	for pageID := range e.closeFallbacks {
		e.endCloseFallbackLocked(pageID)
	}
	e.targets = e.newTargets()
	e.stopDownloadsLocked()
	e.mu.Unlock()
	for _, cancel := range cancelAttaches {
		cancel()
	}
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
}

// NewProfile derives a launcher profile id from the jar identity: one
// profile per jar. The digest is what makes the id stable across restarts,
// which is what makes a persistent jar's logins survive one.
func (e *hostedEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	id, err := hostedProfileID(opts.Jar)
	if err != nil {
		return nil, err
	}
	// Site data is the engine's own (spec §4): a hosted profile is a real
	// browser profile on disk and WebView2 persists it, so AO seeds nothing
	// into it and reads nothing out of it.
	if opts.Allow != nil {
		e.mu.Lock()
		e.policies[id] = opts.Allow
		e.mu.Unlock()
	}
	return &hostedProfile{engine: e, id: id, ephemeral: opts.Ephemeral}, nil
}

// hostedProfileID is the WebView2 profile name of a jar. A worktree jar's
// identity is its workspace path, so its name is the one every earlier build
// derived.
func hostedProfileID(jar string) (string, error) {
	trimmed := strings.TrimSpace(jar)
	if trimmed == "" {
		return "", errors.New("browser: a cookie jar is required for a pane profile")
	}
	digest := sha256.Sum256([]byte(trimmed))
	id := hostProfilePrefix + hex.EncodeToString(digest[:16])
	if err := webview2host.ValidateProfileID(id); err != nil {
		return "", fmt.Errorf("browser: derive pane profile id: %w", err)
	}
	return id, nil
}

// ClearSiteData is the engineSiteData half of Settings → Clear site data.
//
// On this deployment the Manager's own `browser-profiles/` tree is EMPTY:
// every cookie jar lives in the launcher's WebView2 user-data folder on the
// Windows side of the WSL boundary, one folder holding every workspace's
// named profile. The backend cannot delete it by path — it is a different
// machine's filesystem, and a live WebView2 environment holds it open — so
// the clear is a directive like every other pane-host operation, and the
// launcher's answer is what makes it a real clear rather than a hope.
//
// The Manager calls this only after closeBrowser, so no page or profile is
// live. The launcher still closes every controller of its own accord: its
// controller set is the authority on what is actually open, and a stale one
// would keep a handle on the folder being deleted.
func (e *hostedEngine) ClearSiteData(ctx context.Context) error {
	// The clear addresses no page, but the launcher's report machinery is
	// keyed on a page id, so it carries a correlation id minted the same
	// way. Registering the waiter BEFORE dispatching is what makes an
	// instant answer impossible to miss.
	correlationID := newHostedPageID()
	waiter, err := e.watch(correlationID)
	if err != nil {
		return err
	}
	defer e.unwatch(correlationID)

	if !e.dispatch(webview2host.Directive{Op: webview2host.OpClearData, PageID: correlationID}) {
		return errors.New("browser: pane host refused the clear-data directive")
	}

	report, err := awaitHostReport(ctx, waiter, e.clearTimeout)
	if err != nil {
		// The site data may or may not be gone. Saying so is the honest
		// answer: a Settings button that reported success here would leave
		// the user believing cookies were destroyed that are still on disk.
		return fmt.Errorf("browser: clear pane site data: %w", err)
	}
	if report.kind != webview2host.ReportCleared {
		return fmt.Errorf("browser: pane host could not clear site data (%s): %s", report.kind, report.detail)
	}
	return nil
}

// FileURL answers the file URL the Windows-side renderer can actually
// read. The backend's paths are WSL paths that do not exist on Windows;
// `wslpath -w` translates them to the `\\wsl.localhost\<distro>\...` UNC
// view (or a drive path for /mnt/<letter>), which is the same boundary
// the show-in-folder reveal crosses in the other direction.
func (e *hostedEngine) FileURL(ctx context.Context, path string) (string, error) {
	converted, err := e.windowsPath(ctx, path)
	if err != nil {
		return "", err
	}
	return windowsFileURL(converted)
}

// BackendFilePath answers FileURL's inverse: the WSL path behind a file URL
// the Windows-side renderer handed back (a request being authorized, an
// address-bar paste). `wslpath -u` owns the conversion, so a UNC host that
// is not this machine's WSL view errors — and the caller treats that as an
// unauthorized file, which is the correct answer for a share the backend
// cannot stat anyway.
func (e *hostedEngine) BackendFilePath(ctx context.Context, rawURL string) (string, error) {
	windowsPath, err := windowsPathFromFileURL(rawURL)
	if err != nil {
		return "", err
	}
	return e.backendPath(ctx, windowsPath)
}

// windowsPathFromFileURL is windowsFileURL's pure inverse: the URL authority
// becomes the UNC host, an empty one requires a drive-letter path.
func windowsPathFromFileURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
		return "", fmt.Errorf("browser: not a file URL: %q", rawURL)
	}
	if parsed.Host != "" {
		if strings.TrimLeft(parsed.Path, "/") == "" {
			return "", fmt.Errorf("browser: unusable UNC file URL %q", rawURL)
		}
		return `\\` + parsed.Host + strings.ReplaceAll(parsed.Path, "/", `\`), nil
	}
	trimmed := strings.TrimPrefix(parsed.Path, "/")
	if len(trimmed) < 2 || trimmed[1] != ':' {
		return "", fmt.Errorf("browser: file URL %q does not name a Windows path", rawURL)
	}
	return strings.ReplaceAll(trimmed, "/", `\`), nil
}

// windowsFileURL turns a Windows path into the file URL Chromium expects:
// a UNC path's host rides the URL authority (file://wsl.localhost/...),
// a drive path rides an empty one (file:///C:/...). Tag-free and pure so
// the shape is pinned by tests on every platform.
func windowsFileURL(windowsPath string) (string, error) {
	slashed := strings.ReplaceAll(windowsPath, `\`, "/")
	if host, ok := strings.CutPrefix(slashed, "//"); ok {
		hostname, share, found := strings.Cut(host, "/")
		if !found || hostname == "" || share == "" {
			return "", fmt.Errorf("browser: unusable UNC path %q", windowsPath)
		}
		return (&url.URL{Scheme: "file", Host: hostname, Path: "/" + share}).String(), nil
	}
	if len(slashed) < 2 || slashed[1] != ':' {
		return "", fmt.Errorf("browser: unusable Windows path %q", windowsPath)
	}
	return (&url.URL{Scheme: "file", Path: "/" + slashed}).String(), nil
}

// hostedProfile is one workspace's named CoreWebView2Profile in the
// launcher's pane environment.
type hostedProfile struct {
	engine    *hostedEngine
	id        string
	ephemeral bool
}

func (p *hostedProfile) Handle() string { return p.id }

func (p *hostedProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	return p.engine.createPage(ctx, p, hooks)
}

// AttachPage adopts a popup the launcher reported (hosted_popups.go).
func (p *hostedProfile) AttachPage(_ context.Context, handle string, hooks pageHooks) (pageDriver, error) {
	return p.engine.attachPopup(p, handle, hooks)
}

func (p *hostedProfile) CancelDownload(id string) error { return p.engine.cancelDownload(id) }

func (p *hostedProfile) Dispose(context.Context) error {
	p.engine.mu.Lock()
	delete(p.engine.policies, p.id)
	p.engine.mu.Unlock()
	p.engine.dispatch(webview2host.Directive{Op: webview2host.OpCloseProfile, ProfileID: p.id, Ephemeral: p.ephemeral})
	return nil
}

// dispatch validates before emitting. Both ends validate on purpose: the
// launcher's check is the trust boundary, and this one makes a bug fail at
// the line that wrote it instead of as a dropped frame in launcher.log.
func (e *hostedEngine) dispatch(directive webview2host.Directive) bool {
	// Start is the wiring validator for every other path, because nothing
	// dispatches before the Manager has started the engine. ClearSiteData
	// does — the launcher's folder holds site data whether or not this
	// process ever opened a page — so the check lives at the one point every
	// directive passes through rather than at that one call site.
	if e.send == nil {
		e.logf("browser: refusing to emit pane directive %q: the pane host is not wired", directive.Op)
		return false
	}
	if err := directive.Validate(); err != nil {
		e.logf("browser: refusing to emit pane directive %q: %v", directive.Op, err)
		return false
	}
	e.send(directive)
	return true
}
