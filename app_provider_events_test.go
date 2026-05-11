package main

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestUnregisterSessionRemovesSessionForMatchingToken covers the normal
// disconnect path: the session stored with the same token is dropped.
func TestUnregisterSessionRemovesSessionForMatchingToken(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-unregister")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-live",
	}

	app.unregisterSession(thread.ID, "token-live")

	app.mu.Lock()
	_, present := app.sessions[thread.ID]
	app.mu.Unlock()
	if present {
		t.Fatal("expected session to be removed after unregisterSession")
	}
}

// TestUnregisterSessionKeepsSessionWhenTokenIsStale protects against the
// reconnect-race: if an older goroutine signals a disconnect AFTER a
// replacement session has already been installed, we must NOT remove the
// replacement. Matches the behavior asserted in
// TestStaleSessionDisconnectDoesNotRemoveReplacement for the broader path.
func TestUnregisterSessionKeepsSessionWhenTokenIsStale(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-unregister-stale")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-current",
	}

	app.unregisterSession(thread.ID, "token-previous")

	app.mu.Lock()
	current, present := app.sessions[thread.ID]
	app.mu.Unlock()
	if !present {
		t.Fatal("replacement session was dropped on stale unregister")
	}
	if current.token != "token-current" {
		t.Fatalf("token = %q, want token-current preserved", current.token)
	}
}

// TestUnregisterSessionWithNoEntryIsSafe confirms we can safely invoke the
// unregister path even if the entry is already gone (e.g. due to an earlier
// StopSession). It must not panic.
func TestUnregisterSessionWithNoEntryIsSafe(t *testing.T) {
	app := newTestAppWithStore(t)

	app.unregisterSession("thread-absent", "any-token")

	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.sessions) != 0 {
		t.Fatalf("sessions = %v, want empty", app.sessions)
	}
}

// TestSessionEventHandlerDisconnectUnregistersSession verifies the
// disconnect path inside sessionEventHandler: the matching-token session is
// cleaned up when a "disconnected" session_status event arrives.
func TestSessionEventHandlerDisconnectUnregistersSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-disconnect-handler")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-disconnect",
	}

	handler := app.sessionEventHandler(thread.ID, "token-disconnect", "")
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	app.mu.Lock()
	_, present := app.sessions[thread.ID]
	app.mu.Unlock()
	if present {
		t.Fatal("session not removed after disconnect event")
	}
}

// TestSessionEventHandlerNonDisconnectStatusPreservesSession makes sure that
// a non-disconnect session_status (e.g. "ready", "error") does NOT remove
// the session. Only the literal "disconnected" content triggers cleanup.
func TestSessionEventHandlerNonDisconnectStatusPreservesSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-status-preserve")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-ok",
	}

	handler := app.sessionEventHandler(thread.ID, "token-ok", "")
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "ready",
		Timestamp: time.Now(),
	})

	app.mu.Lock()
	_, present := app.sessions[thread.ID]
	app.mu.Unlock()
	if !present {
		t.Fatal("session wrongly removed on non-disconnect status event")
	}
}
