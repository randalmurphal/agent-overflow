package main

import (
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
	if err := app.providerCredentials.WriteNativeCredentialForTest(
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
