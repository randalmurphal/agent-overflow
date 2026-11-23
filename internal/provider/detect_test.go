package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
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

func TestRunVersionCommandWithMock(t *testing.T) {
	script := createMockBinary(t, "#!/bin/sh\necho 'claude-code 1.2.3'")

	version, err := runVersionCommand(script, "--version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "claude-code 1.2.3" {
		t.Fatalf("expected version 'claude-code 1.2.3', got %q", version)
	}
}

func TestDetectProviderCodexReadyWithMock(t *testing.T) {
	script := createMockBinary(t, "#!/bin/sh\necho 'codex-cli 0.154.0'")

	status := DetectProvider("codex", script)
	if !status.Installed || status.Status != "ready" || status.Message != "" {
		t.Fatalf("expected ready codex status, got %+v", status)
	}
	if status.Version != "codex-cli 0.154.0" {
		t.Fatalf("expected version 'codex-cli 0.154.0', got %q", status.Version)
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
	if want := "version check of " + script + " failed: exit status 1"; status.Message != want {
		t.Fatalf("Message = %q, want %q", status.Message, want)
	}
}

// TestDetectProviderCodexBelowMinimum pins the settings row's and the
// banner's text at the minimum's boundary: the status, the installed
// version, the minimum and the path of the binary that was run. A path with
// a space reads whole because the message puts it in parentheses.
func TestDetectProviderCodexBelowMinimum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell scripts require unix")
	}
	dir := filepath.Join(t.TempDir(), "Codex Tools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		output     string
		wantStatus string
		wantText   string
	}{
		{
			name:       "last release before the minimum",
			output:     "codex-cli 0.153.4",
			wantStatus: "version_too_old",
			wantText:   "Codex v0.153.4 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
		},
		{
			name:       "pre-release of the minimum",
			output:     "codex-cli 0.154.0-alpha.1",
			wantStatus: "version_too_old",
			wantText:   "Codex v0.154.0-alpha.1 (%s) is installed; Agent Overflow needs v0.154.0 or newer. Update Codex.",
		},
		{
			name:       "unreadable version",
			output:     "codex version unknown",
			wantStatus: "error",
			wantText:   `Agent Overflow could not read the version of Codex (%s) from "codex version unknown". Update Codex to v0.154.0 or newer.`,
		},
		{
			name:       "no output",
			output:     "",
			wantStatus: "error",
			wantText:   "Agent Overflow could not read the version of Codex (%s). Update Codex to v0.154.0 or newer.",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := mockexec.WriteIn(t, dir, fmt.Sprintf("codex %d", i), "#!/bin/sh\nprintf '%s' '"+tc.output+"'\n")
			status := DetectProvider("codex", script)
			if !status.Installed || status.Status != tc.wantStatus {
				t.Fatalf("status = %+v, want status %q", status, tc.wantStatus)
			}
			if want := fmt.Sprintf(tc.wantText, script); status.Message != want {
				t.Fatalf("Message = %q, want %q", status.Message, want)
			}
		})
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

	return mockexec.WriteIn(t, t.TempDir(), "mock-binary", contents)
}
