//go:build !windows

package supervise

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func (r *desktopRig) phases(t *testing.T) phaseLines {
	t.Helper()
	r.mu.Lock()
	logged := append([]string(nil), r.logged...)
	r.mu.Unlock()
	return parsePhaseLines(t, logged)
}

// servedUpdate stages 1.0.0, which asks for 2.0.0, and 2.0.0 with behavior,
// over a database of one file so each file phase runs once.
func (r *rig) servedUpdate(behavior string) {
	r.t.Helper()
	r.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	r.stageReporting("2.0.0", behavior)
	r.adopt("1.0.0")
	writeFile(r.t, filepath.Join(r.dataDir, DatabaseFiles()[0]), "before")
}

// A supervised serve update runs every phase in the supervisor, which logs
// each once, from accepting the request to the commit, and one summary.
func TestAServeUpdateLogsEachPhaseOnce(t *testing.T) {
	t.Parallel()
	rig := newRig(t)
	rig.servedUpdate(behaviorPrepare)
	if err := rig.runUntil(rig.config(), "committed 2.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state := rig.state(); state.Update == nil || state.Update.State != UpdateCommitted {
		t.Fatalf("state = %+v, want a committed record", state.Update)
	}
	p := parsePhaseLines(t, rig.supervisorLog())
	phases := []string{"accept", "grace", "stop-previous",
		"snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync",
		"record-attempt", "trial.start", "trial.boot", "settle", "discard", "prune"}
	p.wantOnce(t, phases...)
	p.wantNone(t, "trial.stop", "restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear", "restore.record")
	if len(p.failed) != 0 || len(p.skipped) != 0 {
		t.Fatalf("failed %v, skipped %v; want every phase run and passed", p.failed, p.skipped)
	}
	p.wantSummary(t, phases...)
	rig.wantOutcome(t, "committed")
}

// wantOutcome fails unless the supervisor's one summary names outcome.
func (r *rig) wantOutcome(t *testing.T, outcome string) {
	t.Helper()
	for _, line := range r.supervisorLog() {
		if summaryLineRE.MatchString(line) {
			if !strings.Contains(line+" ", " outcome="+outcome+" ") {
				t.Fatalf("summary %q, want outcome=%s", line, outcome)
			}
			return
		}
	}
	t.Fatal("no summary")
}

// A serve trial that fails is stopped and rolled back by the supervisor,
// which logs the failed boot, the stop, the restore and the settlement, and
// ends the update with one summary before the previous version starts.
func TestAServeRollbackLogsEachPhaseOnce(t *testing.T) {
	t.Parallel()
	rig := newRig(t)
	rig.servedUpdate(behaviorFailedStart)
	if err := rig.runUntil(rig.config(), "hello 1.0.0", 2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state := rig.state(); state.Update == nil || state.Update.State != UpdateRolledBack {
		t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	p := parsePhaseLines(t, rig.supervisorLog())
	p.wantOnce(t, "accept", "grace", "stop-previous",
		"snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync",
		"record-attempt", "trial.start", "trial.boot", "trial.stop",
		"restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear", "settle", "discard")
	p.wantNone(t, "prune")
	if len(p.failed) != 1 || p.failed["trial.boot"] != 1 {
		t.Fatalf("failed %v, want the trial's boot", p.failed)
	}
	p.wantSummary(t, "accept", "grace", "stop-previous",
		"snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync",
		"record-attempt", "trial.start", "trial.boot(failed)", "trial.stop",
		"restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.record:skipped", "restore.clear", "settle", "discard")
	rig.wantOutcome(t, "rolled-back")
}

// The helper runs every step in its own process, so every phase's line is
// in the update log: the commands' steps, the trial's, the run's own, and
// one summary.
func TestDesktopUpdateLogsEachPhaseOnce(t *testing.T) {
	t.Parallel()
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	helper := r.update("1.0.0", record.StagedPath, record.InstallPath, trialWritesAndPrepares)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	p := r.phases(t)
	p.wantOnce(t,
		"snapshot.lock", "snapshot.prepare", "snapshot.schema", "snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync",
		"record-attempt",
		"trial.lock", "trial.prepare", "trial.check", "trial.start", "trial.boot", "trial.stop",
		"settle", "discard", "publish")
	p.wantNone(t, "restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear", "remove-staged")
	if len(p.failed) != 0 || len(p.skipped) != 0 {
		t.Fatalf("failed %v, skipped %v; want every phase run and passed", p.failed, p.skipped)
	}
	p.wantSummary(t, "snapshot", "record-attempt", "trial.1", "settle", "publish")
}

// A trial that fails is restored inside the trial step, which logs the
// restore's copy and sync; the stop it did not need is not logged.
func TestDesktopUpdateRollbackLogsTheRestore(t *testing.T) {
	t.Parallel()
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	helper := r.update("2.0.0", record.StagedPath, record.InstallPath, trialWritesAndCrashes)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateRolledBack {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	p := r.phases(t)
	p.wantOnce(t, "trial.start", "trial.boot", "restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear", "settle", "discard", "remove-staged")
	p.wantNone(t, "trial.stop", "publish")
	if p.failed["trial.boot"] != 1 {
		t.Fatalf("failed phases = %v, want trial.boot", p.failed)
	}
	p.wantSummary(t, "snapshot", "record-attempt", "trial.1(failed)", "settle", "discard", "remove-staged")
}

// A recovery that rolls back an update interrupted at every attempt runs
// the restore step, and ends it with a summary too.
func TestDesktopRecoveryLogsTheRestoreStep(t *testing.T) {
	t.Parallel()
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	if _, err := TakeSnapshot(r.layout, r.dataDir, time.Now(), SnapshotOptions{UpdateID: desktopTestID}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, r.db, "half-migrated")
	r.setRecord(t, func(u *UpdateRecord) { u.Attempts = TrialAttemptLimit })
	app := r.update("1.0.0", record.InstallPath, record.InstallPath, "")

	if decision, err := app.Reconcile(context.Background()); err != nil || decision.Action != DesktopLaunch {
		t.Fatalf("Reconcile = %+v, %v", decision, err)
	}
	p := r.phases(t)
	p.wantOnce(t, "restore.lock", "restore.prepare", "restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear", "settle", "discard", "remove-staged")
	p.wantSummary(t, "restore", "settle", "discard", "remove-staged")
}

// A second attempt reuses the first one's backup: the snapshot says it was
// skipped and logs no duration.
func TestDesktopUpdateRetryLogsTheSnapshotSkipped(t *testing.T) {
	t.Parallel()
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	if _, err := TakeSnapshot(r.layout, r.dataDir, time.Now(), SnapshotOptions{UpdateID: desktopTestID}); err != nil {
		t.Fatal(err)
	}
	r.setRecord(t, func(u *UpdateRecord) { u.Attempts = 1 })
	helper := r.update("1.0.0", record.StagedPath, record.InstallPath, trialWritesAndPrepares)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	p := r.phases(t)
	if p.skipped["snapshot"] != 1 {
		t.Fatalf("skipped = %v, want the snapshot once", p.skipped)
	}
	p.wantNone(t, "snapshot.lock", "snapshot.clear", "snapshot.copy", "snapshot.sync")
	p.wantOnce(t, "trial.start", "trial.boot", "trial.stop", "publish")
	p.wantSummary(t, "snapshot:skipped", "record-attempt", "trial.2", "settle", "publish")
}

// The old app's handoff is the update's first phase, ended before its
// shutdown starts; one that fails says so.
func TestDesktopHandOffLogsItsDuration(t *testing.T) {
	t.Parallel()
	h := newHandoffRig(t)
	if err := h.handoff().HandOff(context.Background(), h.downloaded, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	p := parsePhaseLines(t, h.logged)
	p.wantOnce(t, "handoff")

	failing := newHandoffRig(t)
	failing.startErr = errors.New("exec format error")
	if err := failing.handoff().HandOff(context.Background(), failing.downloaded, "2.0.0"); err == nil {
		t.Fatal("HandOff succeeded without its helper")
	}
	if p := parsePhaseLines(t, failing.logged); p.timed["handoff"] != 1 || p.failed["handoff"] != 1 {
		t.Fatalf("timed %v, failed %v; want the handoff logged once as failed", p.timed, p.failed)
	}
}

// Each WSL command logs its own phases where it runs; a command without a
// schema reader says the read was skipped.
func TestUpdateCommandsLogTheirPhases(t *testing.T) {
	t.Parallel()
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	var mu sync.Mutex
	var logged []string
	command := r.command(desktopTestID)
	command.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	if result := command.Space(nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("Space = %+v", result)
	}
	if result := command.Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("Snapshot = %+v", result)
	}
	if result := command.Restore(context.Background(), "a test"); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("Restore = %+v", result)
	}
	if result := command.Discard(context.Background()); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("Discard = %+v", result)
	}
	mu.Lock()
	p := parsePhaseLines(t, logged)
	mu.Unlock()
	p.wantOnce(t, "space",
		"snapshot.lock", "snapshot.prepare", "snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync",
		"restore.lock", "restore.prepare", "restore.mark", "restore.remove", "restore.copy", "restore.sync", "restore.clear",
		"discard")
	p.wantNone(t, "snapshot.schema")
	if p.skipped["snapshot.schema"] != 1 {
		t.Fatalf("skipped = %v, want the schema read", p.skipped)
	}
}
