package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

func TestSwitchThreadAutoResumesAfterSessionDisconnect(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-disconnect")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "session-current",
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	app.sessionEventHandler(thread.ID, "session-current")(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auto-resume after disconnect")
	}
}

func TestStaleSessionDisconnectDoesNotRemoveReplacement(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-stale")
	thread.SessionRef = "provider-session-1"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "session-current",
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	app.sessionEventHandler(thread.ID, "session-stale")(provider.ProviderEvent{
		Kind:      provider.EventSessionStatus,
		ThreadID:  thread.ID,
		Content:   "disconnected",
		Timestamp: time.Now(),
	})

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		t.Fatalf("unexpected auto-resume after stale disconnect for %s", threadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestServiceShutdownClosesSessionsWithoutDeadlock(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	thread.ProjectPath = thread.WorkspacePath
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "shutdown-token"),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "shutdown-token",
		claude:   sess,
	}

	done := make(chan error, 1)
	go func() {
		done <- app.ServiceShutdown()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServiceShutdown() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ServiceShutdown")
	}
}

func TestServiceShutdownReturnsSessionCloseErrors(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-shutdown-error")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	thread.ProjectPath = thread.WorkspacePath
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  "false",
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "shutdown-error-token"),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "shutdown-error-token",
		claude:   sess,
	}

	err = app.ServiceShutdown()
	if err == nil {
		t.Fatal("ServiceShutdown() error = nil, want provider close error")
	}
	if !strings.Contains(err.Error(), "session for thread "+thread.ID) || !strings.Contains(err.Error(), "close claude") {
		t.Fatalf("ServiceShutdown() error = %v, want thread-scoped close context with claude provider", err)
	}
}

func TestStartSessionReturnsExistingSessionCloseError(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	thread := testThread("thread-start-replace-close-error")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	thread.ProjectPath = thread.WorkspacePath
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	startMarker := filepath.Join(t.TempDir(), "replacement-started")
	replacementBinary := writeClaudeMarkerBinary(t, startMarker)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": replacementBinary}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	existing, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudeFailOnCloseBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		app.sessionEventHandler(thread.ID, "replace-close-error-token"),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "replace-close-error-token",
		claude:   existing,
	}

	err = app.startSessionNow(thread.ID)
	if err == nil {
		t.Fatal("startSessionNow() error = nil, want existing session close failure")
	}
	if !strings.Contains(err.Error(), "close claude session for thread "+thread.ID) {
		t.Fatalf("startSessionNow() error = %v, want existing-session close context", err)
	}

	if _, statErr := os.Stat(startMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement session start marker error = %v, want not exist", statErr)
	}

	app.mu.Lock()
	_, ok := app.sessions[thread.ID]
	app.mu.Unlock()
	if ok {
		t.Fatalf("sessions[%s] still present after failed replacement start", thread.ID)
	}
}

func writeClaudePassthroughBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-passthrough.sh")
	script := "#!/bin/sh\ncat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func writeClaudeFailOnCloseBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-fail-on-close.sh")
	script := "#!/bin/sh\ncat >/dev/null\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func writeClaudeMarkerBinary(t *testing.T, markerPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-marker.sh")
	script := "#!/bin/sh\nprintf started >" + shellQuote(markerPath) + "\ncat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
