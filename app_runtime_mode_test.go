package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// createRuntimeTestThread seeds a thread the SetThreadRuntimeMode binding
// can operate on. Returns the ID so tests don't hard-code it.
func createRuntimeTestThread(t *testing.T, app *App, mode provider.RuntimeMode) string {
	t.Helper()
	id := "rt-" + strings.ReplaceAll(string(mode), "-", "_")
	err := app.store.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "runtime",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "claude-sonnet-4-6",
		RuntimeMode:   string(mode),
		CreatedAt:     1,
		UpdatedAt:     1,
	})
	if err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	return id
}

// captureEmissions wires a buffered-channel emit hook onto the app so
// tests can assert which events fired without deterministic sleeps.
func captureEmissions(app *App) *sync.Map {
	out := &sync.Map{}
	app.emitEventFn = func(name string, data any) {
		list, _ := out.LoadOrStore(name, []any{})
		out.Store(name, append(list.([]any), data))
	}
	return out
}

func emissionsFor(m *sync.Map, name string) []any {
	raw, ok := m.Load(name)
	if !ok {
		return nil
	}
	return raw.([]any)
}

// TestSetThreadRuntimeModeHappyPath: persists, emits, reports no reconnect
// when no session is active.
func TestSetThreadRuntimeModeHappyPath(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeFullAccess)

	got, err := app.SetThreadRuntimeMode(id, string(provider.RuntimeAutoAcceptEdits))
	if err != nil {
		t.Fatalf("SetThreadRuntimeMode: %v", err)
	}
	if got.RuntimeMode != string(provider.RuntimeAutoAcceptEdits) {
		t.Errorf("returned mode = %q, want %q", got.RuntimeMode, provider.RuntimeAutoAcceptEdits)
	}
	if got.NeedsReconnect {
		t.Error("no active session — NeedsReconnect should be false")
	}

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeAutoAcceptEdits) {
		t.Errorf("persisted mode = %q, want %q", stored.RuntimeMode, provider.RuntimeAutoAcceptEdits)
	}

	fired := emissionsFor(emissions, "thread:runtime_mode_changed")
	if len(fired) != 1 {
		t.Fatalf("expected 1 runtime_mode_changed emission, got %d", len(fired))
	}
}

// TestSetThreadRuntimeModeRejectsInvalid rejects unknown strings so the UI
// surfaces an error rather than silently coercing to the default.
func TestSetThreadRuntimeModeRejectsInvalid(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeFullAccess)

	_, err := app.SetThreadRuntimeMode(id, "yolo")
	if err == nil {
		t.Fatal("expected error for invalid mode, got nil")
	}
	// Store stays unchanged.
	stored, _ := app.store.GetThread(id)
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Errorf("persisted mode mutated to %q on invalid set", stored.RuntimeMode)
	}
	if fired := emissionsFor(emissions, "thread:runtime_mode_changed"); len(fired) != 0 {
		t.Errorf("no event should fire on invalid mode, got %d", len(fired))
	}
}

// TestSetThreadRuntimeModeIdempotent: same mode is a no-op — doesn't tear
// down a session, doesn't re-emit, returns NeedsReconnect=false.
func TestSetThreadRuntimeModeIdempotent(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeAutoAcceptEdits)

	// Simulate an active session so we can confirm the idempotent path
	// does NOT set NeedsReconnect.
	app.sessions[id] = session{token: "t"}

	got, err := app.SetThreadRuntimeMode(id, string(provider.RuntimeAutoAcceptEdits))
	if err != nil {
		t.Fatalf("SetThreadRuntimeMode: %v", err)
	}
	if got.NeedsReconnect {
		t.Error("idempotent set should NOT request reconnect")
	}
	if fired := emissionsFor(emissions, "thread:runtime_mode_changed"); len(fired) != 0 {
		t.Errorf("no-op should not emit, got %d emissions", len(fired))
	}
}

// TestSetThreadRuntimeModeRestartsWhenSessionActive asserts the legacy binding
// preserves its response shape while the new runtime mode is applied by a
// synchronous session restart.
func TestSetThreadRuntimeModeRestartsWhenSessionActive(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)

	app.sessions[id] = session{token: "t"}

	var startCalls int
	app.startSessionFn = func(threadID string) error {
		startCalls++
		if threadID != id {
			t.Fatalf("startSessionFn threadID = %q, want %q", threadID, id)
		}
		stored, err := app.store.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread during restart: %v", err)
		}
		if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
			t.Fatalf("restart saw runtime mode = %q, want full-access", stored.RuntimeMode)
		}
		return nil
	}

	got, err := app.SetThreadRuntimeMode(id, string(provider.RuntimeFullAccess))
	if err != nil {
		t.Fatalf("SetThreadRuntimeMode: %v", err)
	}
	if got.NeedsReconnect {
		t.Error("runtime-mode changes restart synchronously; NeedsReconnect should be false")
	}
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d, want 1", startCalls)
	}
	stored, _ := app.store.GetThread(id)
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Errorf("persisted mode = %q, want full-access", stored.RuntimeMode)
	}
	fired := emissionsFor(emissions, "thread:runtime_mode_changed")
	if len(fired) != 1 {
		t.Fatalf("expected 1 runtime_mode_changed emission, got %d", len(fired))
	}
	evt, ok := fired[0].(ThreadRuntimeModeChangedEvent)
	if !ok {
		t.Fatalf("runtime_mode_changed payload type = %T", fired[0])
	}
	if evt.NeedsReconnect {
		t.Fatal("runtime_mode_changed NeedsReconnect = true, want false after synchronous restart")
	}
}

func TestUpdateThreadRuntimeModeRollsBackWhenRestartFails(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)
	app.sessions[id] = session{token: "t"}
	restartErr := errors.New("synthetic restart failure")
	var startCalls atomic.Int32
	app.startSessionFn = func(string) error {
		startCalls.Add(1)
		return restartErr
	}

	_, err := app.UpdateThreadRuntimeMode(id, string(provider.RuntimeFullAccess))
	if err == nil {
		t.Fatal("UpdateThreadRuntimeMode error = nil, want restart failure")
	}
	if !strings.Contains(err.Error(), "restart session with updated runtime mode") {
		t.Fatalf("UpdateThreadRuntimeMode error = %v, want restart context", err)
	}
	if got := startCalls.Load(); got != 2 {
		t.Fatalf("startSessionFn calls = %d, want initial restart plus rollback recovery", got)
	}
	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("runtime mode after failed restart = %q, want rollback to approval-required", stored.RuntimeMode)
	}
	if fired := emissionsFor(emissions, "thread:runtime_mode_changed"); len(fired) != 0 {
		t.Fatalf("runtime_mode_changed emissions = %d, want 0 after rollback", len(fired))
	}
}

func TestUpdateThreadRuntimeModeRestoresPreviousSessionOnRestartFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)
	app.sessions[id] = session{token: "old-session"}
	restartErr := errors.New("synthetic restart failure")
	var startCalls atomic.Int32
	app.startSessionFn = func(threadID string) error {
		call := startCalls.Add(1)
		if call == 1 {
			return restartErr
		}
		stored, err := app.store.GetThread(threadID)
		if err != nil {
			t.Fatalf("GetThread during rollback recovery: %v", err)
		}
		if stored.RuntimeMode != string(provider.RuntimeApprovalRequired) {
			t.Fatalf("rollback recovery saw mode = %q, want approval-required", stored.RuntimeMode)
		}
		app.mu.Lock()
		app.sessions[threadID] = session{token: "recovered-session"}
		app.mu.Unlock()
		return nil
	}

	_, err := app.UpdateThreadRuntimeMode(id, string(provider.RuntimeFullAccess))
	if err == nil {
		t.Fatal("UpdateThreadRuntimeMode error = nil, want initial restart failure")
	}
	if got := startCalls.Load(); got != 2 {
		t.Fatalf("startSessionFn calls = %d, want initial restart plus rollback recovery", got)
	}
	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("runtime mode after failed restart = %q, want rollback to approval-required", stored.RuntimeMode)
	}
	app.mu.Lock()
	final := app.sessions[id]
	app.mu.Unlock()
	if final.token != "recovered-session" {
		t.Fatalf("session token after rollback recovery = %q, want recovered-session", final.token)
	}
}

func TestUpdateThreadRuntimeModeWaitsForThreadSendLock(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)
	attempted := make(chan struct{}, 1)
	runtimeModeLockAttemptedForTest = func(threadID string) {
		if threadID == id {
			attempted <- struct{}{}
		}
	}
	t.Cleanup(func() {
		runtimeModeLockAttemptedForTest = nil
	})

	unlock := sendThreadMuRegistry.lockFor(id)
	done := make(chan error, 1)
	go func() {
		_, err := app.UpdateThreadRuntimeMode(id, string(provider.RuntimeFullAccess))
		done <- err
	}()

	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("UpdateThreadRuntimeMode did not reach the send-lock boundary")
	}
	select {
	case err := <-done:
		t.Fatalf("UpdateThreadRuntimeMode returned while send lock was held: %v", err)
	default:
	}
	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread while locked: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("runtime mode changed while send lock was held: %q", stored.RuntimeMode)
	}

	unlock()
	if err := <-done; err != nil {
		t.Fatalf("UpdateThreadRuntimeMode after unlock: %v", err)
	}
	stored, err = app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread after update: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Fatalf("runtime mode after update = %q, want full-access", stored.RuntimeMode)
	}
}

func TestSendRuntimeModeChangeRestartsAfterInflightStart(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)

	firstStartEntered := make(chan struct{})
	releaseFirstStart := make(chan struct{})
	var startCalls atomic.Int32
	app.startSessionFn = func(threadID string) error {
		call := startCalls.Add(1)
		if threadID != id {
			t.Fatalf("startSessionFn threadID = %q, want %q", threadID, id)
		}
		switch call {
		case 1:
			close(firstStartEntered)
			<-releaseFirstStart
			app.mu.Lock()
			app.sessions[threadID] = session{provider: string(provider.Codex), token: "old-mode-session"}
			app.mu.Unlock()
			return nil
		case 2:
			stored, err := app.store.GetThread(id)
			if err != nil {
				t.Fatalf("GetThread during restart: %v", err)
			}
			if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
				t.Fatalf("restart saw runtime mode = %q, want full-access", stored.RuntimeMode)
			}
			app.mu.Lock()
			app.sessions[threadID] = session{provider: string(provider.Codex), token: "new-mode-session"}
			app.mu.Unlock()
			return nil
		default:
			t.Fatalf("unexpected startSessionFn call %d", call)
			return nil
		}
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- app.startSession(id)
	}()
	<-firstStartEntered

	sendErr := make(chan error, 1)
	go func() {
		_, err := app.SendMessageWithOptions(id, "hello", SendMessageOptions{
			RuntimeMode: string(provider.RuntimeFullAccess),
		})
		sendErr <- err
	}()

	select {
	case <-sendErr:
		t.Fatal("SendMessageWithOptions returned before the in-flight start was released")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseFirstStart)
	if err := <-startErr; err != nil {
		t.Fatalf("initial startSession: %v", err)
	}
	err := <-sendErr
	if err == nil || !strings.Contains(err.Error(), "session has no provider") {
		t.Fatalf("SendMessageWithOptions error = %v, want fake provider send failure", err)
	}
	if got := startCalls.Load(); got != 2 {
		t.Fatalf("startSessionFn calls = %d, want 2", got)
	}

	app.mu.Lock()
	final := app.sessions[id]
	app.mu.Unlock()
	if final.token != "new-mode-session" {
		t.Fatalf("final session token = %q, want new-mode-session", final.token)
	}
}

// TestGetThreadRuntimeModeRoundTrips ensures that the read side of the
// binding returns exactly what SetThreadRuntimeMode persisted — the
// normalization path doesn't clobber a valid mode on the way out.
func TestGetThreadRuntimeModeRoundTrips(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeFullAccess)

	for _, mode := range provider.AllRuntimeModes {
		if _, err := app.SetThreadRuntimeMode(id, string(mode)); err != nil {
			t.Fatalf("SetThreadRuntimeMode(%s): %v", mode, err)
		}
		got, err := app.GetThreadRuntimeMode(id)
		if err != nil {
			t.Fatalf("GetThreadRuntimeMode(%s): %v", mode, err)
		}
		if got != string(mode) {
			t.Errorf("round-trip %s: got %q, want %q", mode, got, mode)
		}
	}
}

func TestCreateThreadUsesFallbackRuntimeMode(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.RuntimeMode != string(provider.DefaultRuntimeMode) {
		t.Errorf("new thread runtime_mode = %q, want %q", thread.RuntimeMode, provider.DefaultRuntimeMode)
	}
}

func TestCreateThreadRejectsInvalidRuntimeMode(t *testing.T) {
	app := newTestAppWithStore(t)
	_, err := app.CreateThread(CreateThreadOptions{
		ProjectID:   defaultTestProjectID,
		Provider:    string(provider.Claude),
		Model:       "claude-sonnet-4-6",
		RuntimeMode: "bogus",
	})
	if err == nil {
		t.Fatal("CreateThread error = nil, want invalid runtime mode error")
	}
	if !strings.Contains(err.Error(), `invalid runtime mode "bogus"`) {
		t.Fatalf("CreateThread error = %v, want invalid runtime mode message", err)
	}
}

func TestUpdateThreadRuntimeModeSeedsNextNewThreadForSameModel(t *testing.T) {
	app := newTestAppWithStore(t)

	source, err := createTestThread(t, app, "claude", "/tmp/runtime-source", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("create source thread: %v", err)
	}
	if _, err := app.UpdateThreadRuntimeMode(source.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}

	next, err := createTestThread(t, app, "claude", "/tmp/runtime-next", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("create next thread: %v", err)
	}
	if next.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("new thread runtime_mode = %q, want %q", next.RuntimeMode, provider.RuntimeApprovalRequired)
	}
}
