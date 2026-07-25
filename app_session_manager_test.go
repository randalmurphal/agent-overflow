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

// TestPreInitTeardownTokenGuard: a DOA teardown fired for a stale
// session token must not touch the replacement session a user retry
// already registered. The guard is the token-checked unregister, which
// returns false before any teardown step runs — no store, triage, or
// design wiring is reachable on this path, so a bare App suffices.
func TestPreInitTeardownTokenGuard(t *testing.T) {
	app := &App{
		sessions: map[string]session{
			"t-guard": {provider: "claude", token: "fresh-token"},
		},
	}

	app.teardownDeadPreInitSession("t-guard", "stale-token")

	sess, still := app.sessionManager().get("t-guard")
	if !still || sess.token != "fresh-token" {
		t.Fatalf("replacement session was disturbed by a stale-token teardown: present=%v token=%q", still, sess.token)
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
			app := &App{
				sessions: map[string]session{
					"thread": {provider: string(provider.Codex), token: "token"},
				},
			}
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
