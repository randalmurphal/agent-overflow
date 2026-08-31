// app_harness_ui.go is the backend half of the frontend bridge
// (docs/specs/testing-harness.md §4): a request/reply channel that lets a
// harness caller ask the ATTACHED SPA what it is rendering, over the same
// WebSocket everything else uses.
//
// Why a request/reply pair rather than a push: the answer is a function of
// live DOM, so only the frontend can compute it, and the caller is a Bash
// tool or a Playwright spec that wants one value back from one call.
// `HarnessUIQuery` emits `harness:ui-query {id, pageId, spec}` and parks on
// the id and selected page; the bridge answers with
// `HarnessUIQueryReply(pageId, id, result)`. Both live on
// the Harness receiver, so the reply path exists exactly where the query
// path does — under --harness/--soak, LocalOnly.
//
// Correlation is page-bound. A frontend registers an immutable page ID with
// the per-instance marker before it can answer. A reply from another page is
// dropped while the selected page remains responsible for the waiter.
// A late or duplicate reply is also dropped silently after the waiter is gone.
//   - A timeout says "no frontend attached or harness bridge inactive",
//     because those are the two states a caller can actually fix (open the
//     page; build a frontend whose bootstrap carries harness:true).
package harnessrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/transport"
)

// harnessUIQueryTimeout bounds a caller-driven query. Long enough that a
// first query against a page still hydrating its panes succeeds, short
// enough that an unattended CLI invocation fails inside a human's patience.
const harnessUIQueryTimeout = 10 * time.Second

// harnessUIQueryNoClientGrace bounds how long a query waits when NO client
// is connected to the event bus at all.
//
// The headless case is the common one — `ao-harness ui …` against an
// instance nobody has opened a page on, a bridge command in a script that
// forgot the window — and burning the full 10s per call there turns a
// loop of twenty probes into a four-minute wait for an answer the backend
// already had at millisecond zero. A connection is either present or it
// is not; there is nothing to wait for.
//
// The grace is not zero because a page LOADING is a real state: the
// WebSocket attaches a beat after the navigation, and a query issued in
// that window would otherwise fail on a frontend that is arriving. It is
// polled rather than signalled to keep the bus free of a bridge-specific
// callback — a 10ms poll over 250ms is 25 mutex reads, against a 10s
// stall.
const (
	harnessUIQueryNoClientGrace = 250 * time.Millisecond
	harnessUIQueryClientPoll    = 10 * time.Millisecond
)

// harnessUIQueryEvent is the `harness:ui-query` wire shape. `spec` is
// forwarded verbatim: the tagged union it carries is the BRIDGE's contract
// (frontend/src/lib/harness/), and the backend deliberately knows nothing
// about its kinds beyond the perf ops it issues itself.
type harnessUIQueryEvent struct {
	ID     string          `json:"id"`
	Spec   json.RawMessage `json:"spec"`
	PageID string          `json:"pageId"`
}

// harnessUIBridge is the waiter registry. Its own mutex, deliberately not
// the Harness-wide one: a query parks for up to 10s and must not hold the
// lock that scenario rules, recordings and the replayer share.
type harnessUIBridge struct {
	mu          sync.Mutex
	seq         uint64
	waiters     map[string]harnessUIWaiter
	pages       map[string]HarnessPageIdentity
	pageOwners  map[string]*transport.ConnState
	pageChanged chan struct{}
}

type harnessUIWaiter struct {
	ch     chan json.RawMessage
	pageID string
}

func (b *harnessUIBridge) register() (string, chan json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := "uq-" + strconv.FormatUint(b.seq, 10)
	ch := make(chan json.RawMessage, 1)
	if b.waiters == nil {
		b.waiters = make(map[string]harnessUIWaiter, 1)
	}
	b.waiters[id] = harnessUIWaiter{ch: ch}
	return id, ch
}

func (b *harnessUIBridge) registerPage(identity HarnessPageIdentity, owner *transport.ConnState) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pages == nil {
		b.pages = make(map[string]HarnessPageIdentity, 1)
	}
	if b.pageOwners == nil {
		b.pageOwners = make(map[string]*transport.ConnState, 1)
	}
	if existing, exists := b.pages[identity.PageID]; exists {
		if existing != identity {
			return fmt.Errorf("frontend page %q is already registered", identity.PageID)
		}
		b.pageOwners[identity.PageID] = owner
		return nil
	}
	b.pages[identity.PageID] = identity
	b.pageOwners[identity.PageID] = owner
	if b.pageChanged != nil {
		close(b.pageChanged)
	}
	b.pageChanged = make(chan struct{})
	return nil
}

func (b *harnessUIBridge) unregisterPage(pageID string, owner *transport.ConnState) {
	b.mu.Lock()
	if owner != nil && b.pageOwners[pageID] != owner {
		b.mu.Unlock()
		return
	}
	delete(b.pages, pageID)
	delete(b.pageOwners, pageID)
	if b.pageChanged != nil {
		close(b.pageChanged)
	}
	b.pageChanged = make(chan struct{})
	b.mu.Unlock()
}

func (b *harnessUIBridge) targetPage(spec json.RawMessage) (string, error) {
	var envelope struct {
		PageID string `json:"pageId"`
	}
	if err := json.Unmarshal(spec, &envelope); err != nil {
		return "", fmt.Errorf("decode ui query target: %w", err)
	}
	deadline := time.NewTimer(harnessUIQueryNoClientGrace)
	defer deadline.Stop()
	for {
		b.mu.Lock()
		if envelope.PageID != "" {
			if _, ok := b.pages[envelope.PageID]; !ok {
				b.mu.Unlock()
				return "", fmt.Errorf("frontend page %q is not registered", envelope.PageID)
			}
			b.mu.Unlock()
			return envelope.PageID, nil
		}
		if len(b.pages) == 1 {
			for pageID := range b.pages {
				b.mu.Unlock()
				return pageID, nil
			}
		}
		if len(b.pages) > 1 {
			count := len(b.pages)
			b.mu.Unlock()
			return "", fmt.Errorf("%d frontend pages registered; ui query must name pageId", count)
		}
		changed := b.pageChanged
		if changed == nil {
			changed = make(chan struct{})
			b.pageChanged = changed
		}
		b.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			return "", nil
		}
	}
}

func (b *harnessUIBridge) setWaiterPage(id, pageID string) {
	b.mu.Lock()
	if waiter, ok := b.waiters[id]; ok {
		waiter.pageID = pageID
		b.waiters[id] = waiter
	}
	b.mu.Unlock()
}

func (b *harnessUIBridge) pageIDs() []HarnessPageIdentity {
	b.mu.Lock()
	defer b.mu.Unlock()
	pages := make([]HarnessPageIdentity, 0, len(b.pages))
	for _, page := range b.pages {
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].PageID < pages[j].PageID })
	return pages
}

// release drops a waiter. Idempotent, so the query path can defer it
// unconditionally whether it was answered, timed out, or refused.
func (b *harnessUIBridge) release(id string) {
	b.mu.Lock()
	delete(b.waiters, id)
	b.mu.Unlock()
}

// deliver hands a reply to its waiter, reporting whether one existed. The
// take-and-delete happens under one lock acquisition, which is what makes
// "first reply wins" a property rather than a race.
func (b *harnessUIBridge) deliver(pageID, id string, result json.RawMessage) bool {
	b.mu.Lock()
	waiter, ok := b.waiters[id]
	if ok && waiter.pageID != pageID {
		ok = false
	}
	if ok {
		delete(b.waiters, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered to 1 and this is the only send, so it cannot block even if
	// the caller has already abandoned the channel on a timeout.
	waiter.ch <- result
	return true
}

// pending reports how many queries are parked. Test-facing.
func (b *harnessUIBridge) pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// HarnessUIQuery asks the attached frontend bridge one question and returns
// its answer verbatim.
//
// The spec is a versioned tagged union (`{"v":1,"kind":"viewport"}`,
// `{"v":1,"kind":"element","selector":"..."}`, `globals`, `perf`) defined by
// the bridge. A result carrying `{"error": "..."}` surfaces as the RPC's
// error, so a bridge-side refusal (unknown kind, an unwhitelisted global)
// reads as a failed call rather than a successful empty one.
func (h *Harness) HarnessUIQuery(spec json.RawMessage) (json.RawMessage, error) {
	return h.queryUI(spec, harnessUIQueryTimeout)
}

// HarnessRegisterPage authenticates and records one frontend page. The page
// id is immutable for its load and is removed when its transport connection
// closes. A marker from another harness instance cannot register, even when
// both instances share one browser debugger endpoint.
func (h *Harness) HarnessRegisterPage(ctx context.Context, pageID, marker, origin string) (HarnessPageIdentity, error) {
	pageID = strings.TrimSpace(pageID)
	marker = strings.TrimSpace(marker)
	origin = strings.TrimSpace(origin)
	if pageID == "" || marker == "" || origin == "" {
		return HarnessPageIdentity{}, fmt.Errorf("page registration requires pageId, marker, and origin")
	}
	if marker != h.pageMarker {
		return HarnessPageIdentity{}, fmt.Errorf("frontend page marker does not match this harness instance")
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" {
		return HarnessPageIdentity{}, fmt.Errorf("frontend page origin is invalid")
	}
	if h.config.Host == nil {
		return HarnessPageIdentity{}, fmt.Errorf("harness host unavailable")
	}
	if expected := h.config.Host.ExpectedOrigin(); expected != "" && !sameHarnessOrigin(origin, expected) {
		return HarnessPageIdentity{}, fmt.Errorf("frontend page origin %q does not match this harness origin", origin)
	}
	identity := HarnessPageIdentity{PageID: pageID, Marker: marker, Origin: origin}
	state := transport.ConnStateFromContext(ctx)
	if err := h.ui.registerPage(identity, state); err != nil {
		return HarnessPageIdentity{}, err
	}
	if state != nil {
		if !state.RegisterCleanup(func() { h.ui.unregisterPage(pageID, state) }) {
			h.ui.unregisterPage(pageID, state)
			return HarnessPageIdentity{}, fmt.Errorf("frontend page connection is closing")
		}
	}
	return identity, nil
}

func sameHarnessOrigin(aRaw, bRaw string) bool {
	a, errA := url.Parse(aRaw)
	b, errB := url.Parse(bRaw)
	if errA != nil || errB != nil || a.Scheme != b.Scheme || a.Host == "" || b.Host == "" {
		return false
	}
	if strings.EqualFold(a.Host, b.Host) {
		return true
	}
	return a.Port() == b.Port() && isHarnessLoopback(a.Hostname()) && isHarnessLoopback(b.Hostname())
}

func isHarnessLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HarnessUIQueryReply resolves the waiter for `id` only when pageID is the
// immutable page identity selected for that query. Called by the frontend
// bridge; a wrong page is dropped without disturbing the real waiter.
//
// An id with no waiter is a no-op and NOT an error: it means the caller was
// already answered by another attached frontend or has already timed out,
// and neither is the replying bridge's fault.
func (h *Harness) HarnessUIQueryReply(pageID, id string, result json.RawMessage) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("query id must be non-empty")
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	if err := requireValidJSON("result", result); err != nil {
		return err
	}
	if !h.ui.deliver(pageID, id, result) {
		log.Printf("harness: ui-query reply for page %q and id %q dropped (wrong, late, duplicate, or timed out)", pageID, id)
	}
	return nil
}

// queryUI is the shared request/reply body. The perf sampler uses it with a
// tighter deadline than a caller-driven query gets — a collect that cannot
// answer inside one sample interval is a skipped sample, not a stalled run.
func (h *Harness) queryUI(spec json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	if len(spec) == 0 {
		return nil, fmt.Errorf("spec must be non-empty JSON")
	}
	if err := requireValidJSON("spec", spec); err != nil {
		return nil, err
	}
	pageID := ""
	if h.config.Host != nil {
		_, busAvailable := h.config.Host.ConnectedClients()
		if busAvailable {
			var err error
			pageID, err = h.ui.targetPage(spec)
			if err != nil {
				return nil, err
			}
		}
	}
	id, ch := h.ui.register()
	h.ui.setWaiterPage(id, pageID)
	defer h.ui.release(id)
	if h.config.Host != nil {
		h.config.Host.Emit(eventchan.HarnessUIQuery, harnessUIQueryEvent{ID: id, Spec: spec, PageID: pageID})
	}

	if timeout > harnessUIQueryNoClientGrace {
		// Only shorten the wait; never lengthen one. The perf sampler
		// passes a deadline tighter than the grace and owns its own
		// skipped-sample semantics.
		if answered, result, err := h.awaitClientOrGrace(ch); answered {
			return result, err
		}
		if h.connectedClients() == 0 {
			// Same sentence a timeout produces, because it names the same
			// two fixable states — it just arrives immediately instead of
			// ten seconds later.
			return nil, fmt.Errorf(
				"harness ui query failed after %s: no frontend attached or harness bridge inactive",
				harnessUIQueryNoClientGrace,
			)
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-ch:
		return harnessUIResult(result)
	case <-timer.C:
		return nil, fmt.Errorf(
			"harness ui query timed out after %s: no frontend attached or harness bridge inactive",
			timeout,
		)
	}
}

// awaitClientOrGrace polls for a client to appear on the event bus,
// returning as soon as one is connected — or as soon as the query is
// answered, which a fast local bridge can manage inside the grace.
//
// Reports answered=true only when the reply actually arrived; a false
// return means the caller should re-check connectedClients and decide.
func (h *Harness) awaitClientOrGrace(ch <-chan json.RawMessage) (answered bool, result json.RawMessage, err error) {
	if h.connectedClients() > 0 {
		return false, nil, nil
	}
	deadline := time.NewTimer(harnessUIQueryNoClientGrace)
	defer deadline.Stop()
	poll := time.NewTicker(harnessUIQueryClientPoll)
	defer poll.Stop()
	for {
		select {
		case raw := <-ch:
			res, resErr := harnessUIResult(raw)
			return true, res, resErr
		case <-poll.C:
			if h.connectedClients() > 0 {
				return false, nil, nil
			}
		case <-deadline.C:
			return false, nil, nil
		}
	}
}

// connectedClients reports how many clients are attached to the event
// bus — the only thing the backend can actually know about the bridge.
//
// SubscriberCount, not ChannelSubscriberCount: the SPA subscribes to
// every channel by default, and ChannelSubscriberCount deliberately
// counts only EXPLICIT per-channel subscribers, so it reads zero for a
// perfectly healthy attached page. Zero here means no client at all,
// which is the one state a query can rule out cheaply. A connected client
// whose bundle carries no bridge is indistinguishable from a slow one, so
// that case still waits out the full timeout.
//
// No bus (a test App with only testEmitHook) reports 1: absence of a bus
// is not evidence of absence of a listener, and fast-failing there would
// break every fixture that answers queries directly.
func (h *Harness) connectedClients() int {
	if h.config.Host == nil {
		return 1
	}
	count, available := h.config.Host.ConnectedClients()
	if !available {
		return 1
	}
	return count
}

// harnessUIResult unwraps a bridge answer. `{"error": "..."}` is the
// bridge's one reserved shape; anything else is the answer.
func harnessUIResult(result json.RawMessage) (json.RawMessage, error) {
	var envelope struct {
		Error string `json:"error"`
	}
	// A non-object answer (array, string, null) simply has no error field.
	if err := json.Unmarshal(result, &envelope); err == nil && strings.TrimSpace(envelope.Error) != "" {
		return nil, fmt.Errorf("harness ui query: %s", envelope.Error)
	}
	return result, nil
}
