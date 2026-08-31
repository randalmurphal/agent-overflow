package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/webview2host"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

// The hosted engine: the Windows/WSL implementation of the driver.go seam.
//
// Nothing about a page's OPERATIONS differs from managed Chrome — CDP is
// CDP, so `cdp_page.go` and its siblings drive a WebView2 controller
// unchanged. What differs is everything around a page's LIFETIME:
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
// Only the hosted engine implements it; managed Chrome has no window to
// show. The Manager decides WHICH page is presented (policy); the engine
// decides how a controller is made to appear (mechanism).
//
// SetPageBounds and OpenPageDevTools complete the launcher's directive
// vocabulary. Their callers are the pane's host-rect surface and its
// devtools affordance, both of which are the frontend wave's — the senders
// live here so that wave adds a call site and no protocol.
type paneHost interface {
	ShowPage(handle string)
	HidePage(handle string)
	SetPageBounds(handle string, x, y, width, height float64)
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

	// The two bounds are fields rather than the constants directly so a
	// test can drive the timeout paths without spending them.
	createTimeout time.Duration
	attachTimeout time.Duration

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
}

func newHostedEngine(relay hostRelay, send func(webview2host.Directive), events engineEvents) *hostedEngine {
	return &hostedEngine{
		relay:         relay,
		send:          send,
		events:        events,
		logf:          log.Printf,
		createTimeout: hostCreateTimeout,
		attachTimeout: hostAttachTimeout,

		pending:      make(map[string]chan hostReport),
		pageByTarget: make(map[string]string),
		targetByPage: make(map[string]string),
		shown:        make(map[string]bool),
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
	return nil
}

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
	// opts.Cookies is deliberately dropped. A hosted profile is a real
	// browser profile on disk (spec §4): WebView2 persists its own cookies,
	// and there is no CDP browser context to seed one into. Restoring the
	// AO checkpoint here would write a second, staler copy of state the
	// engine already holds.
	return &hostedProfile{engine: e, id: id, ephemeral: opts.Ephemeral}, nil
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

func (p *hostedProfile) NewPage(ctx context.Context, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
	return p.engine.createPage(ctx, p, hooks, restore)
}

// AttachPage would adopt a page the engine opened by itself. The launcher
// does not surface WebView2's NewWindowRequested, so no popup is ever
// reported and this is unreachable; failing loudly beats returning a
// driver for a controller nobody created.
func (p *hostedProfile) AttachPage(context.Context, string, pageHooks, map[string]map[string]string) (pageDriver, error) {
	return nil, errors.New("browser: the pane host does not report engine-opened popups")
}

// Cookies has nothing to checkpoint. Site data for a hosted profile lives
// in the launcher's own profile directory, per workspace, persisted by the
// engine (spec §4) — and a browser-wide CDP cookie read would cross the
// workspace boundary the profile exists to draw.
func (p *hostedProfile) Cookies(context.Context) ([]*network.CookieParam, error) {
	return nil, nil
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
func (e *hostedEngine) createPage(ctx context.Context, profile *hostedProfile, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
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
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks, restore)
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
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		e.logf("browser: chromedp: "+format, args...)
	}))
	// The first Run dials the tunnel and attaches to whichever page target
	// the browser reports, and chromedp bounds neither: a launcher that
	// named a target it never created would park this call forever. So the
	// wait is ours, on attachCtx, and a timeout tears the connection down
	// rather than leaving a half-built browser behind. Run itself keeps
	// browserCtx, whose lifetime is the connection's, so the success path
	// is unchanged.
	attached := make(chan error, 1)
	go func() { attached <- chromedp.Run(browserCtx) }()
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
	switch event := ev.(type) {
	case *target.EventTargetDestroyed:
		if pageID, ok := e.pageForTarget(string(event.TargetID)); ok {
			go e.retirePage(pageID)
		}
	case *target.EventTargetInfoChanged:
		if event.TargetInfo == nil || event.TargetInfo.Type != "page" {
			return
		}
		if pageID, ok := e.pageForTarget(string(event.TargetInfo.TargetID)); ok {
			e.events.PageInfoChanged(pageID, event.TargetInfo.URL, event.TargetInfo.Title)
		}
	case *cdpbrowser.EventDownloadWillBegin:
		e.events.DownloadStarted(downloadStart{
			Frame: string(event.FrameID), ID: event.GUID,
			URL: event.URL, SuggestedName: event.SuggestedFilename,
		})
	case *cdpbrowser.EventDownloadProgress:
		state := downloadInProgress
		switch event.State {
		case cdpbrowser.DownloadProgressStateCompleted:
			state = downloadCompleted
		case cdpbrowser.DownloadProgressStateCanceled:
			state = downloadCanceled
		}
		e.events.DownloadProgress(downloadProgress{
			ID: event.GUID, Received: event.ReceivedBytes, State: state, FilePath: event.FilePath,
		})
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
	default:
		e.logf("browser: ignoring unknown pane host report %q for page %s", kind, pageID)
	}
}

// retirePage drops the engine's bookkeeping and tells the Manager the page
// is gone. The Manager alone decides what that means for its registry.
func (e *hostedEngine) retirePage(pageID string) {
	e.mu.Lock()
	targetID, known := e.targetByPage[pageID]
	delete(e.targetByPage, pageID)
	delete(e.pageByTarget, targetID)
	delete(e.shown, pageID)
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

func (e *hostedEngine) SetPageBounds(handle string, x, y, width, height float64) {
	e.dispatch(webview2host.Directive{Op: webview2host.OpBounds, PageID: handle, X: x, Y: y, W: width, H: height})
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
