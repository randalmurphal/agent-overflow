package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// claudeProbeReportingNoIdentity is what the probe answers on the first
// canonical refresh after ANY account switch.
//
// The CLI reports its identity out of `~/.claude.json`'s `oauthAccount`
// record, and AO deletes that record on every switch
// (Credentials.retireProviderIdentity) so the provider re-derives it against
// the credential that just landed rather than describing the outgoing
// account's email over the incoming account's tokens. The CLI rewrites it from
// a profile fetch it runs asynchronously during startup — the same detached
// startup work as the token refresh — so the `initialize` answer this probe
// reads is emitted before the identity comes back.
//
// An empty object and an absent `account` key decode identically, so one
// fixture covers both shapes the CLI produces.
const claudeProbeReportingNoIdentity = `{}`

// Reported identity is NOT evidence about the credential, and treating it as
// evidence destroyed logins.
//
// The sequence, observed in production 2026-08-18: switch at 19:37:26 (which
// cleared oauthAccount), usage refresh at 19:37:30, the canonical bearer 401s,
// the probe drives Claude's own forced refresh, and the rotation lands — the
// replacement pair is on disk, timestamped one second before the error the
// user was shown. The probe still reported no identity, which the old check
// read as "Claude credentials expired; log in again" and returned with NIL
// credential bytes. Nil bytes trip refreshProviderAccountUsage's
// `probeErr != nil && len(refreshed) == 0` early return, so the commit never
// ran and the account's slot stayed on the refresh token the server had just
// retired — one restart away from a login nothing can recover.
func TestSelectedClaudeUsageRefreshTrustsTheBytesNotTheReportedIdentity(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original","refreshToken":"original-rt"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"rotated","refreshToken":"rotated-rt"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinaryReporting(
		t,
		canonical,
		rotated,
		claudeProbeReportingNoIdentity,
	)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	// Only the rotated bearer is accepted, so the first probe 401s into the
	// refresh and the retry proves the new pair works.
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "rotated")

	var usage provider.UsageEvent
	app.testEmitHook = func(name string, data any) {
		if name == "provider:usage" {
			usage, _ = data.(provider.UsageEvent)
		}
	}

	if err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	); err != nil {
		t.Fatalf("refreshProviderAccountUsage() error = %v, want success for a healthy rotation", err)
	}
	if usage.RateLimits == nil || len(usage.RateLimits.Limits) == 0 {
		t.Fatal("no usage snapshot emitted for a refresh that succeeded")
	}
	saved, readErr := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != rotated {
		t.Fatalf("saved slot = %s, want the rotated pair %s", saved, rotated)
	}
}

// The verdict may still be "log in again" — but it has to come from the server
// refusing the bytes we hold, and it must NOT cost the rotation.
//
// Claude's refresh tokens are single-use and its endpoint retires the previous
// one the moment it processes the request, so a rotation that reaches disk but
// not the slot leaves the slot holding a consumed token. That is not a stale
// read: it is a login that dies at its next use. The rotated pair therefore
// goes back to the caller on the error path too, and the commit persists it
// before the error surfaces.
func TestSelectedClaudeUsageRefreshKeepsTheRotationWhenTheRetryStill401s(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original","refreshToken":"original-rt"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"rotated","refreshToken":"rotated-rt"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinaryReporting(
		t,
		canonical,
		rotated,
		claudeProbeReportingNoIdentity,
	)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	// Nothing this account holds is accepted, so the retry 401s as well.
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "no-bearer-works")

	var published int
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:usage" {
			published++
		}
	}

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if !errors.Is(err, errClaudeCredentialsExpired) {
		t.Fatalf("refresh error = %v, want errClaudeCredentialsExpired", err)
	}
	saved, readErr := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != rotated {
		t.Fatalf(
			"saved slot = %s, want the rotated pair %s — the previous token is spent",
			saved,
			rotated,
		)
	}
	if published != 0 {
		t.Fatalf("published %d snapshots, want none from a failed probe", published)
	}
}

// The identity guard survives the removal above: a canonical home that a
// DIFFERENT account logged into mid-probe still refuses, and that one rotation
// deliberately does NOT reach this account's slot — the bytes belong to the
// login that now owns the canonical home, and pairing them with this account's
// identity is the split state the guard exists to prevent.
func TestSelectedClaudeUsageRefreshStillRefusesAnotherAccountsLogin(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original","refreshToken":"original-rt"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"rotated","refreshToken":"rotated-rt"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := providerCredentialsForTest(t, app).ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinary(t, canonical, rotated, "someone-else@example.com")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "rotated")

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil || !strings.Contains(err.Error(), "someone-else@example.com") {
		t.Fatalf("refresh error = %v, want the intruding account named", err)
	}
	saved, readErr := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != string(original) {
		t.Fatalf("saved slot = %s, want another account's credential kept out of it", saved)
	}
}
