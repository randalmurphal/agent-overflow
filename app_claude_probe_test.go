package main

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/settings"
)

// writeProbeMockBinary writes a script that mimics the Claude CLI `--max-turns 0`
// probe: print a single system/init NDJSON line with the provided account
// JSON and exit.
func writeProbeMockBinary(t *testing.T, accountJSON string) string {
	t.Helper()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "mock-claude")
	account := ""
	if accountJSON != "" {
		account = ",\"account\":" + accountJSON
	}
	script := "#!/bin/bash\n" +
		"printf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s\",\"model\":\"claude-opus-4-7\",\"cwd\":\"/tmp\",\"tools\":[],\"claude_code_version\":\"2.0.0\"" +
		account + "}\\n'\n" +
		"read -r _ || true\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func TestProbeClaudeAccountReturnsInfo(t *testing.T) {
	// Reset the package-level cache so prior tests don't leak results.
	resetClaudeProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	binary := writeProbeMockBinary(t, `{"subscriptionType":"pro","tokenSource":"oauth"}`)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	info, err := app.ProbeClaudeAccount()
	if err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", info.SubscriptionType)
	}
	if info.TokenSource != "oauth" {
		t.Errorf("TokenSource: got %q, want oauth", info.TokenSource)
	}
}

func TestProbeClaudeAccountCachesByBinary(t *testing.T) {
	resetClaudeProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	// The second invocation must not respawn the mock binary. If it
	// does, the second call would get the same value (since both script
	// runs are deterministic), so we instead rewrite the binary after
	// the first probe to deliberately emit a different subscription.
	binary := writeProbeMockBinary(t, `{"subscriptionType":"first"}`)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	first, err := app.ProbeClaudeAccount()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if first.SubscriptionType != "first" {
		t.Fatalf("first subscription: got %q, want first", first.SubscriptionType)
	}

	// Overwrite the binary in-place with a different account.
	if err := os.WriteFile(binary, []byte(
		"#!/bin/bash\n"+
			"printf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s\",\"model\":\"claude-opus-4-7\",\"cwd\":\"/tmp\",\"tools\":[],\"claude_code_version\":\"2.0.0\",\"account\":{\"subscriptionType\":\"second\"}}\\n'\n"+
			"read -r _ || true\n"+
			"exit 0\n",
	), 0755); err != nil {
		t.Fatalf("rewrite mock: %v", err)
	}

	// Second call hits the cache, not the binary.
	second, err := app.ProbeClaudeAccount()
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if second.SubscriptionType != "first" {
		t.Errorf("second subscription (expected cached): got %q, want first",
			second.SubscriptionType)
	}
}

func TestProbeClaudeAccountSurfacesSpawnErrors(t *testing.T) {
	resetClaudeProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": "/nonexistent/claude-absent-9999",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	_, err := app.ProbeClaudeAccount()
	if err == nil {
		t.Fatal("expected error on missing binary")
	}
}
