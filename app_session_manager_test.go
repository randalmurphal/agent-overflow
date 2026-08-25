package main

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
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

// The thread set a settings-driven sweep visits is sessions ∪ starting, and
// the starting half is what B20 was: a spawn snapshots Settings, a save lands
// before the session registers, and a sweep over the session map alone leaves
// that thread running the pre-save config for its whole life.
//
// A start is included regardless of providerName because an in-flight start
// has no provider yet — the thread row is read inside the start — while a
// REGISTERED session on another provider is not this fan-out's business.
func TestThreadIDsForProviderOrStartingIncludesAStartingSession(t *testing.T) {
	app := &App{
		sessions: map[string]session{
			"live-claude": {provider: string(provider.Claude)},
			"live-codex":  {provider: string(provider.Codex)},
		},
	}
	manager := app.sessionManager()
	startState, leader := manager.beginStart("starting-only")
	if !leader {
		t.Fatal("beginStart did not lead for a fresh thread")
	}
	defer manager.finishStart("starting-only", startState)

	got := threadIDSet(t, manager.threadIDsForProviderOrStarting(string(provider.Claude)))
	if !got["starting-only"] {
		t.Fatal("a thread that is only in startingSessions was skipped")
	}
	if !got["live-claude"] {
		t.Fatal("the registered Claude session was skipped")
	}
	if got["live-codex"] {
		t.Fatal("a registered Codex session was swept by the Claude fan-out")
	}
	if len(got) != 2 {
		t.Fatalf("thread ids = %v, want exactly the Claude session and the start", got)
	}
}

// The registration handoff is the overlap that must not become a hole OR a
// duplicate: runSessionStart puts the session into `sessions` before it clears
// `startingSessions`, so the thread is briefly in both. Reading both maps
// under one lock makes that "at least once"; the seen-set makes it exactly
// once, which matters because each id costs one reconcile's worth of wire I/O.
func TestThreadIDsForProviderOrStartingCountsAHandingOffThreadOnce(t *testing.T) {
	app := &App{
		sessions: map[string]session{
			"handoff": {provider: string(provider.Claude)},
		},
	}
	manager := app.sessionManager()
	startState, leader := manager.beginStart("handoff")
	if !leader {
		t.Fatal("beginStart did not lead for a fresh thread")
	}
	defer manager.finishStart("handoff", startState)

	ids := manager.threadIDsForProviderOrStarting(string(provider.Claude))
	if len(ids) != 1 || ids[0] != "handoff" {
		t.Fatalf("thread ids = %v, want the handing-off thread exactly once", ids)
	}
}

func TestMCPOAuthSessionSnapshotsRespectProviderAndWorkspaceOwnership(t *testing.T) {
	app := &App{sessions: map[string]session{
		"claude-root": {
			provider:   string(provider.Claude),
			claude:     &claude.Session{},
			launchOpts: provider.SessionOptions{WorkDir: "/repo"},
		},
		"claude-worktree": {
			provider:   string(provider.Claude),
			claude:     &claude.Session{},
			launchOpts: provider.SessionOptions{WorkDir: "/repo/.wt/a"},
		},
		"codex": {
			provider: string(provider.Codex),
			codex:    &codex.Session{},
		},
	}}

	claudeSessions := app.sessionManager().claudeMCPSessions("/repo")
	if len(claudeSessions) != 1 || claudeSessions[0].threadID != "claude-root" {
		t.Fatalf("Claude MCP sessions = %+v, want only the matching workspace", claudeSessions)
	}
	codexSessions := app.sessionManager().codexMCPSessions()
	if len(codexSessions) != 1 || codexSessions[0].threadID != "codex" {
		t.Fatalf("Codex MCP sessions = %+v, want every Codex process", codexSessions)
	}
}

func threadIDSet(t *testing.T, ids []string) map[string]bool {
	t.Helper()
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if set[id] {
			t.Fatalf("thread id %q appeared twice in %v", id, ids)
		}
		set[id] = true
	}
	return set
}
