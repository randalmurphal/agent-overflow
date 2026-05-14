package main

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestSessionManagerStartDedupesByThread(t *testing.T) {
	app := &App{
		startingSessions: make(map[string]*sessionStart),
	}
	manager := app.sessionManager()

	first, leader := manager.beginStart("thread-1")
	if !leader {
		t.Fatal("first beginStart should lead")
	}
	second, leader := manager.beginStart("thread-1")
	if leader {
		t.Fatal("second beginStart should wait on existing start")
	}
	if second != first {
		t.Fatal("second beginStart did not return existing start state")
	}

	manager.finishStart("thread-1", first)
	if _, ok := manager.startState("thread-1"); ok {
		t.Fatal("start state still registered after finishStart")
	}
}

func TestSessionManagerRecordActivityHonorsSessionToken(t *testing.T) {
	now := time.Unix(100, 0)
	app := &App{
		sessions: map[string]session{
			"thread-1": {
				token:    "current",
				liveness: newSessionLiveness(now),
			},
		},
	}
	manager := app.sessionManager()

	manager.recordActivity("thread-1", "stale", provider.EventTurnStart, "", now.Add(time.Minute))
	sess, _ := manager.get("thread-1")
	if got := sess.liveness.activeTurns.Load(); got != 0 {
		t.Fatalf("active turns after stale token = %d, want 0", got)
	}

	manager.recordActivity("thread-1", "current", provider.EventTurnStart, "", now.Add(time.Minute))
	if got := sess.liveness.activeTurns.Load(); got != 1 {
		t.Fatalf("active turns after current token = %d, want 1", got)
	}
}

func TestSessionManagerSnapshotAndClear(t *testing.T) {
	app := &App{
		sessions: map[string]session{
			"thread-1": {provider: string(provider.Claude)},
			"thread-2": {provider: string(provider.Codex)},
		},
	}

	snapshot := app.sessionManager().snapshotAndClear()
	if got := len(snapshot); got != 2 {
		t.Fatalf("snapshot length = %d, want 2", got)
	}
	if got := len(app.sessions); got != 0 {
		t.Fatalf("app sessions after snapshot = %d, want 0", got)
	}
}
