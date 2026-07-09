package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// TestUnregisterSessionRemovesSessionForMatchingToken covers the normal
// disconnect path: the session stored with the same token is dropped.
func TestUnregisterSessionRemovesSessionForMatchingToken(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := testThread("thread-unregister")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-live",
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventModelFallback,
		ThreadID: thread.ID,
		ItemID:   "model-fallback:req-unregister",
		Meta:     json.RawMessage(`{"fallbackModel":"claude-opus-4-8"}`),
	}); err != nil {
		t.Fatalf("seed model fallback: %v", err)
	}

	app.unregisterSession(thread.ID, "token-live")

	app.mu.Lock()
	_, present := app.sessions[thread.ID]
	app.mu.Unlock()
	if present {
		t.Fatal("expected session to be removed after unregisterSession")
	}
	if got := app.triage.LiveStateSnapshotForThread(thread.ID).EffectiveModel; got != "" {
		t.Fatalf("effective model after unregister = %q", got)
	}
}

// TestUnregisterSessionKeepsSessionWhenTokenIsStale protects against the
// reconnect-race: if an older goroutine signals a disconnect AFTER a
// replacement session has already been installed, we must NOT remove the
// replacement. Matches the behavior asserted in
// TestStaleSessionDisconnectDoesNotRemoveReplacement for the broader path.
func TestUnregisterSessionKeepsSessionWhenTokenIsStale(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := testThread("thread-unregister-stale")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "token-current",
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventModelFallback,
		ThreadID: thread.ID,
		ItemID:   "model-fallback:req-stale-unregister",
		Meta:     json.RawMessage(`{"fallbackModel":"claude-opus-4-8"}`),
	}); err != nil {
		t.Fatalf("seed model fallback: %v", err)
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
	if got := app.triage.LiveStateSnapshotForThread(thread.ID).EffectiveModel; got != "claude-opus-4-8" {
		t.Fatalf("effective model after stale unregister = %q", got)
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

// TestSessionEventHandlerAutoReconnectsAfterAbnormalDeath verifies the
// recovery hook fires when a session sees the "error" → "disconnected"
// pair (an abnormal exit) and the thread has a stored SessionRef. The
// auto-reconnect is the path that turns SIGTERM / clean-exit-0 / OOM-kill
// into a silently-recovered session without user action.
func TestSessionEventHandlerAutoReconnectsAfterAbnormalDeath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto-reconnect")
	thread.SessionRef = "claude-resume-abc"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "token-dead",
	}

	startCalls := make(chan string, 4)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(threadID string) error {
		startCalls <- threadID
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-dead", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	select {
	case got := <-startCalls:
		if got != thread.ID {
			t.Fatalf("startSessionFn invoked for %q, want %q", got, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-reconnect startSession call")
	}
}

// TestSessionEventHandlerNoAutoReconnectWithoutDeathSignal pins the
// intentional-stop carve-out: a "disconnected" event that arrived without
// a preceding "error" came from us closing the session and must NOT
// resurrect it. Without this gate, calling StopSession on a thread would
// instantly restart its provider.
func TestSessionEventHandlerNoAutoReconnectWithoutDeathSignal(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-clean-stop")
	thread.SessionRef = "claude-resume-clean"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "token-clean",
	}

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-clean", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	select {
	case <-startCalled:
		t.Fatal("startSessionFn called on clean disconnect; auto-reconnect should be gated on prior error event")
	case <-time.After(150 * time.Millisecond):
		// expected — no auto-reconnect for a clean stop
	}
}

// TestSessionEventHandlerNoAutoReconnectWithoutSessionRef confirms the
// SessionRef gate: a death that hits before the provider published a
// resume cursor cannot be auto-recovered (--resume needs a target), so we
// leave the banner up and let the user decide.
func TestSessionEventHandlerNoAutoReconnectWithoutSessionRef(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-sessionref")
	// SessionRef intentionally empty
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "token-early",
	}

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	handler := app.sessionEventHandler(thread.ID, "token-early", string(provider.Claude))
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	select {
	case <-startCalled:
		t.Fatal("startSessionFn called despite empty SessionRef; auto-reconnect should skip when there is nothing to --resume against")
	case <-time.After(150 * time.Millisecond):
		// expected
	}
}

// TestAutoReconnectSingleShotAcrossDeathsThroughHandler exercises the
// loop guard end-to-end: two abnormal-death sequences without an
// intervening EventTurnStart must produce exactly one ReconnectSession
// attempt. Then EventTurnStart clears the guard and a subsequent death
// must produce a fresh attempt. Drives everything through the public
// event handler so the test would fail if attemptAutoReconnect stopped
// gating on markAutoReconnectAttempted, or if EventTurnStart stopped
// clearing the flag.
func TestAutoReconnectSingleShotAcrossDeathsThroughHandler(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-loop-guard")
	thread.SessionRef = "claude-resume-loopguard"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	startCalls := make(chan string, 8)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(threadID string) error {
		startCalls <- threadID
		return nil
	}

	fireDeath := func(token string) {
		handler := app.sessionEventHandler(thread.ID, token, string(provider.Claude))
		app.sessions[thread.ID] = session{provider: string(provider.Claude), token: token}
		handler(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  thread.ID,
			Content:   "error",
			Timestamp: time.Now(),
		})
		handler(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  thread.ID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}

	// First death: auto-reconnect fires.
	fireDeath("tok-death-1")
	select {
	case <-startCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("first death did not trigger auto-reconnect startSession")
	}

	// Second death without intervening turn_started: must be suppressed
	// by the single-shot guard.
	fireDeath("tok-death-2")
	select {
	case got := <-startCalls:
		t.Fatalf("second death triggered a duplicate auto-reconnect (start=%q); loop guard failed", got)
	case <-time.After(150 * time.Millisecond):
		// expected — guard held
	}

	// EventTurnStart clears the guard. Use a fresh handler matching the
	// most recent session; the live session is what the wire-event source
	// would actually emit turn_started under.
	resumeHandler := app.sessionEventHandler(thread.ID, "tok-death-2", string(provider.Claude))
	resumeHandler(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		Timestamp: time.Now(),
	})

	// Third death after a real turn_started: fresh attempt allowed.
	fireDeath("tok-death-3")
	select {
	case <-startCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("post-turn_started death failed to trigger a fresh auto-reconnect")
	}
}

// TestAttemptAutoReconnectSkippedDuringShutdown pins the shutdown-race
// safety check: a death arriving concurrently with a.Stop() must not
// resurrect the provider after the app has begun teardown.
func TestAttemptAutoReconnectSkippedDuringShutdown(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown")
	thread.SessionRef = "claude-resume-shutdown"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	startCalled := make(chan struct{}, 1)
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error {
		startCalled <- struct{}{}
		return nil
	}

	app.shuttingDown.Store(true)

	handler := app.sessionEventHandler(thread.ID, "tok-shutdown", string(provider.Claude))
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "tok-shutdown"}
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "error",
		Timestamp: time.Now(),
	})
	handler(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	select {
	case <-startCalled:
		t.Fatal("auto-reconnect fired during shutdown; shuttingDown gate failed")
	case <-time.After(150 * time.Millisecond):
		// expected — shutdown gated it
	}
}

// TestReconnectSessionSingleFlightSecondCallNoOps pins the gate that
// stops a concurrent ReconnectSession from yanking the in-flight start.
// The second caller returns nil without invoking stop/start.
func TestReconnectSessionSingleFlightSecondCallNoOps(t *testing.T) {
	app := newTestAppWithStore(t)
	var calls []string
	var callsMu sync.Mutex

	startSignal := make(chan struct{})
	startBlock := make(chan struct{})

	app.stopSessionFn = func(threadID string) error {
		callsMu.Lock()
		calls = append(calls, "stop:"+threadID)
		callsMu.Unlock()
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		callsMu.Lock()
		calls = append(calls, "start:"+threadID)
		callsMu.Unlock()
		close(startSignal)
		<-startBlock
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- app.ReconnectSession("thread-sf")
	}()

	<-startSignal // first call is inside start now, holding the gate

	// Second concurrent call must return nil without invoking stop/start.
	if err := app.ReconnectSession("thread-sf"); err != nil {
		t.Fatalf("second ReconnectSession returned err=%v, want nil", err)
	}

	close(startBlock)
	if err := <-done; err != nil {
		t.Fatalf("first ReconnectSession returned err=%v", err)
	}

	callsMu.Lock()
	got := append([]string(nil), calls...)
	callsMu.Unlock()
	want := []string{"stop:thread-sf", "start:thread-sf"}
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v (second call must NOT add a second stop/start pair)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
