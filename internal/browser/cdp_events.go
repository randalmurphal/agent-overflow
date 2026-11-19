package browser

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// chromedp delivers each CDP event method on its own buffered subscription,
// and each subscription here is read by its own goroutine. Within one method
// the events keep the browser's order; across methods they do not, so two
// events the browser sent in one order can be handled in the other. A
// page's frames and its loading are read in the browser's order from one
// ordered input (cdporder, cdpPage.lifecycle). The request set below, fed
// by more than one method, is keyed by the protocol's own ids and settles
// to the same answer in every handling order.

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

// cdpLoadTracker numbers a page's main-frame loading reports, from
// Page.frameStartedLoading and Page.frameStoppedLoading in the order the
// browser sent them (cdpPage.lifecycle): a start means loading and a stop
// idle, whatever came before and however long ago. A page attached
// mid-load has sent neither, and reports nothing until one comes. The
// Manager keeps the highest number (pageHooks.Loading). Used by the
// session's one lifecycle goroutine alone.
type cdpLoadTracker struct {
	handled uint64
}

// observe answers the report for a start (loading) or stop and its number.
func (t *cdpLoadTracker) observe(loading bool) (bool, uint64) {
	t.handled++
	return loading, t.handled
}

// frameTracker is the set of frames a page has, which downloads are routed
// by, from Page.frameAttached, frameNavigated and frameDetached in the
// order the browser sent them and the frame tree read at the page's
// attach at its place among them (cdpPage.lifecycle). An attach or a
// navigation makes a frame live, a detach makes it gone, and the tree
// replaces the set: it reflects every event sent before its reply, and
// the events after it apply on top. The page's own frames bound it.
type frameTracker struct {
	mu   sync.RWMutex
	live map[cdp.FrameID]struct{}
}

func newFrameTracker() *frameTracker {
	return &frameTracker{live: make(map[cdp.FrameID]struct{})}
}

func (t *frameTracker) attached(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live[id] = struct{}{}
}

func (t *frameTracker) detached(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.live, id)
}

// reset makes the set the frames of tree.
func (t *frameTracker) reset(tree *page.FrameTree) {
	live := make(map[cdp.FrameID]struct{})
	var walk func(*page.FrameTree)
	walk = func(node *page.FrameTree) {
		if node == nil {
			return
		}
		if node.Frame != nil {
			live[node.Frame.ID] = struct{}{}
		}
		for _, child := range node.ChildFrames {
			walk(child)
		}
	}
	walk(tree)
	t.mu.Lock()
	t.live = live
	t.mu.Unlock()
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
	return len(t.live)
}
