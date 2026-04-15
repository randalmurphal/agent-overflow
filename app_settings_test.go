package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

func TestGetProviderStatusesUsesConfiguredBinaryPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}

	configDir := t.TempDir()
	app := &App{
		settings: settings.NewService(configDir),
	}

	claudeBinary := createMockBinary(t, "claude-mock 1.2.3")
	codexBinary := createMockBinary(t, "codex-mock 4.5.6")

	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": claudeBinary,
		"codexBinaryPath":  codexBinary,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	statuses, err := app.GetProviderStatuses()
	if err != nil {
		t.Fatalf("GetProviderStatuses() error = %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}

	if statuses[0].Provider != "claude" {
		t.Fatalf("statuses[0].Provider = %q, want claude", statuses[0].Provider)
	}
	if statuses[0].BinaryPath != claudeBinary {
		t.Fatalf("statuses[0].BinaryPath = %q, want %q", statuses[0].BinaryPath, claudeBinary)
	}
	if statuses[0].Version != "claude-mock 1.2.3" {
		t.Fatalf("statuses[0].Version = %q, want %q", statuses[0].Version, "claude-mock 1.2.3")
	}

	if statuses[1].Provider != "codex" {
		t.Fatalf("statuses[1].Provider = %q, want codex", statuses[1].Provider)
	}
	if statuses[1].BinaryPath != codexBinary {
		t.Fatalf("statuses[1].BinaryPath = %q, want %q", statuses[1].BinaryPath, codexBinary)
	}
	if statuses[1].Version != "codex-mock 4.5.6" {
		t.Fatalf("statuses[1].Version = %q, want %q", statuses[1].Version, "codex-mock 4.5.6")
	}
}

func TestGetProviderStatusesFallsBackToDefaultsWithoutSettingsService(t *testing.T) {
	app := &App{}

	statuses, err := app.GetProviderStatuses()
	if err != nil {
		t.Fatalf("GetProviderStatuses() error = %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}

	if statuses[0].Provider != "claude" {
		t.Fatalf("statuses[0].Provider = %q, want claude", statuses[0].Provider)
	}
	if statuses[0].BinaryPath == "" {
		t.Fatal("statuses[0].BinaryPath is empty")
	}

	if statuses[1].Provider != "codex" {
		t.Fatalf("statuses[1].Provider = %q, want codex", statuses[1].Provider)
	}
	if statuses[1].BinaryPath == "" {
		t.Fatal("statuses[1].BinaryPath is empty")
	}
}

func TestGetProviderStatusesDefaultsBlankConfiguredBinaryPaths(t *testing.T) {
	app := &App{
		settings: settings.NewService(t.TempDir()),
	}

	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": "   ",
		"codexBinaryPath":  "",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	statuses, err := app.GetProviderStatuses()
	if err != nil {
		t.Fatalf("GetProviderStatuses() error = %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}

	wantClaude := provider.DetectProvider(string(provider.Claude), settings.DefaultSettings.ClaudeBinaryPath)
	wantCodex := provider.DetectProvider(string(provider.Codex), settings.DefaultSettings.CodexBinaryPath)

	if statuses[0] != wantClaude {
		t.Fatalf("claude status = %+v, want %+v", statuses[0], wantClaude)
	}
	if statuses[1] != wantCodex {
		t.Fatalf("codex status = %+v, want %+v", statuses[1], wantCodex)
	}
}

func TestStartSessionUsesConfiguredClaudeBinaryPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	markerPath := filepath.Join(t.TempDir(), "claude-started")
	claudeBinary := createKeepAliveBinary(t, markerPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": claudeBinary}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	thread := testThread("thread-start-session-binary")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	thread.ProjectPath = thread.WorkspacePath
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	defer func() {
		if err := app.StopSession(thread.ID); err != nil {
			t.Fatalf("StopSession() error = %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("configured Claude binary was not executed; marker %s was never created", markerPath)
}

func createMockBinary(t *testing.T, version string) string {
	t.Helper()

	script := filepath.Join(t.TempDir(), "mock-binary")
	contents := "#!/bin/sh\necho '" + version + "'\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("failed to create mock binary: %v", err)
	}
	return script
}

func createKeepAliveBinary(t *testing.T, markerPath string) string {
	t.Helper()

	script := filepath.Join(t.TempDir(), "mock-provider")
	contents := fmt.Sprintf("#!/bin/sh\ntouch '%s'\ncat\n", markerPath)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("failed to create keepalive binary: %v", err)
	}
	return script
}
