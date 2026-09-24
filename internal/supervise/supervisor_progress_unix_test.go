//go:build !windows

package supervise

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// A serve trial whose hello says it reports progress is judged by the stall
// rule (docs/specs/app-update.md): it fails when its progress stops, when it
// reports a failure, or at the ceiling, and never at the fixed budget a trial
// that reports nothing keeps.

// behaviorMigrate is a trial that reports one step of progress every 100ms
// for steps steps, then prepares.
func behaviorMigrate(steps int) string {
	n := strconv.Itoa(steps)
	return `i=1
while [ $i -le ` + n + ` ]; do
	report $i $i "Applying migration $i of ` + n + `"
	sleep 0.1
	i=$((i + 1))
done
note "migrated $VERSION"
` + behaviorPrepare
}

const (
	// behaviorHeartbeat is a trial that reports one step and then only a
	// heartbeat: alive, with no progress.
	behaviorHeartbeat = `printf 'trial' > "$DB"
report 1 1 "Applying migration 1 of 1"
note "migrating $VERSION"
trap 'note "stopped $VERSION"; exit 0' TERM INT
i=2
while :; do
	report 1 $i "Applying migration 1 of 1"
	sleep 0.1 &
	wait $! 2>/dev/null
	i=$((i + 1))
done`

	// behaviorEndless is a trial that reports progress forever.
	behaviorEndless = `printf 'trial' > "$DB"
note "migrating $VERSION"
trap 'note "stopped $VERSION"; exit 0' TERM INT
i=1
while :; do
	report $i $i "Applying migration $i of 1000"
	sleep 0.1 &
	wait $! 2>/dev/null
	i=$((i + 1))
done`

	// behaviorFailedStart is a boot that knows why it failed, says so and
	// keeps running, as serve does after a failed start. The trap is set
	// first because the stop can follow the frame at once.
	behaviorFailedStart = `printf 'trial' > "$DB"
trap 'note "stopped $VERSION"; exit 0' TERM INT
note "failed $VERSION"
printf '{"type":"failed","reason":"disk I/O error"}\n' >&4
serve_until_stopped`
)

// runUntilRolledBack runs a supervisor until the previous version booted
// again after the trial, and returns the settled record.
func (r *rig) runUntilRolledBack(config Config) *UpdateRecord {
	r.t.Helper()
	if err := r.runUntil(config, "hello 1.0.0", 2); err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	state := r.state()
	if state.Update == nil || state.Update.State != UpdateRolledBack {
		r.t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	if got := r.database(); got != "before agent-overflow.db" {
		r.t.Errorf("database = %q, want the pre-update contents restored", got)
	}
	if stops := countLines(r.lines("log"), "stopped 2.0.0"); stops != 1 {
		r.t.Errorf("the trial was stopped %d times, want once", stops)
	}
	return state.Update
}

func (r *rig) stageUpdateTo(behavior string) {
	r.t.Helper()
	r.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	r.stageReporting("2.0.0", behavior)
	r.adopt("1.0.0")
	writeDatabase(r.t, r.dataDir, "before")
}

// A trial that keeps progressing is not failed by the legacy budget or by
// the window, however long it takes in total, and the judge ends at prepared:
// the committed backend runs on past the window with no progress at all.
func TestATrialThatKeepsProgressingCommitsPastTheLegacyBudget(t *testing.T) {
	rig := newRig(t)
	rig.stageUpdateTo(behaviorMigrate(20))
	config := rig.config()
	config.TrialRule = StallRule{Window: time.Second, Ceiling: time.Minute}
	config.LegacyTrialBudget = 500 * time.Millisecond

	err := rig.runUntilCondition(config, func() {
		rig.waitForLog("committed 2.0.0", 1)
		// Longer than the window, with the committed backend reporting
		// nothing: a judge still armed would roll the commit back here.
		time.Sleep(1500 * time.Millisecond)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := rig.state()
	if state.Update == nil || state.Update.State != UpdateCommitted {
		t.Fatalf("state = %+v, want a committed record", state.Update)
	}
	if got := rig.database(); got != "trial" {
		t.Errorf("database = %q, want the trial's own work", got)
	}
	if hellos := countLines(rig.lines("log"), "hello 1.0.0"); hellos != 1 {
		t.Errorf("1.0.0 booted %d times, want once: nothing rolled back", hellos)
	}
}

// A heartbeat proves the trial alive, not progressing: the window from the
// last progress fails it, worded as every judge of a starting backend words
// a stall.
func TestATrialWithOnlyAHeartbeatIsRolledBackAsStalled(t *testing.T) {
	rig := newRig(t)
	rig.stageUpdateTo(behaviorHeartbeat)
	config := rig.config()
	config.TrialRule = StallRule{Window: 400 * time.Millisecond, Ceiling: time.Minute}
	config.LegacyTrialBudget = time.Minute

	record := rig.runUntilRolledBack(config)
	want := "the new version did not finish starting: no progress for "
	if !strings.HasPrefix(record.Reason, want) ||
		!strings.Contains(record.Reason, "in phase store.migrate (Applying migration 1 of 1)") {
		t.Errorf("reason = %q, want %q naming the phase and step", record.Reason, want)
	}
}

// A trial that says it reports progress and reports none fails at the
// window, not at the legacy budget.
func TestATrialThatReportsNoProgressIsRolledBackAtTheWindow(t *testing.T) {
	rig := newRig(t)
	rig.stageUpdateTo(behaviorHang)
	config := rig.config()
	config.TrialRule = StallRule{Window: 400 * time.Millisecond, Ceiling: time.Minute}
	config.LegacyTrialBudget = time.Minute

	record := rig.runUntilRolledBack(config)
	if want := "the new version reported no progress within 400ms of starting"; record.Reason != want {
		t.Errorf("reason = %q, want %q", record.Reason, want)
	}
}

// A trial that reports its failure is rolled back at once with its own
// reason, well inside a window it would otherwise have had.
func TestATrialThatReportsAFailureIsRolledBackAtOnce(t *testing.T) {
	rig := newRig(t)
	rig.stageUpdateTo(behaviorFailedStart)
	config := rig.config()
	config.TrialRule = StallRule{Window: time.Minute, Ceiling: time.Hour}
	config.LegacyTrialBudget = time.Minute

	record := rig.runUntilRolledBack(config)
	if record.Reason != "disk I/O error" {
		t.Errorf("reason = %q, want the trial's own", record.Reason)
	}
}

// Progress that never ends still ends at the ceiling, which names the last
// step.
func TestATrialThatNeverStopsProgressingIsRolledBackAtTheCeiling(t *testing.T) {
	rig := newRig(t)
	rig.stageUpdateTo(behaviorEndless)
	config := rig.config()
	config.TrialRule = StallRule{Window: time.Second, Ceiling: 800 * time.Millisecond}
	config.LegacyTrialBudget = time.Minute

	record := rig.runUntilRolledBack(config)
	want := "the new version did not finish starting within 800ms (last step: Applying migration "
	if !strings.HasPrefix(record.Reason, want) {
		t.Errorf("reason = %q, want %q", record.Reason, want)
	}
}

// A failed frame means something only from a trial. From a backend already
// serving it is logged and nothing moves.
func TestAFailedFrameOutsideATrialChangesNothing(t *testing.T) {
	rig := newRig(t)
	rig.stageReporting("1.0.0", behaviorFailedStart)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	err := rig.runUntilCondition(rig.config(), func() {
		rig.waitForLog("failed 1.0.0", 1)
		rig.waitForSupervisorLog("outside a trial")
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state := rig.state(); state.Update != nil || state.ActiveVersion != "1.0.0" {
		t.Fatalf("state = %+v, want 1.0.0 active with no record", state)
	}
	if hellos := countLines(rig.lines("log"), "hello 1.0.0"); hellos != 1 {
		t.Errorf("1.0.0 booted %d times, want once", hellos)
	}
	// Stopped once, by the test's own shutdown.
	if stops := countLines(rig.lines("log"), "stopped 1.0.0"); stops != 1 {
		t.Errorf("1.0.0 was stopped %d times, want once", stops)
	}
}

// waitForSupervisorLog blocks until a supervisor log line contains want.
func (r *rig) waitForSupervisorLog(want string) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range r.supervisorLog() {
			if strings.Contains(line, want) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("waited for a supervisor log line containing %q; saw %v", want, r.supervisorLog())
}
