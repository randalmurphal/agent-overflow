package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The auto_vacuum conversion is the last step of the store's v119 deferred
// phase (store.DeferredHost.AwaitFileSwap). New databases are created
// incremental; the step rebuilds an older file as a snapshot swap, and the
// whole point is that the user never notices. storeFileSwapWait decides
// when:
//
//   - not during a supervisor trial, because the swap writes a snapshot
//     beside the database and renames the old file aside, both outside
//     the snapshot boundary a rollback restores;
//   - no live turn, because the swap briefly stops writes;
//   - no commit for the last quiet window, because a commit during the
//     snapshot invalidates it and the attempt is wasted work;
//   - at most one attempt an hour, because the snapshot is a full copy
//     of the database.
const (
	// storeConvertPoll is the wait's tick. It is also the sample interval
	// for the commit watcher, so the quiet window is measured to this
	// granularity.
	storeConvertPoll = 15 * time.Second
	// storeConvertQuietWindow is how long the database must have gone
	// without a commit from any connection.
	storeConvertQuietWindow = 60 * time.Second
	// storeConvertRetryInterval caps attempts. One snapshot writes a
	// full copy of the database; a failed quiet check is cheap, but the
	// copy is not.
	storeConvertRetryInterval = time.Hour
)

// deferredMigrationNoticeID names the one notice deferred migration runs
// raise, so a later run's notice replaces an earlier one and a finished
// run takes it back.
const deferredMigrationNoticeID = "store-deferred-migration"

// deferredMigrationNoticeErrorRunes bounds the error a notice quotes.
const deferredMigrationNoticeErrorRunes = 400

// maintenanceTuning shortens the background maintenance timings so
// tests can drive the retention sweep and the conversion wait without
// waiting out production intervals. A zero field means the production
// constant.
type maintenanceTuning struct {
	settleUptime       time.Duration
	settlePoll         time.Duration
	chunkPause         time.Duration
	convertPoll        time.Duration
	quietWindow        time.Duration
	firstReadsFallback time.Duration
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
// A run that leaves items unfinished raises a notice and the next launch
// retries them; a quit stops the run at the next transaction and the next
// launch resumes it. Idempotent. Shutdown joins it before the store closes.
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
		prior, err := a.store.DeferredMigrationFailure()
		if err != nil {
			log.Printf("app: deferred migrations: %v", err)
		}
		host := store.DeferredHost{
			Pause:         a.maintenancePause(ctx),
			AwaitFileSwap: a.storeFileSwapWait(),
			AgentEndRule:  triage.AgentEndRuleFor,
		}
		runErr := a.store.RunDeferredMigrations(ctx, host)
		if ctx.Err() != nil {
			return
		}
		a.reportDeferredMigrations(prior != nil, runErr)
	}()
}

// stopDeferredMigrations cancels a running deferred migration run and waits
// for it to return. Safe before start and safe to call twice.
func (a *App) stopDeferredMigrations() {
	a.deferredMigrations.halt()
}

// reportDeferredMigrations tells the user how a finished run left the
// database. A phase whose run left failed items raises a notice with their
// count and the first error; the data still reads correctly and the next
// launch retries them. A run that could not read or record its progress
// raises the notice with that error. A clean run takes back the notice an
// earlier run raised.
func (a *App) reportDeferredMigrations(hadFailure bool, runErr error) {
	var send notify.Send
	switch failure, err := a.store.DeferredMigrationFailure(); {
	case runErr != nil:
		log.Printf("app: deferred migrations: %v", runErr)
		send = deferredMigrationNotice("Database maintenance incomplete",
			fmt.Sprintf("%s. Retrying on next start.", truncateRunes(runErr.Error(), deferredMigrationNoticeErrorRunes)))
	case err != nil:
		log.Printf("app: deferred migrations: %v", err)
		send = deferredMigrationNotice("Database maintenance incomplete",
			fmt.Sprintf("%s. Retrying on next start.", truncateRunes(err.Error(), deferredMigrationNoticeErrorRunes)))
	case failure != nil:
		items := "items"
		if failure.Failures == 1 {
			items = "item"
		}
		send = deferredMigrationNotice(failure.Title+" incomplete",
			fmt.Sprintf("%d %s failed; retrying on next start. First error: %s",
				failure.Failures, items, truncateRunes(failure.FirstError, deferredMigrationNoticeErrorRunes)))
	case hadFailure:
		send = notify.Send{ID: deferredMigrationNoticeID, Kind: notify.KindAppUpdate, Retract: true}
	default:
		return
	}
	if !send.Retract {
		send.Target = notify.Target{Kind: notify.TargetNone, BackendID: a.notificationBackendID()}
	}
	// Logged here rather than through logNotificationFailure, whose
	// once-per-code record belongs to the notification queue's goroutine.
	// The preference and attended-screen refusals are the user's choice,
	// not a failure.
	if err := a.notifyOS(send); err != nil {
		var refusal *NotificationError
		if errors.As(err, &refusal) && (refusal.Code == NotificationSuppressed || refusal.Code == NotificationScreenAttended) {
			return
		}
		log.Printf("app: deferred migrations: notice: %v", err)
	}
}

func deferredMigrationNotice(title, body string) notify.Send {
	return notify.Send{ID: deferredMigrationNoticeID, Kind: notify.KindAppUpdate, Title: title, Body: body}
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

// storeFileSwapWait returns the wait the auto_vacuum conversion makes
// before each attempt (store.DeferredHost.AwaitFileSwap). It first waits
// for the activation gate: a supervisor trial never replaces its database
// file. Then it samples every poll until no turn is live, nothing has
// committed for the quiet window, and the run's previous attempt is at
// least storeConvertRetryInterval old. It holds a commit watcher only while
// it waits, because the swap refuses to run while another connection holds
// the file.
func (a *App) storeFileSwapWait() func(context.Context) error {
	quiet := newCommitQuietGate(a.retentionNow())
	return func(ctx context.Context) error {
		if err := WaitForActivation(a, ctx); err != nil {
			return err
		}
		defer quiet.close()
		ticker := time.NewTicker(a.storeConvertPollInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			ready, err := a.storeSwapReady(ctx, quiet, a.retentionNow())
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			if ready {
				return nil
			}
		}
	}
}

// storeSwapReady takes one sample and reports whether a conversion attempt
// may start now. A miss is silent: nearly every sample decides to wait.
func (a *App) storeSwapReady(ctx context.Context, quiet *commitQuietGate, now time.Time) (bool, error) {
	if a.sessionManager().runtime.HasActiveTurn() {
		quiet.disturb(now)
		return false, nil
	}
	watcher, err := quiet.watcher(a.store)
	if err != nil {
		return false, fmt.Errorf("watch commits: %w", err)
	}
	version, err := watcher.DataVersion(ctx)
	if err != nil {
		return false, fmt.Errorf("read data_version: %w", err)
	}
	if !quiet.observe(version, now, a.storeConvertQuietWindow()) {
		return false, nil
	}
	if !quiet.attemptDue(now, storeConvertRetryInterval) {
		return false, nil
	}
	quiet.attempted(now)
	return true, nil
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
