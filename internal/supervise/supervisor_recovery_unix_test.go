//go:build !windows

package supervise

import (
	"strings"
	"testing"
	"time"
)

// The snapshot is taken where the FIRST TRIAL IS SPAWNED, not where the update
// was accepted.
//
// Accepting an update writes the pending record durably and then waits out a
// short grace so the asking child can flush its answer. A crash, a shutdown or
// a kill inside that window used to leave a pending record with no snapshot
// beside it, and the next boot trialled the new version against a database
// nothing could put back. This is that boot: a pending record on disk, no
// snapshot, and a trial that has never run.
func TestAPendingUpdateWithNoSnapshotTakesOneBeforeItsFirstTrial(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorServe)
	rig.stage("2.0.0", behaviorCrash)
	writeDatabase(t, rig.dataDir, "before")
	saveRecord(t, rig, &UpdateRecord{ID: "u1", State: UpdatePending, From: "1.0.0", To: "2.0.0"})
	if !absent(t, rig.layout.SnapshotDir()) {
		t.Fatal("this test is meaningless with a snapshot already on disk")
	}

	if err := rig.runUntil(rig.config(), "hello 1.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The trial ran, so the snapshot must have been taken first.
	if crashes := countLines(rig.lines("log"), "crashing 2.0.0"); crashes != 1 {
		t.Fatalf("the trial started %d times, want once", crashes)
	}
	if !logMentions(rig, "snapshotting the database") {
		t.Errorf("no snapshot was logged: %v", rig.supervisorLog())
	}
	state := rig.state()
	if state.Update.State != UpdateRolledBack {
		t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	// The point of all of it: the trial's write is gone and the marker with it.
	if got := rig.database(); got != "before agent-overflow.db" {
		t.Errorf("database = %q, want the pre-update contents restored", got)
	}
	if !absent(t, rig.layout.MarkerPath()) {
		t.Error("a restore marker survived the rollback")
	}
	if !absent(t, rig.layout.SnapshotDir()) {
		t.Error("the snapshot survived the rollback that consumed it")
	}
}

// The other half of the same fix: a pending update that HAS already trialled
// and has no snapshot cannot be repaired by taking one now. The trial has
// written to the database, so a snapshot taken here would preserve exactly the
// work a rollback exists to undo. The supervisor starts nothing and says which
// command chooses a version by hand.
func TestAMidTrialPendingUpdateWithNoSnapshotStartsNothing(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorServe)
	rig.stage("2.0.0", behaviorPrepare)
	writeDatabase(t, rig.dataDir, "mid-trial")
	saveRecord(t, rig, &UpdateRecord{
		ID: "u1", State: UpdatePending, From: "1.0.0", To: "2.0.0", Attempts: 1,
	})

	stop, done := rig.run(rig.config())
	defer stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run trialled an update whose snapshot is gone")
		}
		for _, want := range []string{"snapshot", "1.0.0", "service update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not name %q", err, want)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return on a mid-trial update with no snapshot")
	}

	if lines := rig.lines("log"); len(lines) != 0 {
		t.Errorf("a version was started anyway: %v", lines)
	}
	if got := rig.database(); got != "mid-trial agent-overflow.db" {
		t.Errorf("database = %q, want it untouched", got)
	}
}

// A settled record stays in the file until the NEXT update collapses it, which
// can be months of nightly restarts. Its outcome is news exactly once: the
// first backend that boots after the update carries it to whoever asked, marks
// it reported, and every later boot activates with nothing to announce.
//
// A rollback is the case that proves the frame needs two versions in it. The
// version answering is 1.0.0 and the version that failed is 2.0.0, and only
// the record knows both.
func TestASettledOutcomeReachesTheBackendExactlyOnce(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorServe)
	rig.stage("2.0.0", behaviorServe)
	writeDatabase(t, rig.dataDir, "before")
	saveRecord(t, rig, &UpdateRecord{
		ID: "u1", State: UpdateRolledBack, From: "1.0.0", To: "2.0.0",
		Reason: "the trial did not report prepared",
	})

	if err := rig.runUntilCondition(rig.config(), func() {
		rig.waitForLog("hello 1.0.0", 1)
		waitForReported(t, rig)
	}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	activate := rig.lines("activate")
	if len(activate) != 1 {
		t.Fatalf("activate frames = %v, want one", activate)
	}
	for _, want := range []string{
		`"updateId":"u1"`,
		`"outcome":"` + string(UpdateRolledBack) + `"`,
		`"targetVersion":"2.0.0"`,
		`"reason":"the trial did not report prepared"`,
	} {
		if !strings.Contains(activate[0], want) {
			t.Errorf("activate frame %s does not carry %s", activate[0], want)
		}
	}

	// Second boot, same record, nothing new to say.
	if err := rig.runUntil(rig.config(), "hello 1.0.0", 2); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	activate = rig.lines("activate")
	if len(activate) != 2 {
		t.Fatalf("activate frames = %v, want two", activate)
	}
	for _, unwanted := range []string{"outcome", "targetVersion", "updateId", "reason"} {
		if strings.Contains(activate[1], unwanted) {
			t.Errorf("the second activate frame %s re-announces %s", activate[1], unwanted)
		}
	}
	if state := rig.state(); state.Update.State != UpdateRolledBack || !state.Update.Reported {
		t.Errorf("record = %+v, want it settled and reported", state.Update)
	}
}

// saveRecord writes a state file whose active version is the record's From,
// which is the only shape Validate accepts.
func saveRecord(t *testing.T, r *rig, record *UpdateRecord) {
	t.Helper()
	state := State{Schema: StateSchema, ActiveVersion: record.From, Update: record}
	if err := SaveState(r.layout, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

// waitForReported blocks until the supervisor has recorded that the outcome
// reached a backend. The child logs its hello before the supervisor has read
// the frame, so the log alone does not say the write happened.
func waitForReported(t *testing.T, r *rig) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, found, err := LoadState(r.layout)
		if err == nil && found && state.Update != nil && state.Update.Reported {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the outcome was never marked reported")
}

func logMentions(r *rig, want string) bool {
	for _, line := range r.supervisorLog() {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
