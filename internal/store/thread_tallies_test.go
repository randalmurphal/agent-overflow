package store

import (
	"database/sql"
	"maps"
	"path/filepath"
	"testing"
	"time"
)

// v154 counts what each thread did before it: its top-level tool calls, the
// tokens its settled turns spent itself, cache reads not among them, and the
// time its settled turns ran. A nested tool call, another row kind, the
// ledger rows of a deleted thread, and what a pointer fork inherited through
// its cut count for nothing; nor does the ledger of a Claude turn that ran a
// native subagent, whose modelUsage includes the subagent's and which kept
// no figure of the thread's own. A Codex turn's ledger is its own.
func TestThreadTalliesMigrationCountsEachThreadsPast(t *testing.T) {
	db := migrateThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/tmp/p', 'P', 1, 1)`)
	for _, id := range []string{"busy", "idle", "forked", "codexy"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
			VALUES (?, 'p', ?, 'claude', '/tmp/p', 'm', 1, 1, 0, 'chat')`, id, id)
	}
	mustExec(t, db, `UPDATE threads SET fork_source_thread_id = 'busy', fork_cut_turn_index = 0 WHERE id = 'forked'`)
	for i, row := range []struct{ id, thread, kind, parent string }{
		{"call-1", "busy", "tool_call", ""}, {"call-2", "busy", "tool_call", ""}, {"nested", "busy", "tool_call", "call-1"},
		{"said", "busy", "assistant_text", ""}, {"call-1", "forked", "tool_call", ""},
		{"agent", "codexy", "tool_call", ""}, {"child", "codexy", "assistant_text", "agent"},
	} {
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, created_at, updated_at)
			VALUES (?, ?, 0, ?, ?, 'assistant', 'completed', '', ?, 1, 1)`, row.id, row.thread, i, row.kind, row.parent)
	}
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, created_at, updated_at)
		VALUES ('own', 'forked', 1, 0, 'tool_call', 'assistant', 'completed', '', '', 1, 1)`)
	for _, turn := range []struct {
		thread           string
		index            int
		started, settled any
	}{{"busy", 0, 1, 11}, {"busy", 1, 20, nil}, {"forked", 0, 1, 11}, {"forked", 1, 30, 35}, {"codexy", 0, 1, 2}} {
		mustExec(t, db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at, stop_reason, assistant_message_id, token_usage_json, error_message)
			VALUES (? || ':' || ?, ?, ?, ?, ?, '', '', '', '')`, turn.thread, turn.index, turn.thread, turn.index, turn.started, turn.settled)
	}
	// busy's turn 0 ran a native subagent (its nested rows); turn 1 did not.
	for _, row := range []struct{ thread, turn, provider string }{
		{"busy", "busy:0", "claude"}, {"busy", "busy:1", "claude"}, {"busy", "", "claude"},
		{"codexy", "codexy:0", "codex"}, {"gone", "gone:0", "claude"},
	} {
		mustExec(t, db, `INSERT INTO usage_ledger (created_at, thread_id, turn_id, provider, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, reasoning_output_tokens)
			VALUES (1, ?, ?, ?, 10, 20, 300, 4, 7)`, row.thread, row.turn, row.provider)
	}

	migrateFrom(t, db, assistantsMigrationVersion-1)

	got := readTallies(t, db)
	want := map[string]ThreadTally{
		"busy":   {ToolCalls: 2, Tokens: 2 * 34, RuntimeMs: 10},
		"forked": {ToolCalls: 1, RuntimeMs: 5},
		"codexy": {ToolCalls: 1, Tokens: 34, RuntimeMs: 1},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("tallies after the upgrade %+v; want %+v", got, want)
	}
}

// From then on each count is kept where the work is recorded, in its own
// transaction, and a read adds the running turn's pending tokens and time.
// Tokens are the thread's own spend its usage settlement names, not the
// billing ledger's rows, which may include its native subagents'; an agent's
// completion record adds nothing. A thread's row goes with it; the spend of
// a thread that no longer exists tallies nothing.
func TestThreadTalliesFollowEveryWrite(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	mustCreateThread(t, s, "other")
	at := func(ms int64) time.Time { return time.UnixMilli(ms) }
	if got, err := s.ThreadTallies([]string{"worker", "other"}, at(0)); err != nil || len(got) != 0 {
		t.Fatalf("tallies of new threads %+v, %v; want none", got, err)
	}
	for i, row := range []struct{ id, kind, parent string }{
		{"a", "tool_call", ""}, {"b", "tool_call", ""}, {"c", "tool_call", "a"}, {"d", "assistant_text", ""},
	} {
		if _, err := appendCarded(s, Item{ID: row.id, ThreadID: "worker", ItemIndex: i, Kind: row.kind, Role: "assistant",
			Status: "completed", ParentID: row.parent, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("append %s: %v", row.id, err)
		}
	}
	usage := UsageLedgerRow{CreatedAt: 1, ThreadID: "worker", TurnID: "t0", Provider: "claude", Model: "m",
		InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 1000, CacheCreationInputTokens: 10, ReasoningOutputTokens: 5}
	if err := s.AppendUsage([]UsageLedgerRow{usage, {CreatedAt: 1, ThreadID: "gone", Model: "m", InputTokens: 9}}); err != nil {
		t.Fatalf("append usage: %v", err)
	}
	pending := usage
	pending.TurnID, pending.InputTokens, pending.OutputTokens, pending.CacheReadInputTokens, pending.CacheCreationInputTokens = "t1", 7, 3, 0, 0
	if _, err := s.PutUsageProgress("scope", "seg", []UsageLedgerRow{pending}); err != nil {
		t.Fatalf("put usage progress: %v", err)
	}
	got, err := s.ThreadTallies([]string{"worker", "other", "worker", "gone"}, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["worker"] != (ThreadTally{ToolCalls: 2, Tokens: 10}) {
		t.Fatalf("tallies %+v; want the worker's two top-level calls and its running turn's pending tokens, no ledger row", got)
	}
	// The running turn settles: its ledger row includes a subagent's 30
	// output tokens, its own spend is 20. The pending tokens become the
	// thread's own, counted once.
	settled := pending
	settled.OutputTokens = 43
	if err := s.AppendUsageAndReconcile("scope", []UsageLedgerRow{settled}, OwnSpend{ThreadID: "worker", Tokens: 20}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	if err := s.AppendUsageAndReconcile("scope", nil, OwnSpend{ThreadID: "gone", Tokens: 9}); err != nil {
		t.Fatalf("settle a deleted thread's usage: %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_tallies WHERE thread_id = 'gone'`); n != 0 {
		t.Fatal("a thread that does not exist was tallied")
	}
	if n := countRows(t, s, `SELECT SUM(input_tokens + cache_creation_input_tokens + output_tokens) FROM usage_ledger WHERE thread_id = 'worker'`); n != 160+50 {
		t.Fatalf("the ledger holds %d; want every row as billed", n)
	}
	tally := func(now int64) ThreadTally {
		t.Helper()
		got, err := s.ThreadTallies([]string{"worker"}, at(now))
		if err != nil {
			t.Fatal(err)
		}
		return got["worker"]
	}
	if got := tally(0); got != (ThreadTally{ToolCalls: 2, Tokens: 20}) {
		t.Fatalf("tallies once the turn settled %+v; want its own spend counted once", got)
	}
	// A native agent's completion record is its own row's count.
	if _, err := appendCarded(s, Item{ID: "done-1", ThreadID: "worker", ItemIndex: 10, Kind: "tool_completion", Role: "assistant",
		Status: "completed", CompletionOf: "spawn-1", Meta: `{"subagentProgress":{"totalTokens":900}}`, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("append the completion: %v", err)
	}
	if got := tally(0).Tokens; got != 20 {
		t.Fatalf("tokens with an agent's completion %d; want the thread's own 20", got)
	}

	// Runtime is the time its turns ran: the running one's to now, then
	// its span once it ends, taken back while it is reopened, moved by a
	// late end, and nothing for the time between turns.
	if err := s.InsertTurn(Turn{TurnID: "worker:0", ThreadID: "worker", TurnIndex: 0, StartedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if got := tally(1500); got.RuntimeMs != 500 || !got.Running {
		t.Fatalf("runtime mid-turn %+v; want 500 ms, running", got)
	}
	if err := s.UpdateTurnCompleted("worker:0", 1400, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := tally(9000); got.RuntimeMs != 400 || got.Running {
		t.Fatalf("runtime at rest %+v; want the turn's 400 ms, not running", got)
	}
	if err := s.ReopenTurn("worker:0"); err != nil {
		t.Fatal(err)
	}
	if got := tally(1500); got.RuntimeMs != 500 || !got.Running {
		t.Fatalf("runtime of the reopened turn %+v; want it counted once, to now", got)
	}
	if err := s.UpdateTurnCompleted("worker:0", 1600, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTurnLatePayload("worker:0", LateTurnPayload{CompletedAtAdvance: 1700}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTurn(Turn{TurnID: "worker:1", ThreadID: "worker", TurnIndex: 1, StartedAt: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTurnCompleted("worker:1", 5300, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := tally(9000); got.RuntimeMs != 700+300 || got.Running {
		t.Fatalf("runtime of two turns %+v; want 1000 ms, the idle time between them not counted", got)
	}

	if err := s.DeleteThread("worker"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_tallies`); n != 0 {
		t.Fatalf("%d tallies left after the delete", n)
	}
}

// History a fork copies is not work the fork did: a pointer fork of a
// source still running its cut turn owns a copy of that turn's tool calls,
// one of a settled source reads them by pointer, and a rewrite of an
// inherited row gives the fork its own copy. None of it counts; each
// fork's own call does, once.
func TestThreadTalliesCountNoCopiedHistory(t *testing.T) {
	s := newTestStore(t)
	for _, source := range []string{"settled", "running"} {
		mustCreateThread(t, s, source)
		for i, row := range []Item{
			{ID: "ask", Kind: "user_text", Role: "user"},
			{ID: "inherited", Kind: "tool_call", Role: "assistant", ToolName: "Bash"},
		} {
			row.ThreadID, row.ItemIndex, row.Status, row.Meta, row.CreatedAt, row.UpdatedAt = source, i, "completed", "{}", 1, 1
			if err := insertCarded(s, row); err != nil {
				t.Fatalf("insert %s/%s: %v", source, row.ID, err)
			}
		}
		if err := s.InsertTurn(Turn{TurnID: source + ":0", ThreadID: source, TurnIndex: 0, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateTurnCompleted("settled:0", 2, "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "settled", "settled-fork", ForkCut{})
	mustPointerFork(t, s, "running", "running-fork", ForkCut{})
	if owned, err := queryIDs(s.db, `SELECT id FROM items WHERE thread_id = 'running-fork' AND kind = 'tool_call'`); err != nil || len(owned) != 1 {
		t.Fatalf("the running source's fork owns %v, %v; want its copy of the call", owned, err)
	}
	// The source's rewrite of the settled call gives its fork a copy.
	summary := "rewritten"
	if _, err := s.UpdateItemFields("settled", "inherited", ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	for _, fork := range []string{"settled-fork", "running-fork"} {
		if _, err := appendCarded(s, Item{ID: "own", ThreadID: fork, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
			Status: "completed", Meta: "{}", CreatedAt: 3, UpdatedAt: 3}); err != nil {
			t.Fatalf("append to %s: %v", fork, err)
		}
	}
	got, err := s.ThreadTallies([]string{"settled", "running", "settled-fork", "running-fork"}, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	for thread, want := range map[string]int64{"settled": 1, "running": 1, "settled-fork": 1, "running-fork": 1} {
		if got[thread].ToolCalls != want {
			t.Fatalf("%s counts %d tool calls (%+v); want %d", thread, got[thread].ToolCalls, got, want)
		}
	}
}

func readTallies(t *testing.T, db *sql.DB) map[string]ThreadTally {
	t.Helper()
	rows, err := db.Query(`SELECT thread_id, tool_calls, tokens, turn_ms FROM thread_tallies`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ThreadTally{}
	for rows.Next() {
		var id string
		var tally ThreadTally
		if err := rows.Scan(&id, &tally.ToolCalls, &tally.Tokens, &tally.RuntimeMs); err != nil {
			t.Fatal(err)
		}
		out[id] = tally
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A restore lands the snapshot's tallies as they are, not recounted by the
// copy of its items and ledger, and the counting goes on after it.
func TestThreadTalliesSurviveARestore(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "x")
	if _, err := appendCarded(st, Item{ID: "call", ThreadID: "t1", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "completed", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendUsageAndReconcile("scope", []UsageLedgerRow{{CreatedAt: 1, ThreadID: "t1", TurnID: "t", Provider: "claude", Model: "m", InputTokens: 40}},
		OwnSpend{ThreadID: "t1", Tokens: 40}); err != nil {
		t.Fatal(err)
	}
	want, err := st.ThreadTallies([]string{"t1"}, time.UnixMilli(0))
	if err != nil || want["t1"].ToolCalls == 0 || want["t1"].Tokens != 40 {
		t.Fatalf("tallies before the snapshot %+v, %v", want, err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if got, err := st.ThreadTallies([]string{"t1"}, time.UnixMilli(0)); err != nil || got["t1"] != want["t1"] {
		t.Fatalf("tallies after the restore %+v, %v; want the snapshot's %+v", got, err, want)
	}
	if _, err := appendCarded(st, Item{ID: "after", ThreadID: "t1", TurnIndex: 2, ItemIndex: 0, Kind: "tool_call", Role: "assistant",
		Status: "completed", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ThreadTallies([]string{"t1"}, time.UnixMilli(0)); err != nil || got["t1"].ToolCalls != want["t1"].ToolCalls+1 {
		t.Fatalf("tallies after a call past the restore %+v, %v; want one more call", got, err)
	}
}
