package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestItemUpdatedDoesNotReopenCompletedToolCall pins the invariant that a
// late-arriving EventToolStart for an already-completed tool_call row
// does NOT transition it back to running or streaming. Codex emits
// `item/updated` notifications which the adapter layer translates into
// EventToolStart with the existing ItemID; the Claude adapter exhibits
// the same pattern when it resynthesises `system/task_started` after a
// reconnect. Either way, the row is authoritative and must survive
// re-delivery.
//
// Regression guard: before the status preservation in
// persistToolCallLaunch landed, a duplicate EventToolStart would rewrite
// status to running, wiping the completed status and breaking the
// frontend's collapsed-card rendering.
func TestItemUpdatedPreservesCompletedToolCallStatus(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-ix",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "tool-ix",
		Meta: completeMeta, Content: "stdout", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	before, _, _ := st.GetThreadItem("t1", "tool-ix")
	if before.Status != statusCompleted {
		t.Fatalf("prerequisite: status after complete = %q, want completed", before.Status)
	}

	// Simulated item/updated: another EventToolStart arrives carrying
	// the same ItemID. The router must not reset the status flag on
	// the completed row.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-ix",
		ItemType: "Bash", Meta: startMeta, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second start: %v", err)
	}

	after, _, _ := st.GetThreadItem("t1", "tool-ix")
	if after.Status != statusCompleted {
		t.Errorf("status after duplicate start = %q, want completed (item/updated must not reopen)", after.Status)
	}
	// CreatedAt must not shift — the row is the same entity.
	if before.CreatedAt != after.CreatedAt {
		t.Errorf("createdAt drifted: before=%d after=%d", before.CreatedAt, after.CreatedAt)
	}
}
