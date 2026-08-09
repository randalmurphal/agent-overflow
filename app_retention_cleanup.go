package main

import (
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/uitrace"
)

// Retention TTL sweep. Background goroutine that prunes stale threads
// (and their on-disk side effects), dated provider-event log files, and
// bug-report bookmark files. Each sweep reads Retention.Days from
// settings live so toggling the window doesn't require a restart;
// Retention.Days <= 0 disables the sweep silently.
//
// Each tick processes the entire eligible backlog in one pass — no
// per-tick cap. The sweep runs in a background goroutine and SQLite's
// 5 s busy_timeout handles contention with user-initiated writes.
// shuttingDown is polled every retentionShutdownCheckEvery threads so
// a quit during a long backfill aborts within a second or two.
//
// Stop pattern mirrors startIdleSessionReaper (chan + WaitGroup), NOT
// the rate-limit probe's appCtx.Done() select. The sweep writes to
// SQLite and stops provider sessions; Shutdown must block on the
// goroutine's exit before the store closes (step 9) and before the
// session map snapshot in step 4.

const (
	// retentionInitialDelay defers the first sweep so app startup
	// and first-paint don't compete with a potentially large backfill.
	retentionInitialDelay = 30 * time.Second

	// retentionSweepInterval is the cadence between sweeps. Six hours
	// is long enough that the per-sweep cost never shows up in
	// profiling and short enough that long-running installs prune
	// predictably.
	retentionSweepInterval = 6 * time.Hour

	// retentionShutdownCheckEvery is how often (in threads processed)
	// the sweep polls a.shuttingDown so a Quit during a long backfill
	// doesn't wait for the full eligible set.
	retentionShutdownCheckEvery = 50

	// retentionCheckpointEvery is how often (in successful deletes) the
	// sweep runs PassiveCheckpoint so a long backfill doesn't grow the
	// WAL unboundedly before the trailing checkpoint. Each commit appends
	// to the WAL; without periodic recycling a 50k-thread backfill can
	// inflate the WAL into the hundreds of MB and stay there until the
	// loop ends.
	retentionCheckpointEvery = 500
)

// startRetentionCleanup launches the background retention sweeper.
// Idempotent: a second call while a sweeper is already running is a
// no-op so test fixtures that exercise ServiceStartup repeatedly
// can't fan out goroutines. Shutdown closes retentionCleanupStop and
// waits on the WaitGroup before the parallel session close runs in
// Step 4, so the sweep can't fire mid-teardown.
func (a *App) startRetentionCleanup() {
	a.mu.Lock()
	if a.retentionCleanupStop != nil {
		a.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	a.retentionCleanupStop = stop
	// Add(1) under the lock so a concurrent stopRetentionCleanup that
	// sees retentionCleanupStop != nil and proceeds to WG.Wait()
	// observes the counter at 1 — same memory-model contract the idle
	// reaper relies on.
	a.retentionCleanupWG.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.retentionCleanupWG.Done()
		// Initial sweep on a short timer, then transition to ticker.
		// The initial timer is select-able so a Shutdown during the
		// 30 s warm-up window doesn't have to wait for the timer to
		// fire before noticing the stop signal.
		initial := time.NewTimer(retentionInitialDelay)
		defer initial.Stop()
		select {
		case <-stop:
			return
		case <-initial.C:
			if a.shuttingDown.Load() {
				return
			}
			a.runRetentionSweep(a.retentionNow())
		}

		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if a.shuttingDown.Load() {
					return
				}
				a.runRetentionSweep(a.retentionNow())
			}
		}
	}()
}

// stopRetentionCleanup signals the goroutine to exit and waits for it
// to return. Safe to call before start (no-op) and safe to call twice
// (the nil-then-close guard makes the second call a no-op).
func (a *App) stopRetentionCleanup() {
	a.mu.Lock()
	stop := a.retentionCleanupStop
	a.retentionCleanupStop = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	a.retentionCleanupWG.Wait()
}

// retentionNow returns the sweep's notion of "now," honoring the
// test-only clock override. Exposed for unit tests via the
// retentionNowFn field; production callers route through here.
func (a *App) retentionNow() time.Time {
	if a.retentionNowFn != nil {
		return a.retentionNowFn()
	}
	return time.Now()
}

// runRetentionSweep performs one sweep tick. Reads Retention.Days
// live from settings; returns immediately if disabled. Logs one
// summary line iff any work happened so disabled installs and idle
// ticks stay silent.
//
// Package-visible so tests can drive a single sweep with a pinned
// clock without spinning the ticker.
func (a *App) runRetentionSweep(now time.Time) {
	if a.settings == nil {
		return
	}
	days := a.settings.Get().Retention.Days
	if days <= 0 {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	cutoffMs := cutoff.UnixMilli()

	threadDeleted, threadFailed := a.runRetentionThreadSweep(cutoffMs)

	var (
		logsDeleted int
		bookDeleted int
		sweepErrs   []error
	)
	if a.configDir != "" {
		var err error
		// One sweep over every daily log the logging package mints — the
		// provider-event stream and the workflow engine stream alike.
		logsDeleted, err = logging.PruneOlderThan(a.configDir, now, cutoff)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("prune logs: %w", err))
		}
		bookDeleted, err = uitrace.PruneBookmarksOlderThan(a.configDir, cutoff)
		if err != nil {
			sweepErrs = append(sweepErrs, fmt.Errorf("prune bookmarks: %w", err))
		}
	}

	if threadDeleted+threadFailed+logsDeleted+bookDeleted > 0 || len(sweepErrs) > 0 {
		log.Printf(
			"app: retention sweep: cutoff=%s threads_deleted=%d threads_failed=%d logs_deleted=%d bookmarks_deleted=%d",
			cutoff.UTC().Format(time.RFC3339),
			threadDeleted, threadFailed, logsDeleted, bookDeleted,
		)
	}
	for _, err := range sweepErrs {
		log.Printf("app: retention sweep: %v", err)
	}

	// Opportunistic WAL recycle when thread rows were actually freed.
	// PassiveCheckpoint is non-blocking and a no-op when there's
	// nothing to reclaim; failure is benign (the next autocheckpoint
	// catches up).
	if threadDeleted > 0 && a.store != nil {
		if err := a.store.PassiveCheckpoint(); err != nil {
			log.Printf("app: retention sweep: passive checkpoint: %v", err)
		}
		// Reclaim freed file space once the sweep has deleted history.
		// VacuumIfFragmented self-gates on the freelist thresholds, so
		// most sweeps skip it; when it runs it holds an exclusive lock
		// for seconds, which is why it only runs here (a controlled
		// background moment) and not during shutdown, where it would
		// stall the quit. SQLITE_BUSY from a concurrent long reader is
		// benign — the next qualifying sweep retries.
		if !a.shuttingDown.Load() {
			start := time.Now()
			if ran, err := a.store.VacuumIfFragmented(); err != nil {
				log.Printf("app: retention sweep: vacuum: %v", err)
			} else if ran {
				log.Printf("app: retention sweep: vacuum reclaimed freed space in %s", time.Since(start).Round(time.Millisecond))
			}

			// Trailing truncating checkpoint. The passive checkpoints
			// above (and the per-batch ones inside the sweep) keep the
			// WAL from growing but never shrink the file, so a sweep
			// that just pushed thousands of thread deletions — and
			// possibly a VACUUM, which appends the entire rebuilt
			// database to the WAL — leaves the session's high-water
			// mark on disk. TRUNCATE is what reclaims it, and this is
			// the right moment for it: a controlled background pass
			// that already took an exclusive lock for the VACUUM.
			// It quiesces reads internally, and a checkpoint it still
			// can't complete reports Busy and changes nothing rather
			// than failing the sweep — the next qualifying sweep, or
			// the next boot, retries. Skipped while shutting down
			// because Store.Close runs the same checkpoint with the
			// read pool already gone.
			res, err := a.store.TruncateCheckpoint()
			switch {
			case err != nil:
				log.Printf("app: retention sweep: truncate checkpoint: %v", err)
			case res.Busy:
				log.Printf("app: retention sweep: truncate checkpoint blocked by an open read; %d frames left in the WAL", res.WALFrames)
			}
		}
	}
}

// runRetentionThreadSweep loads all stale thread ids and routes each
// through the per-thread action lock + deleteThreadTreeLocked path.
// Returns (deleted, failed) counts. Per-thread errors log and
// continue; one bad thread must not prevent the rest from being
// cleaned up.
func (a *App) runRetentionThreadSweep(cutoffMs int64) (deleted, failed int) {
	if a.store == nil {
		return 0, 0
	}
	ids, err := a.store.ThreadIDsOlderThan(cutoffMs)
	if err != nil {
		log.Printf("app: retention sweep: list stale threads: %v", err)
		return 0, 0
	}
	for i, id := range ids {
		// Poll cooperatively so a Quit during a multi-thousand-thread
		// backfill exits quickly. The check is cheap (one atomic load)
		// so doing it every retentionShutdownCheckEvery iterations
		// rather than every iteration is just to avoid bytecode bloat;
		// either cadence would be acceptable.
		if i%retentionShutdownCheckEvery == 0 && a.shuttingDown.Load() {
			return deleted, failed
		}
		unlock := a.threadLocks().Lock(id)
		delErr := a.deleteThreadTreeLocked(id)
		unlock()
		if delErr != nil {
			// errors.Is(delErr, sql.ErrNoRows) is normal (the user
			// raced us and deleted the thread first), but
			// deleteThreadTreeLocked already swallows ErrNoRows at its
			// own boundary, so any error here is genuinely worth
			// logging.
			log.Printf("app: retention sweep: delete thread %s: %v", id, delErr)
			failed++
			continue
		}
		deleted++
		// Recycle the WAL periodically so a multi-thousand-thread
		// backfill doesn't keep growing it before the trailing
		// checkpoint runs. PassiveCheckpoint is non-blocking and
		// benign on failure (the next autocheckpoint catches up), so a
		// best-effort call here is safe even from a hot loop.
		if a.store != nil && deleted%retentionCheckpointEvery == 0 {
			if err := a.store.PassiveCheckpoint(); err != nil {
				log.Printf("app: retention sweep: passive checkpoint: %v", err)
			}
		}
	}
	return deleted, failed
}
