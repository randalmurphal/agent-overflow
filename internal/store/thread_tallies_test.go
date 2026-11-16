package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// v154 counts what each thread did before it: its top-level tool calls and
// the tokens of its settled turns. A nested tool call, another row kind,
// and the ledger rows of a deleted thread count for nothing.
func TestThreadTalliesMigrationCountsEachThreadsPast(t *testing.T) {
	db := migrateThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/tmp/p', 'P', 1, 1)`)
	for _, id := range []string{"busy", "idle"} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
			VALUES (?, 'p', ?, 'claude', '/tmp/p', 'm', 1, 1, 0, 'chat')`, id, id)
	}
	for i, row := range []struct{ id, kind, parent string }{
		{"call-1", "tool_call", ""}, {"call-2", "tool_call", ""}, {"nested", "tool_call", "call-1"},
		{"said", "assistant_text", ""},
	} {
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, created_at, updated_at)
			VALUES (?, 'busy', 0, ?, ?, 'assistant', 'completed', '', ?, 1, 1)`, row.id, i, row.kind, row.parent)
	}
	for _, thread := range []string{"busy", "busy", "gone"} {
		mustExec(t, db, `INSERT INTO usage_ledger (created_at, thread_id, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, reasoning_output_tokens)
			VALUES (1, ?, 10, 20, 300, 4, 7)`, thread)
	}

	migrateFrom(t, db, assistantsMigrationVersion-1)

	if got := readTallies(t, db); len(got) != 1 || got["busy"] != (ThreadTally{ToolCalls: 2, Tokens: 2 * 334}) {
		t.Fatalf("tallies after the upgrade %+v; want busy's two top-level calls and 668 tokens, nothing else", got)
	}
}

// From then on the triggers keep the counts in the write's own transaction,
// and a read adds the running turn's pending tokens. A thread's row goes
// with it; the ledger of a thread that no longer exists tallies nothing.
func TestThreadTalliesFollowEveryWrite(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	mustCreateThread(t, s, "other")
	if got, err := s.ThreadTallies([]string{"worker", "other"}); err != nil || len(got) != 0 {
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
	got, err := s.ThreadTallies([]string{"worker", "other", "worker", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["worker"] != (ThreadTally{ToolCalls: 2, Tokens: 1160 + 10}) {
		t.Fatalf("tallies %+v; want the worker's two top-level calls, its settled turn and its running one", got)
	}
	// The running turn settles: its pending tokens become the ledger's,
	// counted once.
	settled := pending
	settled.CacheReadInputTokens = 40
	if err := s.AppendUsageAndReconcile("scope", []UsageLedgerRow{settled}); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	if got, err := s.ThreadTallies([]string{"worker"}); err != nil || got["worker"] != (ThreadTally{ToolCalls: 2, Tokens: 1160 + 50}) {
		t.Fatalf("tallies once the turn settled %+v, %v; want its final tokens counted once", got, err)
	}
	if err := s.DeleteThread("worker"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_tallies`); n != 0 {
		t.Fatalf("%d tallies left after the delete", n)
	}
}

func readTallies(t *testing.T, db *sql.DB) map[string]ThreadTally {
	t.Helper()
	rows, err := db.Query(`SELECT thread_id, tool_calls, tokens FROM thread_tallies`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ThreadTally{}
	for rows.Next() {
		var id string
		var tally ThreadTally
		if err := rows.Scan(&id, &tally.ToolCalls, &tally.Tokens); err != nil {
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
	if err := st.AppendUsage([]UsageLedgerRow{{CreatedAt: 1, ThreadID: "t1", TurnID: "t", Provider: "claude", Model: "m", InputTokens: 40}}); err != nil {
		t.Fatal(err)
	}
	want, err := st.ThreadTallies([]string{"t1"})
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
	if got, err := st.ThreadTallies([]string{"t1"}); err != nil || got["t1"] != want["t1"] {
		t.Fatalf("tallies after the restore %+v, %v; want the snapshot's %+v", got, err, want)
	}
	if _, err := appendCarded(st, Item{ID: "after", ThreadID: "t1", TurnIndex: 2, ItemIndex: 0, Kind: "tool_call", Role: "assistant",
		Status: "completed", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ThreadTallies([]string{"t1"}); err != nil || got["t1"].ToolCalls != want["t1"].ToolCalls+1 {
		t.Fatalf("tallies after a call past the restore %+v, %v; want one more call", got, err)
	}
}
