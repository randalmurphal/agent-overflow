package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

// claudeCredentialExpiringAt builds a credential whose access token expires at
// the given instant. Claude stores expiry as epoch milliseconds.
func claudeCredentialExpiringAt(token string, expiry time.Time) []byte {
	return fmt.Appendf(
		nil,
		`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`,
		token,
		token+"-refresh",
		expiry.UnixMilli(),
	)
}

// writeSlotCredentialDirectly puts bytes into an account slot the way the
// provider CLI would, bypassing the app's own sign-out refusal. Fixtures need
// it to reproduce a husked slot, which only the provider ever authors.
func writeSlotCredentialDirectly(t *testing.T, app *App, accountID string, data []byte) {
	t.Helper()
	path, err := app.providerCredentials.AccountCredentialPath(string(provider.Claude), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An inactive account whose access token has already expired is NOT refreshed.
// Anthropic's token endpoint commits the refresh-token rotation the moment it
// processes the request — before the client can see the response, with no
// grace on the retired token — so a refresh AO triggers on the user's behalf
// ends that account's chain outright if the answer is lost. There is no copy
// anywhere to fall back to.
//
// So the refresh is declined: the card keeps its last-known usage and says
// why. Nothing is spawned (the fixture's poisoned provider binary fails the
// test from cleanup on any spawn), the slot is untouched, no snapshot is
// published, and no throttle is recorded — no request was sent to earn one.
func TestInactiveExpiredClaudeUsageRefreshSpendsNothing(t *testing.T) {
	app := newTestAppWithStore(t)
	expired := claudeCredentialExpiringAt("stale", time.Now().Add(-time.Hour))
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", expired},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)},
	)
	// Any outbound request fails the test: an expired bearer earns a 401 that
	// says nothing, and the endpoint's 429 throttle is per-bearer and shared
	// across every machine on the account.
	app.rateLimitProbeClientOverride = &http.Client{Transport: tripwireRoundTripper{t: t}}

	var published int
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:usage" {
			published++
		}
	}

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"inactive",
	)
	if !errors.Is(err, errClaudeUsageStale) {
		t.Fatalf("refresh error = %v, want errClaudeUsageStale", err)
	}
	saved, readErr := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"inactive",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != string(expired) {
		t.Fatalf("saved slot = %s, want it untouched by a refusal", saved)
	}
	if published != 0 {
		t.Fatalf("published %d snapshots, want none for a refresh that never ran", published)
	}
	if remaining := app.usageProbe.backoff.Remaining(string(provider.Claude), "inactive"); remaining != 0 {
		t.Fatalf("Remaining(inactive) = %v, want no backoff from a request never sent", remaining)
	}
}

// The token endpoint is the only thing off limits. An inactive account whose
// access token is still live reads its usage over HTTP exactly as before —
// read-only, no CLI, no rotation.
func TestInactiveLiveClaudeUsageRefreshProbesOverHTTP(t *testing.T) {
	app := newTestAppWithStore(t)
	live := claudeCredentialExpiringAt("live", time.Now().Add(8*time.Hour))
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", live},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)},
	)
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "live")

	var usage provider.UsageEvent
	app.testEmitHook = func(name string, data any) {
		if name == "provider:usage" {
			usage, _ = data.(provider.UsageEvent)
		}
	}

	if err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"inactive",
	); err != nil {
		t.Fatalf("refreshProviderAccountUsage() error = %v", err)
	}
	if usage.RateLimits == nil || len(usage.RateLimits.Limits) == 0 {
		t.Fatal("no usage snapshot was emitted for a live inactive account")
	}
	if usage.RateLimits.AccountID != "inactive" {
		t.Fatalf("snapshot account = %q, want inactive", usage.RateLimits.AccountID)
	}
	saved, err := app.providerCredentials.ReadCredential(string(provider.Claude), "inactive", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(live) {
		t.Fatalf("saved slot = %s, want a read-only probe to leave it alone", saved)
	}
}

// The server can retire a bearer the stored expiry still calls live. Same
// verdict, same refusal to heal: only selecting the account rotates it.
func TestInactiveClaudeUsageRefreshReadsA401AsStale(t *testing.T) {
	app := newTestAppWithStore(t)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", claudeCredentialExpiringAt("retired", time.Now().Add(time.Hour))},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)},
	)
	// The endpoint accepts only a bearer this account does not hold.
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "something-else")

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"inactive",
	)
	if !errors.Is(err, errClaudeUsageStale) {
		t.Fatalf("refresh error = %v, want errClaudeUsageStale", err)
	}
}

// A husked slot is a login the provider already ended. The refresh names the
// account and the one recovery there is, rather than reporting a transport
// failure the user cannot act on.
func TestInactiveClaudeUsageRefreshOnAHuskedSlotAsksForALogin(t *testing.T) {
	app := newTestAppWithStore(t)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"old-rt"}}`)},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)},
	)
	writeSlotCredentialDirectly(
		t,
		app,
		"inactive",
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`),
	)
	app.rateLimitProbeClientOverride = &http.Client{Transport: tripwireRoundTripper{t: t}}

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"inactive",
	)
	if err == nil || !strings.Contains(err.Error(), "sign in to this account again") {
		t.Fatalf("refresh error = %v, want the needs-login instruction", err)
	}
	if !strings.Contains(err.Error(), "inactive@example.com") {
		t.Fatalf("error = %v, want the account named", err)
	}
}

// The selected account's refresh is the one rotation AO still triggers, and it
// can come back with the login destroyed: on invalid_grant the CLI blanks the
// canonical credential in place and its probe still answers success, echoing
// the husk's residual identity fields. The bytes are the verdict — trusting
// the probe's account object here is what let the husk reach the slot and
// finish the account off.
func TestSelectedClaudeUsageRefreshRefusesAHuskedCanonicalCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := app.providerCredentials.ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	// The mock reports a fully populated, matching identity — the shape that
	// defeats every check except reading the credential itself.
	husk := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"subscriptionType":"Claude Max"}}`
	binary := writeClaudeRefreshMockBinary(t, canonical, husk, "selected@example.com")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "nothing-valid")
	auditPath := filepath.Join(t.TempDir(), "account-audit.log")
	app.accountAuditPath = auditPath

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil || !strings.Contains(err.Error(), "sign in to this account again") {
		t.Fatalf("refresh error = %v, want the needs-login instruction", err)
	}
	saved, readErr := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != string(original) {
		t.Fatalf("saved slot = %s, want the husk kept out of it", saved)
	}
	audit, auditErr := os.ReadFile(auditPath)
	if auditErr != nil {
		t.Fatalf("read audit log: %v", auditErr)
	}
	if !strings.Contains(string(audit), "signed out") {
		t.Fatalf("audit log = %q, want the sign-out recorded at cause time", audit)
	}
}

// The husk does not always arrive during the refresh: an earlier session's
// failed refresh, or a boot that inherited the brick, leaves the canonical
// credential already blank when the next usage refresh starts. Read as a
// credential those blank tokens are indistinguishable from "never logged in",
// so the HTTP probe would report ErrNoCredentials — "run claude login", naming
// no account and leaving no trail of a login that was destroyed. Refusing at
// entry names the account and records the finding, before anything is sent.
func TestSelectedClaudeUsageRefreshRefusesAnAlreadyHuskedCanonicalCredential(t *testing.T) {
	app := newTestAppWithStore(t)
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	// The husk reaches the canonical store the only way it ever does: written
	// by the CLI itself. Agent Overflow's own write paths refuse these bytes.
	if err := app.providerCredentials.WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"subscriptionType":"Claude Max"}}`),
	); err != nil {
		t.Fatal(err)
	}
	// Nothing may be sent: a blank bearer earns a 401 that says nothing, and
	// the account's own throttle is shared across every machine on it.
	app.rateLimitProbeClientOverride = &http.Client{Transport: tripwireRoundTripper{t: t}}
	auditPath := filepath.Join(t.TempDir(), "account-audit.log")
	app.accountAuditPath = auditPath

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil || !strings.Contains(err.Error(), "sign in to this account again") {
		t.Fatalf("refresh error = %v, want the needs-login instruction", err)
	}
	if !strings.Contains(err.Error(), "selected@example.com") {
		t.Fatalf("error = %v, want the account named", err)
	}
	saved, readErr := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != string(original) {
		t.Fatalf("saved slot = %s, want the last real credential kept", saved)
	}
	audit, auditErr := os.ReadFile(auditPath)
	if auditErr != nil {
		t.Fatalf("read audit log: %v", auditErr)
	}
	if !strings.Contains(string(audit), "signed out") {
		t.Fatalf("audit log = %q, want the sign-out recorded", audit)
	}
}

// The realistic echo of a destroyed login: with the canonical home's
// oauthAccount record absent — which is where the CLI keeps the identity it
// reports, and which a `/logout`-shaped teardown clears — the probe can answer
// success carrying nothing but a plan label and a backend name (spike-verified
// on 2.1.232, 2026-08-16). That shape reads as unauthenticated, so a check
// order that consulted ClaudeUnauthenticated first would report "Claude
// credentials expired; log in again" — generic, unaudited, and naming no
// account — for a state the credential bytes describe exactly. The bytes are
// read first, so the same husk yields the same named verdict as the populated
// echo above.
func TestSelectedClaudeUsageRefreshRefusesAHuskWhoseProbeEchoesNoIdentity(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := app.providerCredentials.ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	husk := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"subscriptionType":"Claude Max"}}`
	binary := writeClaudeRefreshMockBinaryReporting(
		t,
		canonical,
		husk,
		`{"subscriptionType":"Claude Max","apiProvider":"firstParty"}`,
	)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "nothing-valid")
	auditPath := filepath.Join(t.TempDir(), "account-audit.log")
	app.accountAuditPath = auditPath

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil || !strings.Contains(err.Error(), "sign in to this account again") {
		t.Fatalf("refresh error = %v, want the needs-login instruction", err)
	}
	if !strings.Contains(err.Error(), "selected@example.com") {
		t.Fatalf("error = %v, want the account named", err)
	}
	saved, readErr := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != string(original) {
		t.Fatalf("saved slot = %s, want the husk kept out of it", saved)
	}
	audit, auditErr := os.ReadFile(auditPath)
	if auditErr != nil {
		t.Fatalf("read audit log: %v", auditErr)
	}
	if !strings.Contains(string(audit), "signed out") {
		t.Fatalf("audit log = %q, want the sign-out recorded at cause time", audit)
	}
}

// An inactive account's read-only probe still spends a request, so it can
// still earn the endpoint's 429 — and that throttle is per-bearer. The hold has
// to land on the account that earned it and nowhere else, or one inactive
// card's throttle would stall the selected account's refresh and make a
// throttled-but-alive login indistinguishable from a dead one (the 2026-08-03
// shape of this bug).
func TestInactiveClaudeUsageRefreshRecordsItsOwn429Hold(t *testing.T) {
	app := newTestAppWithStore(t)
	live := claudeCredentialExpiringAt("live", time.Now().Add(8*time.Hour))
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", live},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)},
	)
	app.rateLimitProbeClientOverride = rateLimitedUsageClient(t, "45")

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"inactive",
	)
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("refresh error = %v, want *claude.RateLimitedError", err)
	}
	remaining := app.usageProbe.backoff.Remaining(string(provider.Claude), "inactive")
	if remaining <= 0 || remaining > 45*time.Second {
		t.Fatalf("Remaining(inactive) = %v, want (0s, 45s] from the 429", remaining)
	}
	if got := app.usageProbe.backoff.Remaining(string(provider.Claude), "selected"); got != 0 {
		t.Fatalf("Remaining(selected) = %v, want 0 — the throttle is per-bearer", got)
	}
}
