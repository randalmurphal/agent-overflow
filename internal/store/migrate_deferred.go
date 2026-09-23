package store

import (
	"context"
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
// A phase is idempotent and keeps its progress in the data. A quit or a
// returned error leaves the watermark where it was, and the next open resumes
// from what is left. A phase does not fail on one bad row: it reports the row
// once, leaves it, and finishes, so no open repeats the same failure. Unlike
// migration SQL, a phase is live code that runs against the current schema,
// so it has to keep working as the schema moves.
type DeferredMigration struct {
	// Name identifies the phase in logs and in the frozen migration hash.
	Name string
	// Run does the work, calling pause between transactions. It returns nil
	// when it finished or ctx was cancelled.
	Run func(ctx context.Context, s *Store, pause ChunkPause) error
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

// RunDeferredMigrations runs the pending deferred phases in version order,
// calling pause between their transactions, and moves the watermark past each
// phase that finishes. Cancelling ctx stops at the next transaction boundary
// and is not an error. A phase that returns an error stops the run with the
// watermark unchanged, and the error is returned.
func (s *Store) RunDeferredMigrations(ctx context.Context, pause ChunkPause) error {
	return s.runDeferredMigrations(ctx, pause, migrations)
}

func (s *Store) runDeferredMigrations(ctx context.Context, pause ChunkPause, chain []Migration) error {
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
		if err := m.Deferred.Run(ctx, s, pause); err != nil {
			return fmt.Errorf("store: deferred migration v%d (%s): %w", m.Version, m.Deferred.Name, err)
		}
		if ctx.Err() != nil {
			log.Printf("store: deferred migration v%d (%s) stopped after %s; the next open resumes it",
				m.Version, m.Deferred.Name, time.Since(start).Round(time.Millisecond))
			return nil
		}
		recorded, err := s.recordDeferredWatermark(before.ReplicaGeneration, m.Version)
		if err != nil {
			return err
		}
		if !recorded {
			log.Printf("store: deferred migration v%d (%s): the database was restored while it ran; running it on the restored rows",
				m.Version, m.Deferred.Name)
			continue
		}
		log.Printf("store: deferred migration v%d (%s) finished in %s",
			m.Version, m.Deferred.Name, time.Since(start).Round(time.Millisecond))
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

// recordDeferredWatermark moves the watermark to version unless the replica
// generation is no longer generation. The check and the write share one
// writer transaction, so a restore commits entirely before or after it.
func (s *Store) recordDeferredWatermark(generation string, version int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin deferred migration watermark: %w", err)
	}
	defer tx.Rollback()
	current, err := identityFrom(tx)
	if err != nil {
		return false, err
	}
	if current.ReplicaGeneration != generation {
		return false, nil
	}
	if err := writeDeferredWatermark(tx, version); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit deferred migration watermark %d: %w", version, err)
	}
	return true, nil
}
