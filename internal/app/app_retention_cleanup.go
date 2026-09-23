package app

import (
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/store"
	"agent-overflow/internal/uitrace"
)

// Retention TTL sweep. Background goroutine that prunes stale threads
// (and their on-disk side effects), dated provider-event log files, and
// bug-report bookmark files, then repairs stored history (see
// repairStoredHistory). Each sweep reads Retention.Days from
// settings live so toggling the window doesn't require a restart;
// Retention.Days <= 0 disables the deletes silently. It does not
// disable the sweep: the free-space tail is gated by the freelist, not
// by the retention window, so a database the user pruned by hand still
// gets its pages back. See reclaimStoreFreeSpace.
//
// Each tick processes the entire eligible backlog in one pass, paced:
// every delete chunk is followed by retentionChunkPause so no write
// transaction runs back to back with the next one and user writes never
// queue behind more than one chunk. shuttingDown is polled between
// threads so a quit lands within a pause, not at the end of the backlog.
//
// Stop pattern mirrors startIdleSessionReaper (chan + WaitGroup), NOT
// the rate-limit probe's appCtx.Done() select. The sweep writes to
// SQLite and stops provider sessions; Shutdown must block on the
// goroutine's exit before the store closes (step 9) and before the
// session map snapshot in step 4.

const (
	// retentionSettleMinUptime is how long the app must have been
	// running before the first sweep. Boot is the worst moment for it:
	// first paint, session restore and history hydration are all
	// competing for the same writer, and a backlog that has waited days
	// can wait a few more minutes.
	retentionSettleMinUptime = 5 * time.Minute

	// retentionSettlePoll is how often the first-sweep gate re-checks
	// uptime and live turns.
	retentionSettlePoll = 30 * time.Second

	// retentionSweepInterval is the cadence between sweeps. Six hours
	// is long enough that the per-sweep cost never shows up in
	// profiling and short enough that long-running installs prune
	// predictably.
	retentionSweepInterval = 6 * time.Hour

	// retentionChunkPause is the gap the sweep leaves between write
	// chunks, both between the item chunks of one thread delete and
	// between threads. It is what keeps the sweep's share of the write
	// lock to one chunk at a time; without it a 47-thread pass held the
	// writer continuously for 17 s.
	retentionChunkPause = 100 * time.Millisecond

	// retentionCheckpointEvery is how often (in successful deletes) the
	// sweep runs PassiveCheckpoint so a long backfill doesn't grow the
	// WAL unboundedly. Each commit appends to the WAL; without periodic
	// recycling a 50k-thread backfill can inflate the WAL into the
	// hundreds of MB and stay there until the loop ends.
	retentionCheckpointEvery = 500
)

// startRetentionCleanup launches the background retention sweeper.
// Idempotent: a second call while a sweeper is already running is a
// no-op so test fixtures that exercise ServiceStartup repeatedly
// can't fan out goroutines. Shutdown closes retentionCleanupStop and
// waits on the WaitGroup before the parallel session close runs in
// Step 4, so the sweep can't fire mid-teardown.
func (a *App) startRetentionCleanup() {
	stop, started := a.sessionManager().runtime.StartRetentionCleanup()
	if !started {
		return
	}

	go func() {
		defer a.sessionManager().runtime.RetentionCleanupDone()
		if !a.awaitRetentionSettled(stop) {
			return
		}
		a.runRetentionSweep(a.retentionNow())

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

// awaitRetentionSettled blocks until the first sweep may run, and
// reports whether it should. It returns false when the app stops or
// begins shutting down first.
//
// The gate is deliberately not a fixed delay: a sweep that deletes
// months of history is minutes of write work, and the two moments it
// must stay out of are boot and a live turn.
func (a *App) awaitRetentionSettled(stop <-chan struct{}) bool {
	start := a.retentionNow()
	ticker := time.NewTicker(a.retentionSettlePollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return false
		case <-ticker.C:
			if a.shuttingDown.Load() {
				return false
			}
			if a.retentionSettled(a.retentionNow().Sub(start)) {
				return true
			}
		}
	}
}

// retentionSettled reports whether the app is far enough past boot and
// quiet enough for the first sweep.
func (a *App) retentionSettled(uptime time.Duration) bool {
	if uptime < a.retentionSettleUptime() {
		return false
	}
	return !a.sessionManager().runtime.HasActiveTurn()
}

func (a *App) retentionSettleUptime() time.Duration {
	return orDuration(a.maintenance.settleUptime, retentionSettleMinUptime)
}

func (a *App) retentionSettlePollInterval() time.Duration {
	return orDuration(a.maintenance.settlePoll, retentionSettlePoll)
}

// stopRetentionCleanup signals the goroutine to exit and waits for it
// to return. Safe to call before start (no-op) and safe to call twice
// (the nil-then-close guard makes the second call a no-op).
func (a *App) stopRetentionCleanup() {
	a.sessionManager().runtime.StopRetentionCleanup()
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

// retentionPause is the sweep's yield between write chunks. It returns
// immediately once shutdown has begun so a quit never waits out the
// pacing.
func (a *App) retentionPause() {
	if a.shuttingDown.Load() {
		return
	}
	time.Sleep(orDuration(a.maintenance.chunkPause, retentionChunkPause))
}

// runRetentionSweep performs one sweep tick: the TTL deletes, the stored
// history repair, then the store's free-space tail.
//
// Package-visible so tests can drive a single sweep with a pinned
// clock without spinning the ticker.
func (a *App) runRetentionSweep(now time.Time) {
	a.runRetentionDeletes(now)
	a.repairStoredHistory()
	a.reclaimStoreFreeSpace()
}

// runRetentionDeletes prunes everything past the TTL: stale threads and
// what cascades from them, dated log files, and bug-report bookmarks.
// Reads Retention.Days live from settings and returns immediately when
// retention is off. Logs one summary line iff any work happened so
// disabled installs and idle ticks stay silent.
func (a *App) runRetentionDeletes(now time.Time) {
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
	// catches up). The truncating checkpoint that used to follow is
	// deliberately absent: it needs every reader gone, so mid-session it
	// stalls reads for up to the busy timeout. Boot and Close are the
	// two moments where that quiescence is free, and both run it.
	if a.store != nil && threadDeleted > 0 {
		if err := a.store.PassiveCheckpoint(); err != nil {
			log.Printf("app: retention sweep: passive checkpoint: %v", err)
		}
	}
}

// reclaimStoreFreeSpace hands pages on the freelist back to the
// filesystem. It self-gates on the freelist thresholds and on the
// database being auto_vacuum=incremental, so most ticks do nothing; when
// it runs it is a paced series of short write transactions that readers
// never see.
//
// It runs on every tick, whatever the retention setting is and whether
// or not this tick deleted anything. Retention decides what is old
// enough to delete; it says nothing about the space a delete already
// freed, and threads the user deleted by hand leave exactly the same
// freelist. The thresholds are what decide.
//
// Shutdown skips it because the pacing would only delay the quit, and
// freed pages are reused by later writes regardless of when the file
// shrinks.
func (a *App) reclaimStoreFreeSpace() {
	if a.store == nil || a.shuttingDown.Load() {
		return
	}
	reclaim := a.reclaimFreeSpaceFn
	if reclaim == nil {
		reclaim = a.store.ReclaimFreeSpace
	}
	start := time.Now()
	pages, err := reclaim(a.lifeCtx(), orDuration(a.maintenance.chunkPause, retentionChunkPause))
	switch {
	case err != nil:
		log.Printf("app: retention sweep: reclaim free space: %v", err)
	case pages > 0:
		// Under WAL the shortened database lands in the WAL; the file
		// itself shrinks when a checkpoint moves it back.
		if err := a.store.PassiveCheckpoint(); err != nil {
			log.Printf("app: retention sweep: passive checkpoint: %v", err)
		}
		log.Printf("app: retention sweep: reclaimed %d pages in %s", pages, time.Since(start).Round(time.Millisecond))
	}
}

// runRetentionThreadSweep loads all stale thread ids and routes each
// through the per-thread action lock + the paced delete path. Returns
// (deleted, failed) counts. Per-thread errors log and continue; one bad
// thread must not prevent the rest from being cleaned up.
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
		// backfill exits at the next thread boundary. Pacing makes each
		// iteration at least retentionChunkPause long, so checking every
		// iteration is what keeps the quit inside one pause.
		if a.shuttingDown.Load() {
			return deleted, failed
		}
		if i > 0 {
			a.retentionPause()
		}
		unlock := a.threadLocks().Lock(id)
		delErr := a.deleteThreadTreePacedLocked(id, a.retentionPause)
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
		// backfill doesn't keep growing it. PassiveCheckpoint is
		// non-blocking and benign on failure (the next autocheckpoint
		// catches up).
		if deleted%retentionCheckpointEvery == 0 {
			if err := a.store.PassiveCheckpoint(); err != nil {
				log.Printf("app: retention sweep: passive checkpoint: %v", err)
			}
		}
	}
	return deleted, failed
}

// deleteThreadTreePacedLocked is deleteThreadTreeLocked with the store's
// item-chunk pause wired to the sweep's yield.
func (a *App) deleteThreadTreePacedLocked(threadID string, pause store.ChunkPause) error {
	ports := a.threadDeletePorts()
	ports.ChunkPause = pause
	return a.threadApplication().DeleteTree(threadID, false, ports)
}
