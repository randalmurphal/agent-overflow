package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
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

	// launchMu serializes ensureBrowser so a burst of concurrent page
	// creations launches one Chromium rather than one each.
	launchMu sync.Mutex

	mu            sync.Mutex
	disposed      bool
	launchCancel  context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	allocCancel   context.CancelFunc
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
	p.engine.bindPage(driver.Handle(), p)
	return driver, nil
}

// AttachPage adopts a page Chromium opened by itself — a popup, reported by
// dispatchEvent and already bound to this profile. Unlike the launcher-hosted
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
	p.engine.bindPage(driver.Handle(), p)
	return driver, nil
}

func (p *headlessProfile) CancelDownload(id string) {
	browserCtx, ok := p.browser()
	if !ok {
		return
	}
	_ = cdpbrowser.CancelDownload(id).Do(browserCommandContext(browserCtx))
}

// Dispose destroys the profile and everything in it: cancelling the browser
// context drops every page context under it, and cancelling the allocator
// kills the process and WAITS for it to be reaped. The wait is the point —
// an ephemeral profile's directory is removed next, and removing it out
// from under a live Chromium would leave the files it recreates behind.
func (p *headlessProfile) Dispose(context.Context) error {
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return nil
	}
	p.disposed = true
	browserCancel, allocCancel, launchCancel := p.browserCancel, p.allocCancel, p.launchCancel
	p.browserCtx, p.browserCancel, p.allocCancel, p.launchCancel = nil, nil, nil, nil
	p.mu.Unlock()

	if launchCancel != nil {
		launchCancel()
	}
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
	p.engine.forgetProfile(p)
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
	if err := os.RemoveAll(p.ephemeralRoot); err != nil {
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
	if p.browserCtx == nil || p.browserCtx.Err() != nil {
		return nil, false
	}
	return p.browserCtx, true
}

// ensureBrowser launches this profile's Chromium, once.
//
// Bounded on its OWN clock rather than the caller's, like the hosted
// engine's attach and for the same reason: two tool calls can race here,
// and the first one's cancellation must not kill the browser the second is
// about to share. Interrupt cancels it for shutdown.
func (p *headlessProfile) ensureBrowser() (context.Context, error) {
	if browserCtx, ok := p.browser(); ok {
		return browserCtx, nil
	}
	p.launchMu.Lock()
	defer p.launchMu.Unlock()
	if browserCtx, ok := p.browser(); ok {
		return browserCtx, nil
	}

	launchCtx, launchCancel := context.WithTimeout(context.Background(), headlessLaunchTimeout)
	defer launchCancel()
	if err := p.armLaunch(launchCancel); err != nil {
		return nil, err
	}
	defer p.armLaunch(nil)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), p.engine.execOptions(p.userDataDir)...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	abandon := func() {
		browserCancel()
		allocCancel()
	}
	// Registered BEFORE the dial: target discovery is enabled inside it, so
	// a popup opened during the handshake would otherwise be missed.
	chromedp.ListenBrowser(browserCtx, p.dispatchEvent)

	// chromedp bounds the DevTools-line read and nothing after it, so the
	// wait is ours. A Chromium that starts and never answers is an error,
	// never a hang.
	dialed := make(chan error, 1)
	go func() { dialed <- dialCDPBrowser(browserCtx, p.engine.logf) }()
	select {
	case err := <-dialed:
		if err != nil {
			abandon()
			return nil, p.launchFailed(err)
		}
	case <-launchCtx.Done():
		abandon()
		return nil, p.launchFailed(launchCtx.Err())
	}

	// Downloads land ONLY in the AO artifact directory, never the operator's
	// Downloads folder, and allowAndName is what the Manager's own artifact
	// bookkeeping reads: it names each file by its download GUID, which is
	// the handle downloadProgress carries and the name downloads.go renames
	// from. Events are what make Browser.downloadWillBegin/downloadProgress
	// arrive at all.
	if err := cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName).
		WithDownloadPath(p.downloadDir).
		WithEventsEnabled(true).
		Do(browserCommandContext(browserCtx)); err != nil {
		abandon()
		return nil, fmt.Errorf("browser: pin downloads to %s: %w", p.downloadDir, err)
	}

	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		abandon()
		return nil, errors.New("browser: the workspace profile was disposed while its browser started")
	}
	p.browserCtx, p.browserCancel, p.allocCancel = browserCtx, browserCancel, allocCancel
	p.mu.Unlock()
	return browserCtx, nil
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

// launchFailed frames a failed launch around the reason the process gave.
//
// chromedp's own error carries everything the browser printed before it
// died, which is where a sandbox refusal is legible — and it is unbounded,
// because the process decided how much to print. So it is tailed here: the
// text is the payload, and nothing matches on this error.
func (p *headlessProfile) launchFailed(reason error) error {
	return fmt.Errorf("browser: launch Chromium at %s: %s", p.engine.binary, tailOf(reason.Error(), headlessOutputTail))
}

// closeTarget destroys one page of this profile without a driver for it —
// the popup the Manager declined.
func (p *headlessProfile) closeTarget(handle string) {
	browserCtx, ok := p.browser()
	if !ok {
		return
	}
	p.engine.unbindPage(handle)
	if err := target.CloseTarget(target.ID(handle)).Do(browserCommandContext(browserCtx)); err != nil {
		p.engine.logf("browser: close discarded page %s: %v", handle, err)
	}
}

// dispatchEvent translates this profile's browser-level CDP stream into the
// seam's vocabulary.
//
// A page's handle IS its CDP target id here — there is no second identity to
// re-key onto, unlike the launcher-hosted engine — so the engine's whole
// bookkeeping is which profile owns which target. A target this engine never
// bound is dropped rather than reported under a handle nobody owns.
func (p *headlessProfile) dispatchEvent(ev any) {
	if cdpDownloadEvent(ev, p.engine.events) {
		return
	}
	switch event := ev.(type) {
	case *target.EventTargetCreated:
		info := event.TargetInfo
		// An opener is what makes a target a POPUP: every page the Manager
		// asked for is created without one, and the workers, iframes and
		// service workers Chromium also reports are not pages at all.
		if info == nil || info.Type != "page" || info.OpenerID == "" {
			return
		}
		p.engine.bindPage(string(info.TargetID), p)
		p.engine.events.PopupOpened(enginePopup{
			Profile: p.handle, Opener: string(info.OpenerID), Handle: string(info.TargetID),
			URL: info.URL, Title: info.Title,
		})
	case *target.EventTargetInfoChanged:
		info := event.TargetInfo
		if info == nil || info.Type != "page" {
			return
		}
		if _, known := p.engine.profileForPage(string(info.TargetID)); !known {
			return
		}
		p.engine.events.PageInfoChanged(string(info.TargetID), info.URL, info.Title)
	case *target.EventTargetDestroyed:
		handle := string(event.TargetID)
		if !p.engine.unbindPage(handle) {
			return
		}
		// Off the CDP event goroutine: the Manager's own teardown for a
		// closed page must not block this profile's event stream.
		go p.engine.events.PageClosed(handle)
	}
}
