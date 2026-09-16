package store

import (
	"context"
	"fmt"
	"log"
	"time"
)

// AutoVacuumMode is the database file's auto_vacuum setting. It is a
// property of the file, fixed when the first table is created, and
// changeable afterwards only by rebuilding the file
// (ConvertToIncrementalVacuum).
type AutoVacuumMode int

const (
	// AutoVacuumNone leaves freed pages on the freelist. Later writes
	// reuse them, so the file never grows unnecessarily, but it also
	// never shrinks. Databases created before incremental auto-vacuum
	// was adopted are in this mode.
	AutoVacuumNone AutoVacuumMode = 0
	// AutoVacuumFull moves pages out of the file at every commit. Never
	// used here: it puts the work on the writing transaction.
	AutoVacuumFull AutoVacuumMode = 1
	// AutoVacuumIncremental keeps the pointer map that lets
	// PRAGMA incremental_vacuum shrink the file in caller-sized chunks.
	AutoVacuumIncremental AutoVacuumMode = 2
)

func (m AutoVacuumMode) String() string {
	switch m {
	case AutoVacuumNone:
		return "none"
	case AutoVacuumFull:
		return "full"
	case AutoVacuumIncremental:
		return "incremental"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// AutoVacuumMode reports the database file's auto_vacuum setting.
func (s *Store) AutoVacuumMode() (AutoVacuumMode, error) {
	var mode int64
	if err := s.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return AutoVacuumNone, fmt.Errorf("store: read auto_vacuum: %w", err)
	}
	return AutoVacuumMode(mode), nil
}

// Free-space reclamation thresholds. Both must hold before the store
// bothers walking the freelist: the fraction keeps a mostly-live file
// from being trimmed to recover scraps, the absolute floor keeps small
// databases from doing this work at all. Freed pages are reused by later
// writes either way, so reclamation only ever shrinks the file and has
// no deadline.
const (
	reclaimMinFreelistFraction = 0.2
	reclaimMinFreelistBytes    = 64 << 20
)

const (
	// reclaimChunkPages is how many pages one incremental_vacuum step
	// moves. Measured on a 4.4 GB database: 128 pages costs at most
	// ~97 ms for the chunk itself and holds a concurrent writer at most
	// ~79 ms, while 200 pages already pushes writers past 100 ms. Do not
	// raise it.
	reclaimChunkPages = 128
	// reclaimChunkPause is the gap left between chunks so user writes
	// interleave. Each chunk is its own implicit transaction, so the
	// write lock is genuinely free during the pause.
	reclaimChunkPause = 100 * time.Millisecond
)

// ReclaimFreeSpace shrinks the database file by walking the freelist in
// bounded steps, and reports how many pages it returned to the
// filesystem.
//
// It runs only on an incremental auto-vacuum database whose freelist is
// past both thresholds above; on any other database it returns 0 and
// does nothing. `PRAGMA incremental_vacuum` on an auto_vacuum=none
// database is a silent no-op, so the mode check is what makes the
// difference between working and appearing to work.
//
// Every chunk is its own implicit transaction on the writer connection
// and the loop pauses `pause` between chunks, so no user write waits
// longer than one chunk and reads are not affected at all. Cancelling
// ctx stops the loop at the next chunk boundary and is not an error:
// the pages left on the freelist are reclaimed by a later call, and
// they are reused by writes in the meantime.
//
// Pass 0 for pause to use the default. It belongs on a background
// schedule, never on a user-facing path.
func (s *Store) ReclaimFreeSpace(ctx context.Context, pause time.Duration) (int64, error) {
	return s.reclaimFreeSpace(ctx, pause, reclaimMinFreelistBytes, reclaimMinFreelistFraction)
}

func (s *Store) reclaimFreeSpace(ctx context.Context, pause time.Duration, minFreeBytes int64, minFreeFraction float64) (int64, error) {
	if pause <= 0 {
		pause = reclaimChunkPause
	}
	mode, err := s.AutoVacuumMode()
	if err != nil {
		return 0, err
	}
	if mode != AutoVacuumIncremental {
		s.reclaimUnavailableOnce.Do(func() {
			log.Printf("store: free-space reclamation unavailable: auto_vacuum=%s; the file shrinks only after conversion to incremental", mode)
		})
		return 0, nil
	}

	pageSize, pageCount, freelist, err := s.freelistState()
	if err != nil {
		return 0, err
	}
	if pageCount == 0 ||
		freelist*pageSize < minFreeBytes ||
		float64(freelist)/float64(pageCount) < minFreeFraction {
		return 0, nil
	}

	var reclaimed int64
	for {
		if err := ctx.Err(); err != nil {
			return reclaimed, nil
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", reclaimChunkPages)); err != nil {
			if ctx.Err() != nil {
				return reclaimed, nil
			}
			return reclaimed, fmt.Errorf("store: incremental vacuum: %w", err)
		}
		_, _, remaining, err := s.freelistState()
		if err != nil {
			return reclaimed, err
		}
		if remaining >= freelist {
			// No progress: the freelist holds only pages incremental
			// vacuum cannot move right now. Stop rather than spin.
			return reclaimed, nil
		}
		reclaimed += freelist - remaining
		freelist = remaining
		if freelist == 0 {
			return reclaimed, nil
		}
		select {
		case <-ctx.Done():
			return reclaimed, nil
		case <-time.After(pause):
		}
	}
}

// freelistState reads the three page counters the thresholds are built
// from. They are connection-local reads, so they stay on the writer.
func (s *Store) freelistState() (pageSize, pageCount, freelist int64, err error) {
	for _, p := range []struct {
		pragma string
		dest   *int64
	}{
		{"page_size", &pageSize},
		{"page_count", &pageCount},
		{"freelist_count", &freelist},
	} {
		if err := s.db.QueryRow("PRAGMA " + p.pragma).Scan(p.dest); err != nil {
			return 0, 0, 0, fmt.Errorf("store: probe %s: %w", p.pragma, err)
		}
	}
	return pageSize, pageCount, freelist, nil
}
