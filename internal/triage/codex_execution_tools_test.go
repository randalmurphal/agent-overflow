package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// childToolCall writes one tool call of an agent the way a Codex child's
// commandExecution arrives: started, then completed on the same row.
func childToolCall(t *testing.T, r *Router, threadID, parentID, itemID string) {
	t.Helper()
	meta := json.RawMessage(`{"toolName":"Bash","input":{"command":"` + itemID + `"}}`)
	for _, kind := range []provider.EventKind{provider.EventToolStart, provider.EventToolComplete} {
		if err := r.Handle(provider.ProviderEvent{
			Kind: kind, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution",
			ParentToolUseID: parentID, Content: "ok", Meta: meta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("%s %s: %v", kind, itemID, err)
		}
	}
}

func liveToolUses(t *testing.T, r *Router, threadID, launchID string) int {
	t.Helper()
	progress, _ := r.PeekSubagentProgress(threadID, launchID)
	return progress.ToolUses
}

// progressToolUses is the tool count of every provider:subagent_progress
// frame for launchID, with consecutive repeats folded.
func progressToolUses(emissions []emitted, launchID string) []int {
	var out []int
	for _, e := range filterEmissions(emissions, "provider:subagent_progress") {
		frame, ok := e.data.(SubagentProgressEvent)
		if !ok || frame.ItemID != launchID {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != frame.Progress.ToolUses {
			out = append(out, frame.Progress.ToolUses)
		}
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A resumed Codex child's executions each count their own tool calls: the
// live count starts over when the next execution starts, however many the
// last one made, and each completion carries its execution's number read
// from the stored rows.
func TestCodexExecutionToolCountResetsPerExecutionAndLandsOnEachCompletion(t *testing.T) {
	r, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")

	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")
	tickProgress(t, r, "t1", "spawn", provider.SubagentProgressMeta{TaskID: "child", TotalTokens: 100})
	for i, id := range []string{"a1", "a2", "a3"} {
		childToolCall(t, r, "t1", "spawn", id)
		if got := liveToolUses(t, r, "t1", "spawn"); got != i+1 {
			t.Fatalf("after %s live tools = %d, want %d", id, got, i+1)
		}
	}
	// A re-delivered start rewrites a counted row; it is not a new call.
	childToolCall(t, r, "t1", "spawn", "a1")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 3 {
		t.Fatalf("re-delivered call counted again: live tools = %d", got)
	}
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-a", "First answer."))
	first, ok := persistedProgressFor(t, st, "t1", "complete:spawn:turn:A")
	if !ok || first.ToolUses != 3 || first.TotalTokens != 100 {
		t.Fatalf("first completion progress = %+v (present %v), want 3 tools and 100 tokens", first, ok)
	}

	emissions.reset()
	childTurnStatus(t, r, "t1", "spawn", "child", "B", "running")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 0 {
		t.Fatalf("resumed execution starts at %d tools, want 0", got)
	}
	childToolCall(t, r, "t1", "spawn", "b1")
	childToolCall(t, r, "t1", "spawn", "b2")
	// A token tick reports no tool count; it keeps the one the rows gave.
	tickProgress(t, r, "t1", "spawn", provider.SubagentProgressMeta{TaskID: "child", TotalTokens: 250})
	if got := progressToolUses(emissions.snapshot(), "spawn"); !equalInts(got, []int{0, 1, 2}) {
		t.Fatalf("second execution progress frames carry tools %v, want [0 1 2]", got)
	}
	childTurnStatus(t, r, "t1", "spawn", "child", "B", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-b", "Second answer."))
	second, ok := persistedProgressFor(t, st, "t1", "complete:spawn:turn:B")
	if !ok || second.ToolUses != 2 || second.TotalTokens != 250 {
		t.Fatalf("second completion progress = %+v (present %v), want 2 tools and 250 tokens", second, ok)
	}
	if again, _ := persistedProgressFor(t, st, "t1", "complete:spawn:turn:A"); again != first {
		t.Fatalf("later execution changed the first completion: %+v -> %+v", first, again)
	}

	// The finished execution counts nothing more.
	childToolCall(t, r, "t1", "spawn", "late")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 2 {
		t.Fatalf("a row after the execution ended moved the live count to %d", got)
	}
}

// A nested agent's spawn is one tool call of the execution that made it;
// the nested agent's own calls count toward the nested agent only.
func TestCodexExecutionToolCountExcludesNestedAgentCalls(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")

	childToolCall(t, r, "t1", "spawn", "own-1")
	nestedMeta := buildSpawnAgentMeta(t, "grandchild", "running")
	for _, kind := range []provider.EventKind{provider.EventToolStart, provider.EventToolComplete} {
		if err := r.Handle(provider.ProviderEvent{
			Kind: kind, ThreadID: "t1", ItemID: "nested", ItemType: "collab_agent",
			ParentToolUseID: "spawn", Meta: nestedMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("nested spawn %s: %v", kind, err)
		}
	}
	childToolCall(t, r, "t1", "nested", "nested-1")
	childToolCall(t, r, "t1", "nested", "nested-2")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 2 {
		t.Fatalf("outer live tools = %d, want 2 (own call and the nested spawn)", got)
	}
	if got := liveToolUses(t, r, "t1", "nested"); got != 2 {
		t.Fatalf("nested live tools = %d, want its own 2", got)
	}

	childTurnStatus(t, r, "t1", "spawn", "child", "A", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-a", "Done."))
	progress, ok := persistedProgressFor(t, st, "t1", "complete:spawn:turn:A")
	if !ok || progress.ToolUses != 2 {
		t.Fatalf("outer completion progress = %+v (present %v), want 2 tools", progress, ok)
	}
}

// Rows the execution wrote before its start signal arrived are part of it:
// the start seeds the live count from them, so the live number and the
// completion's agree.
func TestCodexExecutionToolCountSeedsFromRowsAlreadyInTheExecution(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	childToolCall(t, r, "t1", "spawn", "early-1")
	childToolCall(t, r, "t1", "spawn", "early-2")

	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 2 {
		t.Fatalf("live tools at the start signal = %d, want the 2 already stored", got)
	}
	childToolCall(t, r, "t1", "spawn", "a1")
	if got := liveToolUses(t, r, "t1", "spawn"); got != 3 {
		t.Fatalf("live tools = %d, want 3", got)
	}
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-a", "Done."))
	if progress, _ := persistedProgressFor(t, st, "t1", "complete:spawn:turn:A"); progress.ToolUses != 3 {
		t.Fatalf("completion tools = %d, want 3", progress.ToolUses)
	}
}

// The completion's number is the stored rows', not the live count's: a
// live value no row backs never reaches it, and a row the live count did
// not see does.
func TestCodexCompletionToolCountReadsTheStoredRows(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")
	childToolCall(t, r, "t1", "spawn", "a1")
	tickProgress(t, r, "t1", "spawn", provider.SubagentProgressMeta{TaskID: "child", ToolUses: 9, TotalTokens: 50})
	next, _, err := st.MaxItemIndexForTurn("t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertSeed(st, store.Item{
		ID: "unseen", ThreadID: "t1", TurnIndex: 0, ItemIndex: next + 1, Kind: itemKindToolCall, Role: "assistant",
		ToolName: "Bash", Status: statusCompleted, ParentID: "spawn", Summary: "unseen", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "completed")
	deliverMailbox(t, r, "t1", "spawn", rootAnswer(t, "final-a", "Done."))
	progress, _ := persistedProgressFor(t, st, "t1", "complete:spawn:turn:A")
	if progress.ToolUses != 2 || progress.TotalTokens != 50 {
		t.Fatalf("completion progress = %+v, want 2 tools from the rows and 50 tokens", progress)
	}
}

// The completion the session's end writes for a running execution carries
// the execution's tool count from its rows.
func TestCodexSessionEndCompletionCarriesTheExecutionToolCount(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
	childTurnStatus(t, r, "t1", "spawn", "child", "A", "running")
	childToolCall(t, r, "t1", "spawn", "a1")
	childToolCall(t, r, "t1", "spawn", "a2")

	r.CleanupThread("t1")
	completion, found, err := st.GetThreadItem("t1", "complete:spawn:turn:A")
	if err != nil || !found || completion.Status != statusKilled {
		t.Fatalf("session-end completion = %+v found=%v err=%v", completion, found, err)
	}
	if progress, _ := persistedProgressFor(t, st, "t1", completion.ID); progress.ToolUses != 2 {
		t.Fatalf("session-end completion tools = %d, want 2", progress.ToolUses)
	}
}

// A completion without execution bounds (a delivery, or a child with no
// turn lifecycle) covers the rows since the launch's previous completion,
// as its digest does, and counts the tool calls among them.
func TestCodexCompletionWithoutExecutionBoundsCountsToolCallsSinceThePreviousCompletion(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn-1", "child-1")
	childToolCall(t, r, "t1", "spawn-1", "c1")
	childToolCall(t, r, "t1", "spawn-1", "c2")

	deliverMailbox(t, r, "t1", "spawn-1", mailboxDelivery(t, "/root/reviewer", "Done."))
	rows := completionRowsFor(t, st, "t1", "spawn-1")
	if len(rows) != 1 {
		t.Fatalf("completions = %+v", rows)
	}
	var bounds struct {
		Start *int `json:"codex_execution_child_start_index"`
	}
	if err := json.Unmarshal([]byte(rows[0].Meta), &bounds); err != nil || bounds.Start != nil {
		t.Fatalf("completion has execution bounds: %s", rows[0].Meta)
	}
	if progress, _ := persistedProgressFor(t, st, "t1", rows[0].ID); progress.ToolUses != 2 {
		t.Fatalf("completion tools = %d, want 2", progress.ToolUses)
	}
	// The terminal the delivery reported ended the execution.
	childToolCall(t, r, "t1", "spawn-1", "late")
	if got := liveToolUses(t, r, "t1", "spawn-1"); got != 2 {
		t.Fatalf("live tools = %d after the execution ended, want 2", got)
	}
}
