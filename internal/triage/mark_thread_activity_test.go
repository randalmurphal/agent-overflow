package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestStreamingDeltaDoesNotBumpThreadActivity is the regression guard for
// the bug this fix addresses: streaming text/thinking events used to
// reshuffle the sidebar on every chunk because the per-row writes
// implicitly touched threads.updated_at. The new contract is that
// item-row mutations leave the activity timestamp alone — only
// MarkThreadActivity moves it.
func TestStreamingDeltaDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	// Multiple streaming text deltas — the path that used to fire
	// `UPDATE threads SET updated_at = ?` on every flush.
	for i := 0; i < 5; i++ {
		evt := provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   "chunk",
			Timestamp: time.Now(),
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle delta %d: %v", i, err)
		}
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across streaming deltas: before=%d after=%d", before, after)
	}
}

// TestUserTextPersistBumpsThreadActivity covers the first sidebar-bump
// boundary: a user-typed message persisted through the triage
// chokepoint advances activity to the row's UpdatedAt.
func TestUserTextPersistBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "user:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "hi",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist user_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != now {
		t.Fatalf("threads.updated_at after user_text persist = %d, want %d", after, now)
	}
}

// TestNonUserTextPersistDoesNotBumpThreadActivity confirms persistence
// of any other item kind (assistant_text, thinking, tool_call, etc.)
// leaves activity alone.
func TestNonUserTextPersistDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "asst:0:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "ok",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist assistant_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across assistant_text persist: before=%d after=%d", before, after)
	}
}

func TestWireOnlyUserTextDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "user:wire:child_prompt_1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "subagent prompt",
		Meta:      `{"provider_item_id":"child_prompt_1","wire_only":true}`,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist wire-only user_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across wire-only user_text: before=%d after=%d", before, after)
	}
}

func TestParentedUserTextDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "spawn-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "Spawn subagent",
		CreatedAt: now - 1,
		UpdatedAt: now - 1,
	}, nil); err != nil {
		t.Fatalf("persist parent tool_call: %v", err)
	}
	if err := router.PersistItem(store.Item{
		ID:        "user:wire:child_prompt_2",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "subagent prompt",
		ParentID:  "spawn-1",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist parented user_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across parented user_text: before=%d after=%d", before, after)
	}
}

func TestInvalidParentedUserTextDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	now := before + 5_000
	if err := router.PersistItem(store.Item{
		ID:        "user:wire:child_prompt_3",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "subagent prompt with missing parent",
		ParentID:  "missing-spawn",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist invalid-parent user_text: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across invalid-parent user_text: before=%d after=%d", before, after)
	}
}

// TestTurnCompleteBumpsThreadActivity covers the second sidebar-bump
// boundary: a settled turn (clean or errored) advances activity. The
// router's settleTurnRow path is the one production caller of
// UpdateTurnCompleted.
func TestTurnCompleteBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		StartedAt: before,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	startEvt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Timestamp: time.Unix(0, before*int64(time.Millisecond)),
	}
	if err := router.Handle(startEvt); err != nil {
		t.Fatalf("handle turn start: %v", err)
	}

	completeAt := before + 10_000
	completeEvt := provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnID:       "turn-1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Unix(0, completeAt*int64(time.Millisecond)),
	}
	if err := router.Handle(completeEvt); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != completeAt {
		t.Fatalf("threads.updated_at after turn complete = %d, want %d", after, completeAt)
	}
}

func TestNestedTurnCompleteDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		StartedAt: before,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	completeAt := before + 10_000
	completeEvt := provider.ProviderEvent{
		Kind:            provider.EventTurnComplete,
		ThreadID:        "t1",
		TurnID:          "turn-1",
		ParentToolUseID: "spawn-1",
		TurnComplete:    normalTurnCompleteMeta(),
		Timestamp:       time.Unix(0, completeAt*int64(time.Millisecond)),
	}
	if err := router.Handle(completeEvt); err != nil {
		t.Fatalf("handle nested turn complete: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("threads.updated_at moved across nested turn complete: before=%d after=%d", before, after)
	}
}

func TestWhitespaceParentTurnCompleteBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		StartedAt: before,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	startEvt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		Timestamp: time.Unix(0, before*int64(time.Millisecond)),
	}
	if err := router.Handle(startEvt); err != nil {
		t.Fatalf("handle turn start: %v", err)
	}

	completeAt := before + 10_000
	completeEvt := provider.ProviderEvent{
		Kind:            provider.EventTurnComplete,
		ThreadID:        "t1",
		TurnID:          "turn-1",
		ParentToolUseID: "   ",
		TurnComplete:    normalTurnCompleteMeta(),
		Timestamp:       time.Unix(0, completeAt*int64(time.Millisecond)),
	}
	if err := router.Handle(completeEvt); err != nil {
		t.Fatalf("handle whitespace-parent turn complete: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != completeAt {
		t.Fatalf("threads.updated_at after whitespace-parent turn complete = %d, want %d", after, completeAt)
	}
}

// TestApprovalRequestBumpsThreadActivity covers the third sidebar-bump
// boundary: an approval request indicates the agent is paused waiting
// on the user — the thread should surface to the top of the sidebar.
func TestApprovalRequestBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	requestMeta, err := json.Marshal(provider.ApprovalRequest{
		RequestID:   "approval-1",
		ThreadID:    "t1",
		TurnID:      "turn-1",
		ToolName:    "Bash",
		Description: "Run command",
	})
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}

	requestAt := before + 2_000
	evt := provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		ItemID:    "approval-1",
		Meta:      requestMeta,
		Timestamp: time.Unix(0, requestAt*int64(time.Millisecond)),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle approval request: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after <= before {
		t.Fatalf("threads.updated_at did not advance on approval request: before=%d after=%d", before, after)
	}
}

// TestApprovalResolveDoesNotBumpThreadActivity — answering an approval
// is the user-text path, not a separate bump. This guards against
// double-bumping when the user replies.
func TestApprovalResolveDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	// Manually drive activity forward so we can detect a regression
	// that bumps it again on resolve.
	if err := st.MarkThreadActivity("t1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	before := readThreadUpdatedAt(t, st, "t1")

	resolveMeta, err := json.Marshal(map[string]any{
		"requestId": "approval-1",
		"decision":  "approved",
	})
	if err != nil {
		t.Fatalf("marshal resolve: %v", err)
	}
	evt := provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "t1",
		ItemID:    "approval-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle approval resolved: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("approval resolve unexpectedly bumped activity: before=%d after=%d", before, after)
	}
}

// TestUserInputResolveDoesNotBumpThreadActivity is the negative twin
// of TestApprovalResolveDoesNotBumpThreadActivity for the structured
// user-input flavor. Submitting an answer is the user-text path, not
// a separate bump.
func TestUserInputResolveDoesNotBumpThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.MarkThreadActivity("t1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	before := readThreadUpdatedAt(t, st, "t1")

	resolveMeta, err := json.Marshal(map[string]any{
		"requestId": "input-1",
		"decision":  "answered",
		"answers":   map[string]string{"scope": "turn"},
	})
	if err != nil {
		t.Fatalf("marshal resolve: %v", err)
	}
	evt := provider.ProviderEvent{
		Kind:      provider.EventUserInputResolved,
		ThreadID:  "t1",
		ItemID:    "input-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle user-input resolved: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after != before {
		t.Fatalf("user-input resolve unexpectedly bumped activity: before=%d after=%d", before, after)
	}
}

// TestUserInputRequestBumpsThreadActivity mirrors the approval-request
// coverage for the structured user-input request flavor.
func TestUserInputRequestBumpsThreadActivity(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	before := readThreadUpdatedAt(t, st, "t1")

	requestMeta, err := json.Marshal(provider.UserInputRequest{
		RequestID: "input-1",
		ThreadID:  "t1",
		TurnID:    "turn-1",
		ToolName:  "user_input",
		Title:     "Pick one",
		Questions: []provider.UserInputQuestion{{
			ID:       "scope",
			Question: "Choose",
			Options: []provider.UserInputQuestionOption{{
				Label:       "turn",
				Description: "this turn",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal user-input request: %v", err)
	}

	requestAt := before + 2_000
	evt := provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  "t1",
		TurnID:    "turn-1",
		ItemID:    "input-1",
		Meta:      requestMeta,
		Timestamp: time.Unix(0, requestAt*int64(time.Millisecond)),
	}
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle user-input request: %v", err)
	}

	after := readThreadUpdatedAt(t, st, "t1")
	if after <= before {
		t.Fatalf("threads.updated_at did not advance on user-input request: before=%d after=%d", before, after)
	}
}

func readThreadUpdatedAt(t *testing.T, st *store.Store, threadID string) int64 {
	t.Helper()
	thr, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	return thr.UpdatedAt
}
