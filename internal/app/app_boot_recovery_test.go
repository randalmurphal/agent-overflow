package app

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// priorInstancePhases are settlePriorInstance's boot phases in order.
var priorInstancePhases = []string{
	"app.recover_crashed_turns",
	"app.recover_codex_background_runtime",
	"app.recover_orphaned_background_tasks",
	"app.sweep_crashed_worktree_setups",
}

func newPriorInstanceTestApp(t *testing.T) (*App, string, *recordingBootProgress) {
	t.Helper()
	a, dbPath := newTestAppWithStorePath(t)
	a.triage = triage.NewRouter(a.store, func(eventchan.Channel, any) {})
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	return a, dbPath, progress
}

// TestSettlePriorInstanceReportsAFailedSweepOnItsPhase: a sweep that fails
// is reported on its own boot phase with its error, and the sweeps after
// it still run. A clean pass reports nothing.
func TestSettlePriorInstanceReportsAFailedSweepOnItsPhase(t *testing.T) {
	a, dbPath, progress := newPriorInstanceTestApp(t)
	thread := testThread("thread-crashed-turn")
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertTurn(store.Turn{TurnID: "crashed", ThreadID: thread.ID, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER keep_turns_open BEFORE UPDATE ON turns BEGIN SELECT RAISE(ABORT, 'injected turn write failure'); END`); err != nil {
		t.Fatal(err)
	}

	a.settlePriorInstance()
	begun, ended, _ := progress.snapshot()
	if strings.Join(begun, ",") != strings.Join(priorInstancePhases, ",") || strings.Join(ended, ",") != strings.Join(begun, ",") {
		t.Fatalf("phases begun %v and ended %v, want %v", begun, ended, priorInstancePhases)
	}
	failures := progress.failures()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "app.recover_crashed_turns: ") ||
		!strings.Contains(failures[0], "injected turn write failure") {
		t.Fatalf("failures = %q, want the crashed-turn sweep's", failures)
	}

	if _, err := raw.Exec(`DROP TRIGGER keep_turns_open`); err != nil {
		t.Fatal(err)
	}
	progress.failed = nil
	a.settlePriorInstance()
	if failures := progress.failures(); len(failures) != 0 {
		t.Fatalf("a clean pass reported %q", failures)
	}
	if turn, open, err := a.store.GetActiveTurn(thread.ID); err != nil || open {
		t.Fatalf("the clean pass left turn %+v (%v) open", turn, err)
	}
}

// TestSettlePriorInstanceReportsEverySweepThatFails: with the store gone
// every sweep fails, and each failure reaches the report on its own phase.
func TestSettlePriorInstanceReportsEverySweepThatFails(t *testing.T) {
	a, _, progress := newPriorInstanceTestApp(t)
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	a.settlePriorInstance()
	failures := progress.failures()
	if len(failures) != len(priorInstancePhases) {
		t.Fatalf("failures = %q, want one per sweep", failures)
	}
	for i, phase := range priorInstancePhases {
		if !strings.HasPrefix(failures[i], phase+": ") || len(failures[i]) == len(phase)+2 {
			t.Errorf("failure %d = %q, want %s with its error", i, failures[i], phase)
		}
	}
}
