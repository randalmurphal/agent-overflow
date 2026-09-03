package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/webview2host"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
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
//     attaching chromedp.
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
	// one /json/version round trip, and the browser-level handshake.
	hostAttachTimeout = 30 * time.Second
	// hostClearTimeout bounds the wait for a `cleared` report. It is
	// deliberately the largest of the three: the launcher closes every
	// controller, releases the environment, and then retries deleting the
	// user-data folder for up to its own 15s budget while the WebView2
	// browser process lets go of its file handles.
	hostClearTimeout = 45 * time.Second
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

// paneHost is the engine capability the Manager's visibility path drives.
// Every windowed engine implements it; an engine with no window to show does
// not. The Manager decides WHICH page is presented (policy); the engine
// decides how a controller is made to appear (mechanism).
type paneHost interface {
	ShowPage(handle string)
	HidePage(handle string)
	// SetPageBounds carries the whole PaneRect: the host scales the
	// CSS-pixel rect by its own client size over the rect's viewport, so
	// no engine ever equates CSS pixels with its native units.
	SetPageBounds(handle string, rect PaneRect)
}

// paneDevTools is the OPTIONAL inspector half, split from paneHost because
// implementing it is a capability claim: WKWebView has no public call that
// opens its inspector (macOS devtools are Safari's Develop menu against an
// inspectable view), so the darwin engine deliberately does not implement
// this and the Manager's assertion failure — "devtools are not available on
// this browser engine" — is the true answer there.
type paneDevTools interface {
	OpenPageDevTools(handle string)
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

	// attachMu serializes ensureBrowser so a burst of concurrent creates
	// establishes one browser connection rather than one each.
	attachMu sync.Mutex

	mu            sync.Mutex
	started       bool
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	attachCancel  context.CancelFunc
	pending       map[string]chan hostReport
	pageByTarget  map[string]string
	targetByPage  map[string]string
	shown         map[string]bool
	bounds        map[string]PaneRect
}

func newHostedEngine(relay hostRelay, send func(webview2host.Directive), accelerators func() keybindings.AcceleratorSet, events engineEvents) *hostedEngine {
	return &hostedEngine{
		relay:         relay,
		send:          send,
		events:        events,
		accelerators:  accelerators,
		logf:          log.Printf,
		createTimeout: hostCreateTimeout,
		attachTimeout: hostAttachTimeout,
		clearTimeout:  hostClearTimeout,

		pending:      make(map[string]chan hostReport),
		pageByTarget: make(map[string]string),
		targetByPage: make(map[string]string),
		shown:        make(map[string]bool),
		bounds:       make(map[string]PaneRect),
	}
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
}

func (e *hostedEngine) Stop() {
	e.mu.Lock()
	browserCancel, allocCancel := e.browserCancel, e.allocCancel
	e.browserCtx, e.browserCancel, e.allocCancel = nil, nil, nil
	e.started = false
	e.pageByTarget = make(map[string]string)
	e.targetByPage = make(map[string]string)
	e.shown = make(map[string]bool)
	e.bounds = make(map[string]PaneRect)
	e.mu.Unlock()
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
}

// NewProfile derives a launcher profile id from the canonical workspace.
// Same isolation semantics as the managed-Chrome BrowserContext: one
// profile per workspace, threads on a workspace share logins. The digest
// is what makes the id stable across restarts, which is what makes those
// logins survive one.
func (e *hostedEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	id, err := hostedProfileID(opts.Workspace)
	if err != nil {
		return nil, err
	}
	// Site data is the engine's own (spec §4): a hosted profile is a real
	// browser profile on disk and WebView2 persists it, so AO seeds nothing
	// into it and reads nothing out of it.
	return &hostedProfile{engine: e, id: id, ephemeral: !opts.Persist}, nil
}

func hostedProfileID(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", errors.New("browser: workspace is required for a pane profile")
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
	converted, err := windowsPathFor(ctx, path)
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
	return runWSLPath(ctx, "-u", windowsPath)
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

// DiscardPage closes a controller the Manager declined to adopt. The handle
// is this engine's page id, because that is what pageDriver.Handle answers.
func (e *hostedEngine) DiscardPage(handle string) {
	e.closePage(handle)
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

// AttachPage would adopt a page the engine opened by itself. The launcher
// does not surface WebView2's NewWindowRequested, so no popup is ever
// reported and this is unreachable; failing loudly beats returning a
// driver for a controller nobody created.
func (p *hostedProfile) AttachPage(context.Context, string, pageHooks) (pageDriver, error) {
	return nil, errors.New("browser: the pane host does not report engine-opened popups")
}

// CancelDownload keeps the CDP path. Browser.cancelDownload's support in
// WebView2 is unverified; a refusal costs one download that finishes
// anyway, which the Manager's own byte caps still bound.
func (p *hostedProfile) CancelDownload(id string) {
	browserCtx, ok := p.engine.browser()
	if !ok {
		return
	}
	_ = cdpbrowser.CancelDownload(id).Do(browserCommandContext(browserCtx))
}

func (p *hostedProfile) Dispose(context.Context) error {
	p.engine.dispatch(webview2host.Directive{Op: webview2host.OpCloseProfile, ProfileID: p.id, Ephemeral: p.ephemeral})
	return nil
}

// createPage runs the full round trip: directive out, report back, chromedp
// attached to the target the report named.
func (e *hostedEngine) createPage(ctx context.Context, profile *hostedProfile, hooks pageHooks) (pageDriver, error) {
	pageID := newHostedPageID()
	waiter, err := e.watch(pageID)
	if err != nil {
		return nil, err
	}
	defer e.unwatch(pageID)

	if !e.dispatch(webview2host.Directive{Op: webview2host.OpCreate, PageID: pageID, ProfileID: profile.id, Ephemeral: profile.ephemeral}) {
		return nil, errors.New("browser: pane host refused the create directive")
	}

	report, err := awaitHostReport(ctx, waiter, e.createTimeout)
	if err != nil {
		// The launcher may still be mid-create; a close for a page it has
		// not finished is harmless, and skipping it would leak a controller.
		e.closePage(pageID)
		return nil, fmt.Errorf("browser: create pane page: %w", err)
	}
	if report.kind != webview2host.ReportCreated {
		return nil, fmt.Errorf("browser: pane host reported %s: %s", report.kind, report.detail)
	}
	targetID := strings.TrimSpace(report.detail)
	if targetID == "" {
		e.closePage(pageID)
		return nil, errors.New("browser: pane host reported a page with no CDP target")
	}

	browserCtx, err := e.ensureBrowser()
	if err != nil {
		e.closePage(pageID)
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(targetID)))
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
	if err != nil {
		e.closePage(pageID)
		return nil, err
	}
	e.bind(pageID, targetID)
	return &hostedPage{pageDriver: driver, engine: e, id: pageID}, nil
}

// ensureBrowser establishes (once) the CDP browser connection through the
// tunnel.
//
// Bounded on its own clock rather than the caller's: two tool calls can
// race here, and the first one's cancellation must not abort the
// connection the second is about to share. Interrupt cancels it for
// shutdown.
func (e *hostedEngine) ensureBrowser() (context.Context, error) {
	if browserCtx, ok := e.browser(); ok {
		return browserCtx, nil
	}
	e.attachMu.Lock()
	defer e.attachMu.Unlock()
	if browserCtx, ok := e.browser(); ok {
		return browserCtx, nil
	}

	attachCtx, attachCancel := context.WithTimeout(context.Background(), e.attachTimeout)
	e.mu.Lock()
	e.attachCancel = attachCancel
	e.mu.Unlock()
	defer func() {
		attachCancel()
		e.mu.Lock()
		e.attachCancel = nil
		e.mu.Unlock()
	}()

	wsURL, err := e.relay.BrowserWebSocketURL(attachCtx)
	if err != nil {
		return nil, fmt.Errorf("browser: reach the pane CDP endpoint: %w", err)
	}
	// NoModifyURL: the URL was already rewritten onto the relay listener,
	// and chromedp's own /json/version probe would re-read the Windows-side
	// address and dial it.
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), wsURL, chromedp.NoModifyURL)
	// The error logger rides the Allocate call in dialCDPBrowser, not a
	// NewContext option: chromedp.WithErrorf lands in an unexported field
	// only Run's own allocation path reads, and this dial bypasses Run.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	// The dial must NOT go through chromedp.Run, because WebView2 answers
	// its Target.createTarget with `-32000 no browser is open` — a WebView2
	// target exists only as a launcher-created controller (the spike's
	// "WebView2 has no /json/new"). dialCDPBrowser (cdp_page.go) is that
	// target-less dial, shared with the headless engine, and it is also the
	// only sender of the target discovery the targetDestroyed backstop in
	// dispatchEvent reads.
	//
	// chromedp bounds none of this: a launcher that never answers would park
	// the dial forever. So the wait is ours, on attachCtx, and a timeout
	// tears the connection down rather than leaving a half-built browser
	// behind.
	attached := make(chan error, 1)
	go func() { attached <- dialCDPBrowser(browserCtx, e.logf) }()
	select {
	case err := <-attached:
		if err != nil {
			browserCancel()
			allocCancel()
			return nil, fmt.Errorf("browser: attach to the pane browser: %w", err)
		}
	case <-attachCtx.Done():
		browserCancel()
		allocCancel()
		return nil, fmt.Errorf("browser: attach to the pane browser: %w", attachCtx.Err())
	}
	chromedp.ListenBrowser(browserCtx, e.dispatchEvent)
	e.mu.Lock()
	e.browserCtx, e.browserCancel, e.allocCancel = browserCtx, browserCancel, allocCancel
	e.mu.Unlock()
	return browserCtx, nil
}

func (e *hostedEngine) browser() (context.Context, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browserCtx == nil || e.browserCtx.Err() != nil {
		return nil, false
	}
	return e.browserCtx, true
}

// dispatchEvent translates the browser-level CDP stream into the seam's
// vocabulary. Target ids are the engine's private business, so every event
// is re-keyed onto the page id the Manager knows; an event for a target
// this engine did not create is dropped rather than reported under a
// handle nobody owns.
//
// Lifecycle is the launcher's to report (`closed` / `process-failed`), and
// targetDestroyed is the backstop for a controller that dies without one.
// Popups are not wired: the launcher does not surface WebView2's
// NewWindowRequested, so no page can be created behind the Manager's back.
func (e *hostedEngine) dispatchEvent(ev any) {
	// Downloads carry no engine identity — the GUID is the handle on every
	// CDP engine — so they are translated by the shared helper.
	if cdpDownloadEvent(ev, e.events) {
		return
	}
	switch event := ev.(type) {
	case *target.EventTargetDestroyed:
		if pageID, ok := e.pageForTarget(string(event.TargetID)); ok {
			// Off the CDP listener goroutine, which is the rule both CDP
			// engines follow and headless_profile.go's dispatchEvent spells
			// out: retirePage ends in the Manager's teardown, and that
			// teardown waits on this same browser connection.
			go e.retirePage(pageID)
		}
	case *target.EventTargetInfoChanged:
		if event.TargetInfo == nil || event.TargetInfo.Type != "page" {
			return
		}
		if pageID, ok := e.pageForTarget(string(event.TargetInfo.TargetID)); ok {
			e.events.PageInfoChanged(pageID, event.TargetInfo.URL, event.TargetInfo.Title)
		}
	}
}

// Report routes one launcher answer. Called from the App's
// BrowserHostReport binding, which is the backend end of the RPC the
// launcher posts over the notification bridge.
func (e *hostedEngine) Report(pageID string, kind webview2host.ReportKind, detail string) {
	switch kind {
	case webview2host.ReportCreated:
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			// Nobody is waiting: the create already timed out or was
			// abandoned. The controller is real, so close it rather than
			// leaving an orphan window in the launcher.
			e.closePage(pageID)
		}
	case webview2host.ReportCreateFailed:
		e.deliver(pageID, hostReport{kind: kind, detail: detail})
	case webview2host.ReportProcessFailed:
		// A process death can land on a page still being created OR on a
		// live one. Whichever it is, the page is gone.
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.retirePage(pageID)
		}
	case webview2host.ReportClosed:
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.retirePage(pageID)
		}
	case webview2host.ReportAccelerator:
		// The launcher already matched and swallowed the chord; the Manager
		// re-matches (same set) and routes it to the owning thread.
		var pressed keybindings.Accelerator
		if err := json.Unmarshal([]byte(detail), &pressed); err != nil {
			e.logf("browser: pane host accelerator for page %s is unreadable: %v", pageID, err)
			return
		}
		e.events.KeyChord(pageID, pressed)
	case webview2host.ReportCleared, webview2host.ReportClearFailed:
		// A clear names a correlation id, not a page: there is no controller
		// to close and no page to retire. Nobody waiting means ClearSiteData
		// already gave up on its own bound and has reported that.
		if !e.deliver(pageID, hostReport{kind: kind, detail: detail}) {
			e.logf("browser: pane host reported %s for an abandoned clear %s", kind, pageID)
		}
	default:
		e.logf("browser: ignoring unknown pane host report %q for page %s", kind, pageID)
	}
}

// retirePage drops the engine's bookkeeping and tells the Manager the page
// is gone. The Manager alone decides what that means for its registry.
//
// PageClosed is called inline here because every SYNCHRONOUS caller is the
// launcher's report path, which is an RPC goroutine of its own. The one
// caller that is a CDP listener goroutine starts this on a goroutine
// instead, and must (dispatchEvent above).
func (e *hostedEngine) retirePage(pageID string) {
	e.mu.Lock()
	targetID, known := e.targetByPage[pageID]
	delete(e.targetByPage, pageID)
	delete(e.pageByTarget, targetID)
	delete(e.shown, pageID)
	delete(e.bounds, pageID)
	e.mu.Unlock()
	if !known {
		return
	}
	e.events.PageClosed(pageID)
}

func (e *hostedEngine) watch(pageID string) (chan hostReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.pending[pageID]; exists {
		return nil, fmt.Errorf("browser: pane page %s is already being created", pageID)
	}
	waiter := make(chan hostReport, 1)
	e.pending[pageID] = waiter
	return waiter, nil
}

func (e *hostedEngine) unwatch(pageID string) {
	e.mu.Lock()
	delete(e.pending, pageID)
	e.mu.Unlock()
}

// deliver hands one report to a pending create. Reports whether a waiter
// took it, which is what tells the caller a late or unmatched report needs
// its own handling.
func (e *hostedEngine) deliver(pageID string, report hostReport) bool {
	e.mu.Lock()
	waiter, ok := e.pending[pageID]
	if ok {
		delete(e.pending, pageID)
	}
	e.mu.Unlock()
	if !ok {
		return false
	}
	waiter <- report
	return true
}

func (e *hostedEngine) bind(pageID, targetID string) {
	e.mu.Lock()
	e.targetByPage[pageID] = targetID
	e.pageByTarget[targetID] = pageID
	e.mu.Unlock()
}

func (e *hostedEngine) pageForTarget(targetID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pageID, ok := e.pageByTarget[targetID]
	return pageID, ok
}

// closePage retires the bookkeeping and asks the launcher to destroy the
// controller. The launcher answers `closed`, which is a no-op by then —
// the close a page's own Close already accounted for must not be reported
// to the Manager twice.
func (e *hostedEngine) closePage(pageID string) {
	e.mu.Lock()
	targetID := e.targetByPage[pageID]
	delete(e.targetByPage, pageID)
	delete(e.pageByTarget, targetID)
	delete(e.shown, pageID)
	delete(e.bounds, pageID)
	e.mu.Unlock()
	e.dispatch(webview2host.Directive{Op: webview2host.OpClose, PageID: pageID})
}

func (e *hostedEngine) ShowPage(handle string) {
	if e.setShown(handle, true) {
		e.dispatch(webview2host.Directive{Op: webview2host.OpShow, PageID: handle})
	}
}

func (e *hostedEngine) HidePage(handle string) {
	if e.setShown(handle, false) {
		e.dispatch(webview2host.Directive{Op: webview2host.OpHide, PageID: handle})
	}
}

func (e *hostedEngine) SetPageBounds(handle string, rect PaneRect) {
	if e.setBounds(handle, rect) {
		e.dispatch(webview2host.Directive{
			Op: webview2host.OpBounds, PageID: handle,
			X: rect.X, Y: rect.Y, W: rect.Width, H: rect.Height,
			CX: rect.ClipX, CY: rect.ClipY, CW: rect.ClipWidth, CH: rect.ClipHeight,
			VW: rect.ViewportWidth, VH: rect.ViewportHeight,
			Bg: rect.Background,
		})
	}
}

// setBounds is the bounds half of the setShown dedupe: the presentation sync
// re-sends the active page's rect on every selection, focus and page-list
// change, and only an actually-moved rect should cost a directive.
func (e *hostedEngine) setBounds(handle string, rect PaneRect) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, known := e.targetByPage[handle]; !known {
		return false
	}
	if e.bounds[handle] == rect {
		return false
	}
	e.bounds[handle] = rect
	return true
}

func (e *hostedEngine) OpenPageDevTools(handle string) {
	e.dispatch(webview2host.Directive{Op: webview2host.OpDevTools, PageID: handle})
}

// setShown reports whether the visibility actually changed for a page this
// engine still owns. Visibility is recomputed on every selection, focus
// and page-list change, so re-emitting an unchanged state would put a
// directive on the wire for every one of them.
func (e *hostedEngine) setShown(handle string, visible bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, known := e.targetByPage[handle]; !known {
		return false
	}
	if e.shown[handle] == visible {
		return false
	}
	e.shown[handle] = visible
	return true
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

// awaitHostReport waits for one report, the caller's cancellation, or the
// bound. Every path returns; a launcher that never answers is an error,
// never a hang.
func awaitHostReport(ctx context.Context, waiter chan hostReport, timeout time.Duration) (hostReport, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case report, ok := <-waiter:
		if !ok {
			return hostReport{}, errors.New("pane host was interrupted")
		}
		return report, nil
	case <-ctx.Done():
		return hostReport{}, ctx.Err()
	case <-timer.C:
		return hostReport{}, fmt.Errorf("pane host did not answer within %s", timeout)
	}
}

func newHostedPageID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// hostedPage is one launcher-hosted controller. Every operation is the CDP
// driver's; only destruction differs, because detaching chromedp leaves a
// real window on the user's screen.
type hostedPage struct {
	pageDriver
	engine    *hostedEngine
	id        string
	closeOnce sync.Once
}

// Handle is the PAGE id, not the CDP target id: it is what the launcher's
// reports name and what every directive addresses, so it is the one handle
// both halves of this engine agree on.
func (p *hostedPage) Handle() string { return p.id }

func (p *hostedPage) Close() {
	p.pageDriver.Close()
	p.closeOnce.Do(func() { p.engine.closePage(p.id) })
}
