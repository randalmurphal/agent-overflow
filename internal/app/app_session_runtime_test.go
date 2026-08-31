package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestPreInitTeardownTokenGuard: a DOA teardown fired for a stale
// session token must not touch the replacement session a user retry
// already registered. The guard is the token-checked unregister, which
// returns false before any teardown step runs — no store, triage, or
// design wiring is reachable on this path, so a bare App suffices.
func TestPreInitTeardownTokenGuard(t *testing.T) {
	app := &App{}
	app.sessionManager().put("t-guard", session{Provider: "claude", Token: "fresh-token"})

	app.teardownDeadPreInitSession("t-guard", "stale-token")

	sess, still := app.sessionManager().get("t-guard")
	if !still || sess.Token != "fresh-token" {
		t.Fatalf("replacement session was disturbed by a stale-token teardown: present=%v token=%q", still, sess.Token)
	}
}

func TestDirectSessionRemovalEmitsAccountDisconnect(t *testing.T) {
	for _, test := range []struct {
		name   string
		remove func(*App) error
	}{
		{
			name: "replacement",
			remove: func(app *App) error {
				return app.stopExistingSessionLocked("thread")
			},
		},
		{
			name: "pre-init failure",
			remove: func(app *App) error {
				app.teardownDeadPreInitSession("thread", "token")
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := &App{}
			app.sessionManager().put("thread", session{Provider: string(provider.Codex), Token: "token"})
			var disconnected int
			app.testEmitHook = func(name string, data any) {
				event, ok := data.(ProviderSessionAccountEvent)
				if name == "provider:session_account" && ok && !event.Connected {
					disconnected++
				}
			}

			if err := test.remove(app); err != nil {
				t.Fatal(err)
			}

			if disconnected != 1 {
				t.Fatalf("disconnect events = %d, want 1", disconnected)
			}
		})
	}
}

func newStartStateApp() *App {
	return &App{}
}

// A panicking leader must still close its start-state and drop the in-flight
// entry. Before finishStart was deferred, one panic left every joiner parked
// on a channel that would never close AND left beginStart handing that dead
// entry to every later start of the thread — a permanently wedged thread.
func TestRunSessionStartPanicReleasesJoinersAndEntry(t *testing.T) {
	app := newStartStateApp()

	joinerParked := make(chan struct{})
	joined := make(chan error, 1)
	panicked := make(chan any, 1)

	go func() {
		defer func() { panicked <- recover() }()
		_ = app.runSessionStart(context.Background(), "thread-1", func() error {
			// Park a joiner on this start before blowing up, so the test
			// covers a waiter that is already committed to the wait.
			<-joinerParked
			panic("provider spawn exploded")
		})
	}()

	// Wait until the leader has registered, then join it.
	waitForStartStateRegistered(t, app, "thread-1")
	go func() {
		joined <- app.runSessionStart(context.Background(), "thread-1", func() error {
			t.Error("joiner ran start(); it should have joined the leader")
			return nil
		})
	}()
	waitForStartJoiners(t, app, "thread-1")
	close(joinerParked)

	select {
	case r := <-panicked:
		if r == nil {
			t.Fatal("leader's panic was swallowed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not unwind")
	}

	select {
	case err := <-joined:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("joiner err = %v, want a panic-derived error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joiner hung after the leader panicked")
	}

	if _, ok := app.sessionManager().startState("thread-1"); ok {
		t.Fatal("start state still registered after a panicking start")
	}

	// The thread is not poisoned: a later start leads normally.
	ran := false
	if err := app.runSessionStart(context.Background(), "thread-1", func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("start after panic: %v", err)
	}
	if !ran {
		t.Fatal("start after panic did not run; the leaked entry was still in place")
	}
}

// A joiner has performed no side effects, so its wait is the caller's to
// abandon. The leader must not notice.
func TestRunSessionStartJoinerHonorsContextCancel(t *testing.T) {
	app := newStartStateApp()

	releaseLeader := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- app.runSessionStart(context.Background(), "thread-1", func() error {
			<-releaseLeader
			return nil
		})
	}()
	waitForStartStateRegistered(t, app, "thread-1")

	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() {
		joined <- app.runSessionStart(ctx, "thread-1", func() error {
			t.Error("joiner ran start(); it should have joined the leader")
			return nil
		})
	}()
	waitForStartJoiners(t, app, "thread-1")

	cancel()
	select {
	case err := <-joined:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("joiner err = %v, want context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "in-flight session start of thread thread-1") {
			t.Fatalf("joiner err = %q, want it to name the wait it abandoned", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled joiner did not return")
	}

	// The leader is unaffected: still running, still the single-flight owner.
	if _, ok := app.sessionManager().startState("thread-1"); !ok {
		t.Fatal("leader's start state disappeared when the joiner gave up")
	}
	close(releaseLeader)
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("leader err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not finish")
	}
	if _, ok := app.sessionManager().startState("thread-1"); ok {
		t.Fatal("start state still registered after the leader finished")
	}
}

// An already-cancelled joiner never blocks, and never disturbs the leader.
func TestRunSessionStartJoinerRefusesDeadContext(t *testing.T) {
	app := newStartStateApp()

	releaseLeader := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- app.runSessionStart(context.Background(), "thread-1", func() error {
			<-releaseLeader
			return nil
		})
	}()
	waitForStartStateRegistered(t, app, "thread-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.runSessionStart(ctx, "thread-1", func() error {
		t.Error("joiner ran start(); it should have joined the leader")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("joiner err = %v, want context.Canceled", err)
	}

	close(releaseLeader)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err = %v, want nil", err)
	}
}

func waitForStartStateRegistered(t *testing.T, app *App, threadID string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := app.sessionManager().startState(threadID); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no in-flight start registered for %s", threadID)
}

// waitForStartJoiners waits until at least one goroutine is parked on the
// thread's start-state. There is no counter to read, so this settles on the
// goroutine having reached the select — good enough because the alternative
// (proceeding early) only ever makes the test's assertions weaker, never
// flaky-green: every assertion that follows also has its own timeout.
func waitForStartJoiners(t *testing.T, app *App, threadID string) {
	t.Helper()

	startState, ok := app.sessionManager().startState(threadID)
	if !ok {
		t.Fatalf("no in-flight start registered for %s", threadID)
	}
	select {
	case <-startState.Done:
		t.Fatalf("start for %s finished before a joiner could park", threadID)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestThreadHasUnresolvedCodexSubagents pins the store question behind
// codex.Config.ResumeHasUnresolvedSubagents — the relevance gate on the
// rollout tail a resumed Codex session uses to recover detached-child mailbox
// deliveries it cannot see as raw events.
//
// The predicate is deliberately "a background spawn launch with no completion
// sibling", NOT the narrower live-background flag: a child's own status signal
// clears that flag the moment the child goes terminal, and the window the tail
// exists for is exactly the one after that, while the child's FINAL_ANSWER is
// still undelivered in the parent's mailbox.
func TestCodexResumeCollabLaunchesReturnsOnlyUnresolvedOwnership(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/w-codex-resume-tail", "gpt-5.3-codex", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "t0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 0,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	if launches, err := app.codexResumeCollabLaunches(thread.ID); err != nil {
		t.Fatalf("load empty resume ownership: %v", err)
	} else if len(launches) != 0 {
		t.Fatalf("a thread that never spawned an agent returned ownership: %+v", launches)
	}

	launch := store.Item{
		ID:           "spawn-1",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent",
		IsBackground: true,
		ToolName:     "collab_agent",
		// The child has already gone terminal and the launch is no longer
		// "live", which is the state the mailbox delivery still has to close.
		Meta: `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]},` +
			`"codex_child_terminal_statuses":{"child-1":"completed"},"live_background_active":false}`,
		CreatedAt: 1000,
		UpdatedAt: 1000,
	}
	if err := app.store.InsertItem(launch); err != nil {
		t.Fatalf("seed spawn launch: %v", err)
	}
	launches, err := app.codexResumeCollabLaunches(thread.ID)
	if err != nil {
		t.Fatalf("load unresolved resume ownership: %v", err)
	}
	if len(launches) != 1 || launches[0].ItemID != "spawn-1" || !strings.Contains(string(launches[0].Meta), `"child-1"`) {
		t.Fatalf("unresolved resume ownership = %+v, want compact spawn-1 metadata", launches)
	}

	if err := app.store.InsertItem(store.Item{
		ID:           "spawn-1-complete",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent -> done",
		CompletionOf: "spawn-1",
		CreatedAt:    2000,
		UpdatedAt:    2000,
	}); err != nil {
		t.Fatalf("seed completion sibling: %v", err)
	}
	if launches, err := app.codexResumeCollabLaunches(thread.ID); err != nil {
		t.Fatalf("load settled resume ownership: %v", err)
	} else if len(launches) != 0 {
		t.Fatalf("a spawn whose answer already landed returned ownership: %+v", launches)
	}
}
