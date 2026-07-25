package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginUsesIsolatedNativeHomeAndBrowserBridge(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture")
	path := filepath.Join(dir, "mock-claude-login")
	script := "#!/bin/bash\n" +
		`printf '%s\n%s\n%s\n%s\n' "$1 $2 $3" "$CLAUDE_CONFIG_DIR" "$BROWSER" "$AGENT_OVERFLOW_BROWSER_HELPER" > "$AO_CAPTURE"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_CAPTURE", capture)

	configDir := filepath.Join(dir, "profile")
	err := Login(context.Background(), LoginConfig{
		Binary:            path,
		ConfigDir:         configDir,
		BrowserExecutable: "/opt/agent-overflow",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"auth login --claudeai", configDir, "/opt/agent-overflow", "1"}
	if len(lines) != len(want) {
		t.Fatalf("capture = %q", data)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestLoginFailureDoesNotSurfaceProviderOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-claude-login-failure")
	script := "#!/bin/bash\n" +
		"echo 'https://auth.example.test/?state=secret' >&2\n" +
		"echo 'access-token-secret' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Login(context.Background(), LoginConfig{Binary: path})
	if err == nil {
		t.Fatal("Login() error = nil")
	}
	got := err.Error()
	if strings.Contains(got, "state=secret") || strings.Contains(got, "access-token-secret") {
		t.Fatalf("Login() leaked provider output: %q", got)
	}
}
