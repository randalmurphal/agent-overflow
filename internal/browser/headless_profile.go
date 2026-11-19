package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"agent-overflow/internal/browser/ids"
)

// headlessProfile is one canonical workspace's Chromium: its own process,
// its own user-data directory, its own cookie jar. The process is launched
// by the first page and dies with the profile.
type headlessProfile struct {
	engine      *headlessEngine
	handle      profileHandle
	userDataDir string
	// ephemeralRoot is the temp directory holding userDataDir when the
	// site-data setting is off, removed on Dispose. Empty when the profile
	// persists, and that emptiness is what stops a persisted workspace's
	// logins from being deleted.
	ephemeralRoot string
	downloadDir   string
	// allow is the jar's navigation policy (profileOptions.Allow), which
	// connect enables browser-wide.
	allow func(owners []frameID, rawURL string) (allowed, resolved bool)

	// launchMu serializes ensureBrowser so a burst of concurrent page
	// creations launches one Chromium rather than one each.
	launchMu sync.Mutex

	mu           sync.Mutex
	disposed     bool
	launchCancel context.CancelFunc
	current      *launchedBrowser
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

func (p *headlessProfile) Handle() profileHandle { return p.handle }

func (p *headlessProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	browserCtx, err := p.ensureBrowser()
	if err != nil {
		return nil, err
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	setup, release := p.engine.beginAttach(ctx)
	driver, err := startCDPPage(setup, browserCtx, pageCtx, pageCancel, hooks)
	release()
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
func (p *headlessProfile) AttachPage(ctx context.Context, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	browserCtx, ok := p.browser()
	if !ok {
		return nil, errors.New("browser: the profile's browser is no longer running")
	}
	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.ID(handle)))
	setup, release := p.engine.beginAttach(ctx)
	driver, err := startCDPPage(setup, browserCtx, pageCtx, pageCancel, hooks)
	release()
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
// that step and reported lost by it, or refused.
func (p *headlessProfile) bindPage(browserCtx context.Context, handle ids.PageHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.ctx != browserCtx || browserCtx.Err() != nil {
		return errors.New("browser: the profile's browser is no longer running")
	}
	p.engine.bindPage(handle, p)
	return nil
}

// CancelDownload cancels download id, marked on its connection's tracker
// first so the cancel Chromium reports is read as AO's (canceling).
func (p *headlessProfile) CancelDownload(id string) error {
	p.mu.Lock()
	current := p.current
	p.mu.Unlock()
	if current == nil || current.ctx.Err() != nil {
		return fmt.Errorf("browser: profile %s has no running browser", p.handle)
	}
	undo := current.downloads.canceling(id)
	if err := callBounded(current.ctx, p.engine.closeTimeout, cdpbrowser.CancelDownload, cdpbrowser.CancelDownloadParams{GUID: id}); err != nil {
		undo()
		return fmt.Errorf("browser: profile %s: cancel download %s: %w", p.handle, id, err)
	}
	return nil
}

// removeDownloadFile removes the file Chromium names by download id in
// the download directory, for a download the Manager did not take
// (downloadTracker), once nothing writes it: a download that finished
// before its cancel took effect left its whole file there. No caller
// waits on it, so a failure is logged.
func (p *headlessProfile) removeDownloadFile(id string) {
	if p.downloadDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(p.downloadDir, filepath.Base(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.engine.logf("browser: profile %s: remove the file of download %s no page took: %v", p.handle, id, err)
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

// awaitLaunch returns once a launch in flight (ensureBrowser, which holds
// launchMu throughout) has returned, its process reaped. It is a barrier
// only: nothing is read or written under the lock.
func (p *headlessProfile) awaitLaunch() {
	p.launchMu.Lock()
	defer p.launchMu.Unlock()
}

// tearDown is the rest of Dispose, after disposeLocked.
func (p *headlessProfile) tearDown(current *launchedBrowser, launchCancel context.CancelFunc) error {
	if launchCancel != nil {
		launchCancel()
	}
	p.awaitLaunch()
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

// closeTarget destroys one page of this profile without a driver for it,
// the popup the Manager declined, within ctx. A target it fails to close
// stays bound to this profile, whose disposal ends it with its browser,
// and a later discard tries again.
func (p *headlessProfile) closeTarget(ctx context.Context, handle ids.PageHandle) error {
	browserCtx, ok := p.browser()
	if !ok {
		// Its browser is gone, and the target with it.
		p.engine.unbindPage(handle)
		return nil
	}
	callCtx, cancel := context.WithCancel(browserCtx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if _, err := chromedp.CallBrowser(callCtx, target.CloseTarget, target.CloseTargetParams{TargetID: target.ID(handle)}); err != nil {
		return fmt.Errorf("browser: close discarded page %s: %w", handle, err)
	}
	p.engine.unbindPage(handle)
	return nil
}

// callBounded sends one browser-level command on browserCtx, bounded by
// timeout: its caller (a download's cancel, which a page's close waits on)
// must end when a connected Chromium stops answering.
func callBounded[P, R any](browserCtx context.Context, timeout time.Duration, cmd cdp.Command[P, R], params P) error {
	ctx, cancel := context.WithTimeout(browserCtx, timeout)
	defer cancel()
	_, err := chromedp.CallBrowser(ctx, cmd, params)
	return err
}
