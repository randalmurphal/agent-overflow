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
	"github.com/chromedp/cdproto/target"
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

// all yields every entry, in no order; yield may delete the entry it is
// given.
func (m *boundedMap[K, V]) all() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for key, entry := range m.entries {
			if !yield(key, entry.value) {
				return
			}
		}
	}
}

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
// by, from the page session's frame and target events in the order the
// browser sent them and the frame tree read at the page's attach at its
// place among them (cdpPage.lifecycle).
//
// A frame the page's own renderer hosts is local: Page.frameAttached or
// frameNavigated makes it live, frameDetached for its removal makes it
// gone, and the tree replaces the set: it reflects every event sent before
// its reply, and the events after it apply on top. A cross-site iframe
// moves to a renderer of its own: Chromium attaches its iframe target to
// the page's session (Target.attachedToTarget, the target id being the
// frame id) and then detaches the frame from the page's renderer with
// reason swap, and the tree no longer names it. Such a frame is remote,
// the page's from its target's attach to its target's detach, and the
// swap leaves its local entry as it is, so a frame that swaps back in is
// still the page's. Frames inside a remote frame's own renderer are
// reported only on its own session, which nothing here reads, so they are
// not the page's. The page's own frames bound both sets.
type frameTracker struct {
	mu    sync.RWMutex
	local map[cdp.FrameID]struct{}
	// remote is each out-of-process frame by the session of its iframe
	// target, and remoteFrames how many sessions name each.
	remote       map[target.SessionID]cdp.FrameID
	remoteFrames map[cdp.FrameID]int
}

func newFrameTracker() *frameTracker {
	return &frameTracker{
		local:  make(map[cdp.FrameID]struct{}),
		remote: make(map[target.SessionID]cdp.FrameID), remoteFrames: make(map[cdp.FrameID]int),
	}
}

func (t *frameTracker) attached(id cdp.FrameID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.local[id] = struct{}{}
}

// detached applies a Page.frameDetached: a frame removed is gone, and one
// swapped into a renderer of its own stays the page's.
func (t *frameTracker) detached(id cdp.FrameID, reason page.FrameDetachedReason) {
	switch reason {
	case page.FrameDetachedReasonSwap:
		return
	case page.FrameDetachedReasonRemove:
	default:
		// A reason this protocol version does not name ends the frame in
		// the page's renderer, as a removal does.
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.local, id)
}

// targetAttached applies a Target.attachedToTarget on the page's session:
// an iframe target is an out-of-process frame of the page.
func (t *frameTracker) targetAttached(session target.SessionID, info *target.Info) {
	if info == nil || info.Type != "iframe" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, known := t.remote[session]; known {
		return
	}
	frame := cdp.FrameID(info.TargetID)
	t.remote[session] = frame
	t.remoteFrames[frame]++
}

// targetDetached applies a Target.detachedFromTarget on the page's
// session: the frame its iframe target was is no longer remote.
func (t *frameTracker) targetDetached(session target.SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	frame, ok := t.remote[session]
	if !ok {
		return
	}
	delete(t.remote, session)
	if t.remoteFrames[frame]--; t.remoteFrames[frame] <= 0 {
		delete(t.remoteFrames, frame)
	}
}

// reset makes the local set the frames of tree. The tree names none of
// the remote frames, which their targets keep.
func (t *frameTracker) reset(tree *page.FrameTree) {
	local := make(map[cdp.FrameID]struct{})
	var walk func(*page.FrameTree)
	walk = func(node *page.FrameTree) {
		if node == nil {
			return
		}
		if node.Frame != nil {
			local[node.Frame.ID] = struct{}{}
		}
		for _, child := range node.ChildFrames {
			walk(child)
		}
	}
	walk(tree)
	t.mu.Lock()
	t.local = local
	t.mu.Unlock()
}

func (t *frameTracker) owns(id cdp.FrameID) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, local := t.local[id]
	return local || t.remoteFrames[id] > 0
}
