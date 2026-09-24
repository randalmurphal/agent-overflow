package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"agent-overflow/internal/startupprogress"
)

// UpdateLockWait is how long a snapshot or trial command waits for the
// previous backend to release the data root's lock, the same budget the
// desktop helper gives the old app to exit.
const UpdateLockWait = 30 * time.Second

// UpdateCommand runs the in-app update's steps for a supervisor on the other
// side of wsl.exe (docs/specs/app-update.md). Each method reports progress
// on Out and returns the result report; the caller writes it and exits.
type UpdateCommand struct {
	DataDir  string
	UpdateID string
	// Out receives the report lines. A failed write means the caller's
	// supervisor is gone: the trial stops and nothing is restored.
	Out io.Writer
	// AcquireLock takes the data root's backend lock, waiting up to wait.
	// The file is what a trial inherits; release drops the lock.
	AcquireLock func(ctx context.Context, wait time.Duration) (lock *os.File, release func(), err error)
	// Now is the clock. nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics. nil is silent.
	Log func(format string, args ...any)
}

// TrialRunOptions are the trial command's own inputs.
type TrialRunOptions struct {
	// Binary and Args start UpdateTrialCommand of the target version.
	Binary string
	Args   []string
	Env    []string
	// Stdout and Stderr receive the trial's output. nil means stderr.
	Stdout, Stderr *os.File
	TargetVersion  string
	// Attempt is the trial's durable attempt number, 1 on the first. It is
	// recorded in the snapshot manifest with what the attempt's trial left
	// (see CheckLiveBeforeAttempt).
	Attempt int
	// Rule and StopTimeout default as in TrialConfig.
	Rule        StallRule
	StopTimeout time.Duration
}

// Started is the first report.
func (c UpdateCommand) Started() UpdateEvent {
	return UpdateEvent{Type: UpdateEventStarted, PID: os.Getpid()}
}

// Snapshot backs up the database for the update. hostAvailable bounds free
// space from the host that holds the data directory's disk (see
// SnapshotOptions).
func (c UpdateCommand) Snapshot(ctx context.Context, hostAvailable *uint64) UpdateEvent {
	relay, ctx, cancel := c.startRelay(ctx)
	defer cancel()
	defer relay.close()
	layout, err := NewAppUpdateLayout(c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	release, result, ok := c.lockAndPrepare(ctx, relay)
	if !ok {
		return result
	}
	defer release()
	relay.step("update.snapshot", "Backing up the database")
	_, err = TakeSnapshot(layout, c.DataDir, c.now(), SnapshotOptions{
		UpdateID:      c.UpdateID,
		HostAvailable: hostAvailable,
		Progress:      relay.copyProgress("update.snapshot", "Backing up the database"),
	})
	if err != nil {
		var space *InsufficientSpaceError
		if errors.As(err, &space) || errors.Is(err, errNoDatabase) || errors.Is(err, errChangedDuringCopy) {
			return failedEvent(UpdateOutcomeRefused, err)
		}
		return failedEvent(UpdateOutcomeFailed, err)
	}
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}
}

// Space answers whether the snapshot this update would take fits, by the
// rule TakeSnapshot applies (SnapshotPlan). It takes no lock and changes
// nothing: it runs while the version being replaced still serves the
// database, so the refusal reaches the user before anything stops.
// hostAvailable is as for Snapshot.
func (c UpdateCommand) Space(hostAvailable *uint64) UpdateEvent {
	layout, err := NewAppUpdateLayout(c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	plan, found, err := PlanSnapshot(layout, c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeFailed, err)
	}
	// No database is the snapshot step's refusal to make, with its reason.
	if found {
		if err := plan.Check(c.DataDir, hostAvailable); err != nil {
			var space *InsufficientSpaceError
			if errors.As(err, &space) {
				return failedEvent(UpdateOutcomeRefused, err)
			}
			return failedEvent(UpdateOutcomeFailed, err)
		}
	}
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}
}

// TrialRun runs the trial against the snapshotted database and restores the
// snapshot when the trial fails.
//
// Cancellation stops the trial and restores nothing: the caller that
// cancelled owns the update's recovery and may be about to retry it.
func (c UpdateCommand) TrialRun(ctx context.Context, opts TrialRunOptions) UpdateEvent {
	relay, ctx, cancel := c.startRelay(ctx)
	defer cancel()
	defer relay.close()
	layout, err := NewAppUpdateLayout(c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	lock, release, result, ok := c.lockAndPrepareFile(ctx, relay)
	if !ok {
		return result
	}
	defer release()
	if event, ok := c.requireSnapshot(layout); !ok {
		return event
	}
	checked, err := CheckLiveBeforeAttempt(layout, c.DataDir)
	if err != nil {
		var changed *LiveDatabaseChangedError
		if errors.As(err, &changed) {
			return failedEvent(UpdateOutcomeChanged, err)
		}
		return failedEvent(UpdateOutcomeRefused, err)
	}
	if !checked {
		c.log("supervise: update %s attempt %d runs on the database an interrupted attempt left, which could not be checked",
			c.UpdateID, opts.Attempt)
	}
	if err := RecordAttempt(layout, c.DataDir, opts.Attempt, false); err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	// ended records what this attempt left once the database is settled:
	// the trial's database, or the restored snapshot. A restore that did
	// not finish leaves the attempt unended.
	ended := func() {
		if err := RecordAttempt(layout, c.DataDir, opts.Attempt, true); err != nil {
			c.log("supervise: update %s attempt %d: %v; a retry runs without checking the database", c.UpdateID, opts.Attempt, err)
		}
	}

	relay.step("update.trial", "Starting "+startupprogress.DisplayVersion(opts.TargetVersion))
	trialErr := RunTrial(ctx, TrialConfig{
		Binary: opts.Binary, Args: opts.Args, Env: opts.Env,
		Stdout: opts.Stdout, Stderr: opts.Stderr,
		Lock:          lock,
		UpdateID:      c.UpdateID,
		TargetVersion: opts.TargetVersion,
		Rule:          opts.Rule,
		StopTimeout:   opts.StopTimeout,
		OnProgress:    relay.Report,
		OnStopping:    func() { relay.step("update.trial.stop", "Stopping the trial") },
		Log:           c.log,
	})
	if trialErr == nil {
		ended()
		return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomePrepared}
	}
	var failed *TrialFailedError
	if !errors.As(trialErr, &failed) {
		ended()
		return failedEvent(UpdateOutcomeFailed, fmt.Errorf("the trial was interrupted: %w", trialErr))
	}
	c.log("supervise: trial of update %s failed: %s", c.UpdateID, failed.Reason)
	relay.step("update.restore", "Restoring the database")
	if err := RestoreSnapshot(layout, c.DataDir, c.UpdateID, failed.Reason, c.now(),
		relay.copyProgress("update.restore", "Restoring the database")); err != nil {
		return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeFailed,
			Reason: fmt.Sprintf("%s, and the database backup could not be restored: %v", failed.Reason, err)}
	}
	ended()
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeRolledBack, Reason: failed.Reason}
}

// Restore puts the snapshot back, or finishes a marked restore. reason is
// recorded in the marker.
func (c UpdateCommand) Restore(ctx context.Context, reason string) UpdateEvent {
	relay, ctx, cancel := c.startRelay(ctx)
	defer cancel()
	defer relay.close()
	layout, err := NewAppUpdateLayout(c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	// lockAndPrepare finishes a marked restore, which is this command's
	// whole job when one is marked.
	release, result, ok := c.lockAndPrepare(ctx, relay)
	if !ok {
		return result
	}
	defer release()
	if event, ok := c.requireSnapshot(layout); !ok {
		return event
	}
	relay.step("update.restore", "Restoring the database")
	if err := RestoreSnapshot(layout, c.DataDir, c.UpdateID, reason, c.now(),
		relay.copyProgress("update.restore", "Restoring the database")); err != nil {
		return failedEvent(UpdateOutcomeFailed, err)
	}
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}
}

// Discard removes the snapshot. It takes no lock: the snapshot is not the
// database, and the update that owns it has settled. A marked restore is
// finished first, because a snapshot a restore still needs must survive.
func (c UpdateCommand) Discard(ctx context.Context) UpdateEvent {
	layout, err := NewAppUpdateLayout(c.DataDir)
	if err != nil {
		return failedEvent(UpdateOutcomeRefused, err)
	}
	if _, found, err := ReadRestoreMarker(layout); err != nil {
		return failedEvent(UpdateOutcomeFailed, err)
	} else if found {
		return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeRefused,
			Reason: "a restore of the database is not finished, so its backup is kept"}
	}
	if err := DiscardSnapshot(layout); err != nil {
		return failedEvent(UpdateOutcomeFailed, err)
	}
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}
}

// requireSnapshot refuses a missing snapshot, or one taken for a different
// update.
func (c UpdateCommand) requireSnapshot(layout Layout) (UpdateEvent, bool) {
	snapshot, found, err := ReadSnapshot(layout)
	if err != nil {
		return failedEvent(UpdateOutcomeFailed, err), false
	}
	if !found {
		return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeNoSnapshot,
			Reason: "there is no backup of the database for this update"}, false
	}
	if snapshot.UpdateID != c.UpdateID {
		return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeNoSnapshot,
			Reason: fmt.Sprintf("the database backup belongs to update %q, not %q", snapshot.UpdateID, c.UpdateID)}, false
	}
	return UpdateEvent{}, true
}

func (c UpdateCommand) lockAndPrepare(ctx context.Context, relay *commandRelay) (func(), UpdateEvent, bool) {
	_, release, result, ok := c.lockAndPrepareFile(ctx, relay)
	return release, result, ok
}

// lockAndPrepareFile takes the lock and runs PrepareDataRoot under it.
func (c UpdateCommand) lockAndPrepareFile(ctx context.Context, relay *commandRelay) (*os.File, func(), UpdateEvent, bool) {
	relay.step("update.lock", "Waiting for the previous version to stop")
	lock, release, err := c.AcquireLock(ctx, UpdateLockWait)
	if err != nil {
		return nil, nil, failedEvent(UpdateOutcomeRefused,
			fmt.Errorf("the database is still in use by another Agent Overflow backend: %w", err)), false
	}
	err = PrepareDataRoot(c.DataDir, PrepareOptions{
		Progress: relay.copyProgress("update.restore", "Finishing an interrupted restore"),
		Log:      c.log,
	})
	if err != nil {
		release()
		outcome := UpdateOutcomeFailed
		if IsPendingUpdate(err) {
			outcome = UpdateOutcomeRefused
		}
		return nil, nil, failedEvent(outcome, err), false
	}
	return lock, release, UpdateEvent{}, true
}

func (c UpdateCommand) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c UpdateCommand) log(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

func failedEvent(outcome UpdateOutcome, err error) UpdateEvent {
	return UpdateEvent{Type: UpdateEventResult, Outcome: outcome, Reason: err.Error()}
}

// commandRelay reports a command's progress on its output. A failed write
// cancels the command's context.
type commandRelay struct {
	*ProgressRelay
	started int64
}

func (c UpdateCommand) startRelay(ctx context.Context) (*commandRelay, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	out := c.Out
	if out == nil {
		out = io.Discard
	}
	relay := &commandRelay{
		ProgressRelay: NewProgressRelay(func(p startupprogress.Progress, liveness bool) error {
			return WriteUpdateEvent(out, UpdateEvent{Type: UpdateEventProgress, Progress: &p, Liveness: liveness})
		}),
		started: c.now().UnixMilli(),
	}
	go func() {
		select {
		case <-relay.Failed():
			c.log("supervise: the update command's output closed; stopping")
			cancel()
		case <-ctx.Done():
		}
	}()
	return relay, ctx, cancel
}

func (r *commandRelay) close() {
	_ = r.Close()
}

// step reports a real step of the command itself.
func (r *commandRelay) step(phase, detail string) {
	now := time.Now().UnixMilli()
	r.Report(startupprogress.Progress{Phase: phase, Detail: detail, StartedAt: r.started, UpdatedAt: now}, false)
}

// copyProgress reports a copy's bytes as real progress.
func (r *commandRelay) copyProgress(phase, detail string) CopyProgress {
	return func(copied, total int64) {
		r.step(phase, fmt.Sprintf("%s: %s of %s", detail, FormatBytes(uint64(copied)), FormatBytes(uint64(total))))
	}
}
