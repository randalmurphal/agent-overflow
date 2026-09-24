package wsllauncher

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// failedTrial is the fixture's failure memory, if any.
func (f *updateFixture) failedTrial() (supervise.FailedTrial, bool) {
	f.t.Helper()
	failed, found, err := supervise.LoadFailedTrial(FailedTrialPath(f.sequence.RecordPath))
	if err != nil {
		f.t.Fatal(err)
	}
	return failed, found
}

func (f *updateFixture) wantFailedTrial(want supervise.FailedTrial) {
	f.t.Helper()
	if got, found := f.failedTrial(); !found || got != want {
		f.t.Fatalf("failure memory = %+v (found %v), want %+v", got, found, want)
	}
}

func (f *updateFixture) wantNoFailedTrial() {
	f.t.Helper()
	if got, found := f.failedTrial(); found {
		f.t.Fatalf("failure memory = %+v, want none", got)
	}
}

func (f *updateFixture) remember(failed supervise.FailedTrial) {
	f.t.Helper()
	if err := supervise.SaveFailedTrial(FailedTrialPath(f.sequence.RecordPath), failed); err != nil {
		f.t.Fatal(err)
	}
}

// trialFailsAt makes the next trial report phase, fail with reason, and
// report the restore that follows.
func (f *updateFixture) trialFailsAt(phase, reason string) {
	f.host.reports = map[string][]startupprogress.Progress{
		supervise.UpdateTrialRunCommand: {
			{Phase: "update.trial", Detail: "Starting v2.0.0"},
			{Phase: "store.migrate", Detail: phase},
			{Phase: "update.restore", Detail: "Restoring the database"},
		},
	}
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRolledBack, reason)
}

var migrationCallsOfOneTrial = []string{
	"snapshot@stable --id <id>",
	"trial-run@stable --id <id> --to 2.0.0 --attempt 1",
	"discard@stable --id <id>",
}

// TestFailureMemoryStopsTheSameMigrationUntilRetry: of two launches of the
// same build over the same schema version, only the first runs the trial;
// the second shows the stored reason and phase with Retry and runs nothing;
// Retry runs the second trial, whose commit forgets the failure.
func TestFailureMemoryStopsTheSameMigrationUntilRetry(t *testing.T) {
	f := newUpdateFixture(t)
	f.trialFailsAt("Applying migration 3 of 7 add_index", "migration v119 failed: disk I/O error")

	first := f.sequence.Migrate(t.Context(), testMigration)
	if first.Launch || first.Retry || first.Title != migrationFailedTitle {
		t.Fatalf("first launch = %+v, want the failure without Retry", first)
	}
	f.wantMigrationCalls(migrationCallsOfOneTrial...)
	f.wantFailedTrial(supervise.FailedTrial{
		Build: "2.0.0", Schema: 118, Reason: "migration v119 failed: disk I/O error",
		Phase: "Applying migration 3 of 7 add_index", AtMs: 1_000_000,
	})

	f.host.calls = nil
	second := f.sequence.Migrate(t.Context(), testMigration)
	want := MigrationEnd{
		Title: migrationFailedTitle,
		Detail: "The last attempt stopped at: Applying migration 3 of 7 add_index. It does not run again on its own, " +
			"so the data is as it was. Reason: migration v119 failed: disk I/O error. Details are in the launcher log.",
		Retry: true,
	}
	if second != want {
		t.Fatalf("second launch = %+v\nwant %+v", second, want)
	}
	if len(f.host.calls) != 0 {
		t.Fatalf("the second launch ran %q", f.host.calls)
	}
	f.wantNoRecord()

	retry := testMigration
	retry.Retry = true
	if end := f.sequence.Migrate(t.Context(), retry); end != (MigrationEnd{Launch: true}) {
		t.Fatalf("Retry = %+v, want a launch", end)
	}
	f.wantMigrationCalls(migrationCallsOfOneTrial...)
	f.wantNoFailedTrial()
	f.wantNoRecord()
}

// TestFailureMemoryAppliesToOneBuildOverOneSchema: a new build, or the same
// build over a database whose schema version changed, runs without Retry,
// and the memory that no longer applies is gone.
func TestFailureMemoryAppliesToOneBuildOverOneSchema(t *testing.T) {
	for _, c := range []struct {
		name string
		req  MigrationRequest
	}{
		{"a new build", MigrationRequest{Distro: "Ubuntu", Payload: testStable, Version: "2.0.1", Schema: 118}},
		{"a changed schema version", MigrationRequest{Distro: "Ubuntu", Payload: testStable, Version: "2.0.0", Schema: 119}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.remember(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "earlier", AtMs: 1})
			if end := f.sequence.Migrate(t.Context(), c.req); end != (MigrationEnd{Launch: true}) {
				t.Fatalf("end = %+v, want a launch", end)
			}
			if len(f.host.calls) != 3 || !strings.HasPrefix(f.host.calls[1], "trial-run@stable") {
				t.Fatalf("calls = %q, want the snapshot and one trial", f.host.calls)
			}
			f.wantNoFailedTrial()
		})
	}

	// The memory is gone even when the new build's migration does not
	// settle, so nothing later reads the old failure.
	f := newUpdateFixture(t)
	f.remember(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "earlier", AtMs: 1})
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeFailed, "the trial was interrupted")
	f.host.answer(supervise.UpdateRestoreCommand, supervise.UpdateOutcomeFailed, "restore failed")
	newBuild := testMigration
	newBuild.Version = "2.0.1"
	if end := f.sequence.Migrate(t.Context(), newBuild); end.Launch || end.Retry || !strings.Contains(end.Title, "could not be restored") {
		t.Fatalf("end = %+v, want the unrestored migration's page", end)
	}
	f.wantNoFailedTrial()
}

// TestARetryThatFailsIsRememberedInstead: the memory always holds the last
// failure, with its reason and phase.
func TestARetryThatFailsIsRememberedInstead(t *testing.T) {
	f := newUpdateFixture(t)
	f.remember(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "earlier", Phase: "earlier phase", AtMs: 1})
	f.trialFailsAt("Applying migration 5 of 7", "out of memory")
	retry := testMigration
	retry.Retry = true
	end := f.sequence.Migrate(t.Context(), retry)
	if end.Launch || end.Retry {
		t.Fatalf("end = %+v, want the failure without Retry", end)
	}
	f.wantMigrationCalls(migrationCallsOfOneTrial...)
	f.wantFailedTrial(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "out of memory", Phase: "Applying migration 5 of 7", AtMs: 1_000_000})
}

// TestEveryUncommittedMigrationIsRemembered: a snapshot that failed or was
// refused, and a trial stopped by another backend's writes, are failures of
// the migration the gate would otherwise repeat at every launch. A failure
// that reported nothing is remembered without a phase, and its page says
// only that the last attempt failed.
func TestEveryUncommittedMigrationIsRemembered(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(*fakeUpdateHost)
		reason string
		phase  string
	}{
		{"the snapshot did not fit", func(h *fakeUpdateHost) {
			h.reports = map[string][]startupprogress.Progress{
				supervise.UpdateSnapshotCommand: {{Phase: "update.snapshot", Detail: "Backing up the database"}},
			}
			h.answer(supervise.UpdateSnapshotCommand, supervise.UpdateOutcomeRefused, "not enough free space")
		}, "the database could not be backed up: not enough free space", "Backing up the database"},
		{"another backend used the database", func(h *fakeUpdateHost) {
			h.reports = map[string][]startupprogress.Progress{supervise.UpdateTrialRunCommand: nil}
			h.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeChanged, "the database changed")
		}, "the database changed", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			c.setup(f.host)
			f.sequence.Migrate(t.Context(), testMigration)
			f.wantFailedTrial(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: c.reason, Phase: c.phase, AtMs: 1_000_000})
			end := f.sequence.Migrate(t.Context(), testMigration)
			wantWhere := "The last attempt failed."
			if c.phase != "" {
				wantWhere = "The last attempt stopped at: " + c.phase + "."
			}
			if !end.Retry || !strings.HasPrefix(end.Detail, wantWhere+" It does not run again on its own") ||
				!strings.Contains(end.Detail, "Reason: "+c.reason+".") {
				t.Fatalf("second launch = %+v", end)
			}
		})
	}
}

// TestAnUnreadableFailureMemoryRunsTheMigration: a memory that cannot be
// read stops nothing; the migration runs, and its outcome replaces it.
func TestAnUnreadableFailureMemoryRunsTheMigration(t *testing.T) {
	f := newUpdateFixture(t)
	var logged []string
	f.sequence.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if err := os.MkdirAll(strings.TrimSuffix(f.sequence.RecordPath, "/app-update-prod.ubuntu.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FailedTrialPath(f.sequence.RecordPath), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.trialFailsAt("Applying migration 1 of 1", "failed")
	if end := f.sequence.Migrate(t.Context(), testMigration); end.Retry || end.Launch {
		t.Fatalf("end = %+v, want the trial's own failure", end)
	}
	f.wantMigrationCalls(migrationCallsOfOneTrial...)
	if joined := strings.Join(logged, "\n"); !strings.Contains(joined, "read the failed trial") {
		t.Fatalf("log = %q", joined)
	}
	f.wantFailedTrial(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "failed", Phase: "Applying migration 1 of 1", AtMs: 1_000_000})
}

// TestAnUpdateTrialIsRememberedForItsTarget: an update whose trial rolls
// back is remembered for its target over the schema version its snapshot
// reported, which the record keeps for later attempts; the migration gate
// then stops that build over that schema. An update that commits forgets
// the memory.
func TestAnUpdateTrialIsRememberedForItsTarget(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.schema = 118
	f.host.reports = map[string][]startupprogress.Progress{
		supervise.UpdateTrialRunCommand: {{Phase: "store.migrate", Detail: "Applying migration 2 of 4"}},
	}
	f.host.fail(supervise.UpdateTrialRunCommand, fmt.Errorf("the command stopped reporting"))
	f.host.answer(supervise.UpdateRestoreCommand, supervise.UpdateOutcomeFailed, "restore failed")
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.Settled() {
		t.Fatalf("Apply = %+v, %v; want the update left pending", end, err)
	}
	if record := f.record(); record.Update.FromSchema != 118 {
		t.Fatalf("the record's schema version = %d, want the snapshot's 118", record.Update.FromSchema)
	}
	f.wantNoFailedTrial()

	// The next attempt takes no snapshot; the record names the schema.
	f.host.schema = 0
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRolledBack, "migration v119 failed")
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.State != supervise.UpdateRolledBack {
		t.Fatalf("Apply = %+v, %v; want rolled back", end, err)
	}
	f.wantFailedTrial(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "migration v119 failed", Phase: "Applying migration 2 of 4", AtMs: 1_000_000})

	f.host.calls = nil
	if end := f.sequence.Migrate(t.Context(), testMigration); !end.Retry {
		t.Fatalf("the gate for the failed target = %+v, want Retry", end)
	}
	if len(f.host.calls) != 0 {
		t.Fatalf("the gate ran %q", f.host.calls)
	}

	f = newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.remember(supervise.FailedTrial{Build: "2.0.0", Schema: 118, Reason: "earlier", AtMs: 1})
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("Apply = %+v, %v; want committed", end, err)
	}
	f.wantNoFailedTrial()
}

// TestAFailedTrialWithoutASchemaIsNotRemembered: a failure that cannot name
// the schema version it started from could never match, so it is logged
// instead.
func TestAFailedTrialWithoutASchemaIsNotRemembered(t *testing.T) {
	f := newUpdateFixture(t)
	var logged []string
	f.sequence.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.save(supervise.UpdatePending, 0, false)
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRolledBack, "failed")
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.State != supervise.UpdateRolledBack {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	f.wantNoFailedTrial()
	if joined := strings.Join(logged, "\n"); !strings.Contains(joined, "the failed trial of 2.0.0 is not remembered") {
		t.Fatalf("log = %q", joined)
	}
}

// TestReconcileRemembersAnUpdateInterruptedAtEveryAttempt: the recovery
// that rolls back an update whose trial was interrupted at every attempt
// remembers it; one interrupted before its trial, or missing its launcher,
// is not a failed trial.
func TestReconcileRemembersAnUpdateInterruptedAtEveryAttempt(t *testing.T) {
	for _, c := range []struct {
		name           string
		attempts       int
		removeLauncher bool
		remembered     bool
	}{
		{"interrupted at every attempt", supervise.TrialAttemptLimit, false, true},
		{"interrupted before its trial", 0, false, false},
		{"its launcher is missing", 1, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			record := f.save(supervise.UpdatePending, c.attempts, false)
			update := *record.Update
			update.FromSchema = 118
			record.Update = &update
			if err := supervise.SaveLauncherRecord(f.sequence.RecordPath, record); err != nil {
				t.Fatal(err)
			}
			if c.removeLauncher {
				if err := os.Remove(f.launcher); err != nil {
					t.Fatal(err)
				}
			}
			if decision, err := f.sequence.Reconcile(t.Context(), "old"); err != nil || decision.Action != ReconcileLaunch {
				t.Fatalf("Reconcile = %+v, %v", decision, err)
			}
			if !c.remembered {
				f.wantNoFailedTrial()
				return
			}
			f.wantFailedTrial(supervise.FailedTrial{
				Build: "2.0.0", Schema: 118, Reason: "the trial was interrupted 2 times without finishing", AtMs: 1_000_000,
			})
		})
	}
}
