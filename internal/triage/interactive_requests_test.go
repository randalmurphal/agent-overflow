package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func TestPendingInteractiveRequestsSnapshotOrdersAndClears(t *testing.T) {
	router := NewRouter(nil, func(eventchan.Channel, any) {})

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
	router := NewRouter(nil, func(eventchan.Channel, any) {})
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
	if st := router.threadStateIfPresent("thread-1"); st != nil {
		t.Fatalf("thread state survived CleanupThread with %d pending user-input order entries, want the whole entry gone", len(st.pendingUserInputOrder))
	}
}

func TestUserInputResolutionCompletesCodexRequestUserInputToolCall(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexQueueTestThread(t, st, "thread-1")

	questions := []provider.UserInputQuestion{{
		ID:       "scope",
		Header:   "Scope",
		Question: "Choose a scope",
		Options: []provider.UserInputQuestionOption{{
			Label:       "turn",
			Description: "Apply only to this turn",
		}},
	}}
	startMeta := mustMarshalJSON(t, map[string]any{
		"toolName": "request_user_input",
		"input": map[string]any{
			"questions": questions,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ItemID:    "item-8",
		ItemType:  "request_user_input",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	requestMeta := mustMarshalJSON(t, provider.UserInputRequest{
		RequestID: "3",
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ToolUseID: "item-8",
		ToolName:  "user_input",
		Title:     "User Input Required",
		Questions: questions,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		ItemID:    "item-8",
		Meta:      requestMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle user-input request: %v", err)
	}

	resolveMeta := mustMarshalJSON(t, map[string]any{
		"requestId": "3",
		"decision":  "answered",
		"answers": map[string]provider.UserInputAnswer{
			"scope": provider.SingleUserInputAnswer("turn"),
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputResolved,
		ThreadID:  "thread-1",
		ItemID:    "3",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle user-input resolve: %v", err)
	}

	item, found, err := st.GetThreadItem("thread-1", "item-8")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !found {
		t.Fatal("request_user_input tool call was not persisted")
	}
	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want %q", item.Status, statusCompleted)
	}
	if item.ToolName != "request_user_input" {
		t.Fatalf("toolName = %q, want request_user_input", item.ToolName)
	}
	var meta struct {
		Answers map[string]provider.UserInputAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("unmarshal item meta: %v", err)
	}
	if got := meta.Answers["scope"]; len(got) != 1 || got[0] != "turn" {
		t.Fatalf("answers = %+v, want scope=turn", meta.Answers)
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

// --- HasPendingWork tests ---

func TestHasPendingWorkNilRouter(t *testing.T) {
	var r *Router
	if r.HasPendingWork("thread-1") {
		t.Fatal("nil router returned true")
	}
}

func TestHasPendingWorkEmptyThread(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	if r.HasPendingWork("") {
		t.Fatal("empty threadID returned true")
	}
}

func TestHasPendingWorkNoState(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	if r.HasPendingWork("thread-1") {
		t.Fatal("fresh router returned true")
	}
}

func TestHasPendingWorkPendingApproval(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	meta := mustMarshalJSON(t, provider.ApprovalRequest{
		RequestID: "a-1",
		ThreadID:  "thread-1",
		ToolName:  "Bash",
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  "thread-1",
		ItemID:    "a-1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !r.HasPendingWork("thread-1") {
		t.Fatal("pending approval: want true")
	}

	resolveMeta := mustMarshalJSON(t, map[string]any{
		"requestId": "a-1",
		"decision":  "approved",
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalResolved,
		ThreadID:  "thread-1",
		ItemID:    "a-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle resolve: %v", err)
	}

	if r.HasPendingWork("thread-1") {
		t.Fatal("after resolve: want false")
	}
}

func TestHasPendingWorkPendingUserInput(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	meta := mustMarshalJSON(t, provider.UserInputRequest{
		RequestID: "ui-1",
		ThreadID:  "thread-1",
		ToolName:  "user_input",
		Title:     "Choose",
		Questions: []provider.UserInputQuestion{{
			ID:       "q",
			Header:   "Q",
			Question: "Pick one",
			Options: []provider.UserInputQuestionOption{
				{Label: "A", Description: "First"},
			},
		}},
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  "thread-1",
		ItemID:    "ui-1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !r.HasPendingWork("thread-1") {
		t.Fatal("pending user input: want true")
	}

	resolveMeta := mustMarshalJSON(t, map[string]any{
		"requestId": "ui-1",
		"decision":  "answered",
	})
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserInputResolved,
		ThreadID:  "thread-1",
		ItemID:    "ui-1",
		Meta:      resolveMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle resolve: %v", err)
	}

	if r.HasPendingWork("thread-1") {
		t.Fatal("after resolve: want false")
	}
}

func TestHasPendingWorkQueuedFlushItems(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	r.RegisterQueueItem("thread-1", QueuedFlushItem{
		ID:      "queue:1",
		Message: "follow-up",
	})

	if !r.HasPendingWork("thread-1") {
		t.Fatal("queued flush item: want true")
	}
}

func TestHasPendingWorkPendingSend(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	r.RegisterPendingSendWithExpectation("thread-1", "user:1", 1, PendingSendExpectation{})

	if !r.HasPendingWork("thread-1") {
		t.Fatal("pending send: want true")
	}
}

func TestHasPendingWorkCleanupThreadClears(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	r.setPendingUserInput("thread-1", provider.UserInputRequest{
		RequestID: "ui-x",
		ThreadID:  "thread-1",
		Questions: []provider.UserInputQuestion{{
			ID: "q", Header: "Q", Question: "?",
		}},
	})
	r.RegisterQueueItem("thread-1", QueuedFlushItem{
		ID: "queue:x", Message: "msg",
	})
	r.RegisterPendingSendWithExpectation("thread-1", "user:x", 1, PendingSendExpectation{})

	if !r.HasPendingWork("thread-1") {
		t.Fatal("multiple sources: want true")
	}

	r.CleanupThread("thread-1")

	if r.HasPendingWork("thread-1") {
		t.Fatal("after CleanupThread: want false")
	}
}

func TestHasPendingWorkIsolatesThreads(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})

	r.RegisterPendingSendWithExpectation("thread-1", "user:1", 1, PendingSendExpectation{})

	if r.HasPendingWork("thread-2") {
		t.Fatal("unrelated thread returned true")
	}
}
