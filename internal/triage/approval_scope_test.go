package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Approval / user-input attribution (docs/specs/agent-visibility.md Q10).
// Everything a subagent causes carries its scope, so the prompt's row
// nests under the agent's card instead of landing between the main
// timeline's agent cards.

func approvalRequestEvent(t *testing.T, threadID string, request provider.ApprovalRequest, eventParent string) provider.ProviderEvent {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return provider.ProviderEvent{
		Kind:            provider.EventApprovalRequest,
		ThreadID:        threadID,
		ItemID:          request.RequestID,
		ParentToolUseID: eventParent,
		Meta:            raw,
		Timestamp:       time.UnixMilli(1_700_000_000_000),
	}
}

func lastApprovalRequest(t *testing.T, emits *emissionLog) *provider.ApprovalRequest {
	t.Helper()
	events := filterEmissions(emits.snapshot(), "provider:approval")
	for i := len(events) - 1; i >= 0; i-- {
		payload, ok := events[i].data.(provider.ApprovalEvent)
		if ok && payload.Action == "request" {
			return payload.Request
		}
	}
	t.Fatal("no provider:approval request emission")
	return nil
}

// seedScopedToolCall persists a running tool_call, optionally attributed
// to a launch, so the approval scope fallback has a row to read.
func seedScopedToolCall(t *testing.T, r *Router, threadID, id, parentID, toolName string) {
	t.Helper()
	if err := r.persistItem(store.Item{
		ID: id, ThreadID: threadID, Kind: itemKindToolCall, Role: "assistant",
		Status: statusRunning, ToolName: toolName, Summary: toolName,
		ParentID: parentID, CreatedAt: 1, UpdatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("seed tool call %s: %v", id, err)
	}
}

// TestApprovalScopeResolutionOrder pins the triage half of Q10 and its
// precedence. A scope the parser resolved from `agent_id` is stronger
// evidence than the row lookup and is not overwritten by it; an
// envelope-carried scope is used when the parser had none; and since
// `can_use_tool` carries no parent of its own, when neither resolved one
// the requested tool's persisted parent_id IS the attribution.
func TestApprovalScopeResolutionOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rowParent   string
		parserScope string
		eventScope  string
		input       json.RawMessage
		want        string
	}{
		{"falls back to the requested tool's own row", "toolu_agent", "", "", json.RawMessage(`{"command":"ls"}`), "toolu_agent"},
		{"parser resolution wins", "toolu_stale", "toolu_parser", "toolu_event", nil, "toolu_parser"},
		{"event envelope beats the row lookup", "toolu_stale", "", "toolu_event", nil, "toolu_event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, emits := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedScopedToolCall(t, r, "t1", tc.rowParent, "", "Agent")
			seedScopedToolCall(t, r, "t1", "toolu_bash", tc.rowParent, "Bash")

			if err := r.Handle(approvalRequestEvent(t, "t1", provider.ApprovalRequest{
				RequestID: "req-1", ThreadID: "t1", ToolUseID: "toolu_bash",
				ParentToolUseID: tc.parserScope, ToolName: "Bash", Kind: "command", Input: tc.input,
			}, tc.eventScope)); err != nil {
				t.Fatal(err)
			}

			if got := lastApprovalRequest(t, emits).ParentToolUseID; got != tc.want {
				t.Fatalf("emitted scope = %q, want %s", got, tc.want)
			}
			pending, ok, _ := r.takePendingApproval("t1", "req-1", 0)
			if !ok {
				t.Fatal("approval was not registered")
			}
			if pending.Request.ParentToolUseID != tc.want {
				t.Fatalf("pending scope = %q, want %s", pending.Request.ParentToolUseID, tc.want)
			}
		})
	}
}

// TestApprovalScopeStaysEmptyForTopLevelAndUnknownTools pins that the
// main agent's own approval is not nested under anything, and that a
// tool with no persisted row (or a non-tool row) leaves the request
// top-level rather than inheriting a guess.
func TestApprovalScopeStaysEmptyForTopLevelAndUnknownTools(t *testing.T) {
	r, st, emits := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedScopedToolCall(t, r, "t1", "toolu_top", "", "Bash")
	if err := r.persistItem(store.Item{
		ID: "text-1", ThreadID: "t1", Kind: itemKindAssistantText, Role: "assistant",
		Status: statusCompleted, Summary: "hello", CreatedAt: 1, UpdatedAt: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		requestID string
		toolUseID string
	}{
		{"top-level tool", "req-top", "toolu_top"},
		{"tool with no row", "req-missing", "toolu_missing"},
		{"id naming a non-tool row", "req-text", "text-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Handle(approvalRequestEvent(t, "t1", provider.ApprovalRequest{
				RequestID: tc.requestID, ThreadID: "t1", ToolUseID: tc.toolUseID,
				ToolName: "Bash", Kind: "command",
			}, "")); err != nil {
				t.Fatal(err)
			}
			if got := lastApprovalRequest(t, emits).ParentToolUseID; got != "" {
				t.Fatalf("scope = %q, want empty", got)
			}
		})
	}
}

// TestDeclinedApprovalSynthesizedRowInheritsTheScope pins the persisted
// half: a tool declined before it ever ran has no row of its own, so the
// row applyApprovalDecision synthesizes must carry the scope resolved
// when the prompt was raised — otherwise the decline renders as the main
// agent's.
func TestDeclinedApprovalSynthesizedRowInheritsTheScope(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedScopedToolCall(t, r, "t1", "toolu_agent", "", "Agent")

	if err := r.Handle(approvalRequestEvent(t, "t1", provider.ApprovalRequest{
		RequestID: "req-1", ThreadID: "t1", ToolUseID: "toolu_bash",
		ParentToolUseID: "toolu_agent", ToolName: "Bash", Kind: "command",
		Input: json.RawMessage(`{"command":"rm -rf /"}`),
	}, "")); err != nil {
		t.Fatal(err)
	}
	resolved, err := json.Marshal(map[string]any{"requestId": "req-1", "decision": statusDeclined})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: "t1", ItemID: "req-1",
		Meta: resolved, Timestamp: time.UnixMilli(1_700_000_000_001),
	}); err != nil {
		t.Fatal(err)
	}

	item, found, err := st.GetThreadItem("t1", "toolu_bash")
	if err != nil || !found {
		t.Fatalf("synthesized row lookup: found=%v err=%v", found, err)
	}
	if item.ParentID != "toolu_agent" {
		t.Fatalf("synthesized row parent = %q, want toolu_agent", item.ParentID)
	}
	if item.Status != statusDeclined {
		t.Fatalf("synthesized row status = %q, want declined", item.Status)
	}
}

// TestUserInputRequestInheritsTheAskingAgentsScope pins the same rule for
// structured questions: an AskUserQuestion a subagent raised belongs on
// its card.
func TestUserInputRequestInheritsTheAskingAgentsScope(t *testing.T) {
	r, st, emits := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedScopedToolCall(t, r, "t1", "toolu_agent", "", "Agent")
	seedScopedToolCall(t, r, "t1", "toolu_ask", "toolu_agent", "AskUserQuestion")

	raw, err := json.Marshal(provider.UserInputRequest{
		RequestID: "req-1", ThreadID: "t1", ToolUseID: "toolu_ask",
		ToolName:  "AskUserQuestion",
		Questions: []provider.UserInputQuestion{{ID: "q1", Header: "Scope"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind: provider.EventUserInputRequest, ThreadID: "t1", ItemID: "req-1",
		Meta: raw, Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	events := filterEmissions(emits.snapshot(), "provider:user_input")
	if len(events) != 1 {
		t.Fatalf("user_input emissions = %d, want 1", len(events))
	}
	payload, ok := events[0].data.(provider.UserInputEvent)
	if !ok || payload.Request == nil {
		t.Fatalf("unexpected payload %T", events[0].data)
	}
	if payload.Request.ParentToolUseID != "toolu_agent" {
		t.Fatalf("user input scope = %q, want toolu_agent", payload.Request.ParentToolUseID)
	}
}
