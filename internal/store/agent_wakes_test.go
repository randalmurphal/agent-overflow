package store

import (
	"errors"
	"testing"
)

// v142 creates both tables with the spec's constraints, and both cascade from
// their thread.
func TestStandingAgentsMigration(t *testing.T) {
	db := migrateThrough(t, standingAgentsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
		VALUES ('t', 'p', 'T', 'claude', '/p', '', 1, 1, 0)`)
	if err := applyMigration(db, migrationByVersion(t, standingAgentsMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", standingAgentsMigrationVersion, err)
	}
	mustExec(t, db, `INSERT INTO standing_agents (thread_id, home, promoted_at) VALUES ('t', '/h', 1)`)
	mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at)
		VALUES ('w', 't', 'loop', 'l', 'armed', 1)`)
	for _, bad := range []string{
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 't', 'cron', 'l2', 'armed', 1)`,
		`INSERT INTO agent_wakes (id, thread_id, kind, label, state, created_at) VALUES ('x', 't', 'loop', 'l2', 'paused', 1)`,
		`INSERT INTO standing_agents (thread_id, home, notify_on_tick, promoted_at) VALUES ('t2', '/h', 2, 1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	var agents, wakes int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM standing_agents), (SELECT count(*) FROM agent_wakes)`).
		Scan(&agents, &wakes); err != nil {
		t.Fatal(err)
	}
	if agents != 0 || wakes != 0 {
		t.Fatalf("after the thread delete: %d agent rows, %d wakes, want both cascaded", agents, wakes)
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
		if _, err := s.RegisterAgentWake(AgentWake{ID: "x" + string(rune('a'+i)), ThreadID: "t", Kind: AgentWakeLoop, Label: "l" + string(rune('a'+i)), IntervalSeconds: 60, NextAt: 1}); err != nil {
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
	if finished, err := s.FinishAgentWake(row.ID, "early", 0); err != nil || finished {
		t.Fatalf("finish before claim: %v %v", finished, err)
	}
	if claimed, err := s.ClaimAgentWake(row.ID, 100, 160, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if claimed, err := s.ClaimAgentWake(row.ID, 100, 160, 0); err != nil || claimed {
		t.Fatalf("second claim: %v %v", claimed, err)
	}
	if finished, err := s.FinishAgentWake(row.ID, "delivered", 0); err != nil || !finished {
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
