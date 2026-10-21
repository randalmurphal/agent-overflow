package app

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// interruptInTurnStartGap parks a turn start after its turns row is
// written and before its open turn is set — the state a workflow
// takeover's GetActiveTurn gate interrupts in — runs interrupt there,
// and returns the thread's error rows once both finished. interrupt must
// not finish while the start is parked: the production sampler waits it
// out.
func interruptInTurnStartGap(t *testing.T, name string, interrupt func(app *App, threadID string) error) []string {
	t.Helper()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()
	thread := testThread(name)
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Token: name + "-token", Codex: sess,
	})

	inGap := make(chan struct{})
	release := make(chan struct{})
	var gateOnce, releaseOnce sync.Once
	releaseStart := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStart)
	app.triage.SetTurnRowWrittenGate(func(string) {
		gateOnce.Do(func() {
			close(inGap)
			<-release
		})
	})
	started := make(chan error, 1)
	go func() {
		started <- app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now(),
		})
	}()
	select {
	case <-inGap:
	case <-time.After(10 * time.Second):
		t.Fatal("turn start never reached the row-written gate")
	}
	if _, active, err := app.store.GetActiveTurn(thread.ID); err != nil || !active {
		t.Fatalf("precondition: GetActiveTurn active=%v err=%v; want the started row", active, err)
	}

	// The start is released only once the interrupt is waiting on it in
	// InterruptTurnIndex; an interrupt that finishes first sampled the gap.
	waiting := make(chan struct{})
	var waitOnce sync.Once
	app.triage.SetInterruptWaitHook(func(_ string, woke bool) {
		if !woke {
			waitOnce.Do(func() { close(waiting) })
		}
	})
	done := make(chan error, 1)
	go func() { done <- interrupt(app, thread.ID) }()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("interrupt finished without waiting on the parked turn start (err=%v): it sampled the gap", err)
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt never waited on the parked turn start")
	}
	releaseStart()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("turn start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("released turn start never returned")
	}
	var interruptErr error
	select {
	case interruptErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt still waiting after the turn start returned")
	}
	if interruptErr != nil {
		t.Fatalf("interrupt: %v", interruptErr)
	}
	// A marker written before the open turn was set would share its id
	// with the turn's next error and be overwritten by it.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: thread.ID, Content: "later error", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("later error: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	var errors []string
	for _, item := range items {
		if item.Kind == "error" {
			errors = append(errors, item.Summary)
		}
	}
	return errors
}

func requireStopAndLaterError(t *testing.T, errors []string) {
	t.Helper()
	if len(errors) != 2 || errors[0] != "Stopped by user" || errors[1] != "later error" {
		t.Fatalf("error rows = %q, want [Stopped by user, later error]", errors)
	}
}

func TestInterruptTurnRecordsStopInTurnStartGap(t *testing.T) {
	t.Parallel()
	requireStopAndLaterError(t, interruptInTurnStartGap(t, "interrupt-start-gap", func(app *App, threadID string) error {
		return app.InterruptTurn(threadID, nil)
	}))
}

func TestPlainInterruptFallbackRecordsStopInTurnStartGap(t *testing.T) {
	t.Parallel()
	requireStopAndLaterError(t, interruptInTurnStartGap(t, "plain-interrupt-start-gap", func(app *App, threadID string) error {
		// The caller owns the thread action lock, as InterruptAndRevertIfClean does.
		unlock, err := app.threadLocks().LockCtx(t.Context(), threadID)
		if err != nil {
			return err
		}
		defer unlock()
		return app.runPlainInterruptLocked(threadID)
	}))
}
