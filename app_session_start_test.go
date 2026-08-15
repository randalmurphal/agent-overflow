package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newStartStateApp() *App {
	return &App{startingSessions: make(map[string]*sessionStart)}
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
	case <-startState.done:
		t.Fatalf("start for %s finished before a joiner could park", threadID)
	case <-time.After(20 * time.Millisecond):
	}
}
