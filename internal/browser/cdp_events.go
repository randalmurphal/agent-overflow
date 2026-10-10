package browser

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// chromedp delivers each CDP event method on its own buffered subscription,
// and each subscription here is read by its own goroutine. Within one method
// the events keep the browser's order; across methods they do not, so two
// events the browser sent in one order can be handled in the other. The
// state below is fed by more than one method.

// errNoBrowserConnection is a browser subscription that connected nothing
// and reported no reason.
var errNoBrowserConnection = errors.New("browser: chromedp connected no browser")

// onTargetEvent subscribes to ev on the target of ctx now, so no event sent
// after it returns is lost, and handles each event on one goroutine until
// ctx ends.
func onTargetEvent[E any](ctx context.Context, ev cdp.Event[E], logf func(string, ...any), handle func(E)) {
	consumeEvents(ctx, ev.Method, chromedp.Events(ctx, ev), logf, handle)
}

// onBrowserEvent is onTargetEvent for the browser-level session of ctx. The
// first subscription on a context connects its browser, and a connection
// that fails is returned rather than logged.
func onBrowserEvent[E any](ctx context.Context, ev cdp.Event[E], logf func(string, ...any), handle func(E)) error {
	events := chromedp.BrowserEvents(ctx, ev)
	if c := chromedp.FromContext(ctx); c == nil || c.Browser == nil {
		// BrowserEvents could not connect the browser, and its iterator
		// holds only that error.
		for _, err := range events {
			return err
		}
		return errNoBrowserConnection
	}
	consumeEvents(ctx, ev.Method, events, logf, handle)
	return nil
}

// consumeEvents ranges over one method's events on its own goroutine. The
// stream ends with the error of ctx when the page or browser goes away. Any
// other error is a payload that did not decode, which ends this method's
// stream for the rest of the session, so it is logged.
func consumeEvents[E any](ctx context.Context, method string, events iter.Seq2[E, error], logf func(string, ...any), handle func(E)) {
	go func() {
		for event, err := range events {
			if err != nil {
				if ctx.Err() == nil {
					logf("browser: %s events stopped: %v", method, err)
				}
				return
			}
			handle(event)
		}
	}()
}

// networkTracker is the in-flight request set the network-idle wait reads.
type networkTracker struct {
	mu       sync.Mutex
	requests map[network.RequestID]struct{}
	last     time.Time
}

func newNetworkTracker(now time.Time) *networkTracker {
	return &networkTracker{requests: make(map[network.RequestID]struct{}), last: now}
}

// started records a Network.requestWillBeSent. redirect is set for the
// continuation of a request that was redirected, which reuses its id.
func (t *networkTracker) started(id network.RequestID, redirect bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests[id] = struct{}{}
	t.last = now
}

// ended records a Network.loadingFinished or Network.loadingFailed.
func (t *networkTracker) ended(id network.RequestID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.requests, id)
	t.last = now
}

// idle reports no request in flight and none started or ended for quiet.
func (t *networkTracker) idle(now time.Time, quiet time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.requests) == 0 && now.Sub(t.last) >= quiet
}

// frameTracker is the set of frames a page has reported, which downloads are
// routed by.
type frameTracker struct {
	mu     sync.RWMutex
	frames map[cdp.FrameID]struct{}
}

func newFrameTracker() *frameTracker {
	return &frameTracker{frames: make(map[cdp.FrameID]struct{})}
}

func (t *frameTracker) attached(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frames[id] = struct{}{}
}

func (t *frameTracker) navigated(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frames[id] = struct{}{}
}

func (t *frameTracker) detached(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.frames, id)
}

func (t *frameTracker) owns(id cdp.FrameID) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.frames[id]
	return ok
}

// downloadTracker translates the two browser-level download events of one
// browser connection into the seam's vocabulary. A download the Manager
// refuses is cancelled through cancel, on its own goroutine because the
// cancel is a CDP round trip.
//
// Shared by both CDP engines: downloads are a browser-level CDP fact with no
// engine-specific identity in them (the GUID IS the handle on both engines),
// so a second copy could only drift.
type downloadTracker struct {
	events engineEvents
	cancel func(id string)
}

func newDownloadTracker(events engineEvents, cancel func(id string)) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel}
}

func (t *downloadTracker) willBegin(event cdpbrowser.EventDownloadWillBegin) {
	if !t.events.DownloadStarted(downloadStart{
		Frame: string(event.FrameID), ID: event.GUID,
		URL: event.URL, SuggestedName: event.SuggestedFilename,
	}) {
		go t.cancel(event.GUID)
	}
}

func (t *downloadTracker) progress(event cdpbrowser.EventDownloadProgress) {
	t.events.DownloadProgress(cdpDownloadProgress(event))
}

func cdpDownloadProgress(event cdpbrowser.EventDownloadProgress) downloadProgress {
	state := downloadInProgress
	switch event.State {
	case cdpbrowser.DownloadProgressStateCompleted:
		state = downloadCompleted
	case cdpbrowser.DownloadProgressStateCanceled:
		state = downloadCanceled
	}
	return downloadProgress{ID: event.GUID, Received: event.ReceivedBytes, State: state, FilePath: event.FilePath}
}

// subscribe connects the tracker to the download events of the browser of
// ctx.
func (t *downloadTracker) subscribe(ctx context.Context, logf func(string, ...any)) error {
	if err := onBrowserEvent(ctx, cdpbrowser.DownloadWillBegin, logf, t.willBegin); err != nil {
		return err
	}
	return onBrowserEvent(ctx, cdpbrowser.DownloadProgress, logf, t.progress)
}
