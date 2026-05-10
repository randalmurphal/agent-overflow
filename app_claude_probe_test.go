package main

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/settings"
)

// writeProbeMockBinary writes a script that mimics the Claude CLI for
// the probe's control_request{subtype:"initialize"} handshake: read one
// line from stdin (the request), then emit a control_response carrying
// the supplied accountJSON in response.response.account. The fixed
// request_id "ao-probe-init" matches probeInitRequestID inside the
// claude package.
func writeProbeMockBinary(t *testing.T, accountJSON string) string {
	t.Helper()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "mock-claude")

	innerResponse := "{}"
	if accountJSON != "" {
		innerResponse = `{"account":` + accountJSON + `}`
	}
	respLine := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init","response":` + innerResponse + `}}`

	script := "#!/bin/bash\n" +
		`read -r _ || true` + "\n" +
		`printf '%s\n' '` + respLine + `'` + "\n" +
		`exit 0` + "\n"
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
	respLine := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init","response":{"account":{"subscriptionType":"second"}}}}`
	if err := os.WriteFile(binary, []byte(
		"#!/bin/bash\n"+
			`read -r _ || true`+"\n"+
			`printf '%s\n' '`+respLine+`'`+"\n"+
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

// RecheckClaudeAccount is the user-initiated path that defends against
// the "cache hides post-login state" bug. The first probe caches the
// pre-login zero-value; without invalidation, ProbeClaudeAccount would
// return that stale entry forever (well, for 5 minutes). Recheck must
// evict and re-probe.
func TestRecheckClaudeAccountBypassesCachedZeroValue(t *testing.T) {
	resetClaudeProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	// First probe: no account field — simulates pre-login state.
	binary := writeProbeMockBinary(t, "")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	first, err := app.ProbeClaudeAccount()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if first.SubscriptionType != "" {
		t.Fatalf("first probe should be empty (pre-login): got %q", first.SubscriptionType)
	}

	// Rewrite the mock to emit a populated account — simulates the
	// user running `claude login` between the two calls.
	respLine := `{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init","response":{"account":{"subscriptionType":"Claude Max"}}}}`
	if err := os.WriteFile(binary, []byte(
		"#!/bin/bash\n"+
			`read -r _ || true`+"\n"+
			`printf '%s\n' '`+respLine+`'`+"\n"+
			"exit 0\n",
	), 0755); err != nil {
		t.Fatalf("rewrite mock: %v", err)
	}

	// Without Recheck, this would hit the cache and return empty —
	// the regression we're guarding against. Recheck must invalidate
	// first and observe the new state.
	second, err := app.RecheckClaudeAccount()
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if second.SubscriptionType != "Claude Max" {
		t.Errorf("recheck SubscriptionType: got %q, want Claude Max (cache should have been invalidated)",
			second.SubscriptionType)
	}
}

// On cache miss, ProbeClaudeAccount emits a `provider:account` event
// so the popover plan label hydrates from the same code path the
// startup hook uses. Cache hits do NOT re-emit (the frontend store
// already has the value). This test pins both behaviors so a future
// edit can't accidentally make Recheck silent.
func TestProbeClaudeAccountEmitsAccountOnMissOnly(t *testing.T) {
	resetClaudeProbeCacheForTest()

	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	binary := writeProbeMockBinary(t, `{"subscriptionType":"Claude Max"}`)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	var events []ProviderAccountEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:account" {
			return
		}
		evt, ok := data.(ProviderAccountEvent)
		if !ok {
			return
		}
		events = append(events, evt)
	}

	// First call — cache miss, must emit.
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("cache miss should emit exactly 1 provider:account; got %d", len(events))
	}
	if events[0].Account.SubscriptionType != "Claude Max" {
		t.Errorf("emitted SubscriptionType: got %q, want Claude Max", events[0].Account.SubscriptionType)
	}

	// Second call — cache hit, must NOT emit (frontend already has it).
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("cache hit must not re-emit; total emissions now %d", len(events))
	}

	// Recheck — cache invalidated then probed; cache miss path again.
	if _, err := app.RecheckClaudeAccount(); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("recheck must emit on the post-invalidate cache miss; total emissions now %d", len(events))
	}
}
