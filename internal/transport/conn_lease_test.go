package transport

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"github.com/coder/websocket"
)

// wireDelta is one item_event delta as it came off the socket, flattened out
// of whatever frame carried it.
type wireDelta struct {
	seq  uint64
	text string
}

// emitDelta publishes one streaming delta for a row, in the shape triage
// emits.
func emitDelta(t *testing.T, f *serverFixture, threadID, itemID, text string, updatedAt int64) {
	t.Helper()
	if _, err := f.bus.EmitEntity(eventchan.ProviderItemEvent, threadID, map[string]any{
		"action":    "delta",
		"threadId":  threadID,
		"itemId":    itemID,
		"kind":      "assistant_text",
		"delta":     text,
		"updatedAt": updatedAt,
	}); err != nil {
		t.Fatalf("emit delta: %v", err)
	}
}

// emitMarker publishes the end-of-collection sentinel on a channel the lease
// leaves alone. Doubling as the "everything else flows unchanged" assertion:
// a backgrounded connection's turn completions must not queue behind the
// delta window.
func emitMarker(t *testing.T, f *serverFixture, threadID string) {
	t.Helper()
	if _, err := f.bus.EmitEntity(eventchan.ProviderTurnCompleted, threadID, map[string]any{
		"threadId": threadID,
	}); err != nil {
		t.Fatalf("emit marker: %v", err)
	}
}

// wireEntries flattens one read frame into its events. Batch frames carry
// several; a plain event frame carries one. What the assertions below count
// is EVENTS, which is what the coalescing rule is about — not how many
// WebSocket messages happened to carry them.
func wireEntries(t *testing.T, raw []byte) []batchEventEntry {
	t.Helper()
	var probe struct {
		Type   string            `json:"type"`
		Events []batchEventEntry `json:"events"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if probe.Type == frameTypeBatch {
		return probe.Events
	}
	if probe.Type != frameTypeEvent {
		return nil
	}
	var frame ServerFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return []batchEventEntry{{Channel: frame.Channel, Seq: frame.Seq, Data: frame.Data}}
}

// asDelta reports the delta text on an item_event entry, or false for any
// other channel or action.
func asDelta(t *testing.T, entry batchEventEntry) (wireDelta, bool) {
	t.Helper()
	if entry.Channel != string(eventchan.ProviderItemEvent) {
		return wireDelta{}, false
	}
	var frame leaseItemFrame
	if err := json.Unmarshal(entry.Data, &frame); err != nil {
		t.Fatalf("decode item event: %v", err)
	}
	if frame.Action != itemStreamActionDelta {
		return wireDelta{}, false
	}
	return wireDelta{seq: entry.Seq, text: frame.Delta}, true
}

// deltasBeforeMarker reads until the marker channel arrives, returning every
// item_event delta seen on the way. One ordered connection means the marker
// is a fence: anything the server meant to send before it has been written.
func deltasBeforeMarker(t *testing.T, conn *websocket.Conn) []wireDelta {
	t.Helper()
	var out []wireDelta
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, entry := range wireEntries(t, readPastHello(t, conn)) {
			if entry.Channel == string(eventchan.ProviderTurnCompleted) {
				return out
			}
			if delta, ok := asDelta(t, entry); ok {
				out = append(out, delta)
			}
		}
	}
	t.Fatalf("the marker never arrived; collected %v", out)
	return nil
}

// deltasUntil reads until `want` item_event deltas have arrived.
func deltasUntil(t *testing.T, conn *websocket.Conn, want int) []wireDelta {
	t.Helper()
	var out []wireDelta
	deadline := time.Now().Add(5 * time.Second)
	for len(out) < want && time.Now().Before(deadline) {
		for _, entry := range wireEntries(t, readPastHello(t, conn)) {
			if delta, ok := asDelta(t, entry); ok {
				out = append(out, delta)
			}
		}
	}
	if len(out) < want {
		t.Fatalf("only %d of %d deltas arrived: %v", len(out), want, out)
	}
	return out
}

// TestConnLeaseBackgroundCoalescesAndResumeFlushes covers the whole lifecycle
// on a real socket: a burst is held, one merged frame per row comes out
// carrying the concatenated text in arrival order and the last merged frame's
// seq, and resuming delivers it without waiting for the window.
func TestConnLeaseBackgroundCoalescesDeltas(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, State: leaseStateBackground})
	barrier(t, conn, "after-lease")

	emitDelta(t, f, "thread-A", "item-1", "Hel", 10)
	emitDelta(t, f, "thread-A", "item-2", "Wor", 11)
	emitDelta(t, f, "thread-A", "item-1", "lo", 12)
	emitDelta(t, f, "thread-A", "item-2", "ld", 13)
	// The window arms when the pump takes the first of those, within a
	// millisecond of here. Held for the immediacy assertion below.
	armedAt := time.Now()
	emitMarker(t, f, "thread-A")

	// The marker is emitted last and arrives first: nothing is coalesced
	// except deltas, and the window is holding all four.
	if held := deltasBeforeMarker(t, conn); len(held) != 0 {
		t.Fatalf("deltas escaped the window: %v", held)
	}

	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, State: leaseStateActive})
	got := deltasUntil(t, conn, 2)
	// Nothing else is emitted after the resume, so only two things could
	// have produced these frames: the resume nudge, or the window's own
	// timer. Arriving inside the window rules the timer out — which is what
	// "active flushes immediately" means, and the margin is the whole window
	// against a loopback round trip.
	if elapsed := time.Since(armedAt); elapsed >= leaseDeltaWindow {
		t.Fatalf("the flush took %s, past the %s window: the resume did not flush it", elapsed, leaseDeltaWindow)
	}
	if len(got) != 2 {
		t.Fatalf("delta frames = %v, want one merged frame per row", got)
	}
	if got[0] != (wireDelta{seq: 3, text: "Hello"}) {
		t.Fatalf("row 1 = %+v, want \"Hello\" at the last merged seq 3", got[0])
	}
	if got[1] != (wireDelta{seq: 4, text: "World"}) {
		t.Fatalf("row 2 = %+v, want \"World\" at the last merged seq 4", got[1])
	}
}

// TestConnLeaseBackgroundFlushesOnTheWindow is the other flush path: no
// resume, so the window's own timer is what delivers. Asserted on CONTENT —
// the property is that a backgrounded client still gets its text, bounded.
func TestConnLeaseBackgroundFlushesOnTheWindow(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, State: leaseStateBackground})
	barrier(t, conn, "after-lease")

	emitDelta(t, f, "thread-A", "item-1", "one ", 10)
	emitDelta(t, f, "thread-A", "item-1", "two", 11)
	emitMarker(t, f, "thread-A")

	if held := deltasBeforeMarker(t, conn); len(held) != 0 {
		t.Fatalf("the marker queued behind the window: %v", held)
	}
	got := deltasUntil(t, conn, 1)
	if got[0] != (wireDelta{seq: 2, text: "one two"}) {
		t.Fatalf("window flush = %+v, want one merged \"one two\" at seq 2", got[0])
	}
}

// TestConnLeaseFlushesPendingDeltaBeforeAMeta: a `meta` re-states a row the
// client already holds, and the frontend needs it to land against text the
// user has already been shown (triage item_events.go). So a pass-through
// action flushes what the window is holding and goes out behind it.
func TestConnLeaseFlushesPendingDeltaBeforeAMeta(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, State: leaseStateBackground})
	barrier(t, conn, "after-lease")

	emitDelta(t, f, "thread-A", "item-1", "text", 10)
	armedAt := time.Now()
	if _, err := f.bus.EmitEntity(eventchan.ProviderItemEvent, "thread-A", map[string]any{
		"action":   "meta",
		"threadId": "thread-A",
		"itemId":   "item-1",
		"kind":     "assistant_text",
		"meta":     `{"pathRefs":[]}`,
	}); err != nil {
		t.Fatalf("emit meta: %v", err)
	}
	emitMarker(t, f, "thread-A")

	got := deltasBeforeMarker(t, conn)
	if len(got) != 1 || got[0].text != "text" {
		t.Fatalf("frames before the marker = %v, want the held delta flushed ahead of the meta", got)
	}
	// The meta did it, not the window: both were emitted in the same
	// millisecond and the whole exchange fits inside one window.
	if elapsed := time.Since(armedAt); elapsed >= leaseDeltaWindow {
		t.Fatalf("the exchange took %s, past the %s window — the flush is not attributable to the meta", elapsed, leaseDeltaWindow)
	}
}

// TestConnLeaseRejectsUnknownState: a spelling this build does not know is a
// bad_params refusal that leaves the lease exactly as it was. Proven by
// streaming afterwards — the connection is still active, so every delta is
// still its own frame.
func TestConnLeaseRejectsUnknownState(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, ID: "bad", State: "suspended"})

	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != ErrCodeBadParams || got.ID != "bad" {
		t.Fatalf("frame = %#v, want a bad_params error echoing the frame id", got)
	}
	// An ABSENT state is the same refusal: "the client meant active" is not
	// a reading this frame is allowed to have.
	sendFrame(t, conn, ClientFrame{Type: frameTypeLease, ID: "empty"})
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Code != ErrCodeBadParams {
		t.Fatalf("frame = %#v, want a bad_params error for an absent state", got)
	}

	barrier(t, conn, "after-refusal")
	emitDelta(t, f, "thread-A", "item-1", "a", 10)
	emitDelta(t, f, "thread-A", "item-1", "b", 11)
	emitMarker(t, f, "thread-A")
	if streamed := deltasBeforeMarker(t, conn); len(streamed) != 2 {
		t.Fatalf("delta frames = %v, want both delivered — the refused frame changed nothing", streamed)
	}
}

// TestConnWithoutLeaseFrameStreamsUnchanged is the compatibility floor. Every
// desktop client, every browser client and every Go client in the tree sends
// no lease, and each must receive exactly the frames it did before the frame
// existed: one per delta, each with its own seq and its own text.
func TestConnWithoutLeaseFrameStreamsUnchanged(t *testing.T) {
	f := newServerFixture(t)
	conn := f.dial(t)
	barrier(t, conn, "connected")

	emitDelta(t, f, "thread-A", "item-1", "a", 10)
	emitDelta(t, f, "thread-A", "item-1", "b", 11)
	emitDelta(t, f, "thread-A", "item-2", "c", 12)
	emitMarker(t, f, "thread-A")

	got := deltasBeforeMarker(t, conn)
	if len(got) != 3 {
		t.Fatalf("delta frames = %v, want one per emit", got)
	}
	for i, want := range []wireDelta{{1, "a"}, {2, "b"}, {3, "c"}} {
		if got[i] != want {
			t.Fatalf("frame %d = %+v, want %+v", i, got[i], want)
		}
	}
}
