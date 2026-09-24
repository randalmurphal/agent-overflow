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
	Now      func() time.Time
	Logf     func(string, ...any)
}

// UpdateEnd is where an update rests after Apply or Reconcile.
type UpdateEnd struct {
	// State is the record's state: committed, rolled-back or failed once
	// the update settled, pending when it could not be.
	State supervise.UpdateState
	// Reason is the settled reason, or what blocks a pending update.
	Reason string
}

// Settled reports whether the update reached a terminal state.
func (e UpdateEnd) Settled() bool { return e.State != supervise.UpdatePending }

// ErrNoUpdateRecord is an --update-apply with no record to apply.
var ErrNoUpdateRecord = errors.New("wsllauncher: there is no update record")

// Apply runs the update id from its durable state: snapshot on the first
// attempt, the trial, then commit or rollback. A committed record repeats
// the commit, which is idempotent. It returns an error when the record
// cannot be read or written, or when a commit step fails after the commit
// became durable; the next launch resumes either.
func (s UpdateSequence) Apply(ctx context.Context, id string) (UpdateEnd, error) {
	record, found, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return UpdateEnd{}, err
	}
	if !found {
		return UpdateEnd{}, ErrNoUpdateRecord
	}
	if record.Update.ID != id {
		return UpdateEnd{}, fmt.Errorf("wsllauncher: the update record is for update %q, not %q", record.Update.ID, id)
	}
	switch record.Update.State {
	case supervise.UpdateCommitted:
		return s.commit(ctx, record)
	case supervise.UpdateRolledBack, supervise.UpdateFailed:
		return UpdateEnd{State: record.Update.State, Reason: record.Update.Reason}, nil
	}
	if record.Update.Attempts >= supervise.TrialAttemptLimit {
		return s.rollBack(ctx, record, fmt.Sprintf(
			"the trial was interrupted %d times without finishing", record.Update.Attempts))
	}

	if record.Update.Attempts == 0 {
		var args []string
		if free, ok := s.Host.HostFreeBytes(record.Distro); ok {
			args = append(args, "--host-free", strconv.FormatUint(free, 10))
		}
		result, err := s.run(ctx, record, record.StagedPayload, supervise.UpdateSnapshotCommand, args...)
		if err != nil {
			return s.settleFailed(ctx, record, "the database could not be backed up: "+err.Error())
		}
		if result.Outcome != supervise.UpdateOutcomeOK {
			return s.settleFailed(ctx, record, "the database could not be backed up: "+result.Reason)
		}
	}

	// Count the attempt durably before it starts, so a trial that kills the
	// machine is found counted by the next launch.
	next, err := record.State.Retry()
	if err != nil {
		return UpdateEnd{}, err
	}
	record.State = next
	if err := supervise.SaveLauncherRecord(s.RecordPath, record); err != nil {
		return UpdateEnd{}, err
	}
	attempt := record.Update.Attempts
	result, err := s.run(ctx, record, record.StagedPayload, supervise.UpdateTrialRunCommand,
		"--to", record.Update.To, "--attempt", strconv.Itoa(attempt))
	if err != nil {
		// A command stopped for stalling may still report a decided
		// trial on its way out. Anything else it reports is the stop.
		var stopped *UpdateCommandStoppedError
		if !errors.As(err, &stopped) || stopped.Result == nil ||
			(stopped.Result.Outcome != supervise.UpdateOutcomePrepared && stopped.Result.Outcome != supervise.UpdateOutcomeRolledBack) {
			return s.rollBack(ctx, record, err.Error())
		}
		result = *stopped.Result
	}
	switch result.Outcome {
	case supervise.UpdateOutcomePrepared:
		next, err := record.State.Settle(supervise.UpdateCommitted, "", s.now())
		if err != nil {
			return UpdateEnd{}, err
		}
		record.State = next
		if err := supervise.SaveLauncherRecord(s.RecordPath, record); err != nil {
			return UpdateEnd{}, err
		}
		return s.commit(ctx, record)
	case supervise.UpdateOutcomeRolledBack:
		return s.settleRolledBack(ctx, record, result.Reason)
	case supervise.UpdateOutcomeRefused, supervise.UpdateOutcomeNoSnapshot:
		// The command changed nothing. On the first attempt the database
		// is the one the snapshot copied, so nothing of the target's ran.
		if attempt <= 1 {
			return s.settleFailed(ctx, record, result.Reason)
		}
		return s.rollBack(ctx, record, result.Reason)
	default:
		return s.rollBack(ctx, record, result.Reason)
	}
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
	update := record.Update
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
		s.discard(ctx, record, record.StablePayload)
		decision, err := s.markReported(record)
		if err != nil {
			return ReconcileDecision{}, err
		}
		decision.UpdatingTo = update.To
		return decision, nil
	case supervise.UpdateRolledBack, supervise.UpdateFailed:
		if update.Reported {
			return ReconcileDecision{Action: ReconcileLaunch, Record: record}, nil
		}
		s.discard(ctx, record, record.StablePayload)
		s.removeStagedPayload(ctx, record)
		return s.markReported(record)
	}

	switch {
	case update.Attempts == 0:
		end, err := s.settleFailed(ctx, record, "the update was interrupted before its trial started")
		if err != nil {
			return ReconcileDecision{}, err
		}
		return s.afterRecovery(end)
	case update.Attempts < supervise.TrialAttemptLimit && fileExists(record.StagedLauncher):
		return ReconcileDecision{Action: ReconcileHandOff, Record: record}, nil
	default:
		reason := fmt.Sprintf("the trial was interrupted %d times without finishing", update.Attempts)
		if update.Attempts < supervise.TrialAttemptLimit {
			reason = "the update was interrupted and its new launcher is missing"
		}
		end, err := s.rollBack(ctx, record, reason)
		if err != nil {
			return ReconcileDecision{}, err
		}
		return s.afterRecovery(end)
	}
}

// afterRecovery launches once a recovery settled the update, and blocks
// when it could not.
func (s UpdateSequence) afterRecovery(end UpdateEnd) (ReconcileDecision, error) {
	record, _, err := supervise.LoadLauncherRecord(s.RecordPath)
	if err != nil {
		return ReconcileDecision{}, err
	}
	if !end.Settled() {
		return ReconcileDecision{Action: ReconcileBlocked, Record: record, Reason: end.Reason}, nil
	}
	return s.markReported(record)
}

// markReported records that a launch acted on the settled record and removes
// the launcher's residue.
func (s UpdateSequence) markReported(record supervise.LauncherRecord) (ReconcileDecision, error) {
	if err := s.Host.RemoveLauncherResidue(record); err != nil {
		s.logf("updater: remove the update's launcher files: %v", err)
	}
	next, changed, err := record.State.MarkReported()
	if err != nil {
		return ReconcileDecision{}, err
	}
	if changed {
		record.State = next
		if err := supervise.SaveLauncherRecord(s.RecordPath, record); err != nil {
			return ReconcileDecision{}, err
		}
	}
	return ReconcileDecision{Action: ReconcileLaunch, Record: record}, nil
}

// commit publishes a committed update. Every step is idempotent, so a commit
// interrupted anywhere is finished by repeating it.
func (s UpdateSequence) commit(ctx context.Context, record supervise.LauncherRecord) (UpdateEnd, error) {
	s.step("update.commit", "Installing "+startupprogress.DisplayVersion(record.Update.To))
	s.discard(ctx, record, record.StagedPayload)
	if err := s.Host.InvalidatePayloadRecord(record); err != nil {
		return UpdateEnd{}, fmt.Errorf("invalidate the installed payload record: %w", err)
	}
	if err := s.Host.CommitPayload(ctx, record); err != nil {
		return UpdateEnd{}, fmt.Errorf("install the new backend: %w", err)
	}
	if err := s.Host.RecordPayload(record); err != nil {
		return UpdateEnd{}, fmt.Errorf("record the new backend: %w", err)
	}
	if err := s.Host.PublishLauncher(record); err != nil {
		return UpdateEnd{}, fmt.Errorf("install the new launcher: %w", err)
	}
	return UpdateEnd{State: supervise.UpdateCommitted}, nil
}

// rollBack restores the database through the stable payload, the version
// that runs next, and settles rolled-back. A restore that does not finish
// leaves the record pending: nothing may start on a database in an unknown
// state, and the next launch tries again.
func (s UpdateSequence) rollBack(ctx context.Context, record supervise.LauncherRecord, reason string) (UpdateEnd, error) {
	s.step("update.restore", "Restoring the previous version")
	result, err := s.run(ctx, record, record.StablePayload, supervise.UpdateRestoreCommand, "--reason", reason)
	if err == nil && result.Outcome == supervise.UpdateOutcomeOK {
		return s.settleRolledBack(ctx, record, reason)
	}
	cause := result.Reason
	if err != nil {
		cause = err.Error()
	}
	s.logf("updater: update %s: restore failed: %s", record.Update.ID, cause)
	return UpdateEnd{State: supervise.UpdatePending, Reason: fmt.Sprintf(
		"The update to %s did not finish (%s), and the database backup could not be restored: %s. Start Agent Overflow again to retry.",
		startupprogress.DisplayVersion(record.Update.To), reason, cause)}, nil
}

func (s UpdateSequence) settleRolledBack(ctx context.Context, record supervise.LauncherRecord, reason string) (UpdateEnd, error) {
	return s.settle(ctx, record, supervise.UpdateRolledBack, reason)
}

func (s UpdateSequence) settleFailed(ctx context.Context, record supervise.LauncherRecord, reason string) (UpdateEnd, error) {
	return s.settle(ctx, record, supervise.UpdateFailed, reason)
}

// settle records an update that ends on the previous version, then removes
// what it left behind.
func (s UpdateSequence) settle(ctx context.Context, record supervise.LauncherRecord, state supervise.UpdateState, reason string) (UpdateEnd, error) {
	next, err := record.State.Settle(state, reason, s.now())
	if err != nil {
		return UpdateEnd{}, err
	}
	record.State = next
	if err := supervise.SaveLauncherRecord(s.RecordPath, record); err != nil {
		return UpdateEnd{}, err
	}
	s.logf("updater: update %s to %s %s: %s", record.Update.ID, record.Update.To, state, reason)
	s.discard(ctx, record, record.StablePayload)
	s.removeStagedPayload(ctx, record)
	return UpdateEnd{State: state, Reason: reason}, nil
}

// discard removes the snapshot. A failure costs disk until the next update
// replaces the snapshot, so it is logged.
func (s UpdateSequence) discard(ctx context.Context, record supervise.LauncherRecord, payload string) {
	result, err := s.run(ctx, record, payload, supervise.UpdateDiscardCommand)
	switch {
	case err != nil:
		s.logf("updater: update %s: discard the database backup: %v", record.Update.ID, err)
	case result.Outcome != supervise.UpdateOutcomeOK:
		s.logf("updater: update %s: discard the database backup: %s: %s", record.Update.ID, result.Outcome, result.Reason)
	}
}

func (s UpdateSequence) removeStagedPayload(ctx context.Context, record supervise.LauncherRecord) {
	if err := s.Host.RemoveStagedPayload(ctx, record); err != nil {
		s.logf("updater: update %s: remove the staged backend: %v", record.Update.ID, err)
	}
}

func (s UpdateSequence) run(ctx context.Context, record supervise.LauncherRecord, payload, command string, args ...string) (supervise.UpdateEvent, error) {
	full := append([]string{"--id", record.Update.ID}, args...)
	return s.Host.RunCommand(ctx, record.Distro, payload, command, full, s.Progress)
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
