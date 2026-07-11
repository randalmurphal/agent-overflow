package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDetectProviderNotFound(t *testing.T) {
	status := DetectProvider("claude", "/nonexistent/path/to/claude-code-binary-xyz")

	if status.Installed {
		t.Fatal("expected Installed=false for nonexistent binary")
	}
	if status.Status != "not_found" {
		t.Fatalf("expected Status 'not_found', got %q", status.Status)
	}
	if status.Provider != "claude" {
		t.Fatalf("expected Provider 'claude', got %q", status.Provider)
	}
	if status.Message == "" {
		t.Fatal("expected a non-empty Message for not_found status")
	}
}

func TestDetectProviderWithRealBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo behaves differently on Windows")
	}

	// "echo" is available on PATH on unix systems.
	status := DetectProvider("claude", "echo")

	if !status.Installed {
		t.Fatal("expected Installed=true for 'echo'")
	}
	if status.Status != "ready" {
		t.Fatalf("expected Status 'ready', got %q", status.Status)
	}
	if status.BinaryPath == "" {
		t.Fatal("expected BinaryPath to be resolved")
	}
}

func TestDetectClaudeVersionWithMock(t *testing.T) {
	script := createMockBinary(t, "#!/bin/sh\necho 'claude-code 1.2.3'")

	version, err := detectClaudeVersion(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "claude-code 1.2.3" {
		t.Fatalf("expected version 'claude-code 1.2.3', got %q", version)
	}
}

func TestDetectCodexVersionWithMock(t *testing.T) {
	script := createMockBinary(t, "#!/bin/sh\necho 'codex 0.9.1'")

	version, err := detectCodexVersion(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "codex 0.9.1" {
		t.Fatalf("expected version 'codex 0.9.1', got %q", version)
	}
}

func TestDetectProviderVersionError(t *testing.T) {
	// Binary that exists but exits with a non-zero status.
	script := createMockBinary(t, "#!/bin/sh\nexit 1")

	status := DetectProvider("claude", script)

	if !status.Installed {
		t.Fatal("expected Installed=true when binary exists")
	}
	if status.Status != "error" {
		t.Fatalf("expected Status 'error' on version error, got %q", status.Status)
	}
	if status.Version != "" {
		t.Fatalf("expected empty Version on error, got %q", status.Version)
	}
	if status.Message == "" {
		t.Fatal("expected Message to contain version error detail")
	}
}

func TestDetectProviderCodexUnsupportedVersion(t *testing.T) {
	script := createMockBinary(t, "#!/bin/sh\necho 'codex 0.36.0'")

	status := DetectProvider("codex", script)

	if !status.Installed {
		t.Fatal("expected Installed=true when binary exists")
	}
	if status.Status != "version_too_old" {
		t.Fatalf("expected Status 'version_too_old' for unsupported Codex CLI, got %q", status.Status)
	}
	if status.Version != "codex 0.36.0" {
		t.Fatalf("expected raw Version to be preserved, got %q", status.Version)
	}
	wantMessage := "Codex CLI v0.36.0 is too old for Agent Overflow. Upgrade to v0.143.0 or newer and restart the app."
	if status.Message != wantMessage {
		t.Fatalf("Message = %q, want %q", status.Message, wantMessage)
	}
}

func TestProviderStatusJSON(t *testing.T) {
	original := ProviderStatus{
		Provider:   "claude",
		Installed:  true,
		Version:    "1.2.3",
		BinaryPath: "/usr/local/bin/claude",
		Status:     "ready",
		Message:    "",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded ProviderStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Provider != original.Provider {
		t.Fatalf("Provider mismatch: %q vs %q", decoded.Provider, original.Provider)
	}
	if decoded.Installed != original.Installed {
		t.Fatalf("Installed mismatch: %v vs %v", decoded.Installed, original.Installed)
	}
	if decoded.Version != original.Version {
		t.Fatalf("Version mismatch: %q vs %q", decoded.Version, original.Version)
	}
	if decoded.BinaryPath != original.BinaryPath {
		t.Fatalf("BinaryPath mismatch: %q vs %q", decoded.BinaryPath, original.BinaryPath)
	}
	if decoded.Status != original.Status {
		t.Fatalf("Status mismatch: %q vs %q", decoded.Status, original.Status)
	}

	// Verify omitempty: Message is empty so it should not appear in JSON.
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map failed: %v", err)
	}
	if _, exists := raw["message"]; exists {
		t.Fatal("expected 'message' to be omitted from JSON when empty")
	}
	// Version should be present (non-empty).
	if _, exists := raw["version"]; !exists {
		t.Fatal("expected 'version' to be present in JSON when non-empty")
	}
}

// createMockBinary writes a shell script to a temp directory and returns its path.
func createMockBinary(t *testing.T, contents string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}

	script := filepath.Join(t.TempDir(), "mock-binary")
	if err := os.WriteFile(script, []byte(contents), 0755); err != nil {
		t.Fatalf("failed to create mock binary: %v", err)
	}
	return script
}
