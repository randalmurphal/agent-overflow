package supervise

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/startupprogress"
)

// The in-app update's sequence after the old version hands off
// (docs/specs/app-update.md): the snapshot on the first attempt, one trial,
// then commit or rollback, and the failure memory. UpdateRun owns that
// order, the recovery table's shared rows and the migration page copy for
// every platform. UpdateSteps is the platform's half: where the record is
// kept and how each step runs. The Windows launcher runs the steps as WSL
// commands (wsllauncher.UpdateSequence); the macOS and Linux helper runs
// them in its own process (DesktopUpdate).

// NewUpdateID returns a fresh update id. It names files, so it is lowercase
// hex.
func NewUpdateID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// ValidUpdateID reports whether id is one NewUpdateID could have made. Ids
// arrive on the command line and name files, so nothing else is accepted.
func ValidUpdateID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// UpdateSteps is a platform's half of an UpdateRun, for one record.
type UpdateSteps interface {
	// Save writes the record with state, durably.
	Save(state State) error
	// RemoveRecord deletes the record. Only a settled migration's record is
	// removed.
	RemoveRecord() error
	// Snapshot and Trial run UpdateCommand's Snapshot and TrialRun for the
	// update through the version whose trial runs: the target of an update,
	// the installed version of a migration. An error is a step that
	// reported no result.
	Snapshot(ctx context.Context, progress func(startupprogress.Progress)) (UpdateEvent, error)
	Trial(ctx context.Context, to string, attempt int, progress func(startupprogress.Progress)) (UpdateEvent, error)
	// Restore and Discard run UpdateCommand's Restore and Discard through
	// the version that runs next.
	Restore(ctx context.Context, reason string, progress func(startupprogress.Progress)) (UpdateEvent, error)
	Discard(ctx context.Context, progress func(startupprogress.Progress)) (UpdateEvent, error)
	// RemoveStaged deletes the staged target of an update if it is present.
	RemoveStaged(ctx context.Context) error
	// Publish installs a committed update's target and discards its
	// snapshot. Every step is idempotent, so a publish interrupted anywhere
	// is finished by repeating it.
	Publish(ctx context.Context) error
}

// UpdateEnd is where an update rests after a step of UpdateRun.
type UpdateEnd struct {
	// State is the record's state: committed, rolled-back or failed once
	// the update settled, pending when it could not be.
	State UpdateState
	// Reason is the settled reason, or what blocks a pending update.
	Reason string
}

// Settled reports whether the update reached a terminal state.
func (e UpdateEnd) Settled() bool { return e.State != UpdatePending }

// MigrationEnd is what a launch does after a migration.
type MigrationEnd struct {
	// Launch is true once the migration committed: the app starts on a
	// database with nothing pending.
	Launch bool
	// Title and Detail are the failure page's copy when Launch is false.
	Title, Detail string
	// Retry is true when the failure memory stopped the migration: the
	// page offers to run it again.
	Retry bool
}

// UpdateRun runs one record's update or migration.
type UpdateRun struct {
	Steps UpdateSteps
	// MemoryPath is the failure memory beside the record (FailedTrialPath).
	MemoryPath string
	// LogName names the log the pages point to, as in "Details are in the
	// launcher log."
	LogName string
	// Progress receives what the update is doing.
	Progress func(startupprogress.Progress)
	Now      func() time.Time
	Logf     func(string, ...any)
}

// Apply continues the update state holds: a pending one runs the snapshot
// on its first attempt, one trial, and the settlement it leads to; a
// committed one repeats the publish, which is idempotent; a settled one
// only reports where it rests. A trial that commits removes the failure
// memory; one that settles rolled back or failed is remembered for its
// target and the schema version its database started from. It returns an
// error when the record cannot be written, or when a publish step fails
// after the commit became durable; the next launch resumes either.
func (r UpdateRun) Apply(ctx context.Context, state State) (UpdateEnd, error) {
	switch state.Update.State {
	case UpdateCommitted:
		return r.Commit(ctx, state)
	case UpdateRolledBack, UpdateFailed:
		return UpdateEnd{State: state.Update.State, Reason: state.Update.Reason}, nil
	}
	trace := trialTrace{schema: state.Update.FromSchema}
	end, err := r.runTrials(ctx, state, &trace)
	if err == nil && (end.State == UpdateRolledBack || end.State == UpdateFailed) {
		r.rememberFailedTrial(state.Update.To, trace, end.Reason)
	}
	return end, err
}

// runTrials is Apply for a pending record. trace receives the schema
// version the snapshot reports and the last progress of the snapshot and
// trial steps.
func (r UpdateRun) runTrials(ctx context.Context, state State, trace *trialTrace) (UpdateEnd, error) {
	if state.Update.Attempts >= TrialAttemptLimit {
		return r.RollBack(ctx, state, fmt.Sprintf(
			"the trial was interrupted %d times without finishing", state.Update.Attempts))
	}

	if state.Update.Attempts == 0 {
		trace.phase = ""
		result, err := r.Steps.Snapshot(ctx, r.traced(trace))
		if result.Schema > 0 {
			trace.schema = result.Schema
			update := *state.Update
			update.FromSchema = result.Schema
			state.Update = &update
		}
		if err != nil {
			return r.SettleFailed(ctx, state, "the database could not be backed up: "+err.Error())
		}
		if result.Outcome != UpdateOutcomeOK {
			return r.SettleFailed(ctx, state, "the database could not be backed up: "+result.Reason)
		}
	}

	// Count the attempt durably before it starts, so a trial that kills the
	// machine is found counted by the next launch.
	next, err := state.Retry()
	if err != nil {
		return UpdateEnd{}, err
	}
	if err := r.Steps.Save(next); err != nil {
		return UpdateEnd{}, err
	}
	state = next
	attempt := state.Update.Attempts
	trace.phase = ""
	result, err := r.Steps.Trial(ctx, state.Update.To, attempt, r.traced(trace))
	if err != nil {
		return r.RollBack(ctx, state, err.Error())
	}
	switch result.Outcome {
	case UpdateOutcomePrepared:
		committed, err := state.Settle(UpdateCommitted, "", r.now())
		if err != nil {
			return UpdateEnd{}, err
		}
		if err := r.Steps.Save(committed); err != nil {
			return UpdateEnd{}, err
		}
		r.forgetFailedTrial()
		return r.Commit(ctx, committed)
	case UpdateOutcomeRolledBack:
		return r.settle(ctx, state, UpdateRolledBack, result.Reason)
	case UpdateOutcomeChanged:
		// Another backend used the database since the update last left
		// it. A restore would discard that work, at any attempt.
		return r.SettleFailed(ctx, state, result.Reason)
	case UpdateOutcomeRefused, UpdateOutcomeNoSnapshot:
		// The step changed nothing. On the first attempt the database is
		// the one the snapshot copied, so nothing of the target's ran.
		if attempt <= 1 {
			return r.SettleFailed(ctx, state, result.Reason)
		}
		return r.RollBack(ctx, state, result.Reason)
	default:
		return r.RollBack(ctx, state, result.Reason)
	}
}

// Commit finishes a committed update: a migration only discards its
// snapshot; an update publishes its target.
func (r UpdateRun) Commit(ctx context.Context, state State) (UpdateEnd, error) {
	if state.Migration() {
		r.Discard(ctx, state)
		return UpdateEnd{State: UpdateCommitted}, nil
	}
	r.step("update.commit", "Installing "+startupprogress.DisplayVersion(state.Update.To))
	if err := r.Steps.Publish(ctx); err != nil {
		return UpdateEnd{}, err
	}
	return UpdateEnd{State: UpdateCommitted}, nil
}

// RollBack restores the database through the version that runs next and
// settles rolled-back. A restore that does not finish leaves the record
// pending: nothing may start on a database in an unknown state, and the
// next launch tries again.
func (r UpdateRun) RollBack(ctx context.Context, state State, reason string) (UpdateEnd, error) {
	r.step(phaseRestore, "Restoring the previous version")
	result, err := r.Steps.Restore(ctx, reason, r.Progress)
	if err == nil && result.Outcome == UpdateOutcomeOK {
		return r.settle(ctx, state, UpdateRolledBack, reason)
	}
	cause := result.Reason
	if err != nil {
		cause = err.Error()
	}
	r.logf("updater: update %s: restore failed: %s", state.Update.ID, cause)
	return UpdateEnd{State: UpdatePending, Reason: fmt.Sprintf(
		"The update to %s did not finish (%s), and the database backup could not be restored: %s. Start Agent Overflow again to retry.",
		startupprogress.DisplayVersion(state.Update.To), reason, cause)}, nil
}

// SettleFailed settles an update that ends on the previous version without
// anything of the target's to undo.
func (r UpdateRun) SettleFailed(ctx context.Context, state State, reason string) (UpdateEnd, error) {
	return r.settle(ctx, state, UpdateFailed, reason)
}

// settle records an update that ends on the previous version, then removes
// what it left behind.
func (r UpdateRun) settle(ctx context.Context, state State, to UpdateState, reason string) (UpdateEnd, error) {
	next, err := state.Settle(to, reason, r.now())
	if err != nil {
		return UpdateEnd{}, err
	}
	if err := r.Steps.Save(next); err != nil {
		return UpdateEnd{}, err
	}
	r.logf("updater: update %s to %s %s: %s", next.Update.ID, next.Update.To, to, reason)
	r.Discard(ctx, next)
	r.RemoveStaged(ctx, next)
	return UpdateEnd{State: to, Reason: reason}, nil
}

// Discard removes the snapshot of the update state holds. A failure costs
// disk until the next update replaces the snapshot, so it is logged.
func (r UpdateRun) Discard(ctx context.Context, state State) {
	result, err := r.Steps.Discard(ctx, r.Progress)
	switch {
	case err != nil:
		r.logf("updater: update %s: discard the database backup: %v", state.Update.ID, err)
	case result.Outcome != UpdateOutcomeOK:
		r.logf("updater: update %s: discard the database backup: %s: %s", state.Update.ID, result.Outcome, result.Reason)
	}
}

// RemoveStaged deletes an update's staged target. A migration stages
// nothing. A failure costs disk and is logged.
func (r UpdateRun) RemoveStaged(ctx context.Context, state State) {
	if state.Migration() {
		return
	}
	if err := r.Steps.RemoveStaged(ctx); err != nil {
		r.logf("updater: update %s: remove the staged version: %v", state.Update.ID, err)
	}
}

// MarkReported records that a launch acted on the settled record.
func (r UpdateRun) MarkReported(state State) (State, error) {
	next, changed, err := state.MarkReported()
	if err != nil || !changed {
		return next, err
	}
	if err := r.Steps.Save(next); err != nil {
		return State{}, err
	}
	return next, nil
}

// RecoverPending is the recovery table's rows for a pending update no
// process is running. One interrupted before its first trial is settled
// failed. One below the attempt limit whose target is still staged
// (canResume) resumes: resume is true and the caller hands it to the
// target. Otherwise the database is restored and the update settles rolled
// back; one whose trial was interrupted at every attempt is remembered.
// stopped is the reason of one that cannot resume below the limit, as in
// "the update was interrupted and its new launcher is missing".
func (r UpdateRun) RecoverPending(ctx context.Context, state State, canResume bool, stopped string) (end UpdateEnd, resume bool, err error) {
	update := state.Update
	switch {
	case update.Attempts == 0:
		end, err = r.SettleFailed(ctx, state, "the update was interrupted before its trial started")
		return end, false, err
	case update.Attempts < TrialAttemptLimit && canResume:
		return UpdateEnd{State: UpdatePending}, true, nil
	}
	exhausted := update.Attempts >= TrialAttemptLimit
	reason := fmt.Sprintf("the trial was interrupted %d times without finishing", update.Attempts)
	if !exhausted {
		reason = stopped
	}
	end, err = r.RollBack(ctx, state, reason)
	if err == nil && exhausted && end.State == UpdateRolledBack {
		r.rememberFailedTrial(update.To, trialTrace{schema: update.FromSchema}, end.Reason)
	}
	return end, false, err
}

// RecoverMigration is the recovery table's rows for a migration record. A
// settled one only has its snapshot left to remove; one interrupted before
// its trial is settled, and the gate starts a fresh migration; one with
// attempts resumes, and the caller continues it. A record that does not
// resume is removed.
func (r UpdateRun) RecoverMigration(ctx context.Context, state State) (resume bool, err error) {
	update := state.Update
	switch {
	case update.Settled():
		r.Discard(ctx, state)
	case update.Attempts == 0:
		if _, err := r.SettleFailed(ctx, state, "the database upgrade was interrupted before its trial started"); err != nil {
			return false, err
		}
	default:
		return true, nil
	}
	r.removeMigration()
	return false, nil
}

// RememberedMigration is the failure page for a migration of build over a
// database at schema that the failure memory stops: ok is true when the
// memory holds the same build's failed trial over that schema version. The
// page offers Retry.
func (r UpdateRun) RememberedMigration(build string, schema int) (MigrationEnd, bool) {
	failed, ok := r.rememberedFailure(build, schema)
	if !ok {
		return MigrationEnd{}, false
	}
	r.logf("updater: the database upgrade of %s over schema v%d failed before (%s); it runs again on Retry",
		build, schema, failed.Reason)
	where := "The last attempt failed."
	if phase := r.boundedClause(failed.Phase); phase != "" {
		where = "The last attempt stopped at: " + phase + "."
	}
	return MigrationEnd{
		Title:  MigrationFailedTitle,
		Detail: where + " It does not run again on its own, so the data is as it was. " + r.reasonSentence(failed.Reason),
		Retry:  true,
	}, true
}

// MigrationNotStarted is the page of a migration whose record could not be
// opened.
func (r UpdateRun) MigrationNotStarted() MigrationEnd {
	return MigrationEnd{
		Title:  "Agent Overflow could not start the database upgrade this version needs.",
		Detail: "Nothing was started, so the data is left as it is. " + r.detailsSentence(),
	}
}

// FinishMigration removes a settled migration's record and says what the
// launch does next. A record that cannot be read or written, or a restore
// that did not finish, leaves the record for the next launch to recover.
func (r UpdateRun) FinishMigration(id string, end UpdateEnd, err error) MigrationEnd {
	switch {
	case err != nil:
		r.logf("updater: migration %s: %v", id, err)
		return MigrationEnd{
			Title:  "The database upgrade could not finish.",
			Detail: "Nothing was started. Start Agent Overflow again to finish it. " + r.detailsSentence(),
		}
	case !end.Settled():
		r.logf("updater: migration %s: %s", id, end.Reason)
		return MigrationEnd{
			Title:  "The database upgrade did not finish, and the database backup could not be restored.",
			Detail: "Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. " + r.detailsSentence(),
		}
	}
	r.logf("updater: migration %s ended %s: %s", id, end.State, end.Reason)
	r.removeMigration()
	switch end.State {
	case UpdateCommitted:
		return MigrationEnd{Launch: true}
	case UpdateRolledBack:
		return MigrationEnd{
			Title:  MigrationFailedTitle,
			Detail: "The backup was restored, so the data is as it was. " + r.reasonSentence(end.Reason),
		}
	default:
		return MigrationEnd{
			Title:  MigrationFailedTitle,
			Detail: "The upgrade did not run, so the data is as it was. " + r.reasonSentence(end.Reason),
		}
	}
}

// removeMigration removes a settled migration's record. A version that
// predates migrations cannot read one, and the launch shows the outcome
// itself, so nothing needs it. A record left behind is removed by the next
// recovery.
func (r UpdateRun) removeMigration() {
	if err := r.Steps.RemoveRecord(); err != nil {
		r.logf("updater: remove the settled migration record: %v", err)
	}
}

// MigrationFailedTitle heads the page of a migration that settled without
// committing, or that the failure memory stopped.
const MigrationFailedTitle = "This version of Agent Overflow could not upgrade the database."

// pageClauseLimit bounds recorded text a page repeats.
const pageClauseLimit = 512

// reasonSentence ends a page's detail with the recorded reason.
func (r UpdateRun) reasonSentence(reason string) string {
	reason = r.boundedClause(reason)
	if reason == "" {
		return r.detailsSentence()
	}
	return "Reason: " + reason + ". " + r.detailsSentence()
}

func (r UpdateRun) detailsSentence() string {
	return "Details are in the " + r.LogName + "."
}

// boundedClause is text for a page: trimmed, without a closing period, and
// at most pageClauseLimit characters.
func (UpdateRun) boundedClause(text string) string {
	text = strings.TrimSuffix(strings.TrimSpace(text), ".")
	if runes := []rune(text); len(runes) > pageClauseLimit {
		text = string(runes[:pageClauseLimit-1]) + "…"
	}
	return text
}

// traced is the progress sink of a snapshot or trial step, whose failure is
// remembered: trace keeps the last progress it reports.
func (r UpdateRun) traced(trace *trialTrace) func(startupprogress.Progress) {
	return func(p startupprogress.Progress) {
		trace.observe(p)
		if r.Progress != nil {
			r.Progress(p)
		}
	}
}

func (r UpdateRun) step(phase, detail string) {
	if r.Progress == nil {
		return
	}
	now := r.now().UnixMilli()
	r.Progress(startupprogress.Progress{Phase: phase, Detail: detail, StartedAt: now, UpdatedAt: now})
}

func (r UpdateRun) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r UpdateRun) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
