package browser

import (
	"context"
	"fmt"
	"slices"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

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
	// A connection browser() refused has ended. Its pages are claimed before
	// a replacement is published, whether or not its watcher has run; the
	// report runs on its own goroutine, as the Manager's teardown it starts
	// must not wait on this attach.
	e.mu.Lock()
	lost := e.browserCtx
	var pages []string
	if lost != nil {
		pages = e.claimLostLocked(lost)
	}
	e.mu.Unlock()
	if len(pages) > 0 {
		go e.reportLost(pages)
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
	allocCtx, allocCancel := remote.NewAllocator(context.Background(), wsURL, remote.NoModifyURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		e.logf("browser: chromedp: "+format, args...)
	}))
	// The dial must NOT go through chromedp.Do, because WebView2 answers
	// its Target.createTarget with `-32000 no browser is open`: a WebView2
	// target exists only as a launcher-created controller (the spike's
	// "WebView2 has no /json/new"). dialCDPBrowser (cdp_page.go) is that
	// target-less dial, shared with the headless engine, and it is also the
	// only sender of the target discovery the targetDestroyed backstop
	// reads.
	//
	// chromedp bounds none of this: a launcher that never answers would park
	// the dial forever. So the wait is ours, on attachCtx, and a timeout
	// tears the connection down rather than leaving a half-built browser
	// behind.
	attached := make(chan error, 1)
	go func() { attached <- dialCDPBrowser(browserCtx, func() error { return e.subscribe(browserCtx) }) }()
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
	e.enableDownloadEvents(browserCtx)
	e.mu.Lock()
	e.browserCtx, e.browserCancel, e.allocCancel = browserCtx, browserCancel, allocCancel
	e.mu.Unlock()
	go e.watchBrowser(browserCtx)
	return browserCtx, nil
}

// connectionLostReason is the crash reason of a page whose CDP connection
// through the relay ended without Stop ending it.
const connectionLostReason = "connection to the pane browser lost"

// watchBrowser retires a browser connection that ended on its own (the
// launcher, its relay or WebView2's browser process went): every page's CDP
// session went with it, so each page bound to it is reported crashed, its
// tab stays failed at its address, and its Reload or a tool call creates the
// page again on a new connection. An attach that finds the connection ended
// first claims those pages itself (ensureBrowser), and Stop forgets every
// page first; either leaves this nothing to report.
func (e *hostedEngine) watchBrowser(browserCtx context.Context) {
	<-browserCtx.Done()
	if e.connectionEnded != nil {
		e.connectionEnded()
	}
	e.mu.Lock()
	pages := e.claimLostLocked(browserCtx)
	e.mu.Unlock()
	e.reportLost(pages)
}

// claimLostLocked retires conn, a browser connection that has ended: it
// drops conn as the engine's connection if it still is, and takes the pages
// bound to it, which only this claim reports, so each is reported once
// whichever of the watcher and the next attach claims first, and a page of
// another connection never is. The caller holds e.mu and reports the pages
// (reportLost).
func (e *hostedEngine) claimLostLocked(conn context.Context) []string {
	if e.browserCtx == conn {
		// The allocator is cancelled nowhere else.
		e.allocCancel()
		e.browserCtx, e.browserCancel, e.allocCancel = nil, nil, nil
	}
	var pages []string
	for pageID, pageConn := range e.pageConn {
		if pageConn == conn {
			pages = append(pages, pageID)
			delete(e.pageConn, pageID)
		}
	}
	slices.Sort(pages)
	return pages
}

// reportLost reports pages, claimed from a lost connection, crashed.
func (e *hostedEngine) reportLost(pages []string) {
	for _, pageID := range pages {
		e.events.PageCrashed(pageID, connectionLostReason)
	}
}

func (e *hostedEngine) browser() (context.Context, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browserCtx == nil || e.browserCtx.Err() != nil {
		return nil, false
	}
	return e.browserCtx, true
}

// subscribe connects the browser-level CDP events of browserCtx to the seam.
// Target ids are the engine's private business, so every event is re-keyed
// onto the page id the Manager knows; an event for a target this engine did
// not create is dropped rather than reported under a handle nobody owns.
//
// Lifecycle is the launcher's to report (`closed` / `process-failed`), and
// targetDestroyed is the backstop for a controller that dies without one.
// Popups are not wired: the launcher does not surface WebView2's
// NewWindowRequested, so no page can be created behind the Manager's back.
func (e *hostedEngine) subscribe(browserCtx context.Context) error {
	// Downloads carry no engine identity (the GUID is the handle on every
	// CDP engine), so the shared tracker translates them; the engine notes
	// each one's browser context on the way (hosted_downloads.go).
	if err := newDownloadTracker(e.downloadEvents(), refusedDownloadCanceler(e.cancelDownload, e.logf)).subscribe(browserCtx, e.logf); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetDestroyed, e.logf, e.targetDestroyed); err != nil {
		return err
	}
	if err := onBrowserEvent(browserCtx, target.TargetCrashed, e.logf, e.targetCrashed); err != nil {
		return err
	}
	return onBrowserEvent(browserCtx, target.TargetInfoChanged, e.logf, e.targetInfoChanged)
}

// targetCrashed reports a page whose renderer died. The launcher's
// process-failed report says the same for the controller; whichever the
// Manager handles first turns the tab into a crashed one, and the other then
// finds no live page.
func (e *hostedEngine) targetCrashed(event target.EventTargetCrashed) {
	if pageID, ok := e.pageForTarget(string(event.TargetID)); ok {
		// Off the event reader: the Manager closes the page's driver.
		go e.events.PageCrashed(pageID, cdpCrashReason(event))
	}
}

func (e *hostedEngine) targetDestroyed(event target.EventTargetDestroyed) {
	if pageID, ok := e.pageForTarget(string(event.TargetID)); ok {
		// On its own goroutine, as both CDP engines report a closed page:
		// retirePage ends in the Manager's teardown, which must not hold
		// the target events queued behind it.
		go e.retirePage(pageID)
	}
}

func (e *hostedEngine) targetInfoChanged(event target.EventTargetInfoChanged) {
	if event.TargetInfo == nil || event.TargetInfo.Type != "page" {
		return
	}
	if pageID, ok := e.pageForTarget(string(event.TargetInfo.TargetID)); ok {
		// The method's events arrive one at a time in the browser's order
		// (consumeEvents), so numbering them here keeps that order.
		e.events.PageInfoChanged(pageID, event.TargetInfo.URL, event.TargetInfo.Title, e.infoOrder.Add(1))
	}
}
