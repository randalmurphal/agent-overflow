package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/errorsx"
)

// Shutdown timeouts. Each subsystem gets its own budget so a slow telemetry
// flush can't eat the replay writer's window. The top-level ServiceShutdown
// wrapper does NOT sum these — it calls Shutdown(context.Background()) and
// lets each subsystem enforce its own deadline below. Callers (tests) that
// want a ceiling on total shutdown latency can pass their own ctx.
const (
	// sessionShutdownTimeout caps how long Shutdown will wait for every
	// provider session to close in parallel before giving up and moving on
	// to the rest of the teardown. Sessions that don't finish in time get
	// abandoned — the Wails process is going away regardless, so the
	// underlying subprocesses will be reaped by the OS.
	sessionShutdownTimeout = 5 * time.Second

	// reactorDrainTimeout caps how long we wait for in-flight triage
	// Handle calls to return before continuing shutdown. Triage work is
	// short-lived in practice (SQLite writes over the local FS), so 2s is
	// generous; the guard is there to stop a stuck goroutine from
	// blocking user-perceived Quit latency.
	reactorDrainTimeout = 2 * time.Second

	// replayShutdownTimeout bounds the replay manager's queue drain.
	replayShutdownTimeout = 2 * time.Second

	// telemetryShutdownTimeout bounds the OTLP exporter flush. The otel
	// package applies its own internal cap on top of this.
	telemetryShutdownTimeout = 5 * time.Second

	// transportShutdownDrainTimeout caps how long Shutdown waits for the
	// embedded HTTP+WS server to drain in-flight RPCs before continuing
	// with subsystem teardown. Short by design — once subsystems start
	// closing, any RPC still running would observe closed-store errors
	// anyway, so we pay a bounded delay to let the polite ones finish.
	transportShutdownDrainTimeout = 3 * time.Second

	// workflowPauseAllTimeout bounds the graceful-quit pause of active workflow
	// runs. Pausing interrupts each in-flight turn and parks its run
	// `needs-human(paused)`; it deliberately does NOT wait for turns to finish,
	// so the budget only has to cover interrupt + park + SQLite writes. A run
	// that misses the window is not lost — its session dies with the process
	// and the next startup sweep parks it `needs-human(interrupted)`, which
	// resume treats identically. Quit latency is the thing worth protecting.
	workflowPauseAllTimeout = 3 * time.Second
)

// Shutdown tears the App down in a documented order. See the numbered steps
// inline for the rationale. Shutdown is idempotent and safe to call on a
// zero-value App: every guard is nil-safe so tests can wire a subset of
// subsystems without threading nil-checks through the call sites.
//
// Errors from individual subsystems are collected and joined — Shutdown
// never aborts on the first failure because every caller is mid-quit and
// we want every subsystem to flush before the process exits.
//
// `//wails:ignore` keeps this method out of the auto-generated TS bindings.
// Shutdown is a process-local lifecycle hook; the frontend has no business
// calling it.
//
//wails:ignore
func (a *App) Shutdown(ctx context.Context) error {
	// Step 0 (pre-shutdown): drain the transport server while every
	// subsystem is still alive. Without this, a webview WS client that
	// fires an RPC during the window between the App's subsystem-close
	// (Step 9 closes SQLite, Step 6 closes terminals, etc) and the
	// post-Run main.go call to srv.Shutdown would hit closed subsystems
	// and race teardown. transport.Server.Shutdown is idempotent (stopOnce),
	// so a later main.go call lands as a no-op.
	if srv := a.transportServer.Load(); srv != nil {
		drainCtx, cancel := contextWithTimeout(ctx, transportShutdownDrainTimeout)
		// Note: we call this BEFORE flipping shuttingDown so any RPCs
		// already mid-flight finish through the existing handlers; new
		// RPCs that arrive after Shutdown is wired bounce off the
		// rootCtx cancellation that Server.Shutdown triggers.
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("transport: drain on app shutdown: %v", err)
		}
		cancel()
	}

	// Step 1: stop accepting new work. Binding entry points check this
	// atomic on every call, so any concurrent RPC after this line fails
	// fast with ErrShuttingDown instead of starting a session we'd have
	// to tear right back down.
	if !a.shuttingDown.CompareAndSwap(false, true) {
		// Already shut down. Return nil so double-invocation (test
		// harness + Wails both calling us) stays a no-op.
		return nil
	}

	var errs []error
	record := func(step string, err error) {
		if a.shutdownInjectErrFn != nil {
			err = a.shutdownInjectErrFn(step, err)
		}
		errs = errorsx.Append(errs, errorsx.WrapLifecycle(step, err))
		if a.shutdownStepFn != nil {
			a.shutdownStepFn(step, err)
		}
	}

	// Step 1a0: stop the workflow scheduler before anything else workflow-side.
	// Pausing every active run (Step 1a) while a cron tick or a chained internal
	// event can still start a new one is a race the human loses: the new run
	// would be admitted after the pause sweep walked past it and would die
	// mid-turn with the process. Stopping is bounded; a scheduler that misses the
	// window is recorded, not waited on.
	if a.workflowScheduler != nil {
		record("stop workflow scheduler", a.stopWorkflowScheduler(ctx))
	}

	// Self-resume timers come down with it, and for the same reason: a schedule
	// firing into the pause sweep below would resume a run this shutdown is
	// parking. The schedules are durable, so the next boot re-arms every one of
	// them.
	a.stopWorkflowAutoResumes()

	// Step 1a: pause every active workflow run. This runs while provider
	// sessions, the engine's ctx, and SQLite are all still alive, because
	// pausing is real work: it interrupts each in-flight turn, releases the
	// run's resource locks, writes the partial envelope, and parks the run
	// `needs-human(paused)` — a state the next launch resumes from on the same
	// provider session. Doing it after Step 1b (or after Step 4) would leave
	// every active run to the crash sweep instead.
	if a.workflowEngine != nil {
		record("pause active workflow runs", a.pauseWorkflowRunsForShutdown())
	}

	// Step 1b: cancel the App-lifetime ctx so fire-and-forget goroutines
	// (rate-limit probe loop, Claude OAuth poller, MCP live-reconcile
	// callbacks) unblock their I/O and exit BEFORE drainTriage runs.
	// Ordering matters: cancelling after the drain would let in-flight
	// goroutines issue triage.Handle calls past the barrier; cancelling
	// here lets them observe ctx.Done() and return without filing more
	// work. Routed through record() so the existing
	// shutdownStepFn/shutdownInjectErrFn hooks observe it — the order
	// test (TestShutdownWalksDocumentedOrder) pins the BEFORE-drainTriage
	// placement. CancelFunc itself can't fail, so the recorded error is
	// always nil in production; shutdownInjectErrFn could synthesise one
	// for aggregation-test purposes. appCancel is nil-safe for tests that
	// construct *App without ServiceStartup.
	record("cancel app context", func() error {
		if a.appCancel != nil {
			a.appCancel()
		}
		return nil
	}())
	if a.workflowDefinitionsWatcher != nil {
		record("close workflow definitions watcher", a.workflowDefinitionsWatcher.Close())
	}
	if a.themeWatcher != nil {
		record("close theme watcher", a.themeWatcher.Close())
	}
	if a.workflowEngine != nil {
		engineErr := a.workflowEngine.Close()
		// No new lifecycle events can arrive after the engine closes. Let the
		// app-side reaction workers finish before SQLite closes, so a landed
		// branch cannot lose its durable receipt and a composed wake cannot
		// lose its delivery during shutdown.
		a.workflowAutoDisposition.Wait()
		a.workflowWake.Wait()
		a.workflowSchedulerQueue.Wait()
		record("close workflow engine", engineErr)
	}

	// Step 2: drain the triage reactor. Any Handle() calls currently
	// running (dispatched from provider sessionEventHandlers) get to
	// finish. We use a short timeout so a stuck goroutine can't block
	// the rest of teardown.
	record("drain triage", a.drainTriage(ctx, reactorDrainTimeout))
	record("drain flush dispatch", a.drainFlushDispatch(ctx, reactorDrainTimeout))

	// Step 3: flush observability writers BEFORE closing provider
	// sessions. Provider close events pass through the replay log; if
	// we close the log first we drop the last few frames of every
	// in-flight session. OTEL flush goes here too because traces
	// attached to in-flight turns should land before the sessions
	// holding those turns go away.
	if a.replay != nil {
		replayCtx, cancel := contextWithTimeout(ctx, replayShutdownTimeout)
		record("close replay manager", a.replay.Shutdown(replayCtx))
		cancel()
	}
	if a.telemetry != nil {
		otelCtx, cancel := contextWithTimeout(ctx, telemetryShutdownTimeout)
		record("shutdown telemetry", a.telemetry.Shutdown(otelCtx))
		cancel()
	}

	// Step 3b: stop the idle-session reaper before snapshotting
	// sessions for Step 4. Otherwise the reaper could fire between the
	// snapshot and the close, see entries the snapshot already moved
	// out, and either close nothing (harmless) or — if a fresh entry
	// is racing in — close a session the Shutdown closer doesn't know
	// about. stopIdleSessionReaper is idempotent and blocks until the
	// goroutine returns.
	a.stopIdleSessionReaper()
	record("stop idle session reaper", nil)

	// Step 3c: stop the retention TTL sweep. The sweep writes to
	// SQLite (cascading thread deletes) and calls deleteThreadTreeLocked
	// which mutates a.sessions via stopSession — running it concurrently
	// with Step 4's snapshotAndClear would race the session map, and
	// running it past Step 9's store close would write to a torn-down
	// store. stopRetentionCleanup is idempotent and blocks until the
	// goroutine returns.
	a.stopRetentionCleanup()
	record("stop retention cleanup", nil)

	// Step 3d: stop the background `git fetch` cadence. Each pass reads
	// the project list from SQLite, so it must be joined before Step 9's
	// store close; it spawns git subprocesses, so leaving it running
	// during teardown would also outlive the window in which their
	// output means anything. Idempotent and blocks until the goroutine
	// returns.
	a.stopBackgroundGitFetch()
	record("stop background git fetch", nil)

	// Step 3e: stop in-flight chat-thread worktree setup runs. Same two
	// reasons as 3d — each run owns a subprocess group whose output stops
	// meaning anything past this point, and settling one writes the thread's
	// durable setup state, so it must join before Step 9's store close.
	// Deliberately leaves those threads' rows at 'running': the recipe was
	// interrupted mid-flight and the worktree's state is genuinely unknown,
	// which is exactly what the next boot's sweep turns into a visible
	// failure. Idempotent and blocks until every goroutine returns.
	a.stopThreadWorktreeSetups()
	record("stop worktree setups", nil)

	// Step 3f: stop an in-flight session import. It writes threads, items and
	// turns straight into SQLite, so it must join before Step 9's store close.
	// Cancelling mid-session is safe: ImportOne rolls the whole session back,
	// so an interrupted run leaves only the sessions it fully finished — and
	// the dedup set keys on the source session id, so the next scan offers the
	// rest again. Idempotent and blocks until the goroutine returns.
	a.stopSessionImports()
	record("stop session imports", nil)

	// Step 3g: join the background thread read-state stamps. Each one is a
	// SQLite write, so it has to land before Step 9 closes the store.
	// Bounded without a timeout here: every stamp carries its own
	// markThreadReadTimeout, so the join cannot outlast the longest one
	// already in flight.
	a.stopMarkThreadReads()
	record("stop thread read stamps", nil)

	// Step 4: stop provider sessions. Each session's Close tears down
	// its own design-thread state as part of the same parallel closer,
	// so a slow design teardown can't serialize behind an unrelated
	// session close. Session closers aggregate their own errors via
	// closeSessionsParallel — we surface them under a single
	// "close provider sessions" step so the order spy sees one entry.
	sessions := a.sessionManager().snapshotAndClear()
	sessionErrs := closeSessionsParallel(a, sessions, sessionShutdownTimeout)
	record("close provider sessions", errors.Join(sessionErrs...))

	// Step 4b: stop the orphan-reaper sidecar (macOS). Sessions that closed
	// cleanly above each released their group; any whose Close was abandoned
	// on the parallel timeout stay watched and get reaped as we close the
	// control pipe here — exactly what we want for a still-alive straggler.
	// Placed after Step 4 so the sidecar stays armed for the whole
	// session-teardown window. No-op when inactive.
	a.stopOrphanReaper()
	record("stop orphan reaper", nil)

	// Step 5: close the design reactor's pending choice requests. This
	// tears down per-thread design state left dangling when a session
	// never reached a clean Close — matches the teardownDesignThread
	// work the session closers did above but is safe to call again
	// (reactor.TeardownThread is a no-op when nothing is pending).
	if a.reactor != nil {
		// Walk the sessions we snapshotted so TeardownThread fires for
		// each thread even if the session close itself failed before
		// reaching its own teardown.
		for threadID := range sessions {
			a.reactor.TeardownThread(threadID)
		}
		record("close design reactor", nil)
	}

	// Step 5b: stop gitwatch subscriptions. Connection-tied subs were
	// already drained at Step 0 (transport drain runs ConnState
	// cleanups via runConnHandler's defer, which call our internal
	// unsubscribeGitWatch). Manager.Close tears down any leftover
	// watchers — primarily the case when shutdown is initiated
	// without an open WS client (tests, headless quit). Pump
	// goroutines exit when their Subscription channels close
	// (Manager.Close blocks on watcher run-loops exiting, which fires
	// broadcastClose, which closes Updates() channels). The pump
	// WaitGroup adds a hard guarantee that no pump can still emit on
	// the wire after this step returns.
	if a.gitWatch != nil {
		a.gitWatch.Close()
		a.gitWatchPumpWG.Wait()
		record("close gitwatch manager", nil)
	}

	a.closePRUpdatePumps()
	record("close PR update subscriptions", nil)

	// Step 5c: stop any design watchers that survived session teardown.
	// Per-session teardownDesignThread already fires from the parallel
	// closer in step 4 for sessions that reached close cleanly, but a
	// session that errored out before installing a teardown hook (or a
	// future code path that creates a watcher without a session) would
	// leave the goroutine alive past App lifetime. Walk the map under
	// the dedicated mu and stop each watcher; safe to call after step 4
	// because session closers don't write to designWatchers concurrently
	// (each calls stopDesignWatcher which acquires the same mutex).
	a.designWatchersMu.Lock()
	leftoverWatchers := make([]*design.Watcher, 0, len(a.designWatchers))
	for _, w := range a.designWatchers {
		leftoverWatchers = append(leftoverWatchers, w)
	}
	a.designWatchers = nil
	a.designWatchersMu.Unlock()
	for _, w := range leftoverWatchers {
		w.Stop()
	}
	if len(leftoverWatchers) > 0 {
		record("close leftover design watchers", nil)
	}

	// Step 6: close PTYs. Must happen after provider sessions because
	// a provider close might emit terminal output events; terminating
	// the terminal manager first would drop those final frames.
	if a.terminals != nil {
		record("close terminal sessions", a.terminals.Shutdown())
	}

	// Step 7: close the headless Chromium driving read_screenshot.
	// MUST run before the design MCP server: designMCP.Close() calls
	// http.Server.Shutdown(context.Background()) which blocks until
	// in-flight handlers return, and any in-flight read_screenshot
	// handler is parked inside Manager.Capture waiting on chromedp.
	// Closing the manager first cancels browserCtx, the chromedp
	// run returns, the handler returns, and step 7b's Shutdown
	// finishes promptly. The opposite order deadlocked shutdown
	// against a long-running capture.
	// Safe on a never-started Manager — the package treats Close as a
	// no-op when allocCancel/browserCancel are nil.
	if a.screenshotManager != nil {
		record("close headless screenshot manager", a.screenshotManager.Close())
	}

	// Step 7b: close the design MCP server. Safe to close once no
	// provider session holds a reference (step 4 guarantees that)
	// and the screenshot manager has been torn down (step 7
	// guarantees in-flight read_screenshot handlers can unblock).
	if a.designMCP != nil {
		record("close design MCP server", a.designMCP.Close())
	}

	// Step 8: close the provider event logger. After providers are
	// gone, nothing else writes to it — close it before SQLite so its
	// final flush isn't racing any other persistence sink.
	if a.logger != nil {
		record("close logger", a.logger.Close())
	}
	// The engine stopped at step 1b, so nothing is still appending here
	// either.
	if a.engineLogger != nil {
		record("close workflow engine logger", a.engineLogger.Close())
	}

	// Step 8b: final settle-goroutine drain. Provider session close at
	// Step 4 may have emitted session_died / EventTurnComplete events
	// that ran through triage AFTER Step 2's drainTriage barrier. Those
	// post-drain Handle calls can spawn async settle goroutines
	// (settleStreamingTextAsync / settleStreamingThinkingAsync) that
	// would otherwise call into SQLite after Step 9 closes it. The
	// 5-second cap matches the busy_timeout PRAGMA: if a settle is
	// genuinely stuck behind a SQL lock, no amount of extra waiting will
	// help and shutdown should proceed.
	if a.triage != nil {
		settleCtx, settleCancel := contextWithTimeout(ctx, 5*time.Second)
		drained := make(chan struct{})
		go func() {
			a.triage.WaitForPendingSettles()
			close(drained)
		}()
		select {
		case <-drained:
		case <-settleCtx.Done():
			log.Printf("app: drain settles timed out after 5s — proceeding with close")
		}
		settleCancel()
	}

	// Step 9: close SQLite last. Triage, replay, provider sessions,
	// and the logger have all flushed by this point; anyone calling
	// into the store after this will see a closed-db error rather
	// than corrupting a half-shut database.
	if a.store != nil {
		record("close store", a.store.Close())
	}

	return errors.Join(errs...)
}

// drainTriage blocks until every in-flight triage Handle() call returns
// or the timeout fires. Shutdown runs this before flushing observability
// + SQLite so no event is persisted after the store is closed. The
// timeout caps Quit latency — a stuck goroutine can delay the rest of
// teardown by at most `timeout`.
//
// A DeadlineExceeded error is returned rather than swallowed so the
// caller (Shutdown) records it in the step sequence; the rest of
// teardown continues regardless.
func (a *App) drainTriage(ctx context.Context, timeout time.Duration) error {
	if a.triage == nil {
		return nil
	}
	drainCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()
	if err := a.triage.Wait(drainCtx); err != nil {
		return fmt.Errorf("drain triage (timeout %s): %w", timeout, err)
	}
	return nil
}

// contextWithTimeout derives a child context bounded by whichever is
// tighter: the parent's remaining deadline or the subsystem's own timeout.
// A nil parent behaves like context.Background — this keeps the test path
// (which passes context.Background directly) and the Wails path (which has
// the parent cancelled mid-shutdown) both working.
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// ServiceShutdown is the Wails v3 lifecycle hook. We keep it as a thin
// wrapper around Shutdown so the Wails runtime drives the same code path
// tests exercise directly.
func (a *App) ServiceShutdown() error {
	return a.Shutdown(context.Background())
}

// lifeCtx returns the App-lifetime ctx fire-and-forget goroutines should
// derive from. Returns context.Background when appCtx is unset, which
// happens for tests that build *App as a struct literal without calling
// ServiceStartup — they get a never-cancelled ctx (matching the
// pre-appCtx behaviour) instead of a nil-parent panic.
func (a *App) lifeCtx() context.Context {
	if a.appCtx != nil {
		return a.appCtx
	}
	return context.Background()
}
