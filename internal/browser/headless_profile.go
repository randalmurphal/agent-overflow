package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// headlessProfile is one canonical workspace's Chromium: its own process,
// its own user-data directory, its own cookie jar. The process is launched
// by the first page and dies with the profile.
type headlessProfile struct {
	engine      *headlessEngine
	handle      string
	userDataDir string
	// ephemeralRoot is the temp directory holding userDataDir when the
	// site-data setting is off, removed on Dispose. Empty when the profile
	// persists, and that emptiness is what stops a persisted workspace's
	// logins from being deleted.
	ephemeralRoot string
	downloadDir   string
	// allow is the workspace's navigation policy, which connect enables
	// browser-wide.
	allow func(url string) bool

	// launchMu serializes ensureBrowser so a burst of concurrent page
	// creations launches one Chromium rather than one each.
	launchMu sync.Mutex

	mu           sync.Mutex
	disposed     bool
	launchCancel context.CancelFunc
	current      *launchedBrowser
}

// launchedBrowser is one Chromium this profile started and its CDP
// connection. The browser context is cancelled when the connection is lost,
// so a context that is done means the browser is gone.
type launchedBrowser struct {
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	process     *chromiumProcess
}

// close ends the connection, then kills the browser and returns once nothing
// it started can still write. An error means some of it may still be
// running.
func (b *launchedBrowser) close() error {
	b.cancel()
	b.allocCancel()
	if b.process == nil {
		// A browser without a process of its own, as the lifetime tests
		// build, has nothing to stop.
		return nil
	}
	return b.process.stop()
}

// closeBrowser ends a browser that served this profile's pages. A persisted
// profile's Chromium is asked to exit before it is killed, because it writes
// some site data only on the way out: cookies are committed in batches every
// thirty seconds, so a kill alone loses a login made just before the profile
// closed. The connection ends first, so nothing the exit reports reaches
// the event handlers. An ephemeral profile's directory is removed next, so its
// Chromium is only killed.
func (p *headlessProfile) closeBrowser(b *launchedBrowser) error {
	if p.ephemeralRoot == "" && b.process != nil {
		b.cancel()
		b.allocCancel()
		if err := b.process.close(p.engine.closeTimeout); err != nil {
			p.engine.logf("browser: profile %s: Chromium did not exit when asked, so it is killed: %v", p.handle, err)
		}
	}
	return b.close()
}

func (p *headlessProfile) Handle() string { return p.handle }

func (p *headlessProfile) NewPage(_ context.Context, hooks pageHooks) (pageDriver, error) {
	browserCtx, err := p.ensureBrowser()
	if err != nil {
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
	if err != nil {
		return nil, err
	}
	if err := p.bindPage(browserCtx, driver.Handle()); err != nil {
		driver.Close()
		return nil, err
	}
	return driver, nil
}

// AttachPage adopts a page Chromium opened by itself: a popup, reported by
// its target events and already bound to this profile. Unlike the launcher-hosted
// engine, this one CAN report popups, because CDP surfaces every target.
func (p *headlessProfile) AttachPage(_ context.Context, handle string, hooks pageHooks) (pageDriver, error) {
	browserCtx, ok := p.browser()
	if !ok {
		return nil, errors.New("browser: the profile's browser is no longer running")
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(handle)))
	driver, err := startCDPPage(browserCtx, pageCtx, pageCancel, hooks)
	if err != nil {
		return nil, err
	}
	if err := p.bindPage(browserCtx, driver.Handle()); err != nil {
		driver.Close()
		return nil, err
	}
	return driver, nil
}

// bindPage binds a page created on browserCtx to this profile while that
// browser is still the profile's and still connected. The check and the
// bind share p.mu with the step that takes a lost browser's pages
// (watchBrowser or retireLostBrowser), so a page is either bound before
// that step and reported closed by it, or refused.
func (p *headlessProfile) bindPage(browserCtx context.Context, handle string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.ctx != browserCtx || browserCtx.Err() != nil {
		return errors.New("browser: the profile's browser is no longer running")
	}
	p.engine.bindPage(handle, p)
	return nil
}

func (p *headlessProfile) CancelDownload(id string) {
	browserCtx, ok := p.browser()
	if !ok {
		return
	}
	if _, err := chromedp.CallBrowser(browserCtx, cdpbrowser.CancelDownload, cdpbrowser.CancelDownloadParams{GUID: id}); err != nil {
		p.engine.logf("browser: profile %s: cancel download %s: %v", p.handle, id, err)
	}
}

// Dispose destroys the profile and everything in it: cancelling the browser
// context drops every page context under it, and Dispose WAITS until every
// process Chromium started has exited (closeBrowser). The wait is the point:
// an ephemeral profile's directory is removed next, and removing it out
// from under a live Chromium would leave the files it recreates behind. A
// launch in flight holds a process the profile has no handle on yet, so
// Dispose cancels it and waits on launchMu until the launch has stopped it.
func (p *headlessProfile) Dispose(context.Context) error {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil
	}
	current, launchCancel := p.disposeLocked()
	p.mu.Unlock()
	return p.tearDown(current, launchCancel)
}

// disposeLocked marks the profile disposed and takes the browser and the
// launch cancel tearDown ends. The caller holds p.mu.
func (p *headlessProfile) disposeLocked() (*launchedBrowser, context.CancelFunc) {
	p.disposed = true
	current, launchCancel := p.current, p.launchCancel
	p.current, p.launchCancel = nil, nil
	return current, launchCancel
}

// tearDown is the rest of Dispose, after disposeLocked.
func (p *headlessProfile) tearDown(current *launchedBrowser, launchCancel context.CancelFunc) error {
	if launchCancel != nil {
		launchCancel()
	}
	// Waits for a launch in flight to return, its process reaped.
	p.launchMu.Lock()
	p.launchMu.Unlock()
	var err error
	if current != nil {
		err = p.closeBrowser(current)
	}
	p.engine.forgetProfile(p)
	if err != nil {
		// The process may still be using the directory. Its owner marker
		// names this backend, so the start-up sweep reclaims it once this
		// backend is gone.
		return fmt.Errorf("browser: stop the profile's browser: %w", err)
	}
	return p.removeEphemeralRoot()
}

// removeEphemeralRoot is the whole of "browserPersistSiteData=false". A
// failure is reported rather than logged: the user was promised the session
// left nothing behind, and cookies still on disk is exactly the thing they
// would want to hear about.
func (p *headlessProfile) removeEphemeralRoot() error {
	if p.ephemeralRoot == "" {
		return nil
	}
	if err := removeEphemeral(p.ephemeralRoot); err != nil {
		return fmt.Errorf("browser: remove ephemeral site data: %w", err)
	}
	return nil
}

// interrupt releases an in-flight launch for a concurrent shutdown. It does
// not tear a launched browser down; Dispose does that.
func (p *headlessProfile) interrupt() {
	p.mu.Lock()
	cancel := p.launchCancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *headlessProfile) browser() (context.Context, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.ctx.Err() != nil {
		return nil, false
	}
	return p.current.ctx, true
}

// ensureBrowser launches this profile's Chromium once. It launches again
// only when it finds the connection lost before watchBrowser has ended the
// profile, and then retires the lost browser first.
//
// Bounded on its OWN clock rather than the caller's, like the hosted
// engine's attach and for the same reason: two tool calls can race here,
// and the first one's cancellation must not kill the browser the second is
// about to share. Interrupt and Dispose cancel it. Every step, from the
// spawn to the download setup, ends when that clock does, and a failed
// launch returns only after its process is reaped.
func (p *headlessProfile) ensureBrowser() (context.Context, error) {
	if browserCtx, ok := p.browser(); ok {
		return browserCtx, nil
	}
	p.launchMu.Lock()
	defer p.launchMu.Unlock()
	if browserCtx, ok := p.browser(); ok {
		return browserCtx, nil
	}
	p.retireLostBrowser()

	launchCtx, launchCancel := context.WithTimeout(context.Background(), headlessLaunchTimeout)
	defer launchCancel()
	if err := p.armLaunch(launchCancel); err != nil {
		return nil, err
	}
	defer p.armLaunch(nil)

	launched, err := p.launch(launchCtx)
	if err != nil {
		return nil, err
	}
	if err := p.adopt(launched); err != nil {
		return nil, errors.Join(err, launched.close())
	}
	return launched.ctx, nil
}

// launch starts a Chromium on this profile's user-data directory and
// connects to it, both bounded by launchCtx. A failed launch returns only
// after its process is reaped.
func (p *headlessProfile) launch(launchCtx context.Context) (*launchedBrowser, error) {
	process, wsURL, err := startChromium(launchCtx, p.engine.binary, chromiumArgs(p.userDataDir), chromiumEnv(p.userDataDir))
	if err != nil {
		return nil, p.launchFailed(err)
	}
	// NoModifyURL: the DevTools line is already the browser websocket URL.
	// chromedp's default would resolve its host again, or query
	// /json/version for a URL without a /devtools/browser/ path.
	allocCtx, allocCancel := remote.NewAllocator(context.Background(), wsURL, remote.NoModifyURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		p.engine.logf("browser: chromedp: "+format, args...)
	}))
	launched := &launchedBrowser{ctx: browserCtx, cancel: browserCancel, allocCancel: allocCancel, process: process}

	// chromedp bounds the handshake with nothing but the dial's own
	// timeout, so the wait is ours: a Chromium that starts and never
	// answers is an error, never a hang.
	connected := make(chan error, 1)
	go func() { connected <- p.connect(browserCtx) }()
	select {
	case err = <-connected:
	case <-launchCtx.Done():
		err = p.launchFailed(launchCtx.Err())
	}
	if err != nil {
		return nil, errors.Join(err, launched.close())
	}
	return launched, nil
}

// connect dials the launched browser with this profile's event handlers,
// pins its downloads and installs the workspace's navigation policy.
func (p *headlessProfile) connect(browserCtx context.Context) error {
	if err := dialCDPBrowser(browserCtx, func() error { return p.subscribe(browserCtx) }); err != nil {
		return p.launchFailed(err)
	}
	// Downloads land ONLY in the AO artifact directory, never the operator's
	// Downloads folder, and allowAndName is what the Manager's own artifact
	// bookkeeping reads: it names each file by its download GUID, which is
	// the handle downloadProgress carries and the name downloads.go renames
	// from. Events are what make Browser.downloadWillBegin/downloadProgress
	// arrive at all.
	if _, err := chromedp.CallBrowser(browserCtx, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{
		Behavior: cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName, DownloadPath: p.downloadDir, EventsEnabled: new(true),
	}); err != nil {
		return fmt.Errorf("browser: pin downloads to %s: %w", p.downloadDir, err)
	}
	// Chromium runs a popup before the Manager can adopt it, and a
	// noopener popup's renderer cannot be held for AttachPage, so the
	// policy is enabled browser-wide as well as on each page. A page's
	// requests pass both.
	if _, err := chromedp.CallBrowser(browserCtx, fetch.Enable, fetch.EnableParams{Patterns: navigationPolicyPatterns}); err != nil {
		return fmt.Errorf("browser: install the workspace navigation policy: %w", err)
	}
	return nil
}

// adopt publishes a freshly launched browser as this profile's. The browser
// it replaces, if any, was retired before the launch (retireLostBrowser),
// and a profile never holds two.
func (p *headlessProfile) adopt(launched *launchedBrowser) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return errors.New("browser: the workspace profile was disposed while its browser started")
	}
	if p.current != nil {
		return errors.New("browser: the workspace profile already has a browser")
	}
	p.current = launched
	go p.watchBrowser(launched.ctx)
	return nil
}

// retireLostBrowser ends the browser a relaunch replaces, before the launch,
// so two Chromiums never share the user-data directory. It is the relaunch's
// side of the race with watchBrowser: both wake when the connection is lost,
// and whichever takes p.mu first takes the lost browser's pages and reports
// them closed. The other finds nothing to report.
//
// The caller holds launchMu, and the report runs on its own goroutine:
// PageClosed can reach Dispose, which waits on launchMu.
func (p *headlessProfile) retireLostBrowser() {
	p.mu.Lock()
	lost := p.current
	if lost == nil || lost.ctx.Err() == nil {
		p.mu.Unlock()
		return
	}
	p.current = nil
	pages := p.engine.pagesOf(p)
	p.mu.Unlock()

	if err := p.closeBrowser(lost); err != nil {
		p.engine.logf("browser: profile %s did not stop the browser it lost: %v", p.handle, err)
	}
	if len(pages) > 0 {
		go p.reportClosed(pages)
	}
}

// watchBrowser ends the profile when its Chromium does.
//
// chromedp cancels the BROWSER CONTEXT when the connection to the process
// is lost (the remote allocator's LostConnection goroutine cancels it), so
// a browser that crashed, was OOM-killed, or was killed by hand is
// observable here and nowhere else. Without it the profile's pages stayed
// in the Manager as rows nothing could drive: every operation on one
// answered "the profile's browser is no longer running" and no event ever
// said the page had gone.
//
// Its own goroutine, deliberately not the CDP listener: the teardown blocks
// until Chromium is reaped, and PageClosed is the Manager's page teardown.
func (p *headlessProfile) watchBrowser(browserCtx context.Context) {
	<-browserCtx.Done()

	p.mu.Lock()
	// Nothing to do, silently, when Dispose cancelled this context, or when
	// a relaunch already retired this browser and reported its pages. Both
	// leave the profile without it.
	if p.current == nil || p.current.ctx != browserCtx {
		p.mu.Unlock()
		return
	}
	// Taking the pages and disposing in one hold of p.mu is what reports
	// each page once: a relaunch after this finds no browser to retire and
	// may not launch, and bindPage refuses any later page.
	pages := p.engine.pagesOf(p)
	current, launchCancel := p.disposeLocked()
	p.mu.Unlock()

	// The teardown comes before the report, so the Dispose a report can
	// trigger returns with Chromium already reaped rather than as a no-op
	// while it is still running.
	if err := p.tearDown(current, launchCancel); err != nil {
		p.engine.logf("browser: profile %s did not clean up after its browser died: %v", p.handle, err)
	}
	p.reportClosed(pages)
}

// reportClosed tells the Manager each of pages has closed, in order.
func (p *headlessProfile) reportClosed(pages []string) {
	for _, handle := range pages {
		p.engine.events.PageClosed(handle)
	}
}

// armLaunch publishes (or clears) the cancel Interrupt reaches the launch
// through, refusing to start one on a disposed profile.
func (p *headlessProfile) armLaunch(cancel context.CancelFunc) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return errors.New("browser: the workspace profile is disposed")
	}
	p.launchCancel = cancel
	return nil
}

// launchFailed frames a failed launch with the binary that failed. A reason
// from startChromium carries the tail of what the browser printed, which is
// where a sandbox refusal is legible.
func (p *headlessProfile) launchFailed(reason error) error {
	return fmt.Errorf("browser: launch Chromium at %s: %w", p.engine.binary, reason)
}

// closeTarget destroys one page of this profile without a driver for it —
// the popup the Manager declined.
func (p *headlessProfile) closeTarget(handle string) {
	browserCtx, ok := p.browser()
	if !ok {
		return
	}
	p.engine.unbindPage(handle)
	if _, err := chromedp.CallBrowser(browserCtx, target.CloseTarget, target.CloseTargetParams{TargetID: target.ID(handle)}); err != nil {
		p.engine.logf("browser: close discarded page %s: %v", handle, err)
	}
}

// subscribe connects the CDP events of the browser connected on browserCtx
// to the seam, and answers the requests the workspace's navigation policy
// paused on it.
//
// A page's handle IS its CDP target id here (there is no second identity to
// re-key onto, unlike the launcher-hosted engine), so the engine's whole
// bookkeeping is which profile owns which target. A target this engine never
// bound is dropped rather than reported under a handle nobody owns.
func (p *headlessProfile) subscribe(browserCtx context.Context) error {
	logf := p.engine.logf
	if err := newDownloadTracker(p.engine.events, p.CancelDownload).subscribe(browserCtx, logf); err != nil {
		return err
	}
	session := chromedp.FromContext(browserCtx).Browser
	if err := onBrowserEvent(browserCtx, fetch.RequestPaused, logf, func(event fetch.EventRequestPaused) {
		answerPausedRequest(browserCtx, session, event, p.allow)
	}); err != nil {
		return err
	}
	targets := newHeadlessTargets(p)
	if err := onBrowserEvent(browserCtx, target.TargetCreated, logf, targets.created); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetInfoChanged, logf, targets.infoChanged); err != nil {
		return err
	}
	return onBrowserEvent(browserCtx, target.TargetDestroyed, logf, targets.destroyed)
}

// headlessTargets handles the target events of one browser connection.
//
// targetCreated, targetInfoChanged and targetDestroyed arrive on three
// subscriptions, so a target id is in one of these states: known (created
// handled, destroyed not), gone (destroyed handled, whether or not created
// was), or info held (an info change handled before created: the newest is
// kept in heldInfo). A created for a gone target neither binds nor reports
// it: a popup is never reported after it is gone. A created that finds held
// info reports the popup with that newer info, or delivers it as an info
// change for a page the Manager created and bound. An info change for a gone
// target is dropped.
//
// known holds every target the browser has, popup or not. The browser's
// live targets bound it, and it goes with the connection. gone is kept to
// absorb a gone target's late created and info changes, and it and heldInfo
// are bounded by orderSlack.
//
// Binding and unbinding happen under mu, so a popup cannot be bound after
// the destroyed handler has looked for it. PageInfoChanged is delivered
// outside mu but under deliver, taken before mu is released, so the Manager
// sees info changes in the order mu handled them: a held info delivered by
// created is never overtaken by a newer change handled after it.
type headlessTargets struct {
	profile *headlessProfile

	mu       sync.Mutex
	known    map[target.ID]struct{}
	gone     *boundedMap[target.ID, struct{}]
	heldInfo *boundedMap[target.ID, *target.Info]

	deliver sync.Mutex
}

func newHeadlessTargets(p *headlessProfile) *headlessTargets {
	return &headlessTargets{
		profile:  p,
		known:    make(map[target.ID]struct{}),
		gone:     newBoundedMap[target.ID, struct{}](orderSlack),
		heldInfo: newBoundedMap[target.ID, *target.Info](orderSlack),
	}
}

func (h *headlessTargets) created(event target.EventTargetCreated) {
	info := event.TargetInfo
	if info == nil {
		return
	}
	h.mu.Lock()
	if _, gone := h.gone.get(info.TargetID); gone {
		h.mu.Unlock()
		return
	}
	h.known[info.TargetID] = struct{}{}
	url, title := info.URL, info.Title
	newer, held := h.heldInfo.take(info.TargetID)
	if held {
		url, title = newer.URL, newer.Title
	}
	p := h.profile
	handle := string(info.TargetID)
	// An opener is what makes a target a POPUP: every page the Manager
	// asked for is created without one, and the workers, iframes and
	// service workers Chromium also reports are not pages at all.
	if info.Type != "page" || info.OpenerID == "" {
		// A page the Manager created is bound before its events arrive, so
		// an info change held for it is delivered now.
		if _, bound := p.engine.profileForPage(handle); !held || !bound {
			h.mu.Unlock()
			return
		}
		h.deliver.Lock()
		h.mu.Unlock()
		defer h.deliver.Unlock()
		p.engine.events.PageInfoChanged(handle, url, title)
		return
	}
	defer h.mu.Unlock()
	p.engine.bindPage(handle, p)
	// On its own goroutine, as the WebKit engines report popups: the
	// Manager adopts a popup through AttachPage, a CDP round trip that must
	// not hold the target events queued behind it.
	go p.engine.events.PopupOpened(enginePopup{
		Profile: p.handle, Opener: string(info.OpenerID), Handle: string(info.TargetID),
		URL: url, Title: title,
	})
}

func (h *headlessTargets) infoChanged(event target.EventTargetInfoChanged) {
	info := event.TargetInfo
	if info == nil || info.Type != "page" {
		return
	}
	h.mu.Lock()
	_, known := h.known[info.TargetID]
	if !known {
		if _, gone := h.gone.get(info.TargetID); !gone {
			h.heldInfo.put(info.TargetID, info)
		}
		h.mu.Unlock()
		return
	}
	p := h.profile
	if _, bound := p.engine.profileForPage(string(info.TargetID)); !bound {
		h.mu.Unlock()
		return
	}
	h.deliver.Lock()
	h.mu.Unlock()
	defer h.deliver.Unlock()
	p.engine.events.PageInfoChanged(string(info.TargetID), info.URL, info.Title)
}

func (h *headlessTargets) destroyed(event target.EventTargetDestroyed) {
	h.mu.Lock()
	delete(h.known, event.TargetID)
	h.heldInfo.delete(event.TargetID)
	h.gone.put(event.TargetID, struct{}{})
	// A page the Manager created is bound before its created event may be
	// handled, so the unbind does not depend on the state above.
	p := h.profile
	handle := string(event.TargetID)
	bound := p.engine.unbindPage(handle)
	h.mu.Unlock()
	if !bound {
		return
	}
	// On its own goroutine for the same reason as the popup above, and as
	// the hosted engine retires a page: PageClosed is the Manager's page
	// teardown, which on the workspace's last page disposes this profile and
	// blocks until Chromium is reaped.
	go p.engine.events.PageClosed(handle)
}

// retained reports how many target ids the handler keeps.
func (h *headlessTargets) retained() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.known) + h.gone.len() + h.heldInfo.len()
}
