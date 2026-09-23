package app

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// One-time conversion of the database file to auto_vacuum=incremental.
//
// New databases are created incremental. A database created before that
// keeps freed pages on its freelist forever: writes reuse them, so the
// file never grows unnecessarily, but nothing short of rebuilding the
// file ever shrinks it. Store.ConvertToIncrementalVacuum does the
// rebuild as a snapshot swap; this decides when, and the whole point is
// that the user never notices. The conditions are all about that:
//
//   - nothing to do once the database reports incremental, so the
//     goroutine exits for good;
//   - no live turn, because the swap briefly stops writes;
//   - no commit for the last quiet window, because a commit during the
//     snapshot invalidates it and the attempt is wasted work;
//   - at most one attempt an hour, because the snapshot is a full copy
//     of the database.
const (
	// storeConvertPoll is the scheduler's tick. It is also the sample
	// interval for the commit watcher, so the quiet window is measured
	// to this granularity.
	storeConvertPoll = 15 * time.Second
	// storeConvertQuietWindow is how long the database must have gone
	// without a commit from any connection.
	storeConvertQuietWindow = 60 * time.Second
	// storeConvertRetryInterval caps attempts. One snapshot writes a
	// full copy of the database; a failed quiet check is cheap, but the
	// copy is not.
	storeConvertRetryInterval = time.Hour
)

// maintenanceTuning shortens the background maintenance timings so
// tests can drive the retention sweep and the conversion scheduler
// without waiting out production intervals. A zero field means the
// production constant.
type maintenanceTuning struct {
	settleUptime time.Duration
	settlePoll   time.Duration
	chunkPause   time.Duration
	convertPoll  time.Duration
	quietWindow  time.Duration
}

// orDuration returns override when it is set, otherwise fallback.
func orDuration(override, fallback time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return fallback
}

// backgroundLoop is a start/stop gate for an app-owned goroutine:
// idempotent start, idempotent stop, and a stop that joins.
type backgroundLoop struct {
	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

// start returns the loop's stop channel, or false when one is running.
func (l *backgroundLoop) start() (chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		return nil, false
	}
	l.stop = make(chan struct{})
	l.wg.Add(1)
	return l.stop, true
}

func (l *backgroundLoop) done() { l.wg.Done() }

// halt signals the goroutine and waits for it to return.
func (l *backgroundLoop) halt() {
	l.mu.Lock()
	stop := l.stop
	l.stop = nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	l.wg.Wait()
}

// startDeferredMigrations runs the store's pending deferred migration phases
// (store.DeferredMigration) in the background, paced like the retention
// sweep: one bounded transaction, then a chunk pause. With nothing pending,
// which is every boot once a build's phases have finished, it starts nothing.
// A quit stops the run at the next transaction and the next launch resumes
// it. Idempotent. Shutdown joins it before the store closes.
func (a *App) startDeferredMigrations() {
	if a.store == nil {
		return
	}
	pending, err := a.store.DeferredMigrationsPending()
	if err != nil {
		log.Printf("app: deferred migrations: %v", err)
		return
	}
	if !pending {
		return
	}
	stop, started := a.deferredMigrations.start()
	if !started {
		return
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		defer a.deferredMigrations.done()
		defer cancel()
		if err := a.store.RunDeferredMigrations(ctx, a.maintenancePause(ctx)); err != nil {
			log.Printf("app: deferred migrations: %v", err)
		}
	}()
}

// stopDeferredMigrations cancels a running deferred migration run and waits
// for it to return. Safe before start and safe to call twice.
func (a *App) stopDeferredMigrations() {
	a.deferredMigrations.halt()
}

// maintenancePause is the chunk pause of a background run bound to ctx. It
// returns early when ctx ends, and at once after shutdown began.
func (a *App) maintenancePause(ctx context.Context) store.ChunkPause {
	return func() {
		if a.shuttingDown.Load() {
			return
		}
		timer := time.NewTimer(orDuration(a.maintenance.chunkPause, retentionChunkPause))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

// startStoreMaintenance launches the auto_vacuum conversion scheduler.
// Idempotent. Shutdown joins it before the store closes.
func (a *App) startStoreMaintenance() {
	stop, started := a.storeMaintenance.start()
	if !started {
		return
	}
	go func() {
		defer a.storeMaintenance.done()
		a.runStoreConversionLoop(stop)
	}()
}

// stopStoreMaintenance signals the scheduler and waits for it to return.
// Safe before start and safe to call twice.
func (a *App) stopStoreMaintenance() {
	a.storeMaintenance.halt()
}

// runStoreConversionLoop polls for a quiet moment and converts once.
// It returns for good as soon as the database is incremental, or when
// the database cannot be converted at all.
func (a *App) runStoreConversionLoop(stop <-chan struct{}) {
	if a.store == nil {
		return
	}
	if mode, err := a.store.AutoVacuumMode(); err != nil {
		log.Printf("app: store conversion: read auto_vacuum: %v", err)
		return
	} else if mode == store.AutoVacuumIncremental {
		return
	}

	quiet := newCommitQuietGate(a.retentionNow())
	defer quiet.close()

	ticker := time.NewTicker(a.storeConvertPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if a.shuttingDown.Load() {
				return
			}
			if done := a.storeConversionTick(quiet, a.retentionNow()); done {
				return
			}
		}
	}
}

// storeConversionTick runs one poll and reports whether the scheduler is
// finished. A miss is silent: this runs every few seconds and nearly all
// of its work is deciding to do nothing.
func (a *App) storeConversionTick(quiet *commitQuietGate, now time.Time) bool {
	if a.sessionManager().runtime.HasActiveTurn() {
		quiet.disturb(now)
		return false
	}
	watcher, err := quiet.watcher(a.store)
	if err != nil {
		log.Printf("app: store conversion: watch commits: %v", err)
		return true
	}
	version, err := watcher.DataVersion(a.lifeCtx())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("app: store conversion: read data_version: %v", err)
		}
		return true
	}
	if !quiet.observe(version, now, a.storeConvertQuietWindow()) {
		return false
	}
	if !quiet.attemptDue(now, storeConvertRetryInterval) {
		return false
	}
	quiet.attempted(now)

	// The watcher holds a connection to the database file, which the
	// swap refuses to run underneath. Closing it also resets the quiet
	// tracking: the next tick starts a fresh window.
	quiet.close()

	result, err := a.store.ConvertToIncrementalVacuum(a.lifeCtx())
	if err != nil {
		// Shutdown cancels the app context, which interrupts the
		// snapshot mid-copy. That is the design, not a failure worth a
		// line in the log.
		if !errors.Is(err, context.Canceled) {
			log.Printf("app: store conversion: %v", err)
		}
		return false
	}
	switch result.Outcome {
	case store.ConvertConverted:
		log.Printf(
			"app: store conversion: database converted to incremental auto-vacuum; %d -> %d bytes, writes blocked %s",
			result.SizeBefore, result.SizeAfter, result.BlockedWindow.Round(time.Millisecond),
		)
		return true
	case store.ConvertAlreadyIncremental:
		return true
	case store.ConvertUnsupported:
		return true
	default:
		// Not quiet after all: a commit landed during the snapshot. The
		// hourly cap keeps this from repeating on a busy install.
		return false
	}
}

func (a *App) storeConvertPollInterval() time.Duration {
	return orDuration(a.maintenance.convertPoll, storeConvertPoll)
}

func (a *App) storeConvertQuietWindow() time.Duration {
	return orDuration(a.maintenance.quietWindow, storeConvertQuietWindow)
}

// commitQuietGate tracks how long the database has gone without a
// commit, plus when the last conversion attempt ran.
//
// PRAGMA data_version only reports commits made by OTHER connections and
// its value is comparable only against an earlier read from the same
// connection, so the watcher owns one and keeps it.
type commitQuietGate struct {
	watch       *store.CommitWatcher
	version     int64
	haveVersion bool
	lastCommit  time.Time
	lastAttempt time.Time
}

func newCommitQuietGate(now time.Time) *commitQuietGate {
	return &commitQuietGate{lastCommit: now}
}

func (g *commitQuietGate) watcher(s *store.Store) (*store.CommitWatcher, error) {
	if g.watch != nil {
		return g.watch, nil
	}
	watch, err := s.NewCommitWatcher(context.Background())
	if err != nil {
		return nil, err
	}
	g.watch = watch
	g.haveVersion = false
	return watch, nil
}

// observe records one data_version sample and reports whether the
// database has been quiet for the whole window.
func (g *commitQuietGate) observe(version int64, now time.Time, window time.Duration) bool {
	if !g.haveVersion || version != g.version {
		g.version = version
		g.haveVersion = true
		g.lastCommit = now
		return false
	}
	return now.Sub(g.lastCommit) >= window
}

// disturb treats the current moment as activity, so the quiet window
// restarts from here.
func (g *commitQuietGate) disturb(now time.Time) { g.lastCommit = now }

func (g *commitQuietGate) attemptDue(now time.Time, interval time.Duration) bool {
	return g.lastAttempt.IsZero() || now.Sub(g.lastAttempt) >= interval
}

func (g *commitQuietGate) attempted(now time.Time) { g.lastAttempt = now }

// close releases the watcher's connection and the quiet tracking that
// depends on it. Safe to call repeatedly.
func (g *commitQuietGate) close() {
	if g.watch == nil {
		return
	}
	if err := g.watch.Close(); err != nil {
		log.Printf("app: store conversion: close commit watcher: %v", err)
	}
	g.watch = nil
	g.haveVersion = false
}
