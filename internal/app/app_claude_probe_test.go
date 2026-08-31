package app

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

// claudeProbeShapeAfterASwitch is what a HEALTHY Claude Max login reports once
// `~/.claude.json`'s `oauthAccount` record is gone — which AO itself deletes on
// every account switch so the provider re-derives its own identity.
//
// Spike-verified against 2.1.234 (2026-08-18): no email, no displayName, no
// tokenSource, and the CLI does not write the record back on a `--max-turns 0`
// probe start at all, so waiting does not help. It is byte-for-byte the shape a
// DESTROYED login echoes, which is exactly why the credential — not the
// reported identity — has to settle the question.
const claudeProbeShapeAfterASwitch = `{"subscriptionType":"Claude Max","apiProvider":"firstParty"}`

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

func claudeUnauthBannerCount(t *testing.T, app *App) *int {
	t.Helper()
	count := 0
	app.testEmitHook = func(name string, data any) {
		if name != "provider:status" {
			return
		}
		evt, ok := data.(providerstatus.Event)
		if ok && evt.Provider == string(provider.Claude) && evt.Status == "unauthenticated" {
			count++
		}
	}
	return &count
}

func newClaudeProbeIdentityApp(t *testing.T, accountJSON string) *App {
	t.Helper()
	resetClaudeProbeCacheForTest()
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	binary := writeProbeMockBinary(t, accountJSON)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	return app
}

// The banner tells the user to run `claude login`. Raising it while a real
// credential is installed is not a cosmetic bug: the instruction, followed,
// replaces a working login. Every account switch used to produce exactly this,
// because the switch deletes the identity record the probe reads.
func TestClaudeProbeDoesNotClaimLoggedOutWhileACredentialExists(t *testing.T) {
	app := newClaudeProbeIdentityApp(t, claudeProbeShapeAfterASwitch)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"live","refreshToken":"live-rt"}}`)},
	)
	banners := claudeUnauthBannerCount(t, app)

	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}
	if *banners != 0 {
		t.Fatalf("raised %d unauthenticated banners for a healthy login", *banners)
	}

	// The cache-hit path emits independently of the miss path, so a second
	// call has to stay just as quiet — otherwise the banner returns for the
	// whole cache lifetime after a single switch.
	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount (cached): %v", err)
	}
	if *banners != 0 {
		t.Fatalf("raised %d unauthenticated banners from the probe cache", *banners)
	}
}

// The husk is the provider's own sign-out marker: tokens blanked in place. A
// credential in that state is not a login, so the banner is the correct and
// only useful answer.
func TestClaudeProbeStillClaimsLoggedOutOnTheSignOutHusk(t *testing.T) {
	app := newClaudeProbeIdentityApp(t, claudeProbeShapeAfterASwitch)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"live","refreshToken":"live-rt"}}`)},
	)
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"subscriptionType":"Claude Max"}}`),
	); err != nil {
		t.Fatal(err)
	}
	banners := claudeUnauthBannerCount(t, app)

	// The probe itself fails here, and should: adoption refuses to pair the
	// selected account with sign-out bytes (incident 2026-08-03). The banner
	// is raised before that refusal, which is the point — the user gets the
	// one instruction that repairs this state rather than only an error.
	_, err := app.ProbeClaudeAccount()
	if err == nil {
		t.Fatal("ProbeClaudeAccount() adopted the provider's sign-out husk")
	}
	if *banners == 0 {
		t.Fatalf("no unauthenticated banner for the sign-out husk (probe err: %v)", err)
	}
}

// No credential at all is the case the banner was written for, and it must
// survive the gate above — a host where nobody has ever run `claude login`
// still needs to be told to.
func TestClaudeProbeStillClaimsLoggedOutWithNoCredentialAtAll(t *testing.T) {
	app := newClaudeProbeIdentityApp(t, claudeProbeShapeAfterASwitch)
	banners := claudeUnauthBannerCount(t, app)

	if _, err := app.ProbeClaudeAccount(); err != nil {
		t.Fatalf("ProbeClaudeAccount: %v", err)
	}
	if *banners == 0 {
		t.Fatal("no unauthenticated banner on a host with no Claude login")
	}
}
