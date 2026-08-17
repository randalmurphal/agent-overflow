package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

// writeClaudeRefreshMockBinary stands in for the Claude CLI's zero-turn
// account probe: it rotates the credential in whichever home it was pointed
// at and reports the account it authenticated as.
//
// It refuses to run with CLAUDE_CONFIG_DIR set. That is the regression guard:
// Claude's refresh-token rotation is single-use and serialized on a lockfile
// scoped to the config home, so rotating the selected account anywhere but the
// canonical home forks the chain and strands the login. Claude also treats the
// variable's presence as "non-default home", which on macOS moves the
// credential to a different Keychain service.
func writeClaudeRefreshMockBinary(t *testing.T, credentialPath, rotated, email string) string {
	t.Helper()
	return writeClaudeRefreshMockBinaryReporting(
		t,
		credentialPath,
		rotated,
		`{"email":"`+email+`","subscriptionType":"max","tokenSource":"oauth"}`,
	)
}

// writeClaudeRefreshMockBinaryReporting is the same mock with the reported
// account object supplied verbatim. Fixtures need it because WHICH fields the
// probe echoes is the variable under test: a refresh the server answered with
// invalid_grant leaves the CLI reporting only what the husk still holds, which
// can be as little as a plan label and a backend name.
func writeClaudeRefreshMockBinaryReporting(
	t *testing.T,
	credentialPath,
	rotated,
	account string,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock-claude")
	response := `{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"ao-probe-init","response":{"account":` +
		account + `}}}`
	script := "#!/bin/bash\n" +
		"if [ -n \"$CLAUDE_CONFIG_DIR\" ]; then\n" +
		"  echo 'refresh ran outside the canonical home' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"read -r _ || true\n" +
		"printf '%s' '" + rotated + "' > " + shellQuote(credentialPath) + "\n" +
		"printf '%s\\n' '" + response + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// expiringUsageClient answers 401 for every bearer but wantBearer, which is
// how the usage endpoint reports a token the provider has since rotated.
func expiringUsageClient(t *testing.T, wantBearer string) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"usage"}`))
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: redirectRoundTripper{target: target, inner: http.DefaultTransport},
	}
}

// The selected account's credential IS the canonical one, so an expired token
// must be rotated there and nowhere else. The rotated value then has to reach
// both the canonical store and the saved slot, or the next switch would
// restore the retired token.
func TestSelectedClaudeUsageRefreshRotatesInTheCanonicalHome(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"rotated"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := app.providerCredentials.ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinary(t, canonical, rotated, "selected@example.com")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("refreshProviderAccountUsage() error = %v", err)
	}

	active, err := app.providerCredentials.ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != rotated {
		t.Fatalf("canonical credential = %s, want the rotated value", active)
	}
	saved, err := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != rotated {
		t.Fatalf("saved slot = %s, want the rotated value", saved)
	}

	// A stale fingerprint would make the next reconciliation treat Agent
	// Overflow's own rotation as an external login.
	app.providerAccountMu.RLock()
	fingerprint := app.providerCredentialFingerprints[string(provider.Claude)]
	app.providerAccountMu.RUnlock()
	if fingerprint != sha256.Sum256([]byte(rotated)) {
		t.Fatal("credential fingerprint was not advanced to the rotated value")
	}
	if usage.RateLimits == nil || len(usage.RateLimits.Limits) == 0 {
		t.Fatal("no usage snapshot was emitted after the refresh")
	}
	if usage.RateLimits.AccountID != "selected" {
		t.Fatalf("snapshot account = %q, want selected", usage.RateLimits.AccountID)
	}
}

// The common refresh: the access token is still valid, so nothing rotates. The
// canonical store then has to be checked against what the probe started from.
// Checking it against a rotation that never happened can never match, which
// failed every ordinary probe — the rings never received a snapshot and the
// credential fingerprint was dropped each time, sending the next reconciliation
// off to spawn an identity probe for a login that had not changed.
func TestSelectedClaudeUsageRefreshWithoutRotationPublishesLimits(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	// A token that still works needs no refresh, so the CLI must not run.
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "must-not-run"),
	}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "original")

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
		t.Fatalf("refreshProviderAccountUsage() error = %v", err)
	}

	if usage.RateLimits == nil || len(usage.RateLimits.Limits) == 0 {
		t.Fatal("no usage snapshot was emitted after the refresh")
	}
	if usage.RateLimits.AccountID != "selected" {
		t.Fatalf("snapshot account = %q, want selected", usage.RateLimits.AccountID)
	}

	active, err := app.providerCredentials.ReadCredential(string(provider.Claude), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != string(original) {
		t.Fatalf("canonical credential = %s, want the original value", active)
	}
	app.providerAccountMu.RLock()
	fingerprint, blessed := app.providerCredentialFingerprints[string(provider.Claude)]
	app.providerAccountMu.RUnlock()
	if !blessed || fingerprint != sha256.Sum256(original) {
		t.Fatal("credential fingerprint no longer blesses the unchanged credential")
	}
}

// The probe measured this account's real quota. Credential bookkeeping failing
// afterwards says nothing about that measurement, so the rings must still
// receive it rather than going blank on a race the user cannot see.
func TestSelectedClaudeUsageRefreshPublishesLimitsWhenTheCommitFails(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	// Stand in for another Claude process rotating the shared credential while
	// the usage endpoint is answering.
	app.rateLimitProbeClientOverride = usageClientWritingCredential(
		t,
		"original",
		func() error {
			return app.providerCredentials.WriteNativeCredentialForTest(
				string(provider.Claude),
				[]byte(`{"claudeAiOauth":{"accessToken":"external"}}`),
			)
		},
	)

	var usage provider.UsageEvent
	app.testEmitHook = func(name string, data any) {
		if name == "provider:usage" {
			usage, _ = data.(provider.UsageEvent)
		}
	}

	err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("refresh error = %v, want the credential change reported", err)
	}
	if usage.RateLimits == nil || len(usage.RateLimits.Limits) == 0 {
		t.Fatal("the probed limits were discarded along with the failed commit")
	}
	if usage.RateLimits.AccountID != "selected" {
		t.Fatalf("snapshot account = %q, want selected", usage.RateLimits.AccountID)
	}
}

// The other side of that rule: a snapshot is only publishable if it provably
// describes the account it would be filed under. Once the selection moves under
// an in-flight probe the refresh cannot know which account it measured, so it
// must publish nothing.
func TestUsageRefreshWithholdsLimitsWhenTheSelectionMoves(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"other", []byte(`{"claudeAiOauth":{"accessToken":"other"}}`)},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)},
	)

	started := make(chan struct{})
	release := make(chan struct{})
	app.rateLimitProbeClientOverride = blockingUsageClient(t, started, release)

	var published int
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		if usage, ok := data.(provider.UsageEvent); ok && usage.Action == "rate_limits" {
			published++
		}
	}

	result := make(chan error, 1)
	go func() {
		result <- app.refreshProviderAccountUsage(
			context.Background(),
			string(provider.Claude),
			"selected",
		)
	}()
	<-started
	if _, err := app.providerAccounts.Activate(string(provider.Claude), "other"); err != nil {
		t.Fatal(err)
	}
	close(release)

	if err := <-result; err == nil || !strings.Contains(err.Error(), "while refreshing usage") {
		t.Fatalf("refresh error = %v, want the moved selection rejected", err)
	}
	if published != 0 {
		t.Fatalf("published %d snapshots, want none for an unattributable probe", published)
	}
}

// usageClientWritingCredential answers the usage endpoint for wantBearer and,
// on the way, runs writeReplacement — the shape of another provider process
// rotating the shared store mid-probe. The rotation is a callback (not a file
// path) so it can go through WriteNativeCredentialForTest and stay ignorant of
// the platform's canonical-store layout.
func usageClientWritingCredential(
	t *testing.T,
	wantBearer string,
	writeReplacement func() error,
) *http.Client {
	t.Helper()
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		once.Do(func() {
			if err := writeReplacement(); err != nil {
				t.Error(err)
			}
		})
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"usage"}`))
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: redirectRoundTripper{target: target, inner: http.DefaultTransport},
	}
}

// A manual refresh during that account's server-imposed backoff must refuse
// up front — retrying into a 429 only extends the penalty — and must do so
// without sending anything: the tripwire transport fails the test on any
// request.
func TestManualUsageRefreshRefusesDuringBackoff(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installUsageTestAccounts(t, app, usageTestAccount{
		"selected",
		[]byte(`{"claudeAiOauth":{"accessToken":"original"}}`),
	})
	app.rateLimitProbeClientOverride = &http.Client{Transport: tripwireRoundTripper{t: t}}

	app.usageBackoff.Note(
		string(provider.Claude),
		"selected",
		&claude.RateLimitedError{RetryAfter: time.Minute},
	)

	err := app.RefreshProviderAccountUsage(string(provider.Claude), "selected")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("refresh error = %v, want the backoff reported", err)
	}
}

// A manual refresh that earns a 429 must record it against exactly that
// account: later probes for it hold, while every other account stays
// refreshable — the server throttle is per-bearer (observed 2026-08-03, when
// a provider-wide hold made throttled-but-alive accounts look dead).
func TestManualUsageRefreshRecordsAPerAccountBackoff(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"other", []byte(`{"claudeAiOauth":{"accessToken":"other"}}`)},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)},
	)
	app.rateLimitProbeClientOverride = rateLimitedUsageClient(t, "45")

	err := app.RefreshProviderAccountUsage(string(provider.Claude), "selected")
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("refresh error = %v, want *claude.RateLimitedError", err)
	}
	remaining := app.usageBackoff.Remaining(string(provider.Claude), "selected")
	if remaining <= 0 || remaining > 45*time.Second {
		t.Fatalf("Remaining(selected) = %v, want (0s, 45s] from the manual 429", remaining)
	}
	if got := app.usageBackoff.Remaining(string(provider.Claude), "other"); got != 0 {
		t.Fatalf("Remaining(other) = %v, want 0 — one account's 429 must not hold the rest", got)
	}
}

// The selected account waiting out a 429 must not block another card's
// refresh: that inactive account's own probe goes through and succeeds.
func TestBackoffOnOneAccountDoesNotBlockAnotherCardsRefresh(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"other", []byte(`{"claudeAiOauth":{"accessToken":"other"}}`)},
		usageTestAccount{"selected", []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)},
	)
	// The inactive account's token is valid, so no CLI refresh may run.
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "must-not-run"),
	}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "other")

	app.usageBackoff.Note(
		string(provider.Claude),
		"selected",
		&claude.RateLimitedError{RetryAfter: time.Hour},
	)

	if err := app.RefreshProviderAccountUsage(string(provider.Claude), "other"); err != nil {
		t.Fatalf("refresh of the un-throttled account failed: %v", err)
	}
}

// tripwireRoundTripper fails the test on any outbound request.
type tripwireRoundTripper struct{ t *testing.T }

func (rt tripwireRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.t.Error("HTTP request sent while the usage endpoint backoff held")
	return nil, errors.New("request blocked by test tripwire")
}

// rateLimitedUsageClient answers every request with 429 + Retry-After.
func rateLimitedUsageClient(t *testing.T, retryAfter string) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: redirectRoundTripper{target: target, inner: http.DefaultTransport},
	}
}

// rotatedThrottledUsageClient answers 401 for the stale bearer — sending the
// probe into its native-refresh heal — and 429 for the rotated bearer, the
// shape of a usage endpoint that throttles while the token endpoint stays
// clear.
func rotatedThrottledUsageClient(
	t *testing.T,
	staleBearer,
	rotatedBearer,
	retryAfter string,
) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + staleBearer:
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer " + rotatedBearer:
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Errorf("usage request with unexpected bearer %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: redirectRoundTripper{target: target, inner: http.DefaultTransport},
	}
}

// The same rule in the canonical home: the CLI already published the rotation
// there, so the slot and the fingerprint must catch up even though the usage
// retry was throttled — otherwise the next switch restores a retired token
// and the next reconciliation misreads Agent Overflow's own rotation as an
// external login.
func TestSelectedClaudeUsageRefreshCommitsRotationWhenRetryIsThrottled(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"rotated"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := app.providerCredentials.ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinary(t, canonical, rotated, "selected@example.com")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = rotatedThrottledUsageClient(t, "original", "rotated", "45")

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("refresh error = %v, want the 429 surfaced", err)
	}
	saved, readErr := app.providerCredentials.ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != rotated {
		t.Fatalf("saved slot = %s, want the rotation committed despite the throttled retry", saved)
	}
	app.providerAccountMu.RLock()
	fingerprint := app.providerCredentialFingerprints[string(provider.Claude)]
	app.providerAccountMu.RUnlock()
	if fingerprint != sha256.Sum256([]byte(rotated)) {
		t.Fatal("credential fingerprint was not advanced to the rotated value")
	}
}

// An in-place refresh means the CLI could return having authenticated as
// somebody else — an external login during the probe. That must not stamp the
// other account's rotated credential onto this one's slot.
func TestSelectedClaudeUsageRefreshRejectsADifferentAccount(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	rotated := `{"claudeAiOauth":{"accessToken":"someone-else"}}`
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	canonical, err := app.providerCredentials.ActiveCredentialPath(string(provider.Claude))
	if err != nil {
		t.Fatal(err)
	}
	binary := writeClaudeRefreshMockBinary(t, canonical, rotated, "intruder@example.com")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = expiringUsageClient(t, "someone-else")

	err = app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Claude),
		"selected",
	)
	if err == nil {
		t.Fatal("refreshProviderAccountUsage() accepted a refresh for a different account")
	}
	if !strings.Contains(err.Error(), "intruder@example.com") {
		t.Fatalf("error = %v, want the observed account named", err)
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
		t.Fatalf("saved slot = %s, want it untouched", saved)
	}
}
