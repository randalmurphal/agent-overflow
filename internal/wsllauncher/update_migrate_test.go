package wsllauncher

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

var updateIDArg = regexp.MustCompile(`--id [0-9a-f]{16}`)

// testMigration is a launch of 2.0.0 whose backend refused to migrate a
// database at schema v118.
var testMigration = MigrationRequest{Distro: "Ubuntu", Payload: testStable, Version: "2.0.0", Schema: 118}

// migrationCalls is the host's calls with the migration's random id
// written as <id>.
func (f *updateFixture) migrationCalls() []string {
	calls := make([]string, len(f.host.calls))
	for i, call := range f.host.calls {
		calls[i] = updateIDArg.ReplaceAllString(call, "--id <id>")
	}
	return calls
}

func (f *updateFixture) wantMigrationCalls(want ...string) {
	f.t.Helper()
	if got := f.migrationCalls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func (f *updateFixture) wantNoRecord() {
	f.t.Helper()
	if _, err := os.Stat(f.sequence.RecordPath); !os.IsNotExist(err) {
		f.t.Fatalf("the record survived: %v", err)
	}
}

// saveMigration writes a migration record m1 of version 2.0.0 in state
// with attempts trial starts.
func (f *updateFixture) saveMigration(state supervise.UpdateState, attempts int) supervise.LauncherRecord {
	f.t.Helper()
	base, err := supervise.Adopt("2.0.0")
	if err != nil {
		f.t.Fatal(err)
	}
	next, err := base.BeginMigration("m1", time.UnixMilli(1))
	if err != nil {
		f.t.Fatal(err)
	}
	for range attempts {
		if next, err = next.Retry(); err != nil {
			f.t.Fatal(err)
		}
	}
	if state != supervise.UpdatePending {
		if next, err = next.Settle(state, "earlier reason", time.UnixMilli(2)); err != nil {
			f.t.Fatal(err)
		}
	}
	record := supervise.LauncherRecord{State: next, Distro: "Ubuntu", StablePayload: testStable}
	if err := supervise.SaveLauncherRecord(f.sequence.RecordPath, record); err != nil {
		f.t.Fatal(err)
	}
	return record
}

// TestMigrateCommitsThroughTheStablePayload: the payload that refused runs
// the snapshot and the trial, nothing is published, and the record is gone
// once the migration committed.
func TestMigrateCommitsThroughTheStablePayload(t *testing.T) {
	f := newUpdateFixture(t)
	f.host.free, f.host.freeKnown = 5<<30, true
	var progress []string
	f.sequence.Progress = func(p startupprogress.Progress) { progress = append(progress, p.Detail) }
	end := f.sequence.Migrate(t.Context(), testMigration)
	if end != (supervise.MigrationEnd{Launch: true}) {
		t.Fatalf("end = %+v", end)
	}
	f.wantMigrationCalls(
		"snapshot@stable --id <id> --host-free 5368709120",
		"trial-run@stable --id <id> --to 2.0.0 --attempt 1",
		"discard@stable --id <id>",
	)
	if f.host.states["snapshot"] != "pending/0" || f.host.states["trial-run"] != "pending/1" || f.host.states["discard"] != "committed/1" {
		t.Fatalf("durable states = %v", f.host.states)
	}
	f.wantNoRecord()
	if len(progress) == 0 || progress[0] != "Preparing to upgrade the database" {
		t.Fatalf("progress = %q", progress)
	}
}

// TestMigrateShowsWhyItDidNotCommit: a migration that settles without
// committing starts nothing, says whether the backup was restored, and
// leaves no record.
func TestMigrateShowsWhyItDidNotCommit(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(*fakeUpdateHost)
		calls  []string
		detail string
	}{
		{"the trial rolled back", func(h *fakeUpdateHost) {
			h.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRolledBack, "migration v119 failed: disk I/O error.")
		}, []string{
			"snapshot@stable --id <id>",
			"trial-run@stable --id <id> --to 2.0.0 --attempt 1",
			"discard@stable --id <id>",
		}, "The backup was restored, so the data is as it was. Reason: migration v119 failed: disk I/O error. Details are in the launcher log."},
		{"the snapshot did not fit", func(h *fakeUpdateHost) {
			h.answer(supervise.UpdateSnapshotCommand, supervise.UpdateOutcomeRefused, "not enough free space")
		}, []string{
			"snapshot@stable --id <id>",
			"discard@stable --id <id>",
		}, "The upgrade did not run, so the data is as it was. Reason: the database could not be backed up: not enough free space. Details are in the launcher log."},
		{"another backend used the database", func(h *fakeUpdateHost) {
			h.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeChanged, "the database changed")
		}, []string{
			"snapshot@stable --id <id>",
			"trial-run@stable --id <id> --to 2.0.0 --attempt 1",
			"discard@stable --id <id>",
		}, "The upgrade did not run, so the data is as it was. Reason: the database changed. Details are in the launcher log."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			c.setup(f.host)
			end := f.sequence.Migrate(t.Context(), testMigration)
			if end.Launch || end.Title != supervise.MigrationFailedTitle || end.Detail != c.detail {
				t.Fatalf("end = %+v, want the page %q", end, c.detail)
			}
			f.wantMigrationCalls(c.calls...)
			f.wantNoRecord()
		})
	}
}

// TestMigrateLeavesAnUnrestoredMigrationForTheNextLaunch: a trial that ended
// undecided and a restore that failed start nothing and keep the record
// pending; the next launch resumes the migration in the launcher.
func TestMigrateLeavesAnUnrestoredMigrationForTheNextLaunch(t *testing.T) {
	f := newUpdateFixture(t)
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeFailed, "the trial was interrupted")
	f.host.answer(supervise.UpdateRestoreCommand, supervise.UpdateOutcomeFailed, "restore failed")
	end := f.sequence.Migrate(t.Context(), testMigration)
	if end.Launch || !strings.Contains(end.Title, "could not be restored") {
		t.Fatalf("end = %+v", end)
	}
	f.wantMigrationCalls(
		"snapshot@stable --id <id>",
		"trial-run@stable --id <id> --to 2.0.0 --attempt 1",
		"restore@stable --id <id> --reason the trial was interrupted",
	)
	record := f.record()
	if !record.Migration() || record.Update.State != supervise.UpdatePending || record.Update.Attempts != 1 {
		t.Fatalf("record = %+v", record.Update)
	}

	f.host.calls = nil
	decision, err := f.sequence.Reconcile(t.Context(), "any")
	if err != nil || decision.Action != ReconcileResume || decision.Record.Update.ID != record.Update.ID {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	if len(f.host.calls) != 0 {
		t.Fatalf("Reconcile ran %q", f.host.calls)
	}
	if end := f.sequence.ResumeMigration(t.Context(), decision.Record); !end.Launch {
		t.Fatalf("resumed end = %+v", end)
	}
	f.wantMigrationCalls(
		"trial-run@stable --id <id> --to 2.0.0 --attempt 2",
		"discard@stable --id <id>",
	)
	f.wantNoRecord()
}

// TestMigrateNeedsNoUpdateInFlight: a pending update's record is never
// replaced; a settled one is.
func TestMigrateNeedsNoUpdateInFlight(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 1, false)
	end := f.sequence.Migrate(t.Context(), testMigration)
	if end.Launch || !strings.Contains(end.Title, "could not start the database upgrade") {
		t.Fatalf("end = %+v", end)
	}
	if len(f.host.calls) != 0 {
		t.Fatalf("calls = %q", f.host.calls)
	}
	f.wantRecord(supervise.UpdatePending, 1, "", false)

	f = newUpdateFixture(t)
	f.save(supervise.UpdateCommitted, 1, true)
	if end := f.sequence.Migrate(t.Context(), testMigration); !end.Launch {
		t.Fatalf("end after a settled update = %+v", end)
	}
	f.wantNoRecord()
}

// TestReconcileMigration is the recovery table for a migration record.
func TestReconcileMigration(t *testing.T) {
	for _, c := range []struct {
		name     string
		state    supervise.UpdateState
		attempts int
		action   ReconcileAction
		calls    []string
		kept     bool
	}{
		{"settled, left behind", supervise.UpdateCommitted, 1, ReconcileLaunch, []string{"discard@stable --id m1"}, false},
		{"rolled back, left behind", supervise.UpdateRolledBack, 1, ReconcileLaunch, []string{"discard@stable --id m1"}, false},
		{"pending, never trialled", supervise.UpdatePending, 0, ReconcileLaunch, []string{"discard@stable --id m1"}, false},
		{"pending, retryable", supervise.UpdatePending, 1, ReconcileResume, nil, true},
		{"pending, at the limit", supervise.UpdatePending, supervise.TrialAttemptLimit, ReconcileResume, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.saveMigration(c.state, c.attempts)
			decision, err := f.sequence.Reconcile(t.Context(), "any")
			if err != nil || decision.Action != c.action {
				t.Fatalf("decision = %+v, %v; want action %d", decision, err, c.action)
			}
			if got := decision.BackendArgs("2.0.0"); got != nil {
				t.Fatalf("BackendArgs = %q, want none", got)
			}
			f.wantCalls(c.calls...)
			if c.kept {
				f.wantRecord(c.state, c.attempts, "", false)
			} else {
				f.wantNoRecord()
			}
		})
	}
}

// TestResumeMigrationAtTheLimitRestores: a migration interrupted at every
// attempt is rolled back through the stable payload and says so.
func TestResumeMigrationAtTheLimitRestores(t *testing.T) {
	f := newUpdateFixture(t)
	record := f.saveMigration(supervise.UpdatePending, supervise.TrialAttemptLimit)
	end := f.sequence.ResumeMigration(t.Context(), record)
	if end.Launch || end.Title != supervise.MigrationFailedTitle ||
		end.Detail != "The backup was restored, so the data is as it was. Reason: the trial was interrupted 2 times without finishing. Details are in the launcher log." {
		t.Fatalf("end = %+v", end)
	}
	f.wantCalls(
		"restore@stable --id m1 --reason the trial was interrupted 2 times without finishing",
		"discard@stable --id m1",
	)
	f.wantNoRecord()
}

// TestMigrationRecordTellsTheBackendNothing: a migration that did not
// commit is shown by the launcher, never passed to the backend as a failed
// update.
func TestMigrationRecordTellsTheBackendNothing(t *testing.T) {
	f := newUpdateFixture(t)
	record := f.saveMigration(supervise.UpdateRolledBack, 1)
	if got := (ReconcileDecision{Record: record}).BackendArgs("2.0.0"); got != nil {
		t.Fatalf("BackendArgs = %q", got)
	}
}

func TestRefusePendingMigrationsArgs(t *testing.T) {
	if got := strings.Join(RefusePendingMigrationsArgs(), " "); got != "--refuse-pending-migrations" {
		t.Fatalf("args = %q", got)
	}
}
