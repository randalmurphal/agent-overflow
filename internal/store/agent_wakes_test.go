package store

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// v143 over a populated database: the rows it finds survive and every
// schema object they use is unchanged; the two new tables have exactly the
// spec's columns, constraints and due index; agent rows and wakes in every
// state written after it read back whole through a store opened on the file;
// and both tables cascade from their thread.
func TestStandingAgentsMigration(t *testing.T) {
	path := fileDBThrough(t, standingAgentsMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('agent', 'p', 'Agent', 'claude', '/p', '', 1, 1, 0),
		('watcher', 'p', 'Watcher', 'codex', '/p', '', 1, 2, 0),
		('plain', 'p', 'Plain', 'claude', '/p', '', 1, 3, 0),
		('shelved', 'p', 'Shelved', 'codex', '/p', '', 1, 4, 1)`)
	mustExec(t, db, `INSERT INTO items (thread_id, id, turn_index, item_index, kind, role, status, summary, tool_name, meta, created_at, updated_at) VALUES
		('agent', 'a0', 0, 0, 'user_text', 'user', 'completed', 'watch the board', '', '{}', 1, 1),
		('watcher', 'w0', 0, 0, 'user_text', 'user', 'completed', 'tail the stream', '', '{}', 1, 1)`)
	threadsBefore := migrationThreadRows(t, db)
	itemsBefore, err := queryIDs(db, `SELECT thread_id || '|' || id || '|' || summary FROM items ORDER BY thread_id, id`)
	if err != nil {
		t.Fatal(err)
	}
	schemaBefore := migrationSchema(t, db)

	if err := applyMigration(db, migrationByVersion(t, standingAgentsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", standingAgentsMigrationVersion, err)
	}

	if got := migrationThreadRows(t, db); !slices.Equal(got, threadsBefore) {
		t.Errorf("thread rows changed:\n got %q\nwant %q", got, threadsBefore)
	}
	if got, err := queryIDs(db, `SELECT thread_id || '|' || id || '|' || summary FROM items ORDER BY thread_id, id`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, itemsBefore) {
		t.Errorf("item rows changed:\n got %q\nwant %q", got, itemsBefore)
	}
	schemaAfter := migrationSchema(t, db)
	for name, sqlText := range schemaBefore {
		if schemaAfter[name] != sqlText {
			t.Errorf("schema object %s changed:\n got %q\nwant %q", name, schemaAfter[name], sqlText)
		}
	}
	var added []string
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	if want := []string{"agent_wakes", "idx_agent_wakes_due", "sqlite_autoindex_agent_wakes_1",
		"sqlite_autoindex_standing_agents_1", "standing_agents"}; !slices.Equal(added, want) {
		t.Errorf("v%d added %v, want %v", standingAgentsMigrationVersion, added, want)
	}

	// name|type|not null|default|primary key, in column order.
	columns := func(table string) []string {
		t.Helper()
		got, err := queryIDs(db, `SELECT name || '|' || type || '|' || "notnull" || '|' || COALESCE(dflt_value, 'NULL') || '|' || pk
			FROM pragma_table_info('`+table+`') ORDER BY cid`)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got, want := columns("standing_agents"), []string{
		"thread_id|TEXT|0|NULL|1",
		"home|TEXT|1|NULL|0",
		"notify_on_tick|INTEGER|1|0|0",
		"promoted_at|INTEGER|1|NULL|0",
	}; !slices.Equal(got, want) {
		t.Errorf("standing_agents columns:\n got %q\nwant %q", got, want)
	}
	if got, want := columns("agent_wakes"), []string{
		"id|TEXT|0|NULL|1",
		"thread_id|TEXT|1|NULL|0",
		"kind|TEXT|1|NULL|0",
		"label|TEXT|1|NULL|0",
		"note|TEXT|1|''|0",
		"command|TEXT|1|''|0",
		"cwd|TEXT|1|''|0",
		"interval_seconds|INTEGER|0|NULL|0",
		"timeout_seconds|INTEGER|0|NULL|0",
		"long_running|INTEGER|1|0|0",
		"next_at|INTEGER|0|NULL|0",
		"last_run_at|INTEGER|0|NULL|0",
		"last_outcome|TEXT|1|''|0",
		"last_output|TEXT|1|''|0",
		"missed|INTEGER|1|0|0",
		"state|TEXT|1|NULL|0",
		"created_at|INTEGER|1|NULL|0",
	}; !slices.Equal(got, want) {
		t.Errorf("agent_wakes columns:\n got %q\nwant %q", got, want)
	}
	if got, err := queryIDs(db, `SELECT name FROM pragma_index_info('idx_agent_wakes_due') ORDER BY seqno`); err != nil {
		t.Fatal(err)
	} else if want := []string{"state", "next_at"}; !slices.Equal(got, want) {
		t.Errorf("idx_agent_wakes_due columns = %q, want %q", got, want)
	}
	for _, table := range []string{"standing_agents", "agent_wakes"} {
		got, err := queryIDs(db, `SELECT "table" || '.' || "to" || '|' || on_delete FROM pragma_foreign_key_list('`+table+`')`)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"threads.id|CASCADE"}; !slices.Equal(got, want) {
			t.Errorf("%s foreign keys = %q, want %q", table, got, want)
		}
	}

	// Agent rows and wakes in every state, on more than one thread.
	mustExec(t, db, `INSERT INTO standing_agents (thread_id, home, notify_on_tick, promoted_at) VALUES
		('agent', '/h/agent', 0, 10), ('watcher', '/h/watcher', 1, 11)`)
	seeded := []AgentWake{
		{ID: "w-armed", ThreadID: "agent", Kind: AgentWakeLoop, Label: "board", Note: "check the board", IntervalSeconds: 600,
			NextAt: 5000, LastRunAt: 4400, LastOutcome: "delivered", State: AgentWakeArmed, CreatedAt: 20},
		{ID: "w-cancelled", ThreadID: "agent", Kind: AgentWakeLoop, Label: "old", Note: "retired", IntervalSeconds: 60,
			LastOutcome: "cancelled", State: AgentWakeCancelled, CreatedAt: 21},
		{ID: "w-running", ThreadID: "watcher", Kind: AgentWakeWatch, Label: "poll", Command: "sh poll.sh", Cwd: "sub",
			IntervalSeconds: 120, TimeoutSeconds: 900, NextAt: 6000, LastRunAt: 5880, LastOutcome: "not delivered",
			LastOutput: "Watch \"poll\" reported.", Missed: 2, State: AgentWakeRunning, CreatedAt: 22},
		{ID: "w-long", ThreadID: "watcher", Kind: AgentWakeWatch, Label: "tail", Command: "sh tail.sh", LongRunning: true,
			LastRunAt: 5900, LastOutcome: "exited; restarting in 1s", State: AgentWakeRunning, CreatedAt: 23},
	}
	for _, w := range seeded {
		long := 0
		if w.LongRunning {
			long = 1
		}
		mustExec(t, db, `INSERT INTO agent_wakes (`+agentWakeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.ThreadID, w.Kind, w.Label, w.Note, w.Command, w.Cwd, nullableMillis(w.IntervalSeconds),
			nullableMillis(w.TimeoutSeconds), long, nullableMillis(w.NextAt), nullableMillis(w.LastRunAt), w.LastOutcome,
			w.LastOutput, w.Missed, w.State, w.CreatedAt)
	}
	for _, bad := range []string{
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'agent', 'cron', 'l2', 'armed', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'agent', 'loop', 'l2', 'paused', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, long_running, state, created_at) VALUES ('x', 'agent', 'watch', 'l2', 2, 'armed', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 'missing', 'loop', 'l2', 'armed', 1)`,
		`INSERT INTO standing_agents (thread_id, home, notify_on_tick, promoted_at) VALUES ('plain', '/h', 2, 1)`,
		`INSERT INTO standing_agents (thread_id, home, promoted_at) VALUES ('missing', '/h', 1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("open the migrated file: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	for _, want := range seeded {
		got, found, err := s.GetAgentWake(want.ID)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", want.ID, found, err)
		}
		if got != want {
			t.Errorf("wake %s read back\n got %+v\nwant %+v", want.ID, got, want)
		}
	}
	live, err := s.ListLiveAgentWakes()
	if err != nil {
		t.Fatal(err)
	}
	var liveIDs []string
	for _, row := range live {
		liveIDs = append(liveIDs, row.ID)
	}
	if want := []string{"w-armed", "w-running", "w-long"}; !slices.Equal(liveIDs, want) {
		t.Errorf("live wakes = %v, want %v", liveIDs, want)
	}
	if agent, found, err := s.GetStandingAgent("watcher"); err != nil || !found ||
		agent != (StandingAgent{ThreadID: "watcher", Home: "/h/watcher", NotifyOnTick: true, PromotedAt: 11}) {
		t.Errorf("watcher's agent row = %+v found=%v err=%v", agent, found, err)
	}

	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 'agent'`); err != nil {
		t.Fatal(err)
	}
	var agents, wakes int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM standing_agents), (SELECT count(*) FROM agent_wakes)`).
		Scan(&agents, &wakes); err != nil {
		t.Fatal(err)
	}
	if agents != 1 || wakes != 2 {
		t.Fatalf("after deleting one thread: %d agent rows, %d wakes, want its own cascaded and 1 and 2 left", agents, wakes)
	}
}

// One label names one row for the life of the thread: re-registering replaces
// it, a cancelled row is revived, the other kind cannot take a live label, and
// the live rows are bounded.
func TestRegisterAgentWakeKeepsOneRowPerLabel(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	first, err := s.RegisterAgentWake(AgentWake{ID: "a", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n1", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RegisterAgentWake(AgentWake{ID: "b", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n2", IntervalSeconds: 120, NextAt: 200})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.IntervalSeconds != 120 || again.Note != "n2" || again.NextAt != 200 {
		t.Fatalf("re-register = %+v, want row %s replaced", again, first.ID)
	}
	if _, err := s.RegisterAgentWake(AgentWake{ID: "c", ThreadID: "t", Kind: AgentWakeWatch, Label: "board", Command: "true"}); !errors.Is(err, ErrAgentWakeLabelTaken) {
		t.Fatalf("watch on a live loop label: %v, want ErrAgentWakeLabelTaken", err)
	}
	if _, cancelled, err := s.CancelAgentWake("t", "board", "", "cancelled"); err != nil || !cancelled {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	revived, err := s.RegisterAgentWake(AgentWake{ID: "d", ThreadID: "t", Kind: AgentWakeWatch, Label: "board", Command: "true", LongRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if revived.ID != first.ID || revived.Kind != AgentWakeWatch || revived.State != AgentWakeArmed || !revived.LongRunning {
		t.Fatalf("revived = %+v", revived)
	}
	for i := 1; i < MaxAgentWakesPerThread; i++ {
		if _, err := s.RegisterAgentWake(AgentWake{ID: "x" + strconv.Itoa(i), ThreadID: "t", Kind: AgentWakeLoop, Label: "l" + strconv.Itoa(i), IntervalSeconds: 60, NextAt: 1}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	if _, err := s.RegisterAgentWake(AgentWake{ID: "over", ThreadID: "t", Kind: AgentWakeLoop, Label: "over", IntervalSeconds: 60, NextAt: 1}); !errors.Is(err, ErrAgentWakeLimit) {
		t.Fatalf("past the bound: %v, want ErrAgentWakeLimit", err)
	}
}

// A run is claimed and finished conditionally, so a second claim, a claim of
// a cancelled row and a finish of a row nobody claimed are all lost races.
func TestAgentWakeRunsAreConditional(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	row, err := s.RegisterAgentWake(AgentWake{ID: "a", ThreadID: "t", Kind: AgentWakeLoop, Label: "l", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	if due, err := s.DueAgentWakes(99, 8); err != nil || len(due) != 0 {
		t.Fatalf("due before its time: %v %v", due, err)
	}
	if due, err := s.DueAgentWakes(100, 8); err != nil || len(due) != 1 {
		t.Fatalf("due at its time: %v %v", due, err)
	}
	if finished, err := s.FinishAgentWake(row.ID, "early", "", 0); err != nil || finished {
		t.Fatalf("finish before claim: %v %v", finished, err)
	}
	if claimed, err := s.ClaimAgentWake(row.ID, 100, 160, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if claimed, err := s.ClaimAgentWake(row.ID, 100, 160, 0); err != nil || claimed {
		t.Fatalf("second claim: %v %v", claimed, err)
	}
	if finished, err := s.FinishAgentWake(row.ID, "delivered", "", 0); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	if next, err := s.NextAgentWakeDue(); err != nil || !next.Set || next.At != 160 {
		t.Fatalf("next due = %+v %v, want 160", next, err)
	}
	if ids, err := s.CancelAgentWakesForThread("t", "", "archived"); err != nil || len(ids) != 1 {
		t.Fatalf("cancel thread: %v %v", ids, err)
	}
	if claimed, err := s.ClaimAgentWake(row.ID, 200, 260, 0); err != nil || claimed {
		t.Fatalf("claim of a cancelled row: %v %v", claimed, err)
	}
	if next, err := s.NextAgentWakeDue(); err != nil || next.Set {
		t.Fatalf("next due after cancel = %+v %v, want none", next, err)
	}
}

// A row's second wake is refused while its first still waits on the queue.
func TestQueueAgentWakeRefusesASecondQueuedWake(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	item := func(id, sendID string) FlushQueueItem {
		return FlushQueueItem{ID: id, ThreadID: "t", SendID: sendID, Message: "m", EnqueuedAt: 1}
	}
	if err := s.QueueAgentWake(item("q1", "agent-wake:a:1"), "agent-wake:a:"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAgentWake(item("q2", "agent-wake:a:2"), "agent-wake:a:"); !errors.Is(err, ErrAgentWakeStillQueued) {
		t.Fatalf("second wake: %v, want ErrAgentWakeStillQueued", err)
	}
	if err := s.QueueAgentWake(item("q3", "agent-wake:b:1"), "agent-wake:b:"); err != nil {
		t.Fatalf("another row's wake: %v", err)
	}
	if err := s.DeleteFlushQueueItem("q1"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAgentWake(item("q4", "agent-wake:a:3"), "agent-wake:a:"); err != nil {
		t.Fatalf("after the first left the queue: %v", err)
	}
}

func TestPromoteStandingAgentIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	row, promoted, err := s.PromoteStandingAgent("t", "/home/t", 5)
	if err != nil || !promoted || row.Home != "/home/t" || row.NotifyOnTick {
		t.Fatalf("promote: %+v %v %v", row, promoted, err)
	}
	if _, promoted, err := s.PromoteStandingAgent("t", "/other", 6); err != nil || promoted {
		t.Fatalf("second promote: %v %v", promoted, err)
	}
	if demoted, err := s.DemoteStandingAgent("t"); err != nil || !demoted {
		t.Fatalf("demote: %v %v", demoted, err)
	}
	if _, found, err := s.GetStandingAgent("t"); err != nil || found {
		t.Fatalf("after demote: %v %v", found, err)
	}
}

// A watch's undelivered wake keeps its body until a later undelivered wake
// replaces it or the label is registered again; a delivery leaves it, a loop
// keeps none, and an output past the cap is refused.
func TestAgentWakeKeepsUndeliveredOutput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	watch, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "true",
		IntervalSeconds: 60, TimeoutSeconds: 120, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := s.RegisterAgentWake(AgentWake{ID: "l", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	read := func(id string) AgentWake {
		t.Helper()
		row, found, err := s.GetAgentWake(id)
		if err != nil || !found {
			t.Fatalf("read %s: %v %v", id, found, err)
		}
		return row
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "not delivered: gone", "first body"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteAgentWakeUndelivered(loop.ID, "not delivered: gone", "loop body"); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "first body" || row.LastOutcome != "not delivered: gone" || row.TimeoutSeconds != 120 {
		t.Fatalf("watch after an undelivered wake = %+v", row)
	}
	if row := read(loop.ID); row.LastOutput != "" {
		t.Fatalf("a loop kept output %q", row.LastOutput)
	}
	if _, err := s.NoteAgentWakeOutcome(watch.ID, "delivered"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "not delivered: joined", ""); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "first body" {
		t.Fatalf("a delivery or an empty output replaced the kept body: %q", row.LastOutput)
	}
	if _, err := s.ReleaseQueuedAgentWake(watch.ID, "not delivered", "second body", 50); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "second body" || row.NextAt != 50 {
		t.Fatalf("after a boot release = %+v", row)
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "x", strings.Repeat("x", MaxAgentWakeOutputBytes+1)); err == nil {
		t.Fatal("an output past the cap was written")
	}
	if again, err := s.RegisterAgentWake(AgentWake{ID: "w2", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "false",
		IntervalSeconds: 60, NextAt: 100}); err != nil || again.LastOutput != "" || again.TimeoutSeconds != 0 {
		t.Fatalf("re-register = %+v %v, want the kept output and the old timeout cleared", again, err)
	}
}

func TestSetStandingAgentNotifyOnTick(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if _, _, found, err := s.SetStandingAgentNotifyOnTick("t", true); err != nil || found {
		t.Fatalf("a thread that is not a standing agent: found=%v err=%v", found, err)
	}
	if _, _, err := s.PromoteStandingAgent("t", "/h", 1); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ on, changed bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		row, changed, found, err := s.SetStandingAgentNotifyOnTick("t", step.on)
		if err != nil || !found || changed != step.changed || row.NotifyOnTick != step.on {
			t.Fatalf("set %v: row=%+v changed=%v found=%v err=%v, want changed=%v", step.on, row, changed, found, err, step.changed)
		}
	}
}
