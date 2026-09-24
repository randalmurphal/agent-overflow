package wsllauncher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

const (
	testStable = "/home/u/.local/share/agent-overflow/bin/agent-overflow"
	testStaged = "/home/u/.local/share/agent-overflow/bin/agent-overflow.update-u1"
)

// fakeUpdateHost records every side effect in order and answers commands
// from per-command queues. A command with no queued answer succeeds.
type fakeUpdateHost struct {
	t          *testing.T
	recordPath string
	calls      []string
	answers    map[string][]fakeAnswer
	free       uint64
	freeKnown  bool
	failAt     string
	// states records the durable record state when each command ran.
	states map[string]string
}

type fakeAnswer struct {
	event supervise.UpdateEvent
	err   error
}

func (h *fakeUpdateHost) answer(command string, outcome supervise.UpdateOutcome, reason string) {
	h.answers[command] = append(h.answers[command], fakeAnswer{event: supervise.UpdateEvent{
		Type: supervise.UpdateEventResult, Outcome: outcome, Reason: reason}})
}

func (h *fakeUpdateHost) fail(command string, err error) {
	h.answers[command] = append(h.answers[command], fakeAnswer{err: err})
}

func (h *fakeUpdateHost) durableState() string {
	record, found, err := supervise.LoadLauncherRecord(h.recordPath)
	if err != nil || !found {
		return "none"
	}
	return string(record.Update.State) + "/" + string(rune('0'+record.Update.Attempts))
}

func (h *fakeUpdateHost) RunCommand(_ context.Context, distro, payload, command string, args []string, onProgress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	if distro != "Ubuntu" {
		h.t.Errorf("command %s ran in distro %q", command, distro)
	}
	name := strings.TrimPrefix(command, "__update-")
	where := "stable"
	if payload == testStaged {
		where = "staged"
	} else if payload != testStable {
		h.t.Errorf("command %s ran payload %q", command, payload)
	}
	h.calls = append(h.calls, name+"@"+where+" "+strings.Join(args, " "))
	h.states[name] = h.durableState()
	if onProgress != nil {
		onProgress(startupprogress.Progress{Detail: name})
	}
	queue := h.answers[command]
	if len(queue) == 0 {
		outcome := supervise.UpdateOutcomeOK
		if command == supervise.UpdateTrialRunCommand {
			outcome = supervise.UpdateOutcomePrepared
		}
		return supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: outcome}, nil
	}
	h.answers[command] = queue[1:]
	return queue[0].event, queue[0].err
}

func (h *fakeUpdateHost) HostFreeBytes(distro string) (uint64, bool) {
	if distro != "Ubuntu" {
		h.t.Errorf("free space asked for distro %q", distro)
	}
	return h.free, h.freeKnown
}

func (h *fakeUpdateHost) step(name string) error {
	h.calls = append(h.calls, name)
	h.states[name] = h.durableState()
	if h.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}

func (h *fakeUpdateHost) CommitPayload(context.Context, supervise.LauncherRecord) error {
	return h.step("commit-payload")
}

func (h *fakeUpdateHost) RemoveStagedPayload(context.Context, supervise.LauncherRecord) error {
	return h.step("remove-staged")
}

func (h *fakeUpdateHost) InvalidatePayloadRecord(supervise.LauncherRecord) error {
	return h.step("invalidate")
}

func (h *fakeUpdateHost) RecordPayload(supervise.LauncherRecord) error { return h.step("record") }

func (h *fakeUpdateHost) PublishLauncher(supervise.LauncherRecord) error { return h.step("publish") }

func (h *fakeUpdateHost) RemoveLauncherResidue(supervise.LauncherRecord) error {
	return h.step("residue")
}

type updateFixture struct {
	t        *testing.T
	host     *fakeUpdateHost
	sequence UpdateSequence
	launcher string
}

func newUpdateFixture(t *testing.T) *updateFixture {
	dir := t.TempDir()
	path := supervise.LauncherRecordPath(dir, "prod")
	host := &fakeUpdateHost{t: t, recordPath: path, answers: map[string][]fakeAnswer{}, states: map[string]string{}}
	launcher := filepath.Join(dir, "runtime", "agent-overflow-2.0.0.exe")
	return &updateFixture{
		t: t, host: host, launcher: launcher,
		sequence: UpdateSequence{
			RecordPath: path, Host: host,
			Now:  func() time.Time { return time.UnixMilli(1_000_000) },
			Logf: func(string, ...any) {},
		},
	}
}

// save writes a record of update u1 from 1.0.0 to 2.0.0 in state with
// attempts trial starts, and creates the staged launcher.
func (f *updateFixture) save(state supervise.UpdateState, attempts int, reported bool) supervise.LauncherRecord {
	f.t.Helper()
	base, err := supervise.Adopt("1.0.0")
	if err != nil {
		f.t.Fatal(err)
	}
	next, err := base.Begin("u1", "2.0.0", time.UnixMilli(1))
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
		if reported {
			if next, _, err = next.MarkReported(); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	record := supervise.LauncherRecord{
		State: next, Distro: "Ubuntu",
		StablePayload: testStable, StagedPayload: testStaged,
		StagedLauncher: f.launcher, InstallPath: `C:\Users\u\AppData\Local\Programs\agent-overflow.exe`,
		TargetFingerprint: "target",
	}
	if err := supervise.SaveLauncherRecord(f.sequence.RecordPath, record); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.launcher), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.launcher, []byte("MZ"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	return record
}

func (f *updateFixture) record() supervise.LauncherRecord {
	f.t.Helper()
	record, found, err := supervise.LoadLauncherRecord(f.sequence.RecordPath)
	if err != nil || !found {
		f.t.Fatalf("record: found=%v err=%v", found, err)
	}
	return record
}

func (f *updateFixture) wantCalls(want ...string) {
	f.t.Helper()
	if strings.Join(f.host.calls, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.host.calls, "\n"), strings.Join(want, "\n"))
	}
}

func (f *updateFixture) wantRecord(state supervise.UpdateState, attempts int, reason string, reported bool) {
	f.t.Helper()
	update := f.record().Update
	if update.State != state || update.Attempts != attempts || update.Reason != reason || update.Reported != reported {
		f.t.Fatalf("record = %+v, want %s attempts=%d reason=%q reported=%v", update, state, attempts, reason, reported)
	}
}

func TestApplyUpdateCommits(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.free, f.host.freeKnown = 5<<30, true
	var progress []string
	f.sequence.Progress = func(p startupprogress.Progress) { progress = append(progress, p.Detail) }
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("end = %+v, %v", end, err)
	}
	f.wantCalls(
		"snapshot@staged --id u1 --host-free 5368709120",
		"trial-run@staged --id u1 --to 2.0.0 --attempt 1",
		"discard@staged --id u1",
		"invalidate", "commit-payload", "record", "publish",
	)
	// The attempt is durable before the trial, the commit before anything
	// is published.
	if f.host.states["trial-run"] != "pending/1" || f.host.states["discard"] != "committed/1" ||
		f.host.states["publish"] != "committed/1" {
		t.Fatalf("durable states = %v", f.host.states)
	}
	f.wantRecord(supervise.UpdateCommitted, 1, "", false)
	if !strings.Contains(strings.Join(progress, "|"), "trial-run|Installing v2.0.0") {
		t.Fatalf("progress = %q", progress)
	}
}

func TestApplyUpdateOmitsUnknownHostSpace(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	if _, err := f.sequence.Apply(t.Context(), "u1"); err != nil {
		t.Fatal(err)
	}
	if f.host.calls[0] != "snapshot@staged --id u1" {
		t.Fatalf("snapshot call = %q", f.host.calls[0])
	}
}

func TestApplyUpdateSettlesATrialTheCommandRolledBack(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRolledBack, "database schema 90 is newer than this build knows")
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateRolledBack || end.Reason != "database schema 90 is newer than this build knows" {
		t.Fatalf("end = %+v, %v", end, err)
	}
	f.wantCalls(
		"snapshot@staged --id u1",
		"trial-run@staged --id u1 --to 2.0.0 --attempt 1",
		"discard@stable --id u1",
		"remove-staged",
	)
	if f.host.states["discard"] != "rolled-back/1" {
		t.Fatalf("discarded before the rollback was durable: %v", f.host.states)
	}
	f.wantRecord(supervise.UpdateRolledBack, 1, "database schema 90 is newer than this build knows", false)
}

func TestApplyUpdateRestoresThroughTheStablePayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*fakeUpdateHost)
		reason string
	}{
		{"failed result", func(h *fakeUpdateHost) {
			h.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeFailed, "boot failed, and the database backup could not be restored: disk")
		}, "boot failed, and the database backup could not be restored: disk"},
		{"no result", func(h *fakeUpdateHost) {
			h.fail(supervise.UpdateTrialRunCommand, errors.New("the __update-trial-run step exited without a result"))
		}, "the __update-trial-run step exited without a result"},
		{"stopped", func(h *fakeUpdateHost) {
			h.fail(supervise.UpdateTrialRunCommand, &UpdateCommandStoppedError{Reason: "stalled",
				Result: &supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomeFailed, Reason: "the trial was interrupted"}})
		}, "stalled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.save(supervise.UpdatePending, 0, false)
			tc.setup(f.host)
			end, err := f.sequence.Apply(t.Context(), "u1")
			if err != nil || end.State != supervise.UpdateRolledBack || end.Reason != tc.reason {
				t.Fatalf("end = %+v, %v", end, err)
			}
			f.wantCalls(
				"snapshot@staged --id u1",
				"trial-run@staged --id u1 --to 2.0.0 --attempt 1",
				"restore@stable --id u1 --reason "+tc.reason,
				"discard@stable --id u1",
				"remove-staged",
			)
			if f.host.states["restore"] != "pending/1" {
				t.Fatalf("settled before the restore finished: %v", f.host.states)
			}
			f.wantRecord(supervise.UpdateRolledBack, 1, tc.reason, false)
		})
	}
}

func TestApplyUpdateUsesADecidedResultFromAStoppedTrial(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.fail(supervise.UpdateTrialRunCommand, &UpdateCommandStoppedError{Reason: "stalled while stopping",
		Result: &supervise.UpdateEvent{Type: supervise.UpdateEventResult, Outcome: supervise.UpdateOutcomePrepared}})
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("end = %+v, %v", end, err)
	}
}

func TestApplyUpdateLeavesAnUnrestoredUpdatePending(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeFailed, "the trial was interrupted")
	f.host.answer(supervise.UpdateRestoreCommand, supervise.UpdateOutcomeRefused, "the database is still in use")
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdatePending {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if !strings.Contains(end.Reason, "the trial was interrupted") || !strings.Contains(end.Reason, "the database is still in use") {
		t.Fatalf("reason = %q", end.Reason)
	}
	f.wantCalls(
		"snapshot@staged --id u1",
		"trial-run@staged --id u1 --to 2.0.0 --attempt 1",
		"restore@stable --id u1 --reason the trial was interrupted",
	)
	f.wantRecord(supervise.UpdatePending, 1, "", false)
}

func TestApplyUpdateFailsWithoutASnapshot(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	f.host.answer(supervise.UpdateSnapshotCommand, supervise.UpdateOutcomeRefused, "Free at least 256 MB")
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateFailed || end.Reason != "the database could not be backed up: Free at least 256 MB" {
		t.Fatalf("end = %+v, %v", end, err)
	}
	f.wantCalls("snapshot@staged --id u1", "discard@stable --id u1", "remove-staged")
	f.wantRecord(supervise.UpdateFailed, 0, "the database could not be backed up: Free at least 256 MB", false)
}

func TestApplyUpdateRefusalDependsOnTheAttempt(t *testing.T) {
	t.Run("first attempt changed nothing", func(t *testing.T) {
		f := newUpdateFixture(t)
		f.save(supervise.UpdatePending, 0, false)
		f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeRefused, "the database changed after it was backed up")
		end, err := f.sequence.Apply(t.Context(), "u1")
		if err != nil || end.State != supervise.UpdateFailed {
			t.Fatalf("end = %+v, %v", end, err)
		}
		f.wantCalls(
			"snapshot@staged --id u1",
			"trial-run@staged --id u1 --to 2.0.0 --attempt 1",
			"discard@stable --id u1",
			"remove-staged",
		)
	})
	t.Run("a retry follows a trial that ran", func(t *testing.T) {
		f := newUpdateFixture(t)
		f.save(supervise.UpdatePending, 1, false)
		f.host.answer(supervise.UpdateTrialRunCommand, supervise.UpdateOutcomeNoSnapshot, "there is no backup")
		end, err := f.sequence.Apply(t.Context(), "u1")
		if err != nil || end.State != supervise.UpdateRolledBack {
			t.Fatalf("end = %+v, %v", end, err)
		}
		f.wantCalls(
			"trial-run@staged --id u1 --to 2.0.0 --attempt 2",
			"restore@stable --id u1 --reason there is no backup",
			"discard@stable --id u1",
			"remove-staged",
		)
	})
}

func TestApplyUpdateAtTheAttemptLimitRollsBack(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, supervise.TrialAttemptLimit, false)
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateRolledBack {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if f.host.calls[0] != "restore@stable --id u1 --reason the trial was interrupted 2 times without finishing" {
		t.Fatalf("calls = %q", f.host.calls)
	}
}

func TestApplyUpdateRepeatsAnInterruptedCommit(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdateCommitted, 1, false)
	f.host.failAt = "publish"
	if _, err := f.sequence.Apply(t.Context(), "u1"); err == nil || !strings.Contains(err.Error(), "install the new launcher") {
		t.Fatalf("err = %v", err)
	}
	f.wantRecord(supervise.UpdateCommitted, 1, "earlier reason", false)
	f.host.calls, f.host.failAt = nil, ""
	end, err := f.sequence.Apply(t.Context(), "u1")
	if err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("end = %+v, %v", end, err)
	}
	f.wantCalls("discard@staged --id u1", "invalidate", "commit-payload", "record", "publish")
}

func TestApplyUpdateChecksTheRecord(t *testing.T) {
	f := newUpdateFixture(t)
	if _, err := f.sequence.Apply(t.Context(), "u1"); !errors.Is(err, ErrNoUpdateRecord) {
		t.Fatalf("err = %v", err)
	}
	f.save(supervise.UpdatePending, 0, false)
	if _, err := f.sequence.Apply(t.Context(), "u2"); err == nil {
		t.Fatal("applied another update's record")
	}
	if err := os.WriteFile(f.sequence.RecordPath, []byte(`{"schema":1,"activeVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sequence.Apply(t.Context(), "u1"); err == nil {
		t.Fatal("applied an invalid record")
	}
	if len(f.host.calls) != 0 {
		t.Fatalf("calls = %q", f.host.calls)
	}
}

func TestReconcileUpdate(t *testing.T) {
	type row struct {
		name        string
		state       supervise.UpdateState
		attempts    int
		reported    bool
		fingerprint string
		noLauncher  bool
		setup       func(*fakeUpdateHost)
		action      ReconcileAction
		reason      string
		updatingTo  string
		calls       []string
		after       func(*updateFixture)
	}
	rows := []row{
		{name: "settled and reported", state: supervise.UpdateRolledBack, attempts: 1, reported: true,
			action: ReconcileLaunch},
		{name: "rolled back, unreported", state: supervise.UpdateRolledBack, attempts: 1,
			action: ReconcileLaunch,
			calls:  []string{"discard@stable --id u1", "remove-staged", "residue"},
			after: func(f *updateFixture) {
				f.wantRecord(supervise.UpdateRolledBack, 1, "earlier reason", true)
			}},
		// The first launch of the target is the one that finishes the
		// update, and only it: the record is reported from then on.
		{name: "committed, the target runs", state: supervise.UpdateCommitted, attempts: 1, fingerprint: "target",
			action: ReconcileLaunch, updatingTo: "2.0.0",
			calls: []string{"discard@stable --id u1", "residue"},
			after: func(f *updateFixture) {
				f.wantRecord(supervise.UpdateCommitted, 1, "earlier reason", true)
			}},
		{name: "committed, publish unfinished", state: supervise.UpdateCommitted, attempts: 1,
			action: ReconcileHandOff},
		{name: "committed, launcher gone", state: supervise.UpdateCommitted, attempts: 1, noLauncher: true,
			action: ReconcileBlocked, reason: "Install v2.0.0 from the releases page."},
		{name: "committed and reported, replaced by hand", state: supervise.UpdateCommitted, attempts: 1, reported: true,
			action: ReconcileLaunch},
		{name: "committed and reported, the target runs again", state: supervise.UpdateCommitted, attempts: 1, reported: true,
			fingerprint: "target", action: ReconcileLaunch},
		{name: "pending, never trialled", state: supervise.UpdatePending,
			action: ReconcileLaunch,
			calls:  []string{"discard@stable --id u1", "remove-staged", "residue"},
			after: func(f *updateFixture) {
				f.wantRecord(supervise.UpdateFailed, 0, "the update was interrupted before its trial started", true)
			}},
		{name: "pending, retryable", state: supervise.UpdatePending, attempts: 1,
			action: ReconcileHandOff},
		{name: "pending, launcher gone", state: supervise.UpdatePending, attempts: 1, noLauncher: true,
			action: ReconcileLaunch,
			calls: []string{"restore@stable --id u1 --reason the update was interrupted and its new launcher is missing",
				"discard@stable --id u1", "remove-staged", "residue"},
			after: func(f *updateFixture) {
				f.wantRecord(supervise.UpdateRolledBack, 1, "the update was interrupted and its new launcher is missing", true)
			}},
		{name: "pending, at the limit", state: supervise.UpdatePending, attempts: supervise.TrialAttemptLimit,
			action: ReconcileLaunch,
			calls: []string{"restore@stable --id u1 --reason the trial was interrupted 2 times without finishing",
				"discard@stable --id u1", "remove-staged", "residue"}},
		{name: "pending, restore fails", state: supervise.UpdatePending, attempts: supervise.TrialAttemptLimit,
			setup: func(h *fakeUpdateHost) {
				h.answer(supervise.UpdateRestoreCommand, supervise.UpdateOutcomeNoSnapshot, "there is no backup of the database for this update")
			},
			action: ReconcileBlocked, reason: "the database backup could not be restored: there is no backup of the database for this update",
			calls: []string{"restore@stable --id u1 --reason the trial was interrupted 2 times without finishing"},
			after: func(f *updateFixture) {
				f.wantRecord(supervise.UpdatePending, supervise.TrialAttemptLimit, "", false)
			}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.save(tc.state, tc.attempts, tc.reported)
			if tc.noLauncher {
				if err := os.Remove(f.launcher); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				tc.setup(f.host)
			}
			fingerprint := tc.fingerprint
			if fingerprint == "" {
				fingerprint = "previous"
			}
			decision, err := f.sequence.Reconcile(t.Context(), fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != tc.action || !strings.Contains(decision.Reason, tc.reason) {
				t.Fatalf("decision = %+v, want action %d reason %q", decision, tc.action, tc.reason)
			}
			if decision.UpdatingTo != tc.updatingTo {
				t.Fatalf("UpdatingTo = %q, want %q", decision.UpdatingTo, tc.updatingTo)
			}
			f.wantCalls(tc.calls...)
			if tc.after != nil {
				tc.after(f)
			}
		})
	}
}

func TestReconcileWithoutARecordLaunches(t *testing.T) {
	f := newUpdateFixture(t)
	decision, err := f.sequence.Reconcile(t.Context(), "any")
	if err != nil || decision.Action != ReconcileLaunch {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
}

func TestReconcileRefusesAnUnreadableRecord(t *testing.T) {
	f := newUpdateFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.sequence.RecordPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.sequence.RecordPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sequence.Reconcile(t.Context(), "any"); err == nil {
		t.Fatal("an unreadable record was treated as none")
	}
}
