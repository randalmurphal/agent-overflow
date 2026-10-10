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
// events the browser sent in one order can be handled in the other. chromedp
// has no exported ordered join of arbitrary methods (Console is the one
// fixed join it offers), so the state below, which is fed by more than one
// method, is keyed by the protocol's own ids and settles to the same answer
// in every handling order.

// errNoBrowserConnection is a browser subscription that connected nothing
// and reported no reason.
var errNoBrowserConnection = errors.New("browser: chromedp connected no browser")

// orderSlack bounds the state each tracker keeps for a half-seen lifecycle:
// an end handled before its start, or an id kept to absorb a late event.
// Two subscription readers drift apart by the events queued between them, a
// handful in practice, so an event that arrives after this many newer ones
// of its kind is treated as new. The bound is what keeps an end whose start
// never comes (one sent before the domain was enabled) from being kept
// forever.
const orderSlack = 256

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

// boundedMap keeps at most limit entries and drops the oldest insertion to
// make room for a new one. Updating an entry keeps its age.
type boundedMap[K comparable, V any] struct {
	limit   int
	next    uint64
	entries map[K]boundedEntry[V]
}

type boundedEntry[V any] struct {
	seq   uint64
	value V
}

func newBoundedMap[K comparable, V any](limit int) *boundedMap[K, V] {
	return &boundedMap[K, V]{limit: limit, entries: make(map[K]boundedEntry[V])}
}

func (m *boundedMap[K, V]) put(key K, value V) {
	if entry, ok := m.entries[key]; ok {
		entry.value = value
		m.entries[key] = entry
		return
	}
	if len(m.entries) >= m.limit {
		var oldest K
		first := true
		var oldestSeq uint64
		for k, entry := range m.entries {
			if first || entry.seq < oldestSeq {
				oldest, oldestSeq, first = k, entry.seq, false
			}
		}
		delete(m.entries, oldest)
	}
	m.next++
	m.entries[key] = boundedEntry[V]{seq: m.next, value: value}
}

func (m *boundedMap[K, V]) get(key K) (V, bool) {
	entry, ok := m.entries[key]
	return entry.value, ok
}

func (m *boundedMap[K, V]) take(key K) (V, bool) {
	entry, ok := m.entries[key]
	delete(m.entries, key)
	return entry.value, ok
}

func (m *boundedMap[K, V]) delete(key K) { delete(m.entries, key) }

func (m *boundedMap[K, V]) len() int { return len(m.entries) }

// networkTracker is the in-flight request set the network-idle wait reads.
//
// A request's start (requestWillBeSent) and end (loadingFinished or
// loadingFailed) arrive on different subscriptions. A request id is in one
// of three states: in flight (start handled, end not), ended early (end
// handled, start not: kept in endedEarly), or settled (both handled: nothing
// kept). A start that finds its early end settles the request. A redirect
// reuses the id for further starts before the one end; such a continuation
// finding its id neither in flight nor ended early belongs before an end
// that has already settled it, so it is ignored.
//
// endedEarly is bounded by orderSlack. A request started before
// Network.enable has an end with no start, and the bound is what drops it.
type networkTracker struct {
	mu         sync.Mutex
	inFlight   map[network.RequestID]struct{}
	endedEarly *boundedMap[network.RequestID, struct{}]
	last       time.Time
}

func newNetworkTracker(now time.Time) *networkTracker {
	return &networkTracker{
		inFlight:   make(map[network.RequestID]struct{}),
		endedEarly: newBoundedMap[network.RequestID, struct{}](orderSlack),
		last:       now,
	}
}

// started records a Network.requestWillBeSent. redirect is set for the
// continuation of a request that was redirected, which reuses its id.
func (t *networkTracker) started(id network.RequestID, redirect bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = now
	if _, ok := t.endedEarly.take(id); ok {
		return
	}
	if redirect {
		// Either still in flight, or already settled by its end.
		return
	}
	t.inFlight[id] = struct{}{}
}

// ended records a Network.loadingFinished or Network.loadingFailed.
func (t *networkTracker) ended(id network.RequestID, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = now
	if _, ok := t.inFlight[id]; ok {
		delete(t.inFlight, id)
		return
	}
	t.endedEarly.put(id, struct{}{})
}

// retained reports how many request ids the tracker keeps.
func (t *networkTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inFlight) + t.endedEarly.len()
}

// idle reports no request in flight and none started or ended for quiet.
func (t *networkTracker) idle(now time.Time, quiet time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inFlight) == 0 && now.Sub(t.last) >= quiet
}

// frameTracker is the set of frames a page has reported, which downloads are
// routed by.
//
// frameAttached, frameNavigated and frameDetached arrive on three
// subscriptions, so the tracker counts rather than toggles. Attaches and
// detaches of one frame alternate, starting with an attach (a frame swapped
// to another process and back is attached twice), so once every event is
// handled the frame is live exactly when it has more attaches than
// detaches, in whatever order they were handled. A navigation stands in for
// the attach of a frame that attached before the page subscribed.
//
// A live frame is kept until it detaches; the page's own frames bound that.
// A frame that is not live keeps its counts in gone to absorb its late
// events, and gone is bounded by orderSlack, so detached frames do not pile
// up.
type frameTracker struct {
	mu   sync.RWMutex
	live map[cdp.FrameID]frameEvents
	gone *boundedMap[cdp.FrameID, frameEvents]
}

// frameEvents counts what has been handled for one frame.
type frameEvents struct {
	attaches, detaches int
	navigated          bool
}

func (f frameEvents) live() bool {
	attaches := f.attaches
	if attaches == 0 && f.navigated {
		attaches = 1
	}
	return attaches > f.detaches
}

func newFrameTracker() *frameTracker {
	return &frameTracker{live: make(map[cdp.FrameID]frameEvents), gone: newBoundedMap[cdp.FrameID, frameEvents](orderSlack)}
}

func (t *frameTracker) attached(id cdp.FrameID) {
	t.record(id, func(f *frameEvents) { f.attaches++ })
}

func (t *frameTracker) navigated(id cdp.FrameID) {
	t.record(id, func(f *frameEvents) { f.navigated = true })
}

func (t *frameTracker) detached(id cdp.FrameID) {
	t.record(id, func(f *frameEvents) { f.detaches++ })
}

func (t *frameTracker) record(id cdp.FrameID, event func(*frameEvents)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.live[id]
	if !ok {
		f, _ = t.gone.get(id)
	}
	event(&f)
	if f.live() {
		t.gone.delete(id)
		t.live[id] = f
		return
	}
	delete(t.live, id)
	t.gone.put(id, f)
}

func (t *frameTracker) owns(id cdp.FrameID) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.live[id]
	return ok
}

// retained reports how many frames the tracker keeps.
func (t *frameTracker) retained() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.live) + t.gone.len()
}

// downloadTracker translates the two browser-level download events of one
// browser connection into the seam's vocabulary. A download the Manager
// refuses is cancelled through cancel, on its own goroutine because the
// cancel is a CDP round trip.
//
// Shared by both CDP engines: downloads are a browser-level CDP fact with no
// engine-specific identity in them (the GUID IS the handle on both engines),
// so a second copy could only drift.
//
// downloadWillBegin and downloadProgress arrive on two subscriptions, and a
// small download can be complete before its start is handled. The Manager
// ignores progress for a download it has not seen start, so the tracker
// keeps each GUID in one of two states: started (progress is forwarded),
// or progress held (the newest progress before the start, forwarded right
// after it). A terminal progress after the start ends the lifecycle and
// nothing is kept. The start and the forward of held progress happen under
// one lock with the progress handler, so no progress slips between them.
//
// Both states are bounded by orderSlack; the oldest GUID is dropped first.
// A dropped started download's later progress is held as if early, which
// the bound then drops in turn. The Manager's own page cancellation still
// settles such a download.
type downloadTracker struct {
	events engineEvents
	cancel func(id string)

	mu        sync.Mutex
	downloads *boundedMap[string, downloadLifecycle]
}

type downloadLifecycle struct {
	started bool
	held    *downloadProgress
}

func newDownloadTracker(events engineEvents, cancel func(id string)) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel, downloads: newBoundedMap[string, downloadLifecycle](orderSlack)}
}

func (t *downloadTracker) willBegin(event cdpbrowser.EventDownloadWillBegin) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lifecycle, _ := t.downloads.take(event.GUID)
	if !t.events.DownloadStarted(downloadStart{
		Frame: string(event.FrameID), ID: event.GUID,
		URL: event.URL, SuggestedName: event.SuggestedFilename,
	}) {
		go t.cancel(event.GUID)
	}
	if held := lifecycle.held; held != nil {
		t.events.DownloadProgress(*held)
		if downloadSettled(held.State) {
			return
		}
	}
	t.downloads.put(event.GUID, downloadLifecycle{started: true})
}

func (t *downloadTracker) progress(event cdpbrowser.EventDownloadProgress) {
	progress := cdpDownloadProgress(event)
	t.mu.Lock()
	defer t.mu.Unlock()
	lifecycle, _ := t.downloads.get(event.GUID)
	if !lifecycle.started {
		t.downloads.put(event.GUID, downloadLifecycle{held: &progress})
		return
	}
	t.events.DownloadProgress(progress)
	if downloadSettled(progress.State) {
		t.downloads.delete(event.GUID)
	}
}

// retained reports how many downloads the tracker keeps.
func (t *downloadTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downloads.len()
}

func downloadSettled(state string) bool {
	return state == downloadCompleted || state == downloadCanceled
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
