package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"
	"net/url"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func TestSelectedCodexUsageCredentialRotationAdvancesReconnectGeneration(t *testing.T) {
	app := newTestAppWithStore(t)
	original := []byte(`{"tokens":{"access_token":"original"}}`)
	rotated := `{"tokens":{"access_token":"rotated"}}`
	installIdentityTestAccount(
		t,
		app,
		string(provider.Codex),
		"selected",
		"selected@example.com",
		original,
	)
	generation := app.providerAccounts.Generation(string(provider.Codex))
	app.sessionManager().put("thread", session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: generation,
		CredentialAccountID:  "selected",
		Liveness:             newSessionLiveness(time.Now()),
	})

	binary := filepath.Join(t.TempDir(), "mock-codex")
	script := "#!/bin/bash\n" +
		"read -r _ || true\nread -r _ || true\nread -r _ || true\nread -r _ || true\n" +
		`printf '%s' '` + rotated + `' > "$CODEX_HOME/auth.json"` + "\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"v2"}}'` + "\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":"selected@example.com","planType":"pro"},"requiresOpenaiAuth":true}}'` + "\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":7,"windowDurationMins":300,"resetsAt":1778479200}}}}'` + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}

	var accountEvent ProviderAccountEvent
	app.testEmitHook = func(name string, data any) {
		if name == "provider:account" {
			accountEvent, _ = data.(ProviderAccountEvent)
		}
	}
	if err := app.refreshProviderAccountUsage(
		context.Background(),
		string(provider.Codex),
		"selected",
	); err != nil {
		t.Fatal(err)
	}

	if got := app.providerAccounts.Generation(string(provider.Codex)); got != generation+1 {
		t.Fatalf("credential generation = %d, want %d", got, generation+1)
	}
	current, _ := app.sessionManager().get("thread")
	if current.Token != "old-process" || current.CredentialGeneration != generation {
		t.Fatalf(
			"running session changed before send gate: token=%q generation=%d",
			current.Token,
			current.CredentialGeneration,
		)
	}
	if accountEvent.Generation != generation+1 || accountEvent.AccountID != "selected" {
		t.Fatalf("provider account event = %+v, want generation %d", accountEvent, generation+1)
	}
	for _, location := range []struct {
		accountID string
		active    bool
	}{
		{active: true},
		{accountID: "selected"},
	} {
		got, err := providerCredentialsForTest(t, app).ReadCredential(
			string(provider.Codex),
			location.accountID,
			location.active,
		)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != rotated {
			t.Fatalf("active=%v credential = %s, want %s", location.active, got, rotated)
		}
	}
}

func TestSelectedUsageRefreshCannotOverwriteExternalCredentialChange(t *testing.T) {
	app := newTestAppWithStore(t)
	original := []byte(`{"claudeAiOauth":{"accessToken":"original"}}`)
	installUsageTestAccounts(t, app, usageTestAccount{"selected", original})

	started := make(chan struct{})
	release := make(chan struct{})
	app.rateLimitProbeClientOverride = blockingUsageClient(t, started, release)

	result := make(chan error, 1)
	go func() {
		result <- app.refreshProviderAccountUsage(
			context.Background(),
			string(provider.Claude),
			"selected",
		)
	}()
	<-started

	external := []byte(`{"claudeAiOauth":{"accessToken":"external"}}`)
	if err := providerCredentialsForTest(t, app).WriteNativeCredentialForTest(
		string(provider.Claude),
		external,
	); err != nil {
		t.Fatal(err)
	}
	close(release)

	if err := <-result; err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("refresh error = %v, want credential-change rejection", err)
	}
	active, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != string(external) {
		t.Fatalf("canonical credential = %s, want external credential", active)
	}
	saved, err := providerCredentialsForTest(t, app).ReadCredential(
		string(provider.Claude),
		"selected",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(original) {
		t.Fatalf("saved credential = %s, want original credential", saved)
	}
}

func TestUsageRefreshCompletesBeforeConcurrentAccountRemoval(t *testing.T) {
	app := newTestAppWithStore(t)
	activeCredential := []byte(`{"claudeAiOauth":{"accessToken":"active"}}`)
	inactiveCredential := []byte(`{"claudeAiOauth":{"accessToken":"inactive"}}`)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{"inactive", inactiveCredential},
		usageTestAccount{"active", activeCredential},
	)

	started := make(chan struct{})
	release := make(chan struct{})
	app.rateLimitProbeClientOverride = blockingUsageClient(t, started, release)

	var (
		eventsMu sync.Mutex
		actions  []string
	)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		usage, ok := data.(provider.UsageEvent)
		if !ok || usage.RateLimits == nil || usage.RateLimits.AccountID != "inactive" {
			return
		}
		eventsMu.Lock()
		actions = append(actions, usage.Action)
		eventsMu.Unlock()
	}

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- app.refreshProviderAccountUsage(
			context.Background(),
			string(provider.Claude),
			"inactive",
		)
	}()
	<-started

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- app.RemoveProviderAccount(string(provider.Claude), "inactive")
	}()
	select {
	case err := <-removeDone:
		t.Fatalf("removal completed before the in-flight refresh: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("removal failed: %v", err)
	}

	if hasRateLimitsSnapshot(app, string(provider.Claude), "inactive") {
		t.Fatal("removed account's usage snapshot reappeared")
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(actions) != 2 ||
		actions[0] != "rate_limits" ||
		actions[1] != "rate_limits_removed" {
		t.Fatalf("usage actions = %v, want refresh then removal", actions)
	}
}

func TestInactiveUsageRefreshesAreSerialized(t *testing.T) {
	app := newTestAppWithStore(t)
	installUsageTestAccounts(
		t,
		app,
		usageTestAccount{
			"inactive",
			[]byte(`{"claudeAiOauth":{"accessToken":"inactive"}}`),
		},
		usageTestAccount{
			"active",
			[]byte(`{"claudeAiOauth":{"accessToken":"active"}}`),
		},
	)

	started := make(chan struct{})
	release := make(chan struct{})
	var (
		once sync.Once
		hits atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"usage"}`))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: target, inner: http.DefaultTransport},
	}

	results := make(chan error, 2)
	refresh := func() {
		results <- app.refreshProviderAccountUsage(
			context.Background(),
			string(provider.Claude),
			"inactive",
		)
	}
	go refresh()
	<-started
	go refresh()
	time.Sleep(100 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent provider probes = %d, want 1", got)
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("refresh failed: %v", err)
		}
	}
}

type usageTestAccount struct {
	id         string
	credential []byte
}

func installUsageTestAccounts(t *testing.T, app *App, values ...usageTestAccount) {
	t.Helper()
	if len(values) == 0 {
		t.Fatal("installUsageTestAccounts requires accounts")
	}
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	var activeCredential []byte
	for _, value := range values {
		if err := credentials.WriteAccountCredential(
			string(provider.Claude),
			value.id,
			value.credential,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.UpsertAndActivate(provideraccounts.Account{
			ID:       value.id,
			Provider: string(provider.Claude),
			Email:    value.id + "@example.com",
		}); err != nil {
			t.Fatal(err)
		}
		activeCredential = value.credential
	}
	active, ok := accounts.Active(string(provider.Claude), time.Now())
	if !ok {
		t.Fatal("active account missing")
	}
	if err := credentials.Activate(string(provider.Claude), "", active.ID); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
	app.providerAccounts.BlessCredentialForTest(string(provider.Claude), activeCredential)
}

func blockingUsageClient(
	t *testing.T,
	started chan<- struct{},
	release <-chan struct{},
) *http.Client {
	t.Helper()
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
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
