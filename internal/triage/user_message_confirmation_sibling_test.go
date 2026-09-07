package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A failed confirmation freezes A's boundary, not authority to relocate B
// after B has independently confirmed at a later provider boundary.
func TestConfirmationRetryPreservesLaterConfirmedSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	first := store.Item{ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "first", CreatedAt: now, UpdatedAt: now}
	second := first
	second.ID, second.Summary = "user:0:flush:2", "second"
	for i, item := range []store.Item{first, second} {
		wireID := []string{"echo-first", "echo-second"}[i]
		if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", item.ID, item, 1, now+int64(i), PendingSendExpectation{ProviderItemID: wireID}); err != nil {
			t.Fatal(err)
		}
	}
	if got := promoteQuietForTest(router, "t1"); len(got) != 2 {
		t.Fatalf("promoted %d rows", len(got))
	}
	secondPromoted := mustGetItem(t, st, "t1", second.ID)
	if err := st.DeleteThreadItem("t1", second.ID); err != nil {
		t.Fatal(err)
	}
	queueTail := func(id string) {
		router.mu.Lock()
		defer router.mu.Unlock()
		router.state("t1").interruptQueue = append(router.state("t1").interruptQueue, queuedPersistence{item: store.Item{
			ID: id, ThreadID: "t1", TurnIndex: 0, Kind: "tool_call", Role: "assistant", Status: "completed", Summary: id, CreatedAt: now, UpdatedAt: now,
		}})
	}
	firstEcho := provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "first", Meta: json.RawMessage(`{"provider_item_id":"echo-first"}`), Timestamp: time.Now()}
	queueTail("tail-before-first")
	if err := router.Handle(firstEcho); err == nil {
		t.Fatal("missing sibling should fail the entire first placement")
	}
	secondPromoted.ItemIndex = 1
	if err := st.InsertItem(secondPromoted); err != nil {
		t.Fatal(err)
	}
	// Another provider boundary is reached while A's cache confirmation is
	// unresolved. B consumes later input, and must keep that later boundary.
	queueTail("tail-before-second")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "second", Meta: json.RawMessage(`{"provider_item_id":"echo-second"}`), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(firstEcho); err != nil {
		t.Fatalf("retry first echo: %v", err)
	}
	a := mustGetItem(t, st, "t1", first.ID)
	b := mustGetItem(t, st, "t1", second.ID)
	tailA := mustGetItem(t, st, "t1", "tail-before-first")
	tailB := mustGetItem(t, st, "t1", "tail-before-second")
	if !(tailA.ItemIndex < a.ItemIndex && a.ItemIndex < tailB.ItemIndex && tailB.ItemIndex < b.ItemIndex) {
		t.Fatalf("confirmation retry changed provider order: tailA=%d A=%d tailB=%d B=%d", tailA.ItemIndex, a.ItemIndex, tailB.ItemIndex, b.ItemIndex)
	}
	for _, pair := range [][2]store.Item{{a, tailA}, {b, tailB}} {
		state, err := itemmeta.DecodePromotionState(pair[0].Meta)
		if err != nil || !state.HasEchoBoundary || state.EchoBoundary != pair[1].ItemIndex {
			t.Fatalf("%s lost its own consumption boundary: %+v, %v (want %d)", pair[0].ID, state, err, pair[1].ItemIndex)
		}
	}
	if _, _, err := st.DeleteConversationFromItem("t1", second.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, tailA.ID, tailB.ID} {
		if _, found, err := st.GetThreadItem("t1", id); err != nil || !found {
			t.Fatalf("B revert lost preceding item %s: %v", id, err)
		}
	}
	if _, _, err := st.DeleteConversationFromItem("t1", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetThreadItem("t1", tailA.ID); err != nil || !found {
		t.Fatalf("A revert lost preceding tail: %v", err)
	}
	if _, found, err := st.GetThreadItem("t1", tailB.ID); err != nil || found {
		t.Fatalf("A revert kept its following output: %v", err)
	}
}
