package wsllauncher

import (
	"context"
	"fmt"
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
func (s UpdateSequence) Migrate(ctx context.Context, req MigrationRequest) supervise.MigrationEnd {
	run := s.run(&supervise.LauncherRecord{})
	if !req.Retry {
		if end, remembered := run.RememberedMigration(req.Version, req.Schema); remembered {
			return end
		}
	}
	s.step("update.migrate", "Preparing to upgrade the database")
	id, err := supervise.NewUpdateID()
	if err == nil {
		_, err = BeginLauncherMigration(s.RecordPath, req.Distro, req.Payload, req.Version, req.Schema, id, s.now())
	}
	if err != nil {
		s.logf("updater: open the database migration: %v", err)
		return run.MigrationNotStarted()
	}
	s.logf("updater: migration %s: the backend at %s refused to migrate its database live; migrating it through a trial", id, req.Payload)
	end, err := s.Apply(ctx, id)
	return run.FinishMigration(id, end, err)
}

// ResumeMigration continues a pending migration Reconcile found
// (ReconcileResume), from its durable state.
func (s UpdateSequence) ResumeMigration(ctx context.Context, record supervise.LauncherRecord) supervise.MigrationEnd {
	s.step("update.migrate", "Resuming the database upgrade")
	s.logf("updater: migration %s: resuming after %d attempts", record.Update.ID, record.Update.Attempts)
	end, err := s.Apply(ctx, record.Update.ID)
	return s.run(&record).FinishMigration(record.Update.ID, end, err)
}

// reconcileMigration is the recovery table for a migration record
// (supervise.UpdateRun.RecoverMigration). One with attempts resumes in this
// launcher.
func (s UpdateSequence) reconcileMigration(ctx context.Context, record supervise.LauncherRecord) (ReconcileDecision, error) {
	resume, err := s.run(&record).RecoverMigration(ctx, record.State)
	if err != nil {
		return ReconcileDecision{}, err
	}
	if resume {
		return ReconcileDecision{Action: ReconcileResume, Record: record}, nil
	}
	return ReconcileDecision{Action: ReconcileLaunch}, nil
}
