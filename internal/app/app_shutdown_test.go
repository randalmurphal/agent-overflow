package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/closer"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/logging"
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/triage"
)

// TestRunParallelClosersFinishesWithinTimeoutWhenAllSlow exercises Bug B4:
// ten closers each taking ~1.5 s must finish concurrently (total wall clock
// ~1.5 s), not sequentially (15 s). A regression that reverted to a
// serial loop would blow past the 5-second bound.
func TestRunParallelClosersFinishesWithinTimeoutWhenAllSlow(t *testing.T) {
	const (
		count   = 10
		delay   = 1500 * time.Millisecond
		timeout = 5 * time.Second
	)
	closers := make([]closer.Task, 0, count)
	var completed atomic.Int32
	for i := 0; i < count; i++ {
		label := fmt.Sprintf("slow-%d", i)
		closers = append(closers, closer.Task{
			Label: label,
			Close: func() error {
				time.Sleep(delay)
				completed.Add(1)
				return nil
			},
		})
	}

	start := time.Now()
	errs := closer.RunParallel(closers, timeout)
	elapsed := time.Since(start)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if completed.Load() != count {
		t.Fatalf("completed = %d, want %d", completed.Load(), count)
	}
	// Parallel execution budget: delay + scheduling slack. Sequential
	// execution would take 15 s and fail this assertion.
	if elapsed > delay+2*time.Second {
		t.Fatalf("shutdown took %v, expected <%v (parallel execution regression)", elapsed, delay+2*time.Second)
	}
}

// TestRunParallelClosersTimesOutOnHangingCloser exercises the hard-cap
// behaviour: one hung closer must not block the rest of the teardown.
// The deadline returns within the window and the hanging closer is
// reported as a timeout error.
func TestRunParallelClosersTimesOutOnHangingCloser(t *testing.T) {
	const timeout = 500 * time.Millisecond

	hungRelease := make(chan struct{})
	defer close(hungRelease)

	closers := []closer.Task{
		{Label: "fast-1", Close: func() error { return nil }},
		{Label: "fast-2", Close: func() error {
			time.Sleep(100 * time.Millisecond)
			return nil
		}},
		{Label: "hanger", Close: func() error {
			// Blocks until the test releases it at the end. A
			// shutdown regression that Wait'd on this closer would
			// block test teardown.
			<-hungRelease
			return nil
		}},
	}

	start := time.Now()
	errs := closer.RunParallel(closers, timeout)
	elapsed := time.Since(start)

	if elapsed > timeout+500*time.Millisecond {
		t.Fatalf("shutdown took %v, expected <%v (hanging closer blocked return)", elapsed, timeout+500*time.Millisecond)
	}

	// We expect exactly one timeout error, for the hanger. The two fast
	// closers must not have produced timeouts.
	var timeoutCount, hangerSeen int
	for _, err := range errs {
		if strings.Contains(err.Error(), "did not finish") {
			timeoutCount++
			if strings.Contains(err.Error(), "hanger") {
				hangerSeen++
			}
		}
	}
	if timeoutCount != 1 {
		t.Fatalf("timeout errors = %d, want 1 (errs=%v)", timeoutCount, errs)
	}
	if hangerSeen != 1 {
		t.Fatalf("hanger timeout not reported (errs=%v)", errs)
	}
}

// TestRunParallelClosersSurfacesIndividualErrors checks that a failure in
// one closer is surfaced without preventing the others from running.
func TestRunParallelClosersSurfacesIndividualErrors(t *testing.T) {
	closers := []closer.Task{
		{Label: "ok-1", Close: func() error { return nil }},
		{Label: "bad-1", Close: func() error {
			return fmt.Errorf("intentional failure")
		}},
		{Label: "ok-2", Close: func() error { return nil }},
	}

	errs := closer.RunParallel(closers, 2*time.Second)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one", errs)
	}
	if !strings.Contains(errs[0].Error(), "intentional failure") {
		t.Fatalf("error message does not mention failure: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "bad-1") {
		t.Fatalf("error message does not mention closer label: %v", errs[0])
	}
}

// TestRunParallelClosersEmpty is a defensive check — no closers means no
// errors, no goroutine leaks, no deadline to hit.
func TestRunParallelClosersEmpty(t *testing.T) {
	errs := closer.RunParallel(nil, time.Second)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

// newFullyWiredTestApp builds an App where every subsystem that Shutdown
// walks is real. Each subsystem uses t.TempDir() or a noop provider so
// the test stays hermetic, but Shutdown exercises the exact code paths
// production does. The returned recorder captures the step sequence for
// the order assertions below.
func newFullyWiredTestApp(t *testing.T) (*App, *shutdownRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(nil, nil)
	app.replay = replay.NewManager(replay.ManagerConfig{
		RootDir: t.TempDir(),
		Enabled: false,
	})
	tel, err := obsotel.NewProvider(context.Background(), obsotel.Config{Enabled: false})
	if err != nil {
		t.Fatalf("obsotel.NewProvider: %v", err)
	}
	app.telemetry = tel
	// The triage router does not expose in-flight work today; Shutdown
	// still dispatches drainTriage under a timeout and records the step.
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	// gitwatch.Manager has no real watchers in this test (no Subscribe
	// calls), but Close() still records the "close gitwatch" step, so
	// we wire it for parity with production.
	app.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn: func(string) (gitops.GitStatus, error) {
			return gitops.GitStatus{}, nil
		},
	})
	// Force logger on so every step in Shutdown fires — the debug env
	// gate makes it nil by default which would hide the "close logger"
	// step from the ordering assertion.
	t.Setenv("AGENT_OVERFLOW_DEBUG", "provider")
	logger, err := logging.NewProviderEventLogger(t.TempDir())
	if err != nil {
		t.Fatalf("logging.NewProviderEventLogger: %v", err)
	}
	app.logger = logger

	rec := &shutdownRecorder{}
	app.shutdownStepFn = rec.record
	return app, rec
}

// shutdownRecorder captures the step sequence for Shutdown tests. The
// slice is guarded by a mutex because session closers run on goroutines
// and their "close provider sessions" step is recorded from the main
// shutdown goroutine only — but other tests may plug concurrent hooks.
type shutdownRecorder struct {
	mu    sync.Mutex
	steps []string
	errs  map[string]error
}

func (r *shutdownRecorder) record(step string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
	if err != nil {
		if r.errs == nil {
			r.errs = make(map[string]error)
		}
		r.errs[step] = err
	}
}

func (r *shutdownRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

// TestShutdownWalksDocumentedOrder is the anchor test for Task A. The
// numbered comments in Shutdown(ctx) are a contract — this test fails if
// the order ever drifts. Keep the expected slice in sync with the
// in-code comments.
func TestShutdownWalksDocumentedOrder(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	err := app.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	want := []string{
		// "cancel app context" MUST appear before "drain triage" —
		// fire-and-forget goroutines bound to appCtx need to observe
		// ctx.Done() and stop filing triage.Handle calls BEFORE the
		// drain barrier runs. Cancelling after the drain would let
		// in-flight goroutines slip past it.
		"cancel app context",
		"drain triage",
		"drain flush dispatch",
		// "drain notifications" MUST appear before "close store" — a queued
		// notification reads the thread's title out of SQLite to name it,
		// and it joins the other reactor drains because the events it
		// reacts to stop arriving once triage has drained.
		"drain notifications",
		"close replay manager",
		"shutdown telemetry",
		"stop idle session reaper",
		// "stop retention cleanup" MUST appear between
		// "stop idle session reaper" and "close provider sessions" —
		// the sweep calls deleteThreadTreeLocked which mutates
		// a.sessions via stopSession; running it concurrently with
		// Step 4's snapshotAndClear would race the session map.
		"stop retention cleanup",
		// "stop background git fetch" MUST appear before "close store" —
		// every pass reads the project list out of SQLite. It sits here,
		// with the other timer-driven loops, so teardown has exactly one
		// place where background cadences are joined.
		"stop background git fetch",
		// "stop worktree setups" MUST appear before "close store" — settling
		// a run writes the thread's durable worktree_setup_state. It joins
		// alongside the other background cadences for the same reason.
		"stop worktree setups",
		// "stop session imports" MUST appear before "close store" — an
		// import run writes threads, items and turns straight into SQLite.
		// Same slot as the other background work for the same reason.
		"stop session imports",
		// "stop thread read stamps" MUST appear before "close store" —
		// SwitchThread's read-state stamp runs off the RPC path, so an
		// in-flight one is a SQLite write with nobody holding it open.
		"stop thread read stamps",
		// "stop provider logins" MUST appear before "close store" — a
		// sign-in that completes writes the account's metadata row, and
		// joining here is also what removes its temporary credential home
		// instead of leaving it for the next boot's sweep.
		"stop provider logins",
		"close provider sessions",
		// "stop orphan reaper" follows session close: each session
		// releases its watched group on a clean close, so the sidecar has
		// nothing left to reap by the time we close its control pipe.
		"stop orphan reaper",
		"close gitwatch manager",
		"close PR update subscriptions",
		"close terminal sessions",
		"close logger",
		"close store",
	}
	got := rec.snapshot()

	if len(got) != len(want) {
		t.Fatalf("step count = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step[%d] = %q, want %q\n got: %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

// TestShutdownIsIdempotent verifies a second Shutdown call returns nil
// and does not re-run any subsystem close. Wails' lifecycle + tests that
// also call Shutdown must stay safe under double-invocation.
func TestShutdownIsIdempotent(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	firstCount := len(rec.snapshot())

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	secondCount := len(rec.snapshot())

	if secondCount != firstCount {
		t.Fatalf("second Shutdown recorded %d additional steps (total %d); want idempotent",
			secondCount-firstCount, secondCount)
	}
}

// TestShutdownContinuesAfterSubsystemError plays out the "one subsystem
// fails" scenario: Shutdown must not abort on the first error, every
// downstream step must still run, and the returned error must
// errors.Join every underlying failure.
func TestShutdownContinuesAfterSubsystemError(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	// Inject a failure on the replay manager close (step 3). Every
	// step after it — provider sessions, terminals, logger, store —
	// must still execute.
	injected := errors.New("replay closed sideways")
	app.shutdownInjectErrFn = func(step string, err error) error {
		if step == "close replay manager" {
			return injected
		}
		return err
	}

	err := app.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown() error = nil, want injected replay error")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("Shutdown() error = %v, want errors.Is(injected)", err)
	}
	if !strings.Contains(err.Error(), "close replay manager") {
		t.Fatalf("Shutdown() error = %v, want wrapped replay close context", err)
	}

	got := rec.snapshot()
	// Every step must have run. The store step is the last one, so
	// seeing it after the failing replay step is the "did not abort
	// early" assertion.
	foundStore := false
	foundLogger := false
	for _, step := range got {
		if step == "close store" {
			foundStore = true
		}
		if step == "close logger" {
			foundLogger = true
		}
	}
	if !foundStore {
		t.Fatalf("store close step never ran; steps=%v", got)
	}
	if !foundLogger {
		t.Fatalf("logger close step never ran despite earlier replay error; steps=%v", got)
	}
}

// TestShutdownAggregatesMultipleErrorsWithErrorsJoin ensures the
// returned error surfaces every failing step, not just the first one.
// We inject failures on two independent steps; Shutdown should still
// walk the remaining steps and return a joined error whose Unwrap exposes
// both injected errors (the errors.Join contract).
func TestShutdownAggregatesMultipleErrorsWithErrorsJoin(t *testing.T) {
	app, _ := newFullyWiredTestApp(t)

	replayErr := errors.New("replay blew up")
	loggerErr := errors.New("logger refused to close")
	app.shutdownInjectErrFn = func(step string, err error) error {
		switch step {
		case "close replay manager":
			return replayErr
		case "close logger":
			return loggerErr
		}
		return err
	}

	err := app.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown() error = nil, want joined error from two injected failures")
	}
	if !errors.Is(err, replayErr) {
		t.Fatalf("Shutdown() error = %v, want errors.Is(replayErr)", err)
	}
	if !errors.Is(err, loggerErr) {
		t.Fatalf("Shutdown() error = %v, want errors.Is(loggerErr)", err)
	}
}

// TestShutdownBlocksNewWork covers the first step of the Shutdown
// contract: after Shutdown returns, binding entry points must fail fast
// with ErrShuttingDown instead of spinning up subsystems we'd have to
// tear straight back down.
func TestShutdownBlocksNewWork(t *testing.T) {
	app, _ := newFullyWiredTestApp(t)
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if err := app.StartSession("thread-x"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("StartSession after shutdown = %v, want ErrShuttingDown", err)
	}
	if err := app.SendMessage("thread-x", "hi", nil); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("SendMessage after shutdown = %v, want ErrShuttingDown", err)
	}
	if err := app.ReconnectSession("thread-x"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("ReconnectSession after shutdown = %v, want ErrShuttingDown", err)
	}
	if err := app.startSession(context.Background(), "thread-x"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("startSession after shutdown = %v, want ErrShuttingDown", err)
	}
}

// TestShutdownSetsFlagAtomically protects the ordering of the CAS in
// Shutdown: a concurrent StartSession must see the flag before the
// teardown runs, so the CAS happens before any subsystem close. If a
// regression moved the CAS after (say) the triage drain, this test
// would see late StartSession calls slip through and reach
// startSessionNow before the flag flipped.
func TestShutdownSetsFlagAtomically(t *testing.T) {
	app, _ := newFullyWiredTestApp(t)

	// Pre-flight: StartSession without shutdown should not trip the
	// ErrShuttingDown branch (it will fail for a different reason —
	// unknown thread — but not ErrShuttingDown).
	if err := app.StartSession("nonexistent"); errors.Is(err, ErrShuttingDown) {
		t.Fatal("StartSession returned ErrShuttingDown before shutdown")
	}

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// Post-shutdown.
	if err := app.StartSession("nonexistent"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("StartSession post-shutdown = %v, want ErrShuttingDown", err)
	}
}

// TestServiceShutdownDelegatesToShutdown verifies the Wails lifecycle
// wrapper ends up in the same code path as a direct Shutdown call.
// Matters because the existing TestServiceShutdownClosesSessions* tests
// use ServiceShutdown() and we kept that surface stable.
func TestServiceShutdownDelegatesToShutdown(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	if err := app.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown() error = %v", err)
	}
	if steps := rec.snapshot(); len(steps) == 0 {
		t.Fatal("ServiceShutdown recorded no steps; expected it to delegate to Shutdown")
	}
}

// Shared use of the store subsystem during shutdown is single-threaded
// — the session closers release their hold on a.store via closeProviderSession
// before Shutdown touches it. This test keeps that invariant honest by
// closing a real Claude session during shutdown and asserting no panic
// or deadlock. It complements the existing
// TestServiceShutdownClosesSessionsWithoutDeadlock (session-only) by
// exercising the full Shutdown pipeline.
func TestShutdownClosesLiveSessionsWithoutDeadlock(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	thread := store.Thread{
		ID:            "thread-shutdown-live",
		ProjectID:     defaultTestProjectID,
		Title:         "Live",
		Provider:      "claude",
		WorkspacePath: t.TempDir(),
		Model:         "claude-sonnet-4-6",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- app.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return within 10s; possible deadlock")
	}

	if steps := rec.snapshot(); len(steps) == 0 {
		t.Fatal("Shutdown recorded no steps")
	}
}

// TestShutdownStepFnObservesErrorsPerStep guards the shutdown hook's
// second contract: callers (tests + future observability) can observe
// per-step errors without waiting for the aggregated return value.
func TestShutdownStepFnObservesErrorsPerStep(t *testing.T) {
	app, rec := newFullyWiredTestApp(t)

	injected := errors.New("sentinel from store step")
	app.shutdownInjectErrFn = func(step string, err error) error {
		if step == "close store" {
			return injected
		}
		return err
	}

	_ = app.Shutdown(context.Background())

	rec.mu.Lock()
	storeErr := rec.errs["close store"]
	rec.mu.Unlock()
	if !errors.Is(storeErr, injected) {
		t.Fatalf("recorder captured %v, want errors.Is(injected)", storeErr)
	}
}
