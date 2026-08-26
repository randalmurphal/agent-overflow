// app_harness_ui.go is the backend half of the frontend bridge
// (docs/specs/testing-harness.md §4): a request/reply channel that lets a
// harness caller ask the ATTACHED SPA what it is rendering, over the same
// WebSocket everything else uses.
//
// Why a request/reply pair rather than a push: the answer is a function of
// live DOM, so only the frontend can compute it, and the caller is a Bash
// tool or a Playwright spec that wants one value back from one call.
// `HarnessUIQuery` emits `harness:ui-query {id, spec}` and parks on the id;
// the bridge answers with `HarnessUIQueryReply(id, result)`. Both live on
// the Harness receiver, so the reply path exists exactly where the query
// path does — under --harness/--soak, LocalOnly.
//
// Correlation rules, all of them consequences of "several frontends may be
// attached to one backend" (a browser tab beside the webview, two tabs of a
// remote session):
//
//   - FIRST reply wins. The waiter is removed under the same lock that reads
//     it, so a second reply for the same id finds nothing.
//   - A late or duplicate reply is DROPPED SILENTLY. It names an id whose
//     caller has already been answered or has already timed out; erroring
//     would turn a harmless race into a red test in the bridge.
//   - A timeout says "no frontend attached or harness bridge inactive",
//     because those are the two states a caller can actually fix (open the
//     page; build a frontend whose bootstrap carries harness:true).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
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
	ID   string          `json:"id"`
	Spec json.RawMessage `json:"spec"`
}

// harnessUIBridge is the waiter registry. Its own mutex, deliberately not
// the Harness-wide one: a query parks for up to 10s and must not hold the
// lock that scenario rules, recordings and the replayer share.
type harnessUIBridge struct {
	mu      sync.Mutex
	seq     uint64
	waiters map[string]chan json.RawMessage
}

func (b *harnessUIBridge) register() (string, chan json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := "uq-" + strconv.FormatUint(b.seq, 10)
	ch := make(chan json.RawMessage, 1)
	if b.waiters == nil {
		b.waiters = make(map[string]chan json.RawMessage, 1)
	}
	b.waiters[id] = ch
	return id, ch
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
func (b *harnessUIBridge) deliver(id string, result json.RawMessage) bool {
	b.mu.Lock()
	ch, ok := b.waiters[id]
	if ok {
		delete(b.waiters, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered to 1 and this is the only send, so it cannot block even if
	// the caller has already abandoned the channel on a timeout.
	ch <- result
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

// HarnessUIQueryReply resolves the waiter for `id`. Called by the frontend
// bridge; never by a test directly, except to prove the drop rules.
//
// An id with no waiter is a no-op and NOT an error: it means the caller was
// already answered by another attached frontend or has already timed out,
// and neither is the replying bridge's fault.
func (h *Harness) HarnessUIQueryReply(id string, result json.RawMessage) error {
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
	if !h.ui.deliver(id, result) {
		log.Printf("harness: ui-query reply for unknown id %q dropped (late, duplicate, or timed out)", id)
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
	id, ch := h.ui.register()
	defer h.ui.release(id)
	h.app.emit(eventchan.HarnessUIQuery, harnessUIQueryEvent{ID: id, Spec: spec})

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
	bus := h.app.eventBus.Load()
	if bus == nil {
		return 1
	}
	return bus.SubscriberCount()
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
