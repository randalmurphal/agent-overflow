package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// createRuntimeTestThread seeds a thread the runtime-mode bindings can
// operate on. Returns the ID so tests don't hard-code it.
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

// TestUpdateThreadRuntimeModeKeepsModeOnRestartFailure pins the reconciler
// semantics: a session with no live-update surface gets a deferred restart,
// the binding returns success immediately (the persisted mode is
// authoritative), the mode-changed event fires, and a restart failure does
// NOT roll the row back — the next lazy start converges on it.
func TestUpdateThreadRuntimeModeKeepsModeOnRestartFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)
	app.sessions[id] = session{token: "t"}
	restartErr := errors.New("synthetic restart failure")
	started := make(chan struct{}, 1)
	app.startSessionFn = func(threadID string) error {
		stored, err := app.store.GetThread(threadID)
		if err != nil {
			t.Errorf("GetThread during deferred restart: %v", err)
		} else if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
			t.Errorf("deferred restart saw mode = %q, want full-access", stored.RuntimeMode)
		}
		started <- struct{}{}
		return restartErr
	}

	if _, err := app.UpdateThreadRuntimeMode(id, string(provider.RuntimeFullAccess)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode error = %v, want nil (restart is deferred)", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred config reconnect never attempted a restart")
	}

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Fatalf("runtime mode after failed restart = %q, want persisted full-access", stored.RuntimeMode)
	}
	if fired := emissionsFor(emissions, "thread:runtime_mode_changed"); len(fired) != 1 {
		t.Fatalf("runtime_mode_changed emissions = %d, want 1", len(fired))
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

	unlock := app.threadLocks().Lock(id)
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
		t.Fatalf("UpdateThreadRuntimeMode returned while thread action lock was held: %v", err)
	default:
	}
	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread while locked: %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("runtime mode changed while thread action lock was held: %q", stored.RuntimeMode)
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

// TestSendRuntimeModeChangeReconnectsAfterInflightStart pins the reconcile
// interaction with an in-flight session start: the mode change waits for
// the start to settle (so it diffs against the session that actually
// exists), the send proceeds without being blocked by the restart, and the
// deferred reconnect then restarts the stale-config session against the
// new mode.
func TestSendRuntimeModeChangeReconnectsAfterInflightStart(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)

	firstStartEntered := make(chan struct{})
	releaseFirstStart := make(chan struct{})
	secondStart := make(chan struct{})
	var startCalls atomic.Int32
	app.startSessionFn = func(threadID string) error {
		call := startCalls.Add(1)
		if threadID != id {
			t.Errorf("startSessionFn threadID = %q, want %q", threadID, id)
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
				t.Errorf("GetThread during restart: %v", err)
			} else if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
				t.Errorf("restart saw runtime mode = %q, want full-access", stored.RuntimeMode)
			}
			app.mu.Lock()
			app.sessions[threadID] = session{provider: string(provider.Codex), token: "new-mode-session"}
			app.mu.Unlock()
			close(secondStart)
			return nil
		default:
			t.Errorf("unexpected startSessionFn call %d", call)
			return nil
		}
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- app.startSession(context.Background(), id)
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

	select {
	case <-secondStart:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred config reconnect never restarted the stale-config session")
	}

	app.mu.Lock()
	final := app.sessions[id]
	app.mu.Unlock()
	if final.token != "new-mode-session" {
		t.Fatalf("final session token = %q, want new-mode-session", final.token)
	}
}

// TestGetThreadRuntimeModeRoundTrips ensures the read side returns exactly
// what UpdateThreadRuntimeMode persisted — the normalization path doesn't
// clobber a valid mode on the way out.
func TestGetThreadRuntimeModeRoundTrips(t *testing.T) {
	app := newTestAppWithStore(t)
	id := createRuntimeTestThread(t, app, provider.RuntimeFullAccess)

	for _, mode := range provider.AllRuntimeModes {
		if _, err := app.UpdateThreadRuntimeMode(id, string(mode)); err != nil {
			t.Fatalf("UpdateThreadRuntimeMode(%s): %v", mode, err)
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
