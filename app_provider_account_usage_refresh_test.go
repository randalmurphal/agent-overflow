package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
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
	path := filepath.Join(t.TempDir(), "mock-claude")
	response := `{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"ao-probe-init","response":{"account":` +
		`{"email":"` + email + `","subscriptionType":"max","tokenSource":"oauth"}}}}`
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
