package triage

import (
	"encoding/json"
	"sync"
	"testing"
)

// recordingDispatcher captures every dispatch call so tests can assert
// on the (threadID, batch) pairs the router produced. Locks because
// fireFlushTriggerOnce can be called from concurrent test goroutines.
type recordingDispatcher struct {
	mu    sync.Mutex
	calls []dispatchCall
}

type dispatchCall struct {
	ThreadID string
	Items    []QueuedFlushItem
}

func (rd *recordingDispatcher) dispatch(threadID string, items []QueuedFlushItem) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.calls = append(rd.calls, dispatchCall{ThreadID: threadID, Items: items})
}

func (rd *recordingDispatcher) snapshot() []dispatchCall {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	out := make([]dispatchCall, len(rd.calls))
	copy(out, rd.calls)
	return out
}

func makeQueueItem(id, message string) QueuedFlushItem {
	return QueuedFlushItem{
		ID:      id,
		Message: message,
		Payload: json.RawMessage(`{}`),
	}
}

func TestRegisterQueueItem_AppendsInOrder(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:2", "third"))

	got := router.QueuedFlushItems("t1")
	if len(got) != 3 {
		t.Fatalf("len: got %d want 3", len(got))
	}
	for i, want := range []string{"queue:0", "queue:1", "queue:2"} {
		if got[i].ID != want {
			t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestRegisterQueueItem_StampsEnqueuedAtWhenZero(t *testing.T) {
	router, _, _ := newTestRouter(t)

	stamp := router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	if stamp == 0 {
		t.Fatalf("stamp: expected non-zero EnqueuedAt, got 0")
	}
	got := router.QueuedFlushItems("t1")
	if got[0].EnqueuedAt != stamp {
		t.Errorf("EnqueuedAt: got %d, want %d", got[0].EnqueuedAt, stamp)
	}
}

func TestRegisterQueueItem_PreservesProvidedEnqueuedAt(t *testing.T) {
	router, _, _ := newTestRouter(t)

	item := makeQueueItem("queue:0", "x")
	item.EnqueuedAt = 12345
	stamp := router.RegisterQueueItem("t1", item)
	if stamp != 12345 {
		t.Fatalf("stamp: got %d, want 12345 (caller-provided)", stamp)
	}
}

func TestRegisterQueueItem_RejectsEmptyIDs(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if got := router.RegisterQueueItem("", makeQueueItem("queue:0", "x")); got != 0 {
		t.Errorf("empty threadID: got %d, want 0", got)
	}
	if got := router.RegisterQueueItem("t1", QueuedFlushItem{Message: "x"}); got != 0 {
		t.Errorf("empty itemID: got %d, want 0", got)
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should still be empty after rejected registers")
	}
}

func TestRegisterQueueItem_IsolatedPerThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("t1 should have queued items")
	}
	if router.HasQueuedFlushItems("t2") {
		t.Errorf("t2 should NOT have queued items — registers are thread-scoped")
	}
}

func TestDropQueueItem_RemovesMatchingEntry_PreservesOrder(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:2", "third"))

	dropped, ok := router.DropQueueItem("t1", "queue:1")
	if !ok {
		t.Fatalf("drop: ok=false")
	}
	if dropped.ID != "queue:1" || dropped.Message != "second" {
		t.Errorf("dropped: got %+v, want id=queue:1 message=second", dropped)
	}

	got := router.QueuedFlushItems("t1")
	if len(got) != 2 {
		t.Fatalf("len after drop: got %d, want 2", len(got))
	}
	if got[0].ID != "queue:0" || got[1].ID != "queue:2" {
		t.Errorf("order after drop: got [%s, %s], want [queue:0, queue:2]", got[0].ID, got[1].ID)
	}
}

func TestDropQueueItem_NoMatch_ReturnsFalse(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if _, ok := router.DropQueueItem("t1", "queue:nope"); ok {
		t.Errorf("drop unmatched id: ok=true, want false")
	}
	got := router.QueuedFlushItems("t1")
	if len(got) != 1 {
		t.Errorf("queue length after no-op drop: got %d, want 1", len(got))
	}
}

func TestDropQueueItem_LastEntry_DeletesMapKey(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	if _, ok := router.DropQueueItem("t1", "queue:0"); !ok {
		t.Fatalf("drop: ok=false")
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("HasQueuedFlushItems after dropping last entry: true, want false")
	}
}

func TestDropAllQueuedItems_ReturnsAllAndClears(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))

	got := router.DropAllQueuedItems("t1")
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].ID != "queue:0" || got[1].ID != "queue:1" {
		t.Errorf("order: got [%s, %s], want [queue:0, queue:1]", got[0].ID, got[1].ID)
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should be empty after DropAllQueuedItems")
	}
}

func TestDropAllQueuedItems_EmptyQueue_ReturnsNil(t *testing.T) {
	router, _, _ := newTestRouter(t)

	got := router.DropAllQueuedItems("t1")
	if got != nil {
		t.Errorf("DropAllQueuedItems on empty queue: got %v, want nil", got)
	}
}

func TestQueuedFlushItems_ReturnsCopySnapshot(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	snapshot := router.QueuedFlushItems("t1")
	if len(snapshot) != 1 {
		t.Fatalf("len: got %d, want 1", len(snapshot))
	}
	// Mutating the snapshot must not affect the router's internal state.
	snapshot[0].ID = "mutated"
	again := router.QueuedFlushItems("t1")
	if again[0].ID != "queue:0" {
		t.Errorf("internal state mutated by snapshot edit: got %q, want queue:0", again[0].ID)
	}
}

func TestFireFlushTriggerOnce_FiresAndConsumes(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))

	if !router.fireFlushTriggerOnce("t1", "round-A") {
		t.Fatalf("fire: returned false, expected true")
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls: got %d, want 1", len(calls))
	}
	if calls[0].ThreadID != "t1" || len(calls[0].Items) != 2 {
		t.Errorf("dispatch[0]: got threadID=%q, items=%d, want t1 / 2", calls[0].ThreadID, len(calls[0].Items))
	}
	if calls[0].Items[0].ID != "queue:0" || calls[0].Items[1].ID != "queue:1" {
		t.Errorf("dispatch order: got [%s, %s], want [queue:0, queue:1]", calls[0].Items[0].ID, calls[0].Items[1].ID)
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should be empty after fire")
	}
}

func TestFireFlushTriggerOnce_SecondCallSameRound_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	if !router.fireFlushTriggerOnce("t1", "round-A") {
		t.Fatalf("first fire: returned false")
	}

	// Re-register an item in the same round; the trigger must not
	// fire again until the next round.
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "y"))

	if router.fireFlushTriggerOnce("t1", "round-A") {
		t.Errorf("second fire same round: returned true, want false")
	}
	if len(rec.snapshot()) != 1 {
		t.Errorf("dispatch should have fired exactly once for round-A")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should retain queue:1 since trigger did not fire again")
	}
}

func TestFireFlushTriggerOnce_DifferentRound_FiresAgain(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	if !router.fireFlushTriggerOnce("t1", "round-A") {
		t.Fatalf("first fire: returned false")
	}

	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "y"))
	if !router.fireFlushTriggerOnce("t1", "round-B") {
		t.Errorf("fire on different round: returned false, want true")
	}
	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("dispatch calls: got %d, want 2", len(calls))
	}
	if calls[1].Items[0].ID != "queue:1" {
		t.Errorf("second dispatch item: got %q, want queue:1", calls[1].Items[0].ID)
	}
}

func TestFireFlushTriggerOnce_EmptyQueue_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	if router.fireFlushTriggerOnce("t1", "round-A") {
		t.Errorf("fire on empty queue: returned true, want false")
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("dispatch on empty queue: got calls, want none")
	}
}

func TestFireFlushTriggerOnce_NilDispatcher_DoesNotConsume(t *testing.T) {
	router, _, _ := newTestRouter(t)
	router.SetFlushDispatcher(nil)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if router.fireFlushTriggerOnce("t1", "round-A") {
		t.Errorf("fire with nil dispatcher: returned true, want false")
	}
	// Items must remain queued — without a dispatcher there's no one
	// to receive the batch, so consuming would lose data.
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue drained despite nil dispatcher")
	}
}

func TestFireFlushTriggerOnce_RejectsEmptyIDs(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if router.fireFlushTriggerOnce("", "round-A") {
		t.Errorf("empty threadID: returned true")
	}
	if router.fireFlushTriggerOnce("t1", "") {
		t.Errorf("empty roundID: returned true")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue mutated by rejected fire calls")
	}
}

func TestSetOpenRound_ClearsTriggerFiredMarker(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.fireFlushTriggerOnce("t1", "round-A")

	// setOpenRound on a new round id should drop the marker so the
	// next fire on round-B isn't blocked by the prior comparison.
	router.setOpenRound("t1", "round-B")
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "y"))

	if !router.fireFlushTriggerOnce("t1", "round-B") {
		t.Errorf("fire on round-B after setOpenRound: returned false, want true")
	}
}

func TestCleanupThread_SweepsQueueAndTriggerMarker(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.fireFlushTriggerOnce("t1", "round-A") // seeds flushTriggerFiredByRound
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "y"))

	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("setup: expected queue:1 to remain after partial drain")
	}

	router.CleanupThread("t1")

	if router.HasQueuedFlushItems("t1") {
		t.Errorf("CleanupThread did not sweep queuedFlushItems")
	}

	// After CleanupThread, the trigger-fired marker is also gone — a
	// fresh session re-attaching to the same threadID (which clears
	// the stoppedThreads flag via EventInit, then opens a new round)
	// must be able to fire its first-round trigger.
	router.RegisterQueueItem("t1", makeQueueItem("queue:2", "z"))
	if !router.fireFlushTriggerOnce("t1", "round-A") {
		t.Errorf("fire after CleanupThread on same roundID: returned false — trigger marker not swept")
	}
}

func TestCleanupThread_IsolatedPerThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.RegisterQueueItem("t2", makeQueueItem("queue:1", "y"))

	router.CleanupThread("t1")

	if router.HasQueuedFlushItems("t1") {
		t.Errorf("t1 queue should be cleared")
	}
	if !router.HasQueuedFlushItems("t2") {
		t.Errorf("t2 queue should NOT be affected by t1 cleanup")
	}
}
