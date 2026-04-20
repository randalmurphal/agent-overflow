package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestInterruptQueueDrainsInArrivalOrder pins the FIFO contract on the
// interrupt queue. While a streaming text item is open, two background
// tool completions (A then B) arrive. The queue holds both, and on
// settle the persistence order MUST be A → B. If the drain switched to
// LIFO (or a map iteration, etc.), tool completions would land in the
// timeline out of order relative to when the provider emitted them —
// users would see the second-finished background task appear before the
// first-finished.
func TestInterruptQueueDrainsInArrivalOrder(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed two background tool_call launches BEFORE streaming text opens —
	// handleToolStart calls settleStreamingScope, which would close a
	// streaming block if one is already open.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "bg"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-A",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-B",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// Now open a streaming text item so the interrupt queue engages.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "streaming ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Sanity: no background_done rows should exist yet (text is streaming).
	doneBefore := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneBefore) != 0 {
		t.Fatalf("background_done rows appeared before queue drain: %d", len(doneBefore))
	}

	completeMetaA, _ := json.Marshal(map[string]any{
		"is_background": true,
		"exit_code":     0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-A",
		Meta: completeMetaA, Content: "body A", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	completeMetaB, _ := json.Marshal(map[string]any{
		"is_background": true,
		"exit_code":     0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-B",
		Meta: completeMetaB, Content: "body B", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	// While streaming is still open, neither completion has been persisted.
	doneQueued := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneQueued) != 0 {
		t.Fatalf("background_done rows should still be queued, got %d", len(doneQueued))
	}

	// Settle streaming by stopping the content block; the queue must drain.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 2 {
		t.Fatalf("expected 2 drained background_done items, got %d", len(drained))
	}

	// Persistence order is the observable fact we care about. ListItems
	// orders by (turn_index, item_index), and item_index is monotonically
	// assigned at persist time — so an item_index < tells us A landed before B.
	var aIdx, bIdx = -1, -1
	for _, it := range drained {
		switch it.CompletionOf {
		case providerScopedItemID("bg-A"):
			aIdx = it.ItemIndex
		case providerScopedItemID("bg-B"):
			bIdx = it.ItemIndex
		}
	}
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("missing completion rows: aIdx=%d bIdx=%d items=%+v", aIdx, bIdx, drained)
	}
	if aIdx >= bIdx {
		t.Fatalf("arrival order violated: A item_index=%d must be < B item_index=%d", aIdx, bIdx)
	}

	// Mirror check on the emission stream: the order in which
	// provider:item_upsert events fire for the two completions must also
	// be A → B (the frontend reconciler replays emissions in order).
	upsertOrder := []string{}
	for _, e := range *emissions {
		if e.eventName != "provider:item_upsert" {
			continue
		}
		item, ok := e.data.(store.Item)
		if !ok {
			continue
		}
		if item.Kind != itemKindBackgroundDone {
			continue
		}
		upsertOrder = append(upsertOrder, item.CompletionOf)
	}
	if len(upsertOrder) < 2 {
		t.Fatalf("expected 2 background_done upserts, got %+v", upsertOrder)
	}
	if upsertOrder[0] != providerScopedItemID("bg-A") ||
		upsertOrder[1] != providerScopedItemID("bg-B") {
		t.Fatalf("upsert arrival order violated: got %+v, want [bg-A, bg-B]", upsertOrder)
	}
}
