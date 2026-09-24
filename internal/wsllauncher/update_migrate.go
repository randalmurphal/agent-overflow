package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/supervise"
)

// The no-live-migration gate (docs/specs/app-update.md): a database is never
// migrated live without a snapshot, however the payload got there. The
// launcher starts its backend with RefusePendingMigrationsFlag. A backend
// whose database has migrations pending answers MigrationsPendingError and
// starts nothing; the launcher stops it, migrates the database through a
// snapshot and a trial of the same payload (a migration record, State.
// BeginMigration), and starts it again on a database with nothing pending.
// This covers every way a new payload reaches a database without an update
// trial: an update from a launcher that predates the trial flow, a second
// distribution after an update, and a launcher replaced by hand.

// RefusePendingMigrationsArgs is the backend argv for the gate.
func RefusePendingMigrationsArgs() []string {
	return []string{"--" + RefusePendingMigrationsFlag}
}

// MigrationRequest is one launch's migration: the backend at Payload in
// Distro, of version Version, refused to migrate its database, which is at
// migration version Schema.
type MigrationRequest struct {
	Distro, Payload, Version string
	Schema                   int
	// Retry runs the migration even when the failure memory holds the same
	// build's failed trial over this schema version: the person asked for
	// it from the failure page.
	Retry bool
}

// MigrationEnd is what a launch does after a migration.
type MigrationEnd struct {
	// Launch is true once the migration committed: the backend starts on a
	// database with nothing pending.
	Launch bool
	// Title and Detail are the failure page's copy when Launch is false.
	Title, Detail string
	// Retry is true when the failure memory stopped the migration: the
	// page offers to run it again (MigrationRequest.Retry).
	Retry bool
}

// BeginLauncherMigration records durably that the database of the backend at
// payload, version version, at migration version schema, is migrated
// through a trial before it starts. A pending record refuses it, as for
// BeginLauncherUpdate; a settled one is replaced.
func BeginLauncherMigration(recordPath, distro, payload, version string, schema int, id string, now time.Time) (supervise.LauncherRecord, error) {
	existing, found, err := supervise.LoadLauncherRecord(recordPath)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	if found && !existing.Update.Settled() {
		return supervise.LauncherRecord{}, fmt.Errorf("the update to %s is still in progress", existing.Update.To)
	}
	base, err := supervise.Adopt(version)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	state, err := base.BeginMigration(id, now)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	update := *state.Update
	update.FromSchema = schema
	state.Update = &update
	record := supervise.LauncherRecord{State: state, Distro: distro, StablePayload: payload}
	if err := supervise.SaveLauncherRecord(recordPath, record); err != nil {
		return supervise.LauncherRecord{}, err
	}
	return record, nil
}

// Migrate migrates the database of the backend that refused to migrate it
// live and has stopped: snapshot, the trial, then commit, or rollback
// through the same payload. The record exists only while the migration is
// pending. Unless the request is a Retry, a remembered failed trial of the
// same build over the same schema version stops it before anything runs.
func (s UpdateSequence) Migrate(ctx context.Context, req MigrationRequest) MigrationEnd {
	if !req.Retry {
		if failed, ok := s.rememberedFailure(req.Version, req.Schema); ok {
			s.logf("updater: the database upgrade of %s over schema v%d failed before (%s); it runs again on Retry",
				req.Version, req.Schema, failed.Reason)
			return rememberedMigrationEnd(failed)
		}
	}
	s.step("update.migrate", "Preparing to upgrade the database")
	id, err := NewUpdateID()
	if err == nil {
		_, err = BeginLauncherMigration(s.RecordPath, req.Distro, req.Payload, req.Version, req.Schema, id, s.now())
	}
	if err != nil {
		s.logf("updater: open the database migration: %v", err)
		return MigrationEnd{
			Title:  "Agent Overflow could not start the database upgrade this version needs.",
			Detail: "Nothing was started, so the data is left as it is. Details are in the launcher log.",
		}
	}
	s.logf("updater: migration %s: the backend at %s refused to migrate its database live; migrating it through a trial", id, req.Payload)
	end, err := s.Apply(ctx, id)
	return s.finishMigration(id, end, err)
}

// ResumeMigration continues a pending migration Reconcile found
// (ReconcileResume), from its durable state.
func (s UpdateSequence) ResumeMigration(ctx context.Context, record supervise.LauncherRecord) MigrationEnd {
	s.step("update.migrate", "Resuming the database upgrade")
	s.logf("updater: migration %s: resuming after %d attempts", record.Update.ID, record.Update.Attempts)
	end, err := s.Apply(ctx, record.Update.ID)
	return s.finishMigration(record.Update.ID, end, err)
}

// finishMigration removes a settled migration's record and says what the
// launch does next. A record that cannot be read or written, or a restore
// that did not finish, leaves the record for the next launch to recover.
func (s UpdateSequence) finishMigration(id string, end UpdateEnd, err error) MigrationEnd {
	switch {
	case err != nil:
		s.logf("updater: migration %s: %v", id, err)
		return MigrationEnd{
			Title:  "The database upgrade could not finish.",
			Detail: "Nothing was started. Start Agent Overflow again to finish it. Details are in the launcher log.",
		}
	case !end.Settled():
		s.logf("updater: migration %s: %s", id, end.Reason)
		return MigrationEnd{
			Title:  "The database upgrade did not finish, and the database backup could not be restored.",
			Detail: "Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. Details are in the launcher log.",
		}
	}
	s.logf("updater: migration %s ended %s: %s", id, end.State, end.Reason)
	s.forgetMigration()
	switch end.State {
	case supervise.UpdateCommitted:
		return MigrationEnd{Launch: true}
	case supervise.UpdateRolledBack:
		return MigrationEnd{
			Title:  migrationFailedTitle,
			Detail: "The backup was restored, so the data is as it was. " + reasonSentence(end.Reason),
		}
	default:
		return MigrationEnd{
			Title:  migrationFailedTitle,
			Detail: "The upgrade did not run, so the data is as it was. " + reasonSentence(end.Reason),
		}
	}
}

// reconcileMigration is the recovery table for a migration record. A
// settled one only has its snapshot and record left to remove; one
// interrupted before its trial is settled and removed, and the gate starts
// a fresh migration; one with attempts resumes in this launcher.
func (s UpdateSequence) reconcileMigration(ctx context.Context, record supervise.LauncherRecord) (ReconcileDecision, error) {
	update := record.Update
	switch {
	case update.Settled():
		s.discard(ctx, record, record.StablePayload)
	case update.Attempts == 0:
		if _, err := s.settleFailed(ctx, record, "the database upgrade was interrupted before its trial started"); err != nil {
			return ReconcileDecision{}, err
		}
	default:
		return ReconcileDecision{Action: ReconcileResume, Record: record}, nil
	}
	s.forgetMigration()
	return ReconcileDecision{Action: ReconcileLaunch}, nil
}

// forgetMigration removes a settled migration's record. A launcher that
// predates migrations cannot read one, and the launch shows the outcome
// itself, so nothing needs it. A record left behind is removed by the next
// Reconcile.
func (s UpdateSequence) forgetMigration() {
	if err := os.Remove(s.RecordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logf("updater: remove the settled migration record: %v", err)
	}
}

// migrationFailedTitle heads the page of a migration that settled without
// committing.
const migrationFailedTitle = "This version of Agent Overflow could not upgrade the database."

// reasonSentence ends the page's detail with the recorded reason.
func reasonSentence(reason string) string {
	reason = boundedClause(reason)
	if reason == "" {
		return "Details are in the launcher log."
	}
	return "Reason: " + reason + ". Details are in the launcher log."
}
