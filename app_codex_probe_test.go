package main

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/settings"
)

// writeCodexProbeMockBinary writes a script that mimics
// `codex app-server` for the probe handshake: drain three NDJSON
// requests from stdin, then emit an init reply (id=1) followed by a
// `account/rateLimits/read` response (id=2) carrying the supplied
// rateLimitsJSON.
func writeCodexProbeMockBinary(t *testing.T, rateLimitsJSON string) string {
	t.Helper()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "mock-codex")

	idTwoFrame := `{"jsonrpc":"2.0","id":2,"result":` + rateLimitsJSON + `}`
	if rateLimitsJSON == "" {
		idTwoFrame = `{"jsonrpc":"2.0","id":2,"result":{}}`
	}

	script := "#!/bin/bash\n" +
		`read -r _ || true` + "\n" +
		`read -r _ || true` + "\n" +
		`read -r _ || true` + "\n" +
		`printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"v2"}}\n'` + "\n" +
		`printf '%s\n' '` + idTwoFrame + `'` + "\n" +
		`exit 0` + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func TestProbeCodexAccountReturnsInfo(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	binary := writeCodexProbeMockBinary(t, `{"rateLimits":{"limitId":"codex","planType":"pro"}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	info, err := app.ProbeCodexAccount()
	if err != nil {
		t.Fatalf("ProbeCodexAccount: %v", err)
	}
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", info.SubscriptionType)
	}
	if info.APIProvider != "openai" {
		t.Errorf("APIProvider: got %q, want openai", info.APIProvider)
	}
}

func TestProbeCodexAccountCachesByBinary(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	binary := writeCodexProbeMockBinary(t, `{"rateLimits":{"planType":"first"}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	first, err := app.ProbeCodexAccount()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if first.SubscriptionType != "first" {
		t.Fatalf("first plan: got %q, want first", first.SubscriptionType)
	}

	// Overwrite the binary in-place; cached call must NOT re-execute it.
	idTwoFrame := `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"planType":"second"}}}`
	if err := os.WriteFile(binary, []byte(
		"#!/bin/bash\n"+
			"read -r _ || true\nread -r _ || true\nread -r _ || true\n"+
			`printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"v2"}}\n'`+"\n"+
			`printf '%s\n' '`+idTwoFrame+`'`+"\n"+
			"exit 0\n",
	), 0755); err != nil {
		t.Fatalf("rewrite mock: %v", err)
	}

	second, err := app.ProbeCodexAccount()
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if second.SubscriptionType != "first" {
		t.Errorf("second plan (expected cached): got %q, want first",
			second.SubscriptionType)
	}
}

func TestProbeCodexAccountSurfacesSpawnErrors(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": "/nonexistent/codex-absent-9999",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	_, err := app.ProbeCodexAccount()
	if err == nil {
		t.Fatal("expected error on missing binary")
	}
}

// RecheckCodexAccount mirrors RecheckClaudeAccount. No frontend caller
// today (Codex login flow lives in the user's terminal), but the
// surface must still bypass the cache so it's ready when a UI lands.
func TestRecheckCodexAccountBypassesCache(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	binary := writeCodexProbeMockBinary(t, `{"rateLimits":{"planType":"first"}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	first, err := app.ProbeCodexAccount()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if first.SubscriptionType != "first" {
		t.Fatalf("first plan: got %q, want first", first.SubscriptionType)
	}

	// Overwrite the mock — simulates plan upgrade between calls.
	idTwoFrame := `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"planType":"team"}}}`
	if err := os.WriteFile(binary, []byte(
		"#!/bin/bash\n"+
			"read -r _ || true\nread -r _ || true\nread -r _ || true\n"+
			`printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"v2"}}\n'`+"\n"+
			`printf '%s\n' '`+idTwoFrame+`'`+"\n"+
			"exit 0\n",
	), 0755); err != nil {
		t.Fatalf("rewrite mock: %v", err)
	}

	second, err := app.RecheckCodexAccount()
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if second.SubscriptionType != "team" {
		t.Errorf("recheck plan: got %q, want team (cache should have been invalidated)",
			second.SubscriptionType)
	}
}
