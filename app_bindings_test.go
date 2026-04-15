package main

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestGetSettingsReturnsCurrentServiceState(t *testing.T) {
	dir := t.TempDir()
	svc := settings.NewService(dir)
	if _, err := svc.Update(map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	app := &App{settings: svc}
	got, err := app.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings() error = %v", err)
	}
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark", got.Theme)
	}
}

func TestUpdateSettingsPersistsPatch(t *testing.T) {
	dir := t.TempDir()
	app := &App{settings: settings.NewService(dir)}

	got, err := app.UpdateSettings(map[string]any{"defaultProvider": "codex"})
	if err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}
	if got.DefaultProvider != "codex" {
		t.Fatalf("DefaultProvider = %q, want codex", got.DefaultProvider)
	}

	reloaded := settings.NewService(dir).Get()
	if reloaded.DefaultProvider != "codex" {
		t.Fatalf("reloaded DefaultProvider = %q, want codex", reloaded.DefaultProvider)
	}
}

func TestGetModelsForProvider(t *testing.T) {
	app := &App{}

	claudeModels, err := app.GetModelsForProvider("claude")
	if err != nil {
		t.Fatalf("GetModelsForProvider(claude) error = %v", err)
	}
	if len(claudeModels) == 0 {
		t.Fatal("expected claude models")
	}
	if claudeModels[0].Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", claudeModels[0].Provider)
	}

	unknown, err := app.GetModelsForProvider("unknown")
	if err != nil {
		t.Fatalf("GetModelsForProvider(unknown) error = %v", err)
	}
	if unknown != nil {
		t.Fatalf("unknown provider models = %v, want nil", unknown)
	}
}

func TestCreateThreadDefaultsInteractionMode(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := app.CreateThread(string(provider.Codex), "/tmp/workspace", "gpt-5.4")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.InteractionMode != "default" {
		t.Fatalf("returned InteractionMode = %q, want default", thread.InteractionMode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.InteractionMode != "default" {
		t.Fatalf("stored InteractionMode = %q, want default", stored.InteractionMode)
	}
}

func TestSwitchThreadAutoResumesStoredSession(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-auto")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	got, err := app.SwitchThread(thread.ID)
	if err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}
	if got.ID != thread.ID {
		t.Fatalf("thread ID = %q, want %q", got.ID, thread.ID)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume")
	}
}

func TestSwitchThreadSkipsAutoResumeWithoutSessionRef(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-resume")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected auto-resume for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestReconnectSessionStopsThenStarts(t *testing.T) {
	app := &App{}
	var calls []string
	app.stopSessionFn = func(threadID string) error {
		calls = append(calls, "stop:"+threadID)
		return nil
	}
	app.startSessionFn = func(threadID string) error {
		calls = append(calls, "start:"+threadID)
		return nil
	}

	if err := app.ReconnectSession("thread-1"); err != nil {
		t.Fatalf("ReconnectSession() error = %v", err)
	}

	want := []string{"stop:thread-1", "start:thread-1"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

func newTestAppWithStore(t *testing.T) *App {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "app.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	return &App{
		store:         st,
		sessions:      make(map[string]session),
		deliberations: make(map[string]*discussion.Deliberation),
	}
}

func testThread(id string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:              id,
		Title:           "Test Thread",
		Provider:        string(provider.Codex),
		WorkspacePath:   "/tmp/workspace",
		ProjectPath:     "/tmp/project",
		Model:           "gpt-5.4",
		InteractionMode: "default",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}
