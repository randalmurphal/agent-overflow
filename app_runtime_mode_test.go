package main

import (
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
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

// TestSetThreadRuntimeModeFlagsReconnectWhenSessionActive asserts the
// frontend-facing reconnect flag is set when we change modes on a live
// session. We don't drive an actual reconnect here — that path runs in a
// goroutine and would require stubbing the provider subprocess — but the
// returned payload reflects the flag, which is what the UI keys off.
func TestSetThreadRuntimeModeFlagsReconnectWhenSessionActive(t *testing.T) {
	app := newTestAppWithStore(t)
	captureEmissions(app)
	id := createRuntimeTestThread(t, app, provider.RuntimeApprovalRequired)

	app.sessions[id] = session{token: "t"}

	// Stub the reconnect hooks so the async goroutine doesn't hit real
	// provider paths (it still runs, but does nothing).
	app.stopSessionFn = func(string) error { return nil }
	app.startSessionFn = func(string) error { return nil }

	got, err := app.SetThreadRuntimeMode(id, string(provider.RuntimeFullAccess))
	if err != nil {
		t.Fatalf("SetThreadRuntimeMode: %v", err)
	}
	if !got.NeedsReconnect {
		t.Error("active session — NeedsReconnect should be true")
	}
	stored, _ := app.store.GetThread(id)
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Errorf("persisted mode = %q, want full-access", stored.RuntimeMode)
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

// TestCreateThreadUsesSettingsDefault: a new thread with no explicit
// runtime mode inherits settings.DefaultRuntimeMode.
func TestCreateThreadUsesSettingsDefault(t *testing.T) {
	app := newTestAppWithStore(t)

	// App created by the test helper doesn't have a settings service
	// wired. CreateThread's fallback reaches provider.DefaultRuntimeMode
	// directly; verify it lands on 'full-access'.
	thread, err := createTestThread(t, app, "claude", "/tmp", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if thread.RuntimeMode != string(provider.DefaultRuntimeMode) {
		t.Errorf("new thread runtime_mode = %q, want %q", thread.RuntimeMode, provider.DefaultRuntimeMode)
	}
}

func TestUpdateThreadRuntimeModePersistsDefaultForNewThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

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
