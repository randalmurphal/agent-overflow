package store

import (
	"context"
	"database/sql"
	"maps"
	"testing"
	"time"
)

func interruptedForTest(summary string) string { return summary + " (interrupted)" }
func unresolvedForTest(summary string) string  { return summary + " (unresolved)" }

// agentEndRuleForTest mirrors triage.AgentEndRuleFor, which the store
// cannot import.
func agentEndRuleForTest(status, source string) AgentEndRule {
	switch {
	case status == "completed":
		return AgentEndRule{StreamingCompletes: true, Summarise: unresolvedForTest}
	case status == "killed" && source != sessionDiedSource:
		return AgentEndRule{Summarise: stopped}
	default:
		return AgentEndRule{Summarise: interruptedForTest}
	}
}

// endedAgentsAtV123 opens a store upgraded to v124 from a v123 database
// whose thread T holds the rows earlier builds left open under agents. Turns
// 0 to 2 are closed and turn 3 is open; "later" rows are newer than the
// upgrade, as a live session's are.
//
//	A  (bg Agent, completed)        A-read running, A-text streaming, A-done completed
//	└─ B (bg Agent, errored launch)  B-read running: died with its session, no sibling
//	K  (bg Agent, stopped)          K-read running
//	D  (bg Agent, session died)     D-text streaming
//	R  (bg Agent, completed)        R-read running; resumed by carrier C, later, no sibling
//	L  (bg Agent, later, no sibling) L-read running
//	P  (bg Agent, no sibling, turn 3) P-read running
//	F  (foreground Agent)           F-read running
//	main-read (top level, turn 3)   running
func endedAgentsAtV123(t *testing.T) *Store {
	t.Helper()
	s := openStoreAt(t)
	mustCreateThread(t, s, "T")
	mustCreateThread(t, s, "idle")
	later := time.Now().Add(time.Hour).UnixMilli()
	row := func(id, parent string, turn int, kind, tool, status string, background bool, created int64, meta string) {
		t.Helper()
		if _, err := upsertCarded(s, Item{
			ID: id, ThreadID: "T", TurnIndex: turn, Kind: kind, Role: "assistant", Status: status,
			Summary: id, ParentID: parent, ToolName: tool, IsBackground: background, Meta: meta,
			CreatedAt: created, UpdatedAt: created,
		}, nil); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	end := func(of, status, source string, turn int) {
		t.Helper()
		if _, err := upsertCarded(s, Item{
			ID: "complete:" + of, ThreadID: "T", TurnIndex: turn, Kind: "tool_completion", Role: "assistant",
			Status: status, Summary: of + " " + status, CompletionOf: of, IsBackground: true,
			Meta: `{"status_source":"` + source + `"}`, CreatedAt: 5, UpdatedAt: 5,
		}, nil); err != nil {
			t.Fatalf("seed the end of %s: %v", of, err)
		}
	}
	for turn := 0; turn <= 3; turn++ {
		if err := s.InsertTurn(Turn{TurnID: "T:" + string(rune('0'+turn)), ThreadID: "T", TurnIndex: turn, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s.db, `UPDATE turns SET completed_at = 10 WHERE thread_id = 'T' AND turn_index < 3`)

	row("A", "", 0, "tool_call", "Agent", "running", true, 1, "{}")
	row("A-read", "A", 1, "tool_call", "Read", "running", false, 2, "{}")
	row("A-text", "A", 1, "assistant_text", "", "streaming", false, 2, "{}")
	row("A-done", "A", 1, "tool_call", "Read", "completed", false, 2, "{}")
	row("B", "A", 1, "tool_call", "Agent", "errored", true, 2, "{}")
	row("B-read", "B", 1, "tool_call", "Read", "running", false, 3, "{}")
	end("A", "completed", "task_notification", 1)

	row("K", "", 0, "tool_call", "Agent", "running", true, 1, "{}")
	row("K-read", "K", 1, "tool_call", "Read", "running", false, 2, "{}")
	end("K", "killed", "task_updated", 1)

	row("D", "", 0, "tool_call", "Agent", "running", true, 1, "{}")
	row("D-text", "D", 1, "assistant_text", "", "streaming", false, 2, "{}")
	end("D", "killed", sessionDiedSource, 1)

	row("R", "", 0, "tool_call", "Agent", "running", true, 1, "{}")
	end("R", "completed", "task_notification", 0)
	row("C", "", 2, "tool_call", "Agent", "running", true, later, `{"transcript_root_id":"R"}`)
	row("R-read", "R", 2, "tool_call", "Read", "running", false, later, "{}")

	row("L", "", 2, "tool_call", "Agent", "running", true, later, "{}")
	row("L-read", "L", 2, "tool_call", "Read", "running", false, later, "{}")

	row("P", "", 3, "tool_call", "Agent", "running", true, 1, "{}")
	row("P-read", "P", 3, "tool_call", "Read", "running", false, 2, "{}")

	row("F", "", 3, "tool_call", "Agent", "running", false, 1, "{}")
	row("F-read", "F", 3, "tool_call", "Read", "running", false, 2, "{}")
	row("main-read", "", 3, "tool_call", "Read", "running", false, 2, "{}")

	// "idle" holds an agent whose rows all settled; its launch stays
	// running at the top level, as every background launch does.
	for _, it := range []Item{
		{ID: "I", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true},
		{ID: "I-read", Kind: "tool_call", ToolName: "Read", Status: "completed", ParentID: "I"},
	} {
		it.ThreadID, it.Role, it.Summary, it.CreatedAt, it.UpdatedAt = "idle", "assistant", it.ID, 1, 1
		if _, err := upsertCarded(s, it, nil); err != nil {
			t.Fatalf("seed %s: %v", it.ID, err)
		}
	}

	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	downgradeSchema(t, db, migrateThrough(t, 123), 123)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close upgraded store: %v", err)
		}
	})
	return s
}

// TestMigrationV124SettlesTheRowsOfEndedAgents: v124 lists the thread with
// open rows under a parent, and its deferred phase settles each ended
// agent's open rows by the rule its end makes, leaves running agents and
// rows no agent owns alone, keeps the thread listed while a dead session's
// turn is open, and finishes on the next run once it is closed.
func TestMigrationV124SettlesTheRowsOfEndedAgents(t *testing.T) {
	s := endedAgentsAtV123(t)
	if got := countRows(t, s, `SELECT count(*) FROM agent_end_backfill WHERE thread_id = 'T'`); got != 1 {
		t.Fatalf("v124 listed T %d times, want once", got)
	}
	if got := countRows(t, s, `SELECT count(*) FROM agent_end_backfill WHERE thread_id = 'idle'`); got != 0 {
		t.Fatal("v124 listed a thread with no open row")
	}
	before := rowStates(t, s, "T")

	pauses := 0
	host := DeferredHost{Pause: func() { pauses++ }, AgentEndRule: agentEndRuleForTest}
	if err := s.RunDeferredMigrations(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	failure := deferredFailureOf(t, s)
	if !deferredPending(t, s) || failure == nil || failure.Version != 124 || failure.Failures != 1 {
		t.Fatalf("with P's turn open the phase left watermark %d, failure %+v; want one failure and v124 pending",
			deferredWatermarkOf(t, s), failure)
	}
	if pauses == 0 {
		t.Error("the phase never paused between its writes")
	}
	want := map[string]string{
		"A-read":    "errored:" + unresolvedForTest("A-read"),
		"A-text":    "completed:A-text",
		"A-done":    before["A-done"],
		"B-read":    "errored:" + interruptedForTest("B-read"),
		"K-read":    "errored:" + stopped("K-read"),
		"D-text":    "errored:" + interruptedForTest("D-text"),
		"R-read":    before["R-read"],
		"L-read":    before["L-read"],
		"P-read":    before["P-read"],
		"F-read":    before["F-read"],
		"main-read": before["main-read"],
		"A":         before["A"],
	}
	got := rowStates(t, s, "T")
	for id, state := range want {
		if got[id] != state {
			t.Errorf("after the first run %s = %q, want %q", id, got[id], state)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM agent_end_backfill WHERE thread_id = 'T'`); n != 1 {
		t.Fatal("the thread left the list with an agent unsettled")
	}

	// The crash sweep closes the dead session's turn; the next run settles
	// P and finishes.
	mustExec(t, s.db, `UPDATE turns SET completed_at = 20 WHERE thread_id = 'T' AND turn_index = 3`)
	if err := s.RunDeferredMigrations(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredFailureOf(t, s) != nil {
		t.Fatalf("the second run left watermark %d, failure %+v", deferredWatermarkOf(t, s), deferredFailureOf(t, s))
	}
	got = rowStates(t, s, "T")
	want["P-read"] = "errored:" + interruptedForTest("P-read")
	for id, state := range want {
		if got[id] != state {
			t.Errorf("after the second run %s = %q, want %q", id, got[id], state)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM agent_end_backfill`); n != 0 {
		t.Fatalf("%d threads still listed after the phase finished", n)
	}
}

// TestMigrationV124PhaseIsIdempotent: a run over rows an earlier run
// settled, as after a quit, writes nothing.
func TestMigrationV124PhaseIsIdempotent(t *testing.T) {
	s := endedAgentsAtV123(t)
	mustExec(t, s.db, `UPDATE turns SET completed_at = 20 WHERE thread_id = 'T' AND turn_index = 3`)
	host := DeferredHost{AgentEndRule: agentEndRuleForTest}
	run := &deferredRun{host: host}
	if err := settleEndedAgentRows(context.Background(), s, run); err != nil || run.failures != 0 {
		t.Fatalf("first run: %v, %d failures", err, run.failures)
	}
	settled := rowStates(t, s, "T")
	stamp := historyStampOf(t, s, "T")
	mustExec(t, s.db, `INSERT INTO agent_end_backfill(thread_id, opened_at) VALUES ('T', ?)`, time.Now().UnixMilli())
	run = &deferredRun{host: host}
	if err := settleEndedAgentRows(context.Background(), s, run); err != nil || run.failures != 0 {
		t.Fatalf("second run: %v, %d failures", err, run.failures)
	}
	if got := rowStates(t, s, "T"); !maps.Equal(got, settled) {
		t.Fatalf("the second run changed rows:\n got %v\nwant %v", got, settled)
	}
	if historyStampOf(t, s, "T") != stamp {
		t.Fatal("the second run moved the thread's history stamp")
	}
}

// TestMigrationV124PhaseFailsWithoutARule: a host that supplies no rule
// leaves the listed thread in place and reports it.
func TestMigrationV124PhaseFailsWithoutARule(t *testing.T) {
	s := endedAgentsAtV123(t)
	before := rowStates(t, s, "T")
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if failure := deferredFailureOf(t, s); failure == nil || failure.Version != 124 {
		t.Fatalf("failure = %+v, want v124's", failure)
	}
	if got := rowStates(t, s, "T"); !maps.Equal(got, before) {
		t.Fatal("the phase wrote rows without a rule")
	}
}
