package triage

import (
	"testing"

	"agent-overflow/internal/store"
)

func TestRegisterAndConsumePendingSend_FIFO(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	first, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok {
		t.Fatalf("expected first pending send to consume, got ok=false")
	}
	if first.AOItemID != "user:0" || first.TurnIndex != 0 {
		t.Fatalf("first pop: got %+v, want AOItemID=user:0 TurnIndex=0", first)
	}

	second, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok {
		t.Fatalf("expected second pending send to consume, got ok=false")
	}
	if second.AOItemID != "user:1" || second.TurnIndex != 1 {
		t.Fatalf("second pop: got %+v, want AOItemID=user:1 TurnIndex=1", second)
	}

	zero, ok := router.consumeMatchingPendingSend("t1", "")
	if ok {
		t.Fatalf("expected empty FIFO to return ok=false, got entry %+v", zero)
	}
	if zero != (pendingSend{}) {
		t.Fatalf("empty pop should return zero value, got %+v", zero)
	}
}

func TestPendingSendIsolatedPerThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("t1 should report has-pending after register")
	}
	if router.HasPendingSendForThread("t2") {
		t.Fatalf("t2 should NOT report has-pending — registers are thread-scoped")
	}

	zero, ok := router.consumeMatchingPendingSend("t2", "")
	if ok {
		t.Fatalf("consume on t2 should return ok=false, got entry %+v", zero)
	}

	// t1's entry must still be intact.
	head, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || head.AOItemID != "user:0" {
		t.Fatalf("t1 head after t2 consume attempt: got ok=%v entry=%+v", ok, head)
	}
}

func TestHasPendingSendForThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("fresh router should report no pending sends")
	}

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("expected has-pending after register")
	}

	if _, ok := router.consumeMatchingPendingSend("t1", ""); !ok {
		t.Fatalf("consume should succeed")
	}

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("expected no pending sends after final consume")
	}
}

func TestClearPendingSendForFailure_RemovesMatchingEntry(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})

	router.ClearPendingSendForFailure("t1", "user:1")

	first, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || first.AOItemID != "user:0" {
		t.Fatalf("first pop after clear: ok=%v entry=%+v want user:0", ok, first)
	}

	second, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || second.AOItemID != "user:2" {
		t.Fatalf("second pop after clear: ok=%v entry=%+v want user:2", ok, second)
	}

	if _, ok := router.consumeMatchingPendingSend("t1", ""); ok {
		t.Fatalf("queue should be empty after clearing one + consuming two")
	}
}

func TestClearPendingSendForFailure_NoMatch_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	router.ClearPendingSendForFailure("t1", "user:does-not-exist")

	first, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || first.AOItemID != "user:0" {
		t.Fatalf("first pop after no-op clear: ok=%v entry=%+v", ok, first)
	}
	second, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || second.AOItemID != "user:1" {
		t.Fatalf("second pop after no-op clear: ok=%v entry=%+v", ok, second)
	}
}

func TestClearPendingSendsForThread_SweepsAll(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})

	router.clearPendingSendsForThread("t1")

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("queue should be empty after sweep")
	}
	if _, ok := router.consumeMatchingPendingSend("t1", ""); ok {
		t.Fatalf("consume should fail after sweep")
	}
}

func TestMarkWireOnlyUserTextSeen_DedupBehavior(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if !router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("first sighting should return true")
	}
	if router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("second sighting of same id on same thread should return false (dup)")
	}

	if !router.markWireOnlyUserTextSeen("t1", "wire-2") {
		t.Fatalf("different id on same thread should return true (new)")
	}

	if !router.markWireOnlyUserTextSeen("t2", "wire-1") {
		t.Fatalf("same id on different thread should return true (thread-scoped)")
	}
	if router.markWireOnlyUserTextSeen("t2", "wire-1") {
		t.Fatalf("second sighting on t2 should return false (per-thread dedup)")
	}
}

func TestCleanupThread_SweepsPendingSendAndWireOnlySeen(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	router.markWireOnlyUserTextSeen("t1", "wire-1")
	router.markWireOnlyUserTextSeen("t1", "wire-2")

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("setup: expected pending sends before cleanup")
	}

	router.CleanupThread("t1")

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("CleanupThread should sweep pending sends")
	}
	if _, ok := router.consumeMatchingPendingSend("t1", ""); ok {
		t.Fatalf("CleanupThread should leave consume returning ok=false")
	}

	// After cleanup, the wire-only set is gone — a previously-seen id
	// must register as new again.
	if !router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("CleanupThread should sweep wireOnlyUserTextSeen — first sighting after cleanup should return true")
	}
}

func TestEagerPersistDeferredFlushSends_PersistsAndNilsDeferred(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	item1 := store.Item{
		ID:        "user:0:flush:1",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "first queued message",
		Meta:      `{"attachmentIds":[]}`,
		CreatedAt: 1,
		UpdatedAt: 1,
	}
	item2 := store.Item{
		ID:        "user:0:flush:2",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "second queued message",
		CreatedAt: 2,
		UpdatedAt: 2,
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "q-1", item1, 0, PendingSendExpectation{})
	router.RegisterPendingFlushSendWithExpectation("t1", "q-2", item2, 0, PendingSendExpectation{})

	result := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(result) != 2 {
		t.Fatalf("expected 2 eagerly persisted items, got %d", len(result))
	}
	if result[0].UserItemID != "user:0:flush:1" || result[0].Content != "first queued message" {
		t.Fatalf("unexpected first result: %+v", result[0])
	}
	if result[1].UserItemID != "user:0:flush:2" || result[1].Content != "second queued message" {
		t.Fatalf("unexpected second result: %+v", result[1])
	}

	// Items should be persisted in the store.
	row1, found, err := st.GetThreadItem("t1", "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("item 1 not found in store: found=%v err=%v", found, err)
	}
	if row1.Summary != "first queued message" {
		t.Fatalf("unexpected summary: %s", row1.Summary)
	}
	_, found2, err := st.GetThreadItem("t1", "user:0:flush:2")
	if err != nil || !found2 {
		t.Fatalf("item 2 not found in store: found=%v err=%v", found2, err)
	}

	// DeferredItem should be nil'd — echo takes stamp-only branch.
	head, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok {
		t.Fatalf("expected pending send to be consumable")
	}
	if head.DeferredItem != nil {
		t.Fatalf("DeferredItem should be nil after eager persist")
	}
	if head.QueueItemID != "q-1" {
		t.Fatalf("QueueItemID should be preserved: got %s", head.QueueItemID)
	}

	// Should have emitted provider:item_event upserts.
	upsertCount := 0
	for _, e := range emissions.snapshot() {
		if e.eventName == "provider:item_event" {
			upsertCount++
		}
	}
	if upsertCount < 2 {
		t.Fatalf("expected at least 2 item_event emissions, got %d", upsertCount)
	}
}

func TestEagerPersistDeferredFlushSends_SkipsNonFlushPendingSends(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Direct send (non-flush) — has no QueueItemID.
	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	result := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(result) != 0 {
		t.Fatalf("should not eagerly persist non-flush pending sends, got %d", len(result))
	}

	// The non-flush entry should still be consumable.
	if _, ok := router.consumeMatchingPendingSend("t1", ""); !ok {
		t.Fatalf("non-flush pending send should survive eager persist")
	}
}

func TestClearPendingSendsByItemIDs(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:0:flush:1", 0, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:0:flush:2", 0, PendingSendExpectation{})

	router.ClearPendingSendsByItemIDs("t1", []string{"user:0:flush:1", "user:0:flush:2"})

	// The non-flush entry should survive.
	head, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok {
		t.Fatalf("expected surviving entry")
	}
	if head.AOItemID != "user:0" {
		t.Fatalf("wrong survivor: got %s", head.AOItemID)
	}

	// Queue should be empty after the one survivor is consumed.
	if _, ok := router.consumeMatchingPendingSend("t1", ""); ok {
		t.Fatalf("expected empty queue after consuming survivor")
	}
}

func TestMaxPendingSendTurnIndex(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if _, ok := router.MaxPendingSendTurnIndex("t1"); ok {
		t.Fatalf("empty thread should return ok=false")
	}

	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	max, ok := router.MaxPendingSendTurnIndex("t1")
	if !ok || max != 1 {
		t.Fatalf("single entry: got max=%d ok=%v, want 1/true", max, ok)
	}

	router.RegisterPendingSendWithExpectation("t1", "user:3", 3, PendingSendExpectation{})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})
	max, ok = router.MaxPendingSendTurnIndex("t1")
	if !ok || max != 3 {
		t.Fatalf("multi entry: got max=%d ok=%v, want 3/true", max, ok)
	}

	if _, ok := router.MaxPendingSendTurnIndex("t2"); ok {
		t.Fatalf("unrelated thread should return ok=false")
	}
}

func TestClearPendingSendsByItemIDs_EmptyThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// Should not panic on empty thread.
	router.ClearPendingSendsByItemIDs("t1", []string{"user:0:flush:1"})
	router.ClearPendingSendsByItemIDs("", []string{"user:0:flush:1"})
}

// Identity matching: when entries carry an ExpectedProviderItemID, the echo's
// provider_item_id selects the matching entry — not the FIFO head — and a
// non-matching echo consumes nothing (it is provider-injected content).

func TestConsumeMatchingPendingSend_IdentityMatchPopsByID(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{ProviderItemID: "uuid-a"})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "uuid-b"})

	got, ok := router.consumeMatchingPendingSend("t1", "uuid-b")
	if !ok || got.AOItemID != "user:1" {
		t.Fatalf("consume by uuid-b: got ok=%v entry=%+v, want user:1", ok, got)
	}

	// user:0 must survive untouched as the remaining head.
	head, ok := router.peekPendingSendHead("t1")
	if !ok || head.AOItemID != "user:0" {
		t.Fatalf("remaining head: got ok=%v entry=%+v, want user:0", ok, head)
	}
}

func TestConsumeMatchingPendingSend_NoMatch_LeavesQueueIntact(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{ProviderItemID: "uuid-a"})
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "uuid-b"})

	got, ok := router.consumeMatchingPendingSend("t1", "uuid-injected")
	if ok {
		t.Fatalf("non-matching echo must not consume any entry, popped %+v", got)
	}

	// Both entries survive in order.
	first, _ := router.consumeMatchingPendingSend("t1", "uuid-a")
	second, _ := router.consumeMatchingPendingSend("t1", "uuid-b")
	if first.AOItemID != "user:0" || second.AOItemID != "user:1" {
		t.Fatalf("queue disturbed by non-matching echo: first=%+v second=%+v", first, second)
	}
}

func TestConsumeMatchingPendingSend_IDLessEchoPopsHead(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// Head expects an id, but the echo carries none. The carve-out pops the
	// head anyway so the id-less-echo diagnostics in handleUserText keep
	// firing against a matched entry (injected content always carries a uuid,
	// so an id-less echo can only be our own send on a degraded wire).
	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{ProviderItemID: "uuid-a"})

	got, ok := router.consumeMatchingPendingSend("t1", "")
	if !ok || got.AOItemID != "user:0" {
		t.Fatalf("id-less echo should pop head: got ok=%v entry=%+v", ok, got)
	}
}

// The 2026-08-24 mispop. An entry registered with ByClientID has announced
// that its echo will name it, so an echo carrying NO client id is provably
// somebody else's — and FIFO-popping it anyway stamps one message onto the
// other's row and leaves the real echo to persist as injected context. The
// id-less echo must skip past it to the first entry that can actually be
// consumed positionally.
func TestConsumeMatchingPendingSend_IDLessEchoSkipsClientIDEntries(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// A queued send dispatched with `clientUserMessageId` — it sits at the
	// head, waiting for the echo that names it.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:m1", store.Item{
		ID:        "user:3:flush:1",
		ThreadID:  "t1",
		TurnIndex: 3,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued",
	}, 10, PendingSendExpectation{ByClientID: true})
	// A plain send registered behind it (pre-identity Codex shape).
	router.RegisterPendingSendWithExpectation("t1", "user:4", 4, PendingSendExpectation{})

	got, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-1", "")
	if !ok || got.AOItemID != "user:4" {
		t.Fatalf("id-less echo popped %+v (ok=%v), want the plain entry user:4", got, ok)
	}

	head, ok := router.peekPendingSendHead("t1")
	if !ok || head.AOItemID != "user:3:flush:1" {
		t.Fatalf("the client-id entry did not survive: ok=%v head=%+v", ok, head)
	}

	named, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-2", "user:3:flush:1")
	if !ok || named.AOItemID != "user:3:flush:1" {
		t.Fatalf("the naming echo popped %+v (ok=%v), want user:3:flush:1", named, ok)
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatal("queue should be empty after both echoes")
	}
}

// With every entry expecting a client id — the steady state once all Codex
// sends stamp one — an id-less echo consumes NOTHING rather than falling back
// to the head.
func TestConsumeMatchingPendingSend_IDLessEcho_MatchesNothingWhenAllEntriesExpectClientIDs(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ByClientID: true})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{ByClientID: true})

	if got, ok := router.consumeMatchingPendingSendForEcho("t1", "codex-item-1", ""); ok {
		t.Fatalf("an id-less echo popped %+v", got)
	}
	if got, ok := router.consumeMatchingPendingSendForEcho("t1", "", ""); ok {
		t.Fatalf("a fully id-less echo popped %+v", got)
	}
	if head, ok := router.peekPendingSendHead("t1"); !ok || head.AOItemID != "user:1" {
		t.Fatalf("queue disturbed: ok=%v head=%+v", ok, head)
	}
}

// The skip applies to the identity scan too, not just the head-pop: a Claude
// entry sitting BEHIND a client-id entry is still reachable by its own uuid,
// and the client-id entry is never a candidate for it.
func TestConsumeMatchingPendingSend_IdentityScanSkipsClientIDEntries(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ByClientID: true})
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{ProviderItemID: "uuid-b"})

	got, ok := router.consumeMatchingPendingSendForEcho("t1", "uuid-b", "")
	if !ok || got.AOItemID != "user:2" {
		t.Fatalf("identity echo popped %+v (ok=%v), want user:2", got, ok)
	}
	if head, ok := router.peekPendingSendHead("t1"); !ok || head.AOItemID != "user:1" {
		t.Fatalf("the client-id entry did not survive: ok=%v head=%+v", ok, head)
	}
	// And an unrelated provider item id still matches nothing.
	if got, ok := router.consumeMatchingPendingSendForEcho("t1", "uuid-injected", ""); ok {
		t.Fatalf("an injected uuid popped %+v", got)
	}
}

// The registrar derives ExpectedClientID from the entry's own AOItemID — the
// id that went on the wire as `clientUserMessageId` — so a caller can never
// register an expectation that disagrees with what was dispatched.
func TestRegisterPendingSendWithExpectation_StampsTheAOItemIDAsTheClientID(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSendWithExpectation("t1", "user:7", 7, PendingSendExpectation{ByClientID: true})
	head, ok := router.peekPendingSendHead("t1")
	if !ok {
		t.Fatal("no entry registered")
	}
	if head.ExpectedClientID != "user:7" {
		t.Fatalf("ExpectedClientID = %q, want the AO row id user:7", head.ExpectedClientID)
	}
	if head.ExpectedProviderItemID != "" {
		t.Fatalf("ExpectedProviderItemID = %q, want empty for a client-id send", head.ExpectedProviderItemID)
	}

	// The Claude-shaped registrations must stay untouched.
	router.RegisterPendingSendWithExpectation("t2", "user:1", 1, PendingSendExpectation{ProviderItemID: "uuid-a"})
	claude, ok := router.peekPendingSendHead("t2")
	if !ok || claude.ExpectedClientID != "" || claude.ExpectedProviderItemID != "uuid-a" {
		t.Fatalf("claude entry = %+v (ok=%v), want ExpectedClientID empty and uuid-a expected", claude, ok)
	}
}

func TestConsumeMatchingPendingSend_HeadWithoutExpectedID_FIFOPop(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// Codex compatibility: entries registered without an expected id keep
	// order-based matching even when the echo carries a provider item id.
	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})

	got, ok := router.consumeMatchingPendingSend("t1", "codex_item_7")
	if !ok || got.AOItemID != "user:0" {
		t.Fatalf("FIFO pop for id-less head: got ok=%v entry=%+v", ok, got)
	}
}
