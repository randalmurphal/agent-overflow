package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestPendingInteractiveRequestsSnapshotOrdersAndClears(t *testing.T) {
	router := NewRouter(nil, func(string, any) {})

	approvalMeta := mustMarshalJSON(t, provider.ApprovalRequest{
		RequestID:   "approval-1",
		ThreadID:    "thread-1",
		TurnID:      "turn-1",
		ToolName:    "Bash",
		Description: "Run command",
		Title:       "Approve command",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ItemID:    "approval-1",
		Meta:      approvalMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle approval request: %v", err)
	}

	userInputMeta := mustMarshalJSON(t, provider.UserInputRequest{
		RequestID: "input-1",
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ToolName:  "user_input",
		Title:     "Choose",
		Questions: []provider.UserInputQuestion{{
			ID:       "scope",
			Header:   "Scope",
			Question: "Choose a scope",
			Options: []provider.UserInputQuestionOption{{
				Label:       "turn",
				Description: "Apply only to this turn",
			}},
		}},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ItemID:    "input-1",
		Meta:      userInputMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle user-input request: %v", err)
	}

	snapshot := router.PendingInteractiveRequests("thread-1")
	if got := len(snapshot.Approvals); got != 1 {
		t.Fatalf("approvals len = %d, want 1", got)
	}
	if snapshot.Approvals[0].RequestID != "approval-1" {
		t.Fatalf("approval request id = %q, want approval-1", snapshot.Approvals[0].RequestID)
	}
	if got := len(snapshot.UserInputs); got != 1 {
		t.Fatalf("user inputs len = %d, want 1", got)
	}
	if snapshot.UserInputs[0].Questions[0].Options[0].Label != "turn" {
		t.Fatalf("option label = %q, want turn", snapshot.UserInputs[0].Questions[0].Options[0].Label)
	}

	resolveInputMeta := mustMarshalJSON(t, map[string]any{
		"requestId": "input-1",
		"decision":  "answered",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputResolved,
		ThreadID:  "thread-1",
		ItemID:    "input-1",
		Meta:      resolveInputMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle user-input resolution: %v", err)
	}

	snapshot = router.PendingInteractiveRequests("thread-1")
	if got := len(snapshot.UserInputs); got != 0 {
		t.Fatalf("user inputs after resolve = %d, want 0", got)
	}
	if got := len(snapshot.Approvals); got != 1 {
		t.Fatalf("approvals after user-input resolve = %d, want 1", got)
	}

	resolveApprovalMeta := mustMarshalJSON(t, map[string]any{
		"requestId": "approval-1",
		"decision":  "declined",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "thread-1",
		ItemID:    "approval-1",
		Meta:      resolveApprovalMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle approval resolution: %v", err)
	}

	snapshot = router.PendingInteractiveRequests("thread-1")
	if len(snapshot.Approvals) != 0 || len(snapshot.UserInputs) != 0 {
		t.Fatalf("snapshot after resolutions = %+v, want empty", snapshot)
	}
}

func TestPendingInteractiveRequestsCleanupThreadClearsOrder(t *testing.T) {
	router := NewRouter(nil, func(string, any) {})
	router.setPendingUserInput("thread-1", provider.UserInputRequest{
		RequestID: "input-1",
		ThreadID:  "thread-1",
		Questions: []provider.UserInputQuestion{{
			ID:       "scope",
			Header:   "Scope",
			Question: "Choose a scope",
		}},
	})

	router.CleanupThread("thread-1")

	if got := router.PendingInteractiveRequests("thread-1"); len(got.UserInputs) != 0 {
		t.Fatalf("snapshot after cleanup = %+v, want no user inputs", got)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if got := len(router.pendingUserInputOrder); got != 0 {
		t.Fatalf("pendingUserInputOrder len after cleanup = %d, want 0", got)
	}
}

func mustMarshalJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
