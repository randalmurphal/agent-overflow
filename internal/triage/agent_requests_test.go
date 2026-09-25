package triage

// An agent's open approval and question prompts are the agent's
// (docs/architecture/turn-lifecycle.md, §Agent-owned rows): the parent
// turn's end leaves them open, and the agent's end settles them.

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func agentTool(t *testing.T, router *Router, threadID, id, toolName, scope string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: id, ItemType: toolName, ParentToolUseID: scope,
		Meta: parkMeta(t, map[string]any{"toolName": toolName, "input": map[string]any{}}),
	})
}

func agentApproval(t *testing.T, router *Router, threadID, requestID, toolUseID, scope string) {
	t.Helper()
	parkHandle(t, router, approvalRequestEvent(t, threadID, provider.ApprovalRequest{
		RequestID: requestID, ThreadID: threadID, ToolUseID: toolUseID,
		ParentToolUseID: scope, ToolName: "Bash", Kind: "command",
		Input: json.RawMessage(`{"command":"make"}`),
	}, ""))
}

func agentQuestion(t *testing.T, router *Router, threadID, requestID, toolUseID, scope string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventUserInputRequest, ThreadID: threadID, ItemID: requestID,
		Meta: parkMeta(t, map[string]any{
			"requestId": requestID, "threadId": threadID, "toolUseId": toolUseID,
			"parentToolUseId": scope, "toolName": "AskUserQuestion",
			"questions": []provider.UserInputQuestion{{ID: "q1", Header: "Target", Question: "Which target?"}},
		}),
	})
}

func pendingRequestIDs(router *Router, threadID string) (approvals, questions []string) {
	pending := router.PendingInteractiveRequests(threadID)
	for _, a := range pending.Approvals {
		approvals = append(approvals, a.RequestID)
	}
	for _, q := range pending.UserInputs {
		questions = append(questions, q.RequestID)
	}
	return approvals, questions
}

// resolveEvents lists the request ids of the resolve events emitted on
// channel, with their decisions.
func resolveEvents(emits *emissionLog, channel string) map[string]string {
	out := map[string]string{}
	for _, e := range filterEmissions(emits.snapshot(), channel) {
		switch payload := e.data.(type) {
		case provider.ApprovalEvent:
			if payload.Action == "resolve" {
				out[payload.RequestID] = payload.Decision
			}
		case provider.UserInputEvent:
			if payload.Action == "resolve" {
				out[payload.RequestID] = payload.Decision
			}
		}
	}
	return out
}

func TestParentTurnEndLeavesAnAgentsPromptsOpen(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	agentTool(t, router, "t1", "tu-a-bash", "Bash", "tu-a")
	agentTool(t, router, "t1", "tu-a-ask", "AskUserQuestion", "tu-a")
	agentApproval(t, router, "t1", "req-agent", "tu-a-bash", "tu-a")
	agentQuestion(t, router, "t1", "req-ask", "tu-a-ask", "tu-a")
	agentApproval(t, router, "t1", "req-main", "tu-main", "")
	if !router.ClaimApprovalResponse("t1", "req-agent") {
		t.Fatal("the first answer to the agent's approval was refused")
	}
	// An approval answered before the agent's tool row is written leaves
	// its decision waiting for the row.
	agentApproval(t, router, "t1", "req-early", "tu-a-late", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: "t1", ItemID: "req-early",
		Meta: parkMeta(t, map[string]any{"requestId": "req-early", "decision": "approved"}),
	})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})

	agentTool(t, router, "t1", "tu-a-late", "Bash", "tu-a")
	if got := mustItem(t, st, "t1", "tu-a-late"); got.Decision != "approved" {
		t.Fatalf("the parent's end dropped the decision waiting for the agent's tool: %q", got.Decision)
	}

	approvals, questions := pendingRequestIDs(router, "t1")
	if len(approvals) != 1 || approvals[0] != "req-agent" || len(questions) != 1 || questions[0] != "req-ask" {
		t.Fatalf("after the parent's end pending approvals=%v questions=%v, want only the agent's", approvals, questions)
	}
	if !router.HasPendingWork("t1") {
		t.Fatal("a thread whose agent waits on the user reads as idle")
	}
	if router.ClaimApprovalResponse("t1", "req-agent") {
		t.Fatal("the parent's end forgot the agent's approval was answered")
	}

	// The answer lands on the agent's own row.
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: "t1", ItemID: "req-agent",
		Meta: parkMeta(t, map[string]any{"requestId": "req-agent", "decision": statusDeclined}),
	})
	if got := mustItem(t, st, "t1", "tu-a-bash"); got.Status != statusDeclined || got.Decision != statusDeclined || got.ParentID != "tu-a" {
		t.Fatalf("the agent's declined tool = status %q decision %q parent %q", got.Status, got.Decision, got.ParentID)
	}
	if _, found, err := st.GetThreadItem("t1", "req-agent"); err != nil || found {
		t.Fatalf("the answer wrote a row named for the request: found=%v err=%v", found, err)
	}
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventUserInputResolved, ThreadID: "t1", ItemID: "req-ask",
		Meta: parkMeta(t, map[string]any{"requestId": "req-ask", "decision": "answered",
			"answers": map[string]provider.UserInputAnswer{"q1": provider.SingleUserInputAnswer("all")}}),
	})
	var meta struct {
		Answers map[string]provider.UserInputAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(mustItem(t, st, "t1", "tu-a-ask").Meta), &meta); err != nil {
		t.Fatalf("question row meta: %v", err)
	}
	if got := meta.Answers["q1"]; len(got) != 1 || got[0] != "all" {
		t.Fatalf("the agent's question row answers = %+v, want q1=all", meta.Answers)
	}
}

func TestAgentEndSettlesItsOwnPrompts(t *testing.T) {
	router, st, emits := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkLaunchAgent(t, router, "t1", "tu-b", "task-b", "")
	agentTool(t, router, "t1", "tu-a-bash", "Bash", "tu-a")
	agentTool(t, router, "t1", "tu-a-ask", "AskUserQuestion", "tu-a")
	agentApproval(t, router, "t1", "req-a", "tu-a-bash", "tu-a")
	agentQuestion(t, router, "t1", "req-a-ask", "tu-a-ask", "tu-a")
	agentApproval(t, router, "t1", "req-b", "tu-b-bash", "tu-b")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
	if !router.ClaimApprovalResponse("t1", "req-a") {
		t.Fatal("the first answer to agent a's approval was refused")
	}

	agentKilled(t, router, "t1", "tu-a", "task-a", "")
	if router.ClaimApprovalResponse("t1", "req-a") {
		t.Fatal("a second answer to the ended agent's answered approval was let through")
	}

	approvals, questions := pendingRequestIDs(router, "t1")
	if len(approvals) != 1 || approvals[0] != "req-b" || len(questions) != 0 {
		t.Fatalf("after agent a's end pending approvals=%v questions=%v, want only agent b's approval", approvals, questions)
	}
	if got := resolveEvents(emits, "provider:approval"); got["req-a"] != "lost" || got["req-b"] != "" {
		t.Fatalf("approval resolves = %v, want req-a lost and nothing for req-b", got)
	}
	if got := resolveEvents(emits, "provider:user_input"); got["req-a-ask"] != "lost" {
		t.Fatalf("question resolves = %v, want req-a-ask lost", got)
	}
	if got := mustItem(t, st, "t1", "tu-a-bash"); got.Status != statusErrored || !isStopped(got.Summary) {
		t.Fatalf("the ended agent's asking tool = %q %q, want errored and stopped", got.Status, got.Summary)
	}

	// An answer that raced the end finds no prompt and writes no row.
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: "t1", ItemID: "req-a",
		Meta: parkMeta(t, map[string]any{"requestId": "req-a", "decision": statusDeclined}),
	})
	if _, found, err := st.GetThreadItem("t1", "req-a"); err != nil || found {
		t.Fatalf("a late answer wrote a row named for the request: found=%v err=%v", found, err)
	}
	if got := mustItem(t, st, "t1", "tu-a-bash"); got.Status != statusErrored {
		t.Fatalf("a late answer reopened the ended agent's tool: %q", got.Status)
	}

	agentKilled(t, router, "t1", "tu-b", "task-b", "")
	if router.HasPendingWork("t1") {
		t.Fatal("prompts outlived the agents that asked them")
	}
}
