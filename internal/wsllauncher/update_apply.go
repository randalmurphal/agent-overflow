package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// The Windows launcher's in-app update after the old launcher hands off
// (docs/specs/app-update.md, Windows sequence steps 5 to 8 and the recovery
// table). UpdateSequence owns the order and the record; UpdateHost owns
// everything outside the record, which is Windows- or WSL-specific.

// UpdateHost performs the update's side effects.
type UpdateHost interface {
	// RunCommand runs one update command of the backend at payload in
	// distro.
	RunCommand(ctx context.Context, distro, payload, command string, args []string, onProgress func(startupprogress.Progress)) (supervise.UpdateEvent, error)
	// HostFreeBytes is the free space on the Windows drive that holds the
	// distribution's virtual disk. ok is false when it is unknown.
	HostFreeBytes(distro string) (free uint64, ok bool)
	// CommitPayload renames the staged payload over the stable path. The
	// staged payload being gone means an earlier commit already moved it.
	CommitPayload(ctx context.Context, record supervise.LauncherRecord) error
	// RemoveStagedPayload deletes the staged payload if it is present.
	RemoveStagedPayload(ctx context.Context, record supervise.LauncherRecord) error
	// InvalidatePayloadRecord clears the installed-payload record, so a crash
	// between here and RecordPayload makes the next launch reinstall.
	InvalidatePayloadRecord(record supervise.LauncherRecord) error
	// RecordPayload records the committed payload as installed.
	RecordPayload(record supervise.LauncherRecord) error
	// PublishLauncher replaces the install path with the staged launcher.
	PublishLauncher(record supervise.LauncherRecord) error
	// RemoveLauncherResidue deletes the staged launcher and anything the
	// publish set aside.
	RemoveLauncherResidue(record supervise.LauncherRecord) error
}

// UpdateSequence runs and recovers the update a record describes.
type UpdateSequence struct {
	RecordPath string
	Host       UpdateHost
	// Progress receives what the update is doing, for the loading page.
	Progress func(startupprogress.Progress)
	// Self is this launcher. Apply refuses an update another running
	// launcher is applying, and Join names Self as the update's joiner.
	Self supervise.ProcessRef
	// JoinPoll is how often Join looks at the applier and its progress.
	// Zero is UpdateJoinPoll.
	JoinPoll time.Duration
	Now      func() time.Time
	Logf     func(string, ...any)
}

// ErrNoUpdateRecord is an --update-apply with no record to apply.
var ErrNoUpdateRecord = errors.New("wsllauncher: there is no update record")

// Apply runs the update id from its durable state (supervise.UpdateRun.
// Apply). It returns an error when the record cannot be read or written, or
// when a commit step fails after the commit became durable; the next launch
// resumes either. A migration record runs the same steps through the stable
// payload and has nothing to publish.
func (s UpdateSequence) Apply(ctx context.Context, id string) (supervise.UpdateEnd, error) {
	record, found, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return supervise.UpdateEnd{}, err
	}
	if !found {
		return supervise.UpdateEnd{}, ErrNoUpdateRecord
	}
	if record.Update.ID != id {
		return supervise.UpdateEnd{}, fmt.Errorf("wsllauncher: the update record is for update %q, not %q", record.Update.ID, id)
	}
	if applier := record.Applier; applier != nil && *applier != s.Self {
		running, err := applier.Running()
		if err != nil {
			return supervise.UpdateEnd{}, fmt.Errorf("wsllauncher: check the launcher applying update %s: %w", id, err)
		}
		if running {
			return supervise.UpdateEnd{}, fmt.Errorf("wsllauncher: update %s is being applied by another launcher (pid %d)", id, applier.PID)
		}
	}
	return s.run(&record).Apply(ctx, record.State)
}

// ReconcileAction is what a launcher at the install path does after
// Reconcile.
type ReconcileAction int

const (
	// ReconcileLaunch is an ordinary launch.
	ReconcileLaunch ReconcileAction = iota
	// ReconcileHandOff starts the staged launcher to resume the update and
	// exits.
	ReconcileHandOff
	// ReconcileBlocked starts nothing and shows the reason.
	ReconcileBlocked
	// ReconcileJoin waits for the launcher applying the update and decides
	// again (Join).
	ReconcileJoin
	// ReconcileRelaunch starts the launcher at the install path, which
	// waits for this one to exit, and exits. Only Join returns it.
	ReconcileRelaunch
	// ReconcileResume continues the record's pending migration in this
	// launcher (ResumeMigration), which has no other launcher to hand it
	// to, then launches if it committed.
	ReconcileResume
)

// ReconcileDecision is Reconcile's answer.
type ReconcileDecision struct {
	Action ReconcileAction
	Record supervise.LauncherRecord
	Reason string
	// UpdatingTo is the version whose update this launch finishes: set on
	// the first launch of a committed target, whose backend's startup
	// report names it (UpdatingToArgs).
	UpdatingTo string
}

// BackendArgs is the argv that tells a backend of version backendVersion
// what the record settled: the update this launch finishes, or an update
// from that version that rolled back or failed, with its reason.
func (d ReconcileDecision) BackendArgs(backendVersion string) []string {
	args := UpdatingToArgs(d.UpdatingTo)
	to, reason, ok := d.Record.UnsuccessfulUpdate(backendVersion)
	if !ok || reason == "" {
		return args
	}
	if runes := []rune(reason); len(runes) > updateFailedReasonLimit {
		reason = string(runes[:updateFailedReasonLimit-1]) + "…"
	}
	return append(args, "--"+UpdateFailedToFlag, to, "--"+UpdateFailedReasonFlag, reason)
}

// Reconcile applies the recovery table for a launcher at the install path
// whose embedded payload digest is fingerprint. It runs before WSL starts
// the backend and while this process holds the single-instance identity.
func (s UpdateSequence) Reconcile(ctx context.Context, fingerprint string) (ReconcileDecision, error) {
	record, found, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return ReconcileDecision{}, err
	}
	if !found {
		return ReconcileDecision{Action: ReconcileLaunch}, nil
	}
	// The launcher applying the update holds no single-instance identity,
	// so this launch can start while it runs. The update is in flight, not
	// interrupted, whatever the record says.
	if applier := record.Applier; applier != nil {
		running, err := applier.Running()
		if err != nil {
			return ReconcileDecision{}, fmt.Errorf("wsllauncher: check the launcher applying update %s: %w", record.Update.ID, err)
		}
		if running {
			return ReconcileDecision{Action: ReconcileJoin, Record: record}, nil
		}
	}
	if record.Migration() {
		return s.reconcileMigration(ctx, record)
	}
	update := record.Update
	run := s.run(&record)
	isTarget := fingerprint == record.TargetFingerprint
	switch update.State {
	case supervise.UpdateCommitted:
		if update.Reported {
			return ReconcileDecision{Action: ReconcileLaunch, Record: record}, nil
		}
		if !isTarget {
			if !fileExists(record.StagedLauncher) {
				return ReconcileDecision{Action: ReconcileBlocked, Record: record, Reason: fmt.Sprintf(
					"The update to %s finished, but the new launcher is missing, so it could not be installed. Install %s from the releases page.",
					startupprogress.DisplayVersion(update.To), startupprogress.DisplayVersion(update.To))}, nil
			}
			return ReconcileDecision{Action: ReconcileHandOff, Record: record}, nil
		}
		run.Discard(ctx, record.State)
		decision, err := s.markReported(run, record)
		if err != nil {
			return ReconcileDecision{}, err
		}
		decision.UpdatingTo = update.To
		return decision, nil
	case supervise.UpdateRolledBack, supervise.UpdateFailed:
		if update.Reported {
			return ReconcileDecision{Action: ReconcileLaunch, Record: record}, nil
		}
		run.Discard(ctx, record.State)
		run.RemoveStaged(ctx, record.State)
		return s.markReported(run, record)
	}

	end, resume, err := run.RecoverPending(ctx, record.State, fileExists(record.StagedLauncher), "its new launcher")
	if err != nil {
		return ReconcileDecision{}, err
	}
	if resume {
		return ReconcileDecision{Action: ReconcileHandOff, Record: record}, nil
	}
	return s.afterRecovery(end)
}

// afterRecovery launches once a recovery settled the update, and blocks
// when it could not.
func (s UpdateSequence) afterRecovery(end supervise.UpdateEnd) (ReconcileDecision, error) {
	record, _, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return ReconcileDecision{}, err
	}
	if !end.Settled() {
		return ReconcileDecision{Action: ReconcileBlocked, Record: record, Reason: end.Reason}, nil
	}
	return s.markReported(s.run(&record), record)
}

// markReported records that a launch acted on the settled record and removes
// the launcher's residue.
func (s UpdateSequence) markReported(run supervise.UpdateRun, record supervise.LauncherRecord) (ReconcileDecision, error) {
	if err := s.Host.RemoveLauncherResidue(record); err != nil {
		s.logf("updater: remove the update's launcher files: %v", err)
	}
	next, err := run.MarkReported(record.State)
	if err != nil {
		return ReconcileDecision{}, err
	}
	record.State = next
	return ReconcileDecision{Action: ReconcileLaunch, Record: record}, nil
}

// run is the shared sequence over record, whose steps run as WSL commands
// (launcherSteps).
func (s UpdateSequence) run(record *supervise.LauncherRecord) supervise.UpdateRun {
	return supervise.UpdateRun{
		Steps:      &launcherSteps{s: s, record: record},
		MemoryPath: supervise.FailedTrialPath(s.RecordPath),
		LogName:    "launcher log",
		Progress:   s.Progress,
		Now:        s.Now,
		Logf:       s.logf,
	}
}

// launcherSteps is supervise.UpdateSteps for the launcher's record: each
// step is an update command of a backend in the record's distro. The
// snapshot and the trial run through the record's trial payload, restore
// and discard through the stable payload, the version that runs next.
type launcherSteps struct {
	s      UpdateSequence
	record *supervise.LauncherRecord
}

func (l *launcherSteps) Save(state supervise.State) error {
	next := *l.record
	next.State = state
	if err := supervise.SaveLauncherRecord(l.s.RecordPath, next); err != nil {
		return err
	}
	*l.record = next
	return nil
}

func (l *launcherSteps) RemoveRecord() error {
	if err := os.Remove(l.s.RecordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *launcherSteps) Snapshot(ctx context.Context, progress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	var args []string
	if free, ok := l.s.Host.HostFreeBytes(l.record.Distro); ok {
		args = append(args, "--host-free", strconv.FormatUint(free, 10))
	}
	return l.command(ctx, l.record.TrialPayload(), supervise.UpdateSnapshotCommand, progress, args...)
}

// Trial runs the trial command. A command stopped for stalling may still
// report a decided trial on its way out, which decides the trial; anything
// else it reports is the stop.
func (l *launcherSteps) Trial(ctx context.Context, to string, attempt int, progress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	result, err := l.command(ctx, l.record.TrialPayload(), supervise.UpdateTrialRunCommand, progress,
		"--to", to, "--attempt", strconv.Itoa(attempt))
	var stopped *UpdateCommandStoppedError
	if err != nil && errors.As(err, &stopped) && stopped.Result != nil &&
		(stopped.Result.Outcome == supervise.UpdateOutcomePrepared || stopped.Result.Outcome == supervise.UpdateOutcomeRolledBack) {
		return *stopped.Result, nil
	}
	return result, err
}

func (l *launcherSteps) Restore(ctx context.Context, reason string, progress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	return l.command(ctx, l.record.StablePayload, supervise.UpdateRestoreCommand, progress, "--reason", reason)
}

func (l *launcherSteps) Discard(ctx context.Context, progress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	return l.command(ctx, l.record.StablePayload, supervise.UpdateDiscardCommand, progress)
}

func (l *launcherSteps) RemoveStaged(ctx context.Context) error {
	return l.s.Host.RemoveStagedPayload(ctx, *l.record)
}

// Publish installs the committed update. The snapshot is discarded through
// the staged payload, which is the one that knows the command while the
// stable path still holds the previous backend. Every step is idempotent,
// so a commit interrupted anywhere is finished by repeating it.
func (l *launcherSteps) Publish(ctx context.Context) error {
	record := *l.record
	result, err := l.command(ctx, record.StagedPayload, supervise.UpdateDiscardCommand, l.s.Progress)
	switch {
	case err != nil:
		l.s.logf("updater: update %s: discard the database backup: %v", record.Update.ID, err)
	case result.Outcome != supervise.UpdateOutcomeOK:
		l.s.logf("updater: update %s: discard the database backup: %s: %s", record.Update.ID, result.Outcome, result.Reason)
	}
	if err := l.s.Host.InvalidatePayloadRecord(record); err != nil {
		return fmt.Errorf("invalidate the installed payload record: %w", err)
	}
	if err := l.s.Host.CommitPayload(ctx, record); err != nil {
		return fmt.Errorf("install the new backend: %w", err)
	}
	if err := l.s.Host.RecordPayload(record); err != nil {
		return fmt.Errorf("record the new backend: %w", err)
	}
	if err := l.s.Host.PublishLauncher(record); err != nil {
		return fmt.Errorf("install the new launcher: %w", err)
	}
	return nil
}

func (l *launcherSteps) command(ctx context.Context, payload, command string, progress func(startupprogress.Progress), args ...string) (supervise.UpdateEvent, error) {
	full := append([]string{"--id", l.record.Update.ID}, args...)
	return l.s.Host.RunCommand(ctx, l.record.Distro, payload, command, full, progress)
}

func (s UpdateSequence) step(phase, detail string) {
	if s.Progress == nil {
		return
	}
	now := s.now().UnixMilli()
	s.Progress(startupprogress.Progress{Phase: phase, Detail: detail, StartedAt: now, UpdatedAt: now})
}

func (s UpdateSequence) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s UpdateSequence) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
