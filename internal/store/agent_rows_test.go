package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// agentRowsFixture is one thread whose turn 0 launched background agent
// "A" (top level) and whose turn 1 is open. Everything open under A was
// written in turn 0, the launch turn, whichever turn it arrived in:
//
//	A (bg Agent)                 running (the spawn record)
//	├─ A-read   (Read)           running
//	├─ A-text   (assistant_text) streaming
//	├─ A-done   (Read)           completed
//	├─ A-fg     (fg Agent)       running
//	│  ├─ A-fg-read              running
//	│  └─ A-fg-text              streaming
//	├─ A-shell  (bg Bash)        running (owns its own rows)
//	└─ B        (bg Agent)       running
//	   └─ B-read                 running
//
// Turn 1 holds a top-level Read "main-read" and text "main-text", both open.
func agentRowsFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	mustCreateThread(t, s, "T")
	row := func(id, parent string, turn int, kind, tool, status string, background bool) {
		t.Helper()
		if _, err := upsertCarded(s, Item{
			ID: id, ThreadID: "T", TurnIndex: turn, Kind: kind, Role: "assistant", Status: status,
			Summary: id, ParentID: parent, ToolName: tool, IsBackground: background, CreatedAt: 1, UpdatedAt: 1,
		}, nil); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	row("A", "", 0, "tool_call", "Agent", "running", true)
	row("A-read", "A", 0, "tool_call", "Read", "running", false)
	row("A-text", "A", 0, "assistant_text", "", "streaming", false)
	row("A-done", "A", 0, "tool_call", "Read", "completed", false)
	row("A-fg", "A", 0, "tool_call", "Agent", "running", false)
	row("A-fg-read", "A-fg", 0, "tool_call", "Read", "running", false)
	row("A-fg-text", "A-fg", 0, "assistant_text", "", "streaming", false)
	row("A-shell", "A", 0, "tool_call", "Bash", "running", true)
	row("B", "A", 0, "tool_call", "Agent", "running", true)
	row("B-read", "B", 0, "tool_call", "Read", "running", false)
	row("main-read", "", 1, "tool_call", "Read", "running", false)
	row("main-text", "", 1, "assistant_text", "", "streaming", false)
	return s
}

func rowStates(t *testing.T, s *Store, threadID string) map[string]string {
	t.Helper()
	items, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		out[item.ID] = item.Status + ":" + item.Summary
	}
	return out
}

func stopped(summary string) string { return summary + " (stopped)" }

// TestAgentOwnedParentsFollowsTheChainToTheNearestOwner: the rows under a
// background launch are its agent's, through a foreground agent inside
// it; a Codex spawn card and a foreground launch own nothing.
func TestAgentOwnedParentsFollowsTheChainToTheNearestOwner(t *testing.T) {
	s := agentRowsFixture(t)
	if _, err := upsertCarded(s, Item{ID: "spawn", ThreadID: "T", Kind: "tool_call", Role: "assistant", ToolName: "collab_agent",
		Status: "completed", IsBackground: true, Summary: "spawn", CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	owned, err := s.AgentOwnedScopes("T", []string{"A", "A-fg", "B", "A-read", "main-read", "spawn", "missing", ""})
	if err != nil {
		t.Fatalf("AgentOwnedScopes: %v", err)
	}
	want := map[string]bool{"A": true, "A-fg": true, "B": true, "A-read": true, "main-read": false, "spawn": false, "missing": false}
	for id, is := range want {
		if owned[id] != is {
			t.Errorf("owned[%s] = %v, want %v", id, owned[id], is)
		}
	}
	if _, asked := owned[""]; asked {
		t.Error("the empty scope is the main thread's and is not asked about")
	}
}

// TestUpsertAgentEndSettlesTheAgentsOpenRowsInItsTransaction: the sibling
// write settles every open row A owns, through its foreground agent, and
// none of a background launch inside it, which ends on its own.
func TestUpsertAgentEndSettlesTheAgentsOpenRowsInItsTransaction(t *testing.T) {
	s := agentRowsFixture(t)
	sibling := Item{ID: "complete:A", ThreadID: "T", TurnIndex: 0, Kind: "tool_completion", Role: "assistant",
		Status: "killed", Summary: "A stopped", CompletionOf: "A", IsBackground: true, CreatedAt: 2, UpdatedAt: 2}
	written, settled, err := s.UpsertAgentEnd(sibling, nil, "A", AgentEndRule{Summarise: stopped}, 5)
	if err != nil {
		t.Fatalf("UpsertAgentEnd: %v", err)
	}
	if written.ID != "complete:A" || written.Status != "killed" {
		t.Fatalf("sibling = %s:%s", written.ID, written.Status)
	}
	var ids []string
	for _, row := range settled {
		ids = append(ids, row.Item.ID+":"+row.WasStatus+">"+row.Item.Status)
		if row.Item.UpdatedAt != 5 || row.Item.Summary != stopped(row.WasSummary) {
			t.Errorf("%s settled as %q at %d", row.Item.ID, row.Item.Summary, row.Item.UpdatedAt)
		}
	}
	slices.Sort(ids)
	want := []string{
		"A-fg-read:running>errored", "A-fg-text:streaming>errored", "A-fg:running>errored",
		"A-read:running>errored", "A-text:streaming>errored",
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("settled %v, want %v", ids, want)
	}
	states := rowStates(t, s, "T")
	for id, state := range map[string]string{
		"A-done": "completed:A-done", "A-shell": "running:A-shell", "B": "running:B", "B-read": "running:B-read",
		"main-read": "running:main-read", "main-text": "streaming:main-text",
	} {
		if states[id] != state {
			t.Errorf("%s = %s, want %s (not A's to settle)", id, states[id], state)
		}
	}
	assertSubagentStampParity(t, s, "T", "after agent end", true)

	// Idempotent: a second write of the sibling finds nothing open.
	if _, again, err := s.UpsertAgentEnd(sibling, nil, "A", AgentEndRule{Summarise: stopped}, 6); err != nil || len(again) != 0 {
		t.Fatalf("second UpsertAgentEnd settled %d rows, err %v", len(again), err)
	}
}

// TestUpsertAgentEndCompletesStreamingRowsOfAFinishedAgent: an agent that
// reported leaves its open text completed as it stands and its open tool
// calls errored.
func TestUpsertAgentEndCompletesStreamingRowsOfAFinishedAgent(t *testing.T) {
	s := agentRowsFixture(t)
	sibling := Item{ID: "complete:A", ThreadID: "T", Kind: "tool_completion", Role: "assistant",
		Status: "completed", Summary: "A done", CompletionOf: "A", IsBackground: true, CreatedAt: 2, UpdatedAt: 2}
	unresolved := func(s string) string { return s + " (unresolved)" }
	if _, _, err := s.UpsertAgentEnd(sibling, nil, "A", AgentEndRule{StreamingCompletes: true, Summarise: unresolved}, 5); err != nil {
		t.Fatalf("UpsertAgentEnd: %v", err)
	}
	states := rowStates(t, s, "T")
	for id, state := range map[string]string{
		"A-text": "completed:A-text", "A-fg-text": "completed:A-fg-text",
		"A-read": "errored:A-read (unresolved)", "A-fg": "errored:A-fg (unresolved)",
	} {
		if states[id] != state {
			t.Errorf("%s = %s, want %s", id, states[id], state)
		}
	}
}

// TestForceCloseLeavesAgentOwnedToolCalls: a turn's force-close settles
// the main thread's orphans and never an agent's rows, even in the turn
// that launched it.
func TestForceCloseLeavesAgentOwnedToolCalls(t *testing.T) {
	s := agentRowsFixture(t)
	for _, turn := range []int{0, 1} {
		if _, err := s.ForceCloseRunningToolCallsInTurn("T", turn, func(s string) string { return s + " (unresolved)" }, 5); err != nil {
			t.Fatalf("force-close turn %d: %v", turn, err)
		}
	}
	states := rowStates(t, s, "T")
	if states["main-read"] != "errored:main-read (unresolved)" {
		t.Errorf("main-read = %s, want it force-closed", states["main-read"])
	}
	for _, id := range []string{"A-read", "A-fg", "A-fg-read", "B-read"} {
		if !strings.HasPrefix(states[id], "running:") {
			t.Errorf("%s = %s, want it left to its agent", id, states[id])
		}
	}
}

// TestCrashRecoverySettlesOnlyTheTurnsOwnRows: the crashed-turn sweep
// settles the rows no agent owns; the agents' own rows wait for their
// session_died completion, and a second boot finds no running agent.
func TestCrashRecoverySettlesOnlyTheTurnsOwnRows(t *testing.T) {
	s := agentRowsFixture(t)
	for _, turn := range []Turn{
		{TurnID: "T:0", ThreadID: "T", TurnIndex: 0, StartedAt: 1},
		{TurnID: "T:1", ThreadID: "T", TurnIndex: 1, StartedAt: 1},
	} {
		if err := s.InsertTurn(turn); err != nil {
			t.Fatalf("insert turn: %v", err)
		}
	}
	interrupted := func(s string) string { return s + " (interrupted)" }
	if _, err := s.RecoverCrashedTurns(interrupted, 5); err != nil {
		t.Fatalf("RecoverCrashedTurns: %v", err)
	}
	states := rowStates(t, s, "T")
	if states["main-read"] != "errored:main-read (interrupted)" || states["main-text"] != "errored:main-text (interrupted)" {
		t.Errorf("main rows = %s, %s; want both interrupted", states["main-read"], states["main-text"])
	}
	for _, id := range []string{"A-read", "A-text", "A-fg", "B-read"} {
		if states[id] == "" || strings.HasPrefix(states[id], "errored:") {
			t.Errorf("%s = %s, want it left to its agent's completion", id, states[id])
		}
	}
	for _, agent := range []string{"B", "A"} {
		sibling := Item{ID: "complete:" + agent, ThreadID: "T", Kind: "tool_completion", Role: "assistant",
			Status: "killed", Summary: agent + " died", CompletionOf: agent, IsBackground: true, CreatedAt: 6, UpdatedAt: 6}
		if _, _, err := s.UpsertAgentEnd(sibling, nil, agent, AgentEndRule{Summarise: stopped}, 6); err != nil {
			t.Fatalf("end %s: %v", agent, err)
		}
	}
	shell := Item{ID: "complete:A-shell", ThreadID: "T", Kind: "tool_completion", Role: "assistant",
		Status: "killed", Summary: "shell died", CompletionOf: "A-shell", ParentID: "A", IsBackground: true, CreatedAt: 6, UpdatedAt: 6}
	if _, err := upsertCarded(s, shell, nil); err != nil {
		t.Fatalf("end shell: %v", err)
	}
	for id, state := range rowStates(t, s, "T") {
		if strings.HasPrefix(state, "running:") || strings.HasPrefix(state, "streaming:") {
			if id != "A" && id != "B" && id != "A-shell" {
				t.Errorf("%s is still %s after every owner ended", id, state)
			}
		}
	}
	marked, err := s.markLiveSubagentChainsDirty()
	if err != nil {
		t.Fatalf("second boot pass: %v", err)
	}
	if len(marked) != 0 {
		t.Fatalf("the second boot pass found running agents in %v", marked)
	}
	if n, err := s.RecoverSubagentCards(context.Background()); err != nil || n != 0 {
		t.Fatalf("second RecoverSubagentCards stamped %d, err %v", n, err)
	}
}

// TestAgentSubtreeUnsettledSQLWalksTheParentIndex: the subtree walk reads
// idx_items_parent, so an agent's end costs its own subtree, not the
// thread.
func TestAgentSubtreeUnsettledSQLWalksTheParentIndex(t *testing.T) {
	s := agentRowsFixture(t)
	var details []string
	for _, row := range explainPlan(t, s, agentSubtreeUnsettledSQL, "T", "A") {
		details = append(details, row.detail)
	}
	plan := strings.Join(details, "\n")
	if strings.Count(plan, "idx_items_parent") < 2 {
		t.Fatalf("both arms of the walk must read idx_items_parent:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN items") || strings.Contains(plan, "SCAN c") {
		t.Fatalf("the walk scans items:\n%s", plan)
	}
}
