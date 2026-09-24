package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// DeferredMigration is the part of a one-time data fix that is too long to
// run while the store opens. The chain applies its migration's SQL and
// records the version as usual; the phase runs after open, through
// RunDeferredMigrations, as paced transactions that each stay inside the
// background-maintenance stall budget (docs/decisions.md).
//
// The gate is PRAGMA user_version, the deferred watermark: every phase of a
// migration at or below it has finished. The chain's own version rows cannot
// carry it. The chain treats MAX(version) as applied, so a phase recorded
// when it finished would be skipped once a later migration recorded, and
// holding later migrations back until it finished would run the app on a
// schema they had not reached. The watermark sits in the database header, so
// with nothing pending the check is one header read and an integer
// comparison. A database the chain creates has nothing for a phase to fix,
// so its watermark starts at the latest phase.
//
// A phase's steps are idempotent and keep their progress in the data. A run
// that fails on an item reports it with its id, leaves it as it is and goes
// on, so one bad item cannot stop the rest of the work. A run that left
// failed items does not move the watermark: it records how many failed and
// the first error in deferred_migration_failures, in the same database as
// the watermark, and the next open runs the phase again, which finds only
// the work still left. There is no attempt cap. A quit leaves the watermark
// and the record as they were. Unlike migration SQL, a step is live code that
// runs against the current schema, so it has to keep working as the schema
// moves.
type DeferredMigration struct {
	// Title names the phase to the user when a run leaves items unfinished.
	Title string
	// Steps run in order on every run of the phase.
	Steps []DeferredStep
}

// DeferredStep is one part of a deferred phase.
type DeferredStep struct {
	// Name identifies the step in logs and in the frozen migration hash.
	Name string
	// Run does the work through run: it pauses between transactions and
	// reports each item it leaves in place. It returns nil when it finished
	// or ctx was cancelled. An error means the step could not go on, and
	// counts as one failed item.
	Run func(ctx context.Context, s *Store, run *deferredRun) error
}

// DeferredHost is what a deferred run needs from the process running it.
type DeferredHost struct {
	// Pause runs between two transactions. Nil means no pause.
	Pause ChunkPause
	// AwaitFileSwap blocks until the database file may be replaced under
	// the running process (ConvertToIncrementalVacuum). It returns nil when
	// it may, ctx.Err() once ctx ends, and any other error when it cannot
	// tell. Nil means at once.
	AwaitFileSwap func(ctx context.Context) error
}

// deferredRun is one run of a phase's steps.
type deferredRun struct {
	host     DeferredHost
	failures int
	first    error
}

func (r *deferredRun) pause() {
	if r.host.Pause != nil {
		r.host.Pause()
	}
}

func (r *deferredRun) awaitFileSwap(ctx context.Context) error {
	if r.host.AwaitFileSwap == nil {
		return ctx.Err()
	}
	return r.host.AwaitFileSwap(ctx)
}

// fail reports an item the run leaves in place. err names the item.
func (r *deferredRun) fail(err error) {
	r.failures++
	if r.first == nil {
		r.first = err
	}
	log.Printf("store: deferred migration: %v", err)
}

// DeferredFailure is what the last run of a pending deferred phase left
// unfinished.
type DeferredFailure struct {
	Version int
	// Title names the phase to the user.
	Title string
	// Failures counts the items the run left in place.
	Failures int
	// FirstError is the first of their errors.
	FirstError string
}

// latestDeferredVersion is the version of the last migration that carries a
// deferred phase, or 0.
var latestDeferredVersion = func() int {
	latest := 0
	for _, m := range migrations {
		if m.Deferred != nil {
			latest = m.Version
		}
	}
	return latest
}()

func readDeferredWatermark(q sqlQueryer) (int, error) {
	var watermark int
	if err := q.QueryRow(`PRAGMA user_version`).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("store: read deferred migration watermark: %w", err)
	}
	return watermark, nil
}

func writeDeferredWatermark(exec sqlExecutor, version int) error {
	// PRAGMA arguments cannot be bound.
	if _, err := exec.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return fmt.Errorf("store: record deferred migration watermark %d: %w", version, err)
	}
	return nil
}

// DeferredMigrationsPending reports whether a deferred migration phase has
// not finished on this database.
func (s *Store) DeferredMigrationsPending() (bool, error) {
	watermark, err := readDeferredWatermark(s.db)
	if err != nil {
		return false, err
	}
	return watermark < latestDeferredVersion, nil
}

// DeferredMigrationFailure returns what the last run of a pending phase left
// unfinished, or nil when no pending phase has recorded a failed run.
func (s *Store) DeferredMigrationFailure() (*DeferredFailure, error) {
	return s.deferredMigrationFailure(migrations)
}

func (s *Store) deferredMigrationFailure(chain []Migration) (*DeferredFailure, error) {
	watermark, err := readDeferredWatermark(s.db)
	if err != nil {
		return nil, err
	}
	var failure DeferredFailure
	err = s.db.QueryRow(`SELECT version, failures, first_error FROM deferred_migration_failures
 WHERE version > ? ORDER BY version LIMIT 1`, watermark).Scan(&failure.Version, &failure.Failures, &failure.FirstError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read deferred migration failure: %w", err)
	}
	for _, m := range chain {
		if m.Version == failure.Version && m.Deferred != nil {
			failure.Title = m.Deferred.Title
		}
	}
	return &failure, nil
}

// RunDeferredMigrations runs the pending deferred phases in version order
// and moves the watermark past each phase whose run finished every item. A
// run that left items unfinished records them (DeferredMigrationFailure) and
// stops; the next call runs that phase again. Cancelling ctx stops at the
// next transaction boundary, records nothing and is not an error. An error
// is returned only when the watermark or the record cannot be read or
// written.
func (s *Store) RunDeferredMigrations(ctx context.Context, host DeferredHost) error {
	return s.runDeferredMigrations(ctx, host, migrations)
}

func (s *Store) runDeferredMigrations(ctx context.Context, host DeferredHost, chain []Migration) error {
	s.deferredMu.Lock()
	defer s.deferredMu.Unlock()
	for ctx.Err() == nil {
		watermark, err := readDeferredWatermark(s.db)
		if err != nil {
			return err
		}
		m := nextDeferredMigration(chain, watermark)
		if m == nil {
			return nil
		}
		// RestoreFrom replaces the rows, takes the watermark from the
		// snapshot and re-mints the replica generation. A phase that ran
		// across a restore did not see all of the restored rows, so it is
		// not recorded and runs again from the restored watermark.
		before, err := identityFrom(s.db)
		if err != nil {
			return err
		}
		start := time.Now()
		run := &deferredRun{host: host}
		for _, step := range m.Deferred.Steps {
			if ctx.Err() != nil {
				break
			}
			if err := step.Run(ctx, s, run); err != nil && ctx.Err() == nil {
				run.fail(fmt.Errorf("v%d %s: %w", m.Version, step.Name, err))
			}
		}
		if ctx.Err() != nil {
			log.Printf("store: deferred migration v%d (%s) stopped after %s; the next open resumes it",
				m.Version, m.Deferred.Title, time.Since(start).Round(time.Millisecond))
			return nil
		}
		recorded, err := s.recordDeferredRun(before.ReplicaGeneration, m.Version, run)
		if err != nil {
			return err
		}
		if !recorded {
			log.Printf("store: deferred migration v%d (%s): the database was restored while it ran; running it on the restored rows",
				m.Version, m.Deferred.Title)
			continue
		}
		if run.failures > 0 {
			log.Printf("store: deferred migration v%d (%s) left %d failed items after %s; the next open retries them",
				m.Version, m.Deferred.Title, run.failures, time.Since(start).Round(time.Millisecond))
			return nil
		}
		log.Printf("store: deferred migration v%d (%s) finished in %s",
			m.Version, m.Deferred.Title, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// nextDeferredMigration returns the first migration in chain whose deferred
// phase is above watermark, or nil.
func nextDeferredMigration(chain []Migration, watermark int) *Migration {
	for i := range chain {
		if chain[i].Deferred != nil && chain[i].Version > watermark {
			return &chain[i]
		}
	}
	return nil
}

// recordDeferredRun records the outcome of a run of version's phase unless
// the replica generation is no longer generation. A run that finished every
// item moves the watermark to version and clears the failure record; one
// that did not records its failures and leaves the watermark. The check and
// the write share one writer transaction, so a restore commits entirely
// before or after it.
func (s *Store) recordDeferredRun(generation string, version int, run *deferredRun) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin deferred migration record: %w", err)
	}
	defer tx.Rollback()
	current, err := identityFrom(tx)
	if err != nil {
		return false, err
	}
	if current.ReplicaGeneration != generation {
		return false, nil
	}
	if run.failures == 0 {
		if err := writeDeferredWatermark(tx, version); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM deferred_migration_failures WHERE version <= ?`, version); err != nil {
			return false, fmt.Errorf("store: clear deferred migration v%d failures: %w", version, err)
		}
	} else if _, err := tx.Exec(`INSERT INTO deferred_migration_failures (version, failures, first_error, failed_at)
 VALUES (?, ?, ?, ?)
 ON CONFLICT(version) DO UPDATE SET
     failures = excluded.failures,
     first_error = excluded.first_error,
     failed_at = excluded.failed_at`, version, run.failures, run.first.Error(), time.Now().UnixMilli()); err != nil {
		return false, fmt.Errorf("store: record deferred migration v%d failures: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit deferred migration v%d record: %w", version, err)
	}
	return true, nil
}
