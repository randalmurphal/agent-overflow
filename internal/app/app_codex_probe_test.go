package app

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
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
	accountStore, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nativeHome := t.TempDir()
	credentials := newTestProviderCredentials(t, nativeHome)
	activeCredential, err := credentials.ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(activeCredential), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeCredential, []byte(`{"tokens":{"access_token":"test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accountStore, credentials)

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

// ProbeCodexAccount must emit a `provider:usage` event with the
// rate-limit snapshot pulled from the same `account/rateLimits/read`
// response that yields AccountInfo. Without this the 5h/7d rings stay
// empty until the user runs a turn — and stale if the user exhausted
// the limit in another Codex surface (TUI, CLI) before the app boot.
func TestProbeCodexAccountEmitsRateLimitsOnCacheMiss(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	accountStore, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	activeCredential, err := credentials.ActiveCredentialPath(string(provider.Codex))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(activeCredential), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeCredential, []byte(`{"tokens":{"access_token":"test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accountStore, credentials)

	var (
		mu       sync.Mutex
		captured []provider.UsageEvent
	)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		evt, ok := data.(provider.UsageEvent)
		if !ok {
			return
		}
		mu.Lock()
		captured = append(captured, evt)
		mu.Unlock()
	}

	binary := writeCodexProbeMockBinary(t,
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":91,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":7,"windowDurationMins":10080,"resetsAt":1776372636}}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if _, err := app.ProbeCodexAccount(); err != nil {
		t.Fatalf("ProbeCodexAccount: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("provider:usage emissions: got %d, want 1", len(captured))
	}
	evt := captured[0]
	if evt.Action != "rate_limits" {
		t.Errorf("Action: got %q, want rate_limits", evt.Action)
	}
	if evt.RateLimits == nil {
		t.Fatal("RateLimits: got nil, want non-nil snapshot")
	}
	if evt.RateLimits.Provider != string(provider.Codex) {
		t.Errorf("RateLimits.Provider: got %q, want %q", evt.RateLimits.Provider, provider.Codex)
	}
	if evt.RateLimits.AccountID == "" {
		t.Fatal("RateLimits.AccountID is empty after unmanaged account adoption")
	}
	if account, ok := accountStore.Active(string(provider.Codex), time.Now()); !ok ||
		account.ID != evt.RateLimits.AccountID ||
		account.RateLimits == nil {
		t.Fatalf("adopted account did not persist attributed limits: %+v, ok=%v", account, ok)
	}
	if len(evt.RateLimits.Limits) != 2 {
		t.Fatalf("RateLimits.Limits len: got %d, want 2", len(evt.RateLimits.Limits))
	}
	if evt.RateLimits.Limits[0].WindowMins != 300 || evt.RateLimits.Limits[0].UsedPercent != 91 {
		t.Errorf("Limits[0]: got %+v, want {WindowMins:300, UsedPercent:91}", evt.RateLimits.Limits[0])
	}
	if evt.RateLimits.Limits[1].WindowMins != 10080 || evt.RateLimits.Limits[1].UsedPercent != 7 {
		t.Errorf("Limits[1]: got %+v, want {WindowMins:10080, UsedPercent:7}", evt.RateLimits.Limits[1])
	}
}

// Cache hits must NOT re-emit. The frontend store already has the
// snapshot from the original cache-miss emission, and a stale re-emit
// (the cached probe is up to ProbeCache.TTL old) could overwrite a
// fresher value pushed by an active session via the
// account/rateLimits/updated notification path.
func TestProbeCodexAccountSkipsRateLimitsEmitOnCacheHit(t *testing.T) {
	resetCodexProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	var (
		mu       sync.Mutex
		captured []provider.UsageEvent
	)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		if evt, ok := data.(provider.UsageEvent); ok {
			mu.Lock()
			captured = append(captured, evt)
			mu.Unlock()
		}
	}

	binary := writeCodexProbeMockBinary(t,
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":13,"windowDurationMins":10080,"resetsAt":1776372636}}}`)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if _, err := app.ProbeCodexAccount(); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if _, err := app.ProbeCodexAccount(); err != nil {
		t.Fatalf("second (cached) probe: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("provider:usage emissions: got %d, want 1 (cache-miss emits, cache-hit does not)", len(captured))
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
