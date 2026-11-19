package browser

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/browser/cdporder"
	"agent-overflow/internal/browser/ids"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

// A headless profile's Chromium process and its CDP connection: launch, adoption and loss.

// launchedBrowser is one Chromium this profile started and its CDP
// connection. The browser context is cancelled when the connection is lost,
// so a context that is done means the browser is gone.
type launchedBrowser struct {
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	process     *chromiumProcess
	// downloads is the connection's download tracker: CancelDownload marks
	// AO's cancels on it, and close settles its claims on files no page
	// took once the process has stopped.
	downloads *downloadTracker
}

// close ends the connection, then kills the browser and returns once nothing
// it started can still write, then removes the files of the downloads no
// page took. An error means some of it may still be running; the files go
// anyway, since a writer still running writes to an unlinked file.
func (b *launchedBrowser) close() error {
	b.cancel()
	b.allocCancel()
	var err error
	if b.process != nil {
		// A browser without a process of its own, as the lifetime tests
		// build, has nothing to stop.
		err = b.process.stop()
	}
	if b.downloads != nil {
		b.downloads.stop()
	}
	return err
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
	defer p.disarmLaunch()

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
	process, wsURL, err := startChromium(launchCtx, p.engine.reaper, p.engine.binary, chromiumArgs(p.userDataDir), chromiumEnv(p.userDataDir))
	if err != nil {
		return nil, p.launchFailed(err)
	}
	// NoModifyURL: the DevTools line is already the browser websocket URL.
	// chromedp's default would resolve its host again, or query
	// /json/version for a URL without a /devtools/browser/ path.
	allocCtx, allocCancel := cdporder.NewAllocator(context.Background(), wsURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		p.engine.logf("browser: chromedp: "+format, args...)
	}))
	targets := newHeadlessTargets(p)
	launched := &launchedBrowser{
		ctx: browserCtx, cancel: browserCancel, allocCancel: allocCancel, process: process,
		downloads: p.downloadTracker(browserCtx, targets),
	}

	// chromedp bounds the handshake with nothing but the dial's own
	// timeout, so the wait is ours: a Chromium that starts and never
	// answers is an error, never a hang.
	connected := make(chan error, 1)
	go func() { connected <- p.connect(browserCtx, targets, launched.downloads) }()
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
func (p *headlessProfile) connect(browserCtx context.Context, targets *headlessTargets, downloads *downloadTracker) error {
	if err := dialCDPBrowser(browserCtx, func() error { return p.subscribe(browserCtx, targets, downloads) }); err != nil {
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
	// The policy's one interception: browser-wide, so it sees a popup's
	// requests before the Manager adopts it (a noopener popup's renderer
	// cannot be held for AttachPage) as well as every page's.
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
// them lost (reportLost). The other finds nothing to report.
//
// The caller holds launchMu, and the report runs on its own goroutine:
// PageCrashed closes the page, which can reach Dispose, which waits on
// launchMu.
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
		go p.reportLost(pages)
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
// until Chromium is reaped, and PageCrashed is the Manager's page teardown.
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
	p.reportLost(pages)
}

// browserLostReason is the crash reason of a page whose whole browser ended
// without the profile ending it (a crash, an OOM kill, a kill by hand).
const browserLostReason = "browser process exited"

// reportLost tells the Manager each of pages crashed with its browser, in
// order: the page was not closed by anyone, so its tab stays, failed at its
// address, and a tool call or its Reload restores it in a new browser.
func (p *headlessProfile) reportLost(pages []ids.PageHandle) {
	for _, handle := range pages {
		p.engine.events.PageCrashed(handle, browserLostReason)
	}
}

// armLaunch publishes the cancel Interrupt reaches the launch through,
// refusing to start one on a disposed profile.
func (p *headlessProfile) armLaunch(cancel context.CancelFunc) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return errors.New("browser: the workspace profile is disposed")
	}
	p.launchCancel = cancel
	return nil
}

// disarmLaunch clears the cancel armLaunch published, as the launch ends.
func (p *headlessProfile) disarmLaunch() {
	p.mu.Lock()
	p.launchCancel = nil
	p.mu.Unlock()
}

// launchFailed frames a failed launch with the binary that failed. A reason
// from startChromium carries the tail of what the browser printed, which is
// where a sandbox refusal is legible.
func (p *headlessProfile) launchFailed(reason error) error {
	return fmt.Errorf("browser: launch Chromium at %s: %w", p.engine.binary, reason)
}
