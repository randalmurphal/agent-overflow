package triage

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// recordingDispatcher captures every dispatch call so tests can assert
// on the (threadID, batch) pairs the router produced. Locks because
// tryFlushQueue can be called from concurrent test goroutines.
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

func (rd *recordingDispatcher) waitForAtLeastCalls(t *testing.T, want int) []dispatchCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := rd.snapshot()
		if len(calls) >= want {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatcher calls: got %d, want at least %d", len(calls), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func makeQueueItem(id, message string) QueuedFlushItem {
	return QueuedFlushItem{
		ID:      id,
		Message: message,
		Payload: json.RawMessage(`{}`),
	}
}

func seedCodexThreadWithLiveSubagent(t *testing.T, st *store.Store, threadID string) int64 {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            threadID,
		ProjectID:     triageTestProjectID,
		Title:         "Codex",
		Provider:      "codex",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create Codex thread: %v", err)
	}
	if err := st.InsertItem(store.Item{
		ID:           "spawn-active",
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]}}`,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed Codex subagent launch: %v", err)
	}
	return now
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

func TestTryFlushQueue_FiresAndConsumes(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))

	if !router.tryFlushQueue("t1") {
		t.Fatalf("fire: returned false, expected true")
	}
	calls := rec.waitForAtLeastCalls(t, 1)
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

func TestFlushQueuedItems_DispatchesQueuedItems(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))

	if !router.FlushQueuedItems("t1") {
		t.Fatalf("FlushQueuedItems returned false")
	}
	calls := rec.waitForAtLeastCalls(t, 1)
	if len(calls) != 1 {
		t.Fatalf("dispatch calls: got %d, want 1", len(calls))
	}
	if calls[0].ThreadID != "t1" || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatch call: got %#v", calls[0])
	}
}

// TestTryFlushQueue_SecondCallWithNewItems_FiresAgain is the regression
// guard for the per-round suppression bug. The original
// fireFlushTriggerOnce tracked a "fired this round" marker so a
// second call within the same round no-op'd. That blocked any user
// message queued AFTER the first drain from ever reaching the
// provider while the round stayed open — observed in production with
// a long-running multi-bash sequence (subagents + 2 inline bashes):
// the first-seam flush set the marker, the user queued a second
// message during the first bash, and the second bash's trigger was
// silently suppressed. The marker is now removed; the queue-empty
// check provides idempotency.
func TestTryFlushQueue_SecondCallWithNewItems_FiresAgain(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	if !router.tryFlushQueue("t1") {
		t.Fatalf("first fire: returned false")
	}

	// User queues a second message after the first drain. Within the
	// same wire round, the next top-level tool_use must drain again —
	// otherwise the second message is locked out for the rest of the
	// round (the production bug).
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))

	if !router.tryFlushQueue("t1") {
		t.Fatalf("second fire (new items, same round): returned false — per-round suppression regressed")
	}
	calls := rec.waitForAtLeastCalls(t, 2)
	if len(calls) != 2 {
		t.Fatalf("dispatch calls: got %d, want 2", len(calls))
	}
	for _, call := range calls {
		if len(call.Items) != 1 {
			t.Fatalf("dispatch batch size: got %d, want 1", len(call.Items))
		}
	}
	for i, want := range []string{"queue:0", "queue:1"} {
		if calls[i].Items[0].ID != want {
			t.Errorf("dispatch[%d] item: got %q, want %q", i, calls[i].Items[0].ID, want)
		}
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should be empty after second fire")
	}
}

func TestTryFlushQueue_SecondCallEmptyQueue_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	if !router.tryFlushQueue("t1") {
		t.Fatalf("first fire: returned false")
	}

	// No new items — the second call sees an empty queue and no-ops.
	// This is the natural idempotency the queue-empty check provides;
	// the previous per-round marker was redundant for this case.
	if router.tryFlushQueue("t1") {
		t.Errorf("second fire (empty queue): returned true, want false")
	}
	if len(rec.waitForAtLeastCalls(t, 1)) != 1 {
		t.Errorf("dispatch should have fired exactly once when queue stays empty after first drain")
	}
}

func TestTryFlushQueue_EmptyQueue_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	if router.tryFlushQueue("t1") {
		t.Errorf("fire on empty queue: returned true, want false")
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("dispatch on empty queue: got calls, want none")
	}
}

func TestTryFlushQueue_NilDispatcher_DoesNotConsume(t *testing.T) {
	router, _, _ := newTestRouter(t)
	router.SetFlushDispatcher(nil)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if router.tryFlushQueue("t1") {
		t.Errorf("fire with nil dispatcher: returned true, want false")
	}
	// Items must remain queued — without a dispatcher there's no one
	// to receive the batch, so consuming would lose data.
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue drained despite nil dispatcher")
	}
}

func TestMaybeFlushQueueAtBoundary_BlocksOnLiveCodexSubagent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	seedCodexThreadWithLiveSubagent(t, st, "t-codex")

	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	router.RegisterQueueItem("t-codex", makeQueueItem("queue:0", "wait for subagent"))

	if router.maybeFlushQueueAtBoundary("t-codex") {
		t.Fatal("maybeFlushQueueAtBoundary flushed despite live Codex subagent")
	}
	if !router.HasQueuedFlushItems("t-codex") {
		t.Fatal("queue should remain while Codex subagent is live")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("dispatcher calls: got %+v, want none", calls)
	}
}

func TestCleanupThread_MarksLiveCodexSubagentLaunchesInactive(t *testing.T) {
	router, st, _ := newTestRouter(t)
	seedCodexThreadWithLiveSubagent(t, st, "t-codex")

	active, err := st.HasLiveCodexSubagentLaunch("t-codex")
	if err != nil {
		t.Fatalf("has live before cleanup: %v", err)
	}
	if !active {
		t.Fatal("setup: expected live Codex subagent launch")
	}

	router.CleanupThread("t-codex")

	active, err = st.HasLiveCodexSubagentLaunch("t-codex")
	if err != nil {
		t.Fatalf("has live after cleanup: %v", err)
	}
	if active {
		t.Fatal("CleanupThread should mark Codex subagent launches inactive")
	}
}

func TestTryFlushQueue_RejectsEmptyThreadID(t *testing.T) {
	router, _, _ := newTestRouter(t)
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))

	if router.tryFlushQueue("") {
		t.Errorf("empty threadID: returned true")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue mutated by rejected fire call")
	}
}

func TestCleanupThread_SweepsQueue(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.tryFlushQueue("t1")
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "y"))

	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("setup: expected queue:1 to remain after partial drain")
	}

	router.CleanupThread("t1")

	if router.HasQueuedFlushItems("t1") {
		t.Errorf("CleanupThread did not sweep queuedFlushItems")
	}

	// A fresh registration after cleanup must drain normally — no
	// stale state from the prior session blocks the new flush.
	router.RegisterQueueItem("t1", makeQueueItem("queue:2", "z"))
	if !router.tryFlushQueue("t1") {
		t.Errorf("fire after CleanupThread: returned false")
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

// TestTryFlushQueue_BatchStaysCountedAcrossHandoff pins the round-14
// close-out fix for the boundary-drain handoff window (C14-1): between
// deleting a batch from queuedFlushItems and the App dispatcher
// recording it in-flight, the batch must still be visible to the
// revert-on-interrupt predicate through QueuedFlushItemCount (the
// claim count). Without it, a Stop click landing inside the dispatcher
// callback sees zero pending flush work on a turn with no durable
// agent rows and wrongly reverts the turn-starting prompt while the
// follow-up is delivered.
func TestTryFlushQueue_BatchStaysCountedAcrossHandoff(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterQueueItem("t1", QueuedFlushItem{ID: "queue:q1", Message: "one"})
	router.RegisterQueueItem("t1", QueuedFlushItem{ID: "queue:q2", Message: "two"})

	var duringHandoff int
	router.SetFlushDispatcher(func(threadID string, items []QueuedFlushItem) {
		// The predicate's triage read, taken while the batch is neither
		// in the queue map nor yet recorded in-flight by the App layer.
		duringHandoff = router.QueuedFlushItemCount(threadID)
	})

	if !router.FlushQueuedItems("t1") {
		t.Fatal("FlushQueuedItems did not dispatch the queued batch")
	}
	if duringHandoff != 2 {
		t.Fatalf("QueuedFlushItemCount during handoff = %d, want 2 — a Stop in this window would see the batch as no pending work", duringHandoff)
	}
	if after := router.QueuedFlushItemCount("t1"); after != 0 {
		t.Fatalf("QueuedFlushItemCount after handoff = %d, want 0 — the claim must drop once the dispatcher returns", after)
	}
}

func TestFlushSettlementIsExactlyOnceAndCombinationPreservesBoth(t *testing.T) {
	var leftCount, rightCount atomic.Int32
	left := NewFlushSettlement(func() { leftCount.Add(1) })
	right := NewFlushSettlement(func() { rightCount.Add(1) })
	combined := CombineFlushSettlements(left, right)

	var callers sync.WaitGroup
	for range 32 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			combined.Settle()
		}()
	}
	callers.Wait()
	if got := leftCount.Load(); got != 1 {
		t.Fatalf("left settlement fired %d times, want 1", got)
	}
	if got := rightCount.Load(); got != 1 {
		t.Fatalf("right settlement fired %d times, want 1", got)
	}

	// Nil and zero values are legitimate for ordinary user-authored queue
	// items and must remain safe at unconditional settlement call sites.
	(*FlushSettlement)(nil).Settle()
	new(FlushSettlement).Settle()
}

// TestNextFlushSequence_SerializesWithDeferredEchoPersist pins the
// allocation race behind the codex-steer e2e flake (2026-08-25): the
// echo path pops a deferred entry and persists its row inside one
// flush-anchored section, so while that section runs the message is in
// NEITHER the registry nor SQLite. NextFlushSequence must take the same
// anchor — an unanchored reader landing in the window re-issues the
// consumed message's sequence, and the next steer's echo upserts over
// its row (three queued steers, "second prompt" gone).
//
// deferredPersistGate holds the anchored section open at exactly the
// pop->persist point. The allocation must (a) not return while the
// section holds the anchor and (b) return the sequence PAST the
// consumed message once it does.
func TestNextFlushSequence_SerializesWithDeferredEchoPersist(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)

	deferred := store.Item{
		ID:        "user:1:flush:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "second prompt",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}
	router.RegisterPendingFlushSendWithExpectation(
		"t1", "queue:second", deferred, 0, PendingSendExpectation{ByClientID: true})

	entered := make(chan struct{})
	release := make(chan struct{})
	router.deferredPersistGate = func() {
		close(entered)
		<-release
	}

	echoDone := make(chan error, 1)
	go func() {
		echoDone <- router.Handle(provider.ProviderEvent{
			Kind:      provider.EventUserText,
			ThreadID:  "t1",
			Content:   "second prompt",
			Meta:      json.RawMessage(`{"provider_item_id":"item_second","client_id":"user:1:flush:1"}`),
			Timestamp: time.Now(),
		})
	}()
	<-entered // the echo now holds the anchor, entry popped, row not yet persisted

	type allocResult struct {
		seq int
		err error
	}
	allocDone := make(chan allocResult, 1)
	go func() {
		seq, err := router.NextFlushSequence("t1", 1)
		allocDone <- allocResult{seq, err}
	}()

	// (a) The allocation must block on the anchor. A result now means it
	// read both sources inside the window — the collision.
	select {
	case res := <-allocDone:
		t.Fatalf("NextFlushSequence returned %d (err=%v) inside the pop->persist window; want it blocked on the flush anchor", res.seq, res.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-echoDone; err != nil {
		t.Fatalf("echo handle: %v", err)
	}
	res := <-allocDone
	if res.err != nil {
		t.Fatalf("NextFlushSequence: %v", res.err)
	}
	// (b) Past the persisted row: never 1 again.
	if res.seq != 2 {
		t.Fatalf("NextFlushSequence = %d after the echo persisted user:1:flush:1, want 2", res.seq)
	}
}
