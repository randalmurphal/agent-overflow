package store

import (
	"context"
	"errors"
	"fmt"
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

// TestAgentOwnersFollowsTheChainToTheNearestOwner: the rows under a
// background launch are its agent's, through a foreground agent inside
// it, and a background launch inside it owns its own; a Codex spawn card
// and a foreground launch own nothing.
func TestAgentOwnersFollowsTheChainToTheNearestOwner(t *testing.T) {
	s := agentRowsFixture(t)
	if _, err := upsertCarded(s, Item{ID: "spawn", ThreadID: "T", Kind: "tool_call", Role: "assistant", ToolName: "collab_agent",
		Status: "completed", IsBackground: true, Summary: "spawn", CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// B-read first, so B's answer is learned on the way to A-fg-read's.
	owners, err := s.AgentOwners("T", []string{"B-read", "A-fg-read", "A", "A-fg", "B", "A-read", "main-read", "spawn", "missing", ""})
	if err != nil {
		t.Fatalf("AgentOwners: %v", err)
	}
	want := map[string]string{"B-read": "B", "A-fg-read": "A", "A": "A", "A-fg": "A", "B": "B", "A-read": "A",
		"main-read": "", "spawn": "", "missing": ""}
	for id, owner := range want {
		if got, asked := owners[id]; !asked || got != owner {
			t.Errorf("owners[%s] = %q (asked %v), want %q", id, got, asked, owner)
		}
	}
	if _, asked := owners[""]; asked {
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

// TestAgentsEndLeavesItsBackgroundLaunchesLiveForTheStopCount: an agent's
// end settles none of the background launches inside it, so they stay
// live under an ended agent. The count a stop confirms (a revert, a
// session restart) holds them; the flush queue's gate stays top-level.
func TestAgentsEndLeavesItsBackgroundLaunchesLiveForTheStopCount(t *testing.T) {
	s := agentRowsFixture(t)
	count := func() int {
		t.Helper()
		n, err := s.CountLiveRunningBackgroundToolCalls("T")
		if err != nil {
			t.Fatalf("CountLiveRunningBackgroundToolCalls: %v", err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("live background launches before A's end = %d, want A, A-shell and B", n)
	}
	sibling := Item{ID: "complete:A", ThreadID: "T", Kind: "tool_completion", Role: "assistant",
		Status: "completed", Summary: "A done", CompletionOf: "A", IsBackground: true, CreatedAt: 2, UpdatedAt: 2}
	if _, _, err := s.UpsertAgentEnd(sibling, nil, "A", AgentEndRule{StreamingCompletes: true, Summarise: stopped}, 5); err != nil {
		t.Fatalf("UpsertAgentEnd: %v", err)
	}
	assertSettled(t, s, "T", "A")
	assertLive(t, s, "T", "A-shell")
	assertLive(t, s, "T", "B")
	if n := count(); n != 2 {
		t.Fatalf("live background launches after A's end = %d, want A-shell and B", n)
	}
	if blocking, err := s.HasQueueBlockingBackgroundToolCall("T"); err != nil || blocking {
		t.Fatalf("HasQueueBlockingBackgroundToolCall = %v, %v; want false: only top-level work blocks the queue", blocking, err)
	}
	recoverable, err := s.ListRecoverableClaudeBackgroundLaunchesForThread("T")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range recoverable {
		ids = append(ids, item.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"A-shell", "B"}) {
		t.Fatalf("launches a session end settles = %v, want A-shell and B", ids)
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

// seedAgentSubtreeCostThread writes, in one bulk-load transaction,
// background agent A with 41 open rows (a foreground agent inside it, 20
// running rows under A and 20 streaming rows under the foreground agent),
// and `others` completed rows under a second agent Z.
func seedAgentSubtreeCostThread(t *testing.T, s *Store, thread string, others int) {
	t.Helper()
	mustCreateThread(t, s, thread)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := setHistoryBulkLoadTx(tx, thread, true, "test seed"); err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
	    summary, parent_id, is_background, tool_name, created_at, updated_at)
	  VALUES (?, ?, 0, ?, ?, 'assistant', ?, ?, ?, ?, ?, 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	row := func(id, parent, kind, status, tool string, background bool) {
		t.Helper()
		index++
		if _, err := insert.Exec(id, thread, index, kind, status, id, parent, background, tool); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	row("A", "", "tool_call", "running", "Agent", true)
	row("A-fg", "A", "tool_call", "running", "Agent", false)
	for i := range 20 {
		row(fmt.Sprintf("A-read-%d", i), "A", "tool_call", "running", "Read", false)
		row(fmt.Sprintf("A-fg-text-%d", i), "A-fg", "assistant_text", "streaming", "", false)
	}
	row("Z", "", "tool_call", "running", "Agent", true)
	for i := range others {
		row(fmt.Sprintf("Z-%d", i), "Z", "tool_call", "completed", "Read", false)
	}
	if err := insert.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.restampSubagentAggregatesTx(tx, thread); err != nil {
		t.Fatal(err)
	}
	if err := setHistoryBulkLoadTx(tx, thread, false, "test seed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// writerReadPagesForTest counts the page-cache lookups of reading every
// row query returns on the writer connection.
func writerReadPagesForTest(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	writerPagesForTest(t, s, true)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return writerPagesForTest(t, s, false)
}

// TestAgentSubtreeReadsCostTheSubtreeNotTheThread: an agent's subtree
// reads, and the end that settles it, cost the same whether or not the
// thread holds 3,000 more rows under another parent. The threads share
// one database, so every probe descends the same B-trees. The tail
// thread, written last and sorted last, holds the last page of each,
// where SQLite seeks without descending from the root, so neither
// measured thread gets that discount. Reading the other rows once in
// index order costs about 70 page lookups, and reading them for each
// queued row about 3,000, so a read's slack is 16; the end, many
// statements over rows it moves, is allowed a tenth of the latter.
func TestAgentSubtreeReadsCostTheSubtreeNotTheThread(t *testing.T) {
	const others = 3000
	s := newTestStore(t)
	seedAgentSubtreeCostThread(t, s, "small", 0)
	seedAgentSubtreeCostThread(t, s, "big", others)
	seedAgentSubtreeCostThread(t, s, "tail", 1000)
	compare := func(what string, small, big, slack int) {
		t.Helper()
		t.Logf("%s: %d pages beside no other rows, %d beside %d", what, small, big, others)
		if big > small+slack {
			t.Errorf("%s touches %d pages beside %d other rows and %d beside none: it reads the thread", what, big, others, small)
		}
	}
	for name, query := range map[string]string{
		"agentSubtreeUnsettledSQL": agentSubtreeUnsettledSQL,
		"agentSubtreeStreamingSQL": agentSubtreeStreamingSQL,
	} {
		compare(name, writerReadPagesForTest(t, s, query, "small", "A"), writerReadPagesForTest(t, s, query, "big", "A"), 16)
	}
	end := func(thread string) int {
		t.Helper()
		sibling := Item{ID: "complete:A", ThreadID: thread, Kind: "tool_completion", Role: "assistant",
			Status: "killed", Summary: "A stopped", CompletionOf: "A", IsBackground: true, CreatedAt: 2, UpdatedAt: 2}
		var settled []SettledRow
		pages := storePageAccessesForTest(t, s, "end A in "+thread, func() error {
			var err error
			_, settled, err = s.UpsertAgentEnd(sibling, nil, "A", AgentEndRule{Summarise: stopped}, 5)
			return err
		})
		if len(settled) != 41 {
			t.Errorf("the end of A in %s settled %d rows, want 41", thread, len(settled))
		}
		return pages
	}
	compare("UpsertAgentEnd", end("small"), end("big"), others/10)
}
