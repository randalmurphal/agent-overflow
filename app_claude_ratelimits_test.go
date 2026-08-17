package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/settings"
)

// TestProviderTurnActivity locks the contract the periodic usage poll gates
// on: no activity ever recorded polls nothing (an app with open-but-idle
// threads must cost zero usage requests), activity is scoped per provider,
// and a mark taken after the last activity reads as "no new turns".
func TestProviderTurnActivity(t *testing.T) {
	app := newTestAppWithStore(t)

	if app.providerTurnCompletedSince(string(provider.Claude), time.Time{}) {
		t.Fatalf("expected no activity before any turn completed")
	}

	before := time.Now().Add(-time.Millisecond)
	app.noteProviderTurnActivity(string(provider.Claude))

	if !app.providerTurnCompletedSince(string(provider.Claude), time.Time{}) {
		t.Fatalf("expected activity after a Claude turn completed (zero mark)")
	}
	if !app.providerTurnCompletedSince(string(provider.Claude), before) {
		t.Fatalf("expected activity after a mark predating the turn")
	}
	if app.providerTurnCompletedSince(string(provider.Codex), time.Time{}) {
		t.Fatalf("Claude activity must not read as Codex activity")
	}
	if app.providerTurnCompletedSince(string(provider.Claude), time.Now()) {
		t.Fatalf("a mark after the last turn must read as idle")
	}
}

// TestProbeClaudeRateLimits_EmitsOnSuccess covers the happy path: the
// probe call succeeds, the snapshot lands on the provider:usage channel,
// and the event carries no threadId (rate limits are account-wide).
func TestProbeClaudeRateLimits_EmitsOnSuccess(t *testing.T) {
	app := newTestAppWithStore(t)
	// Seed the canonical credential AFTER the fixture's HOME detach so the
	// probe finds it under the home this test controls.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"bearer-test"}}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.19")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.45")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778814000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: srvURL, inner: http.DefaultTransport},
	}

	var captured struct {
		mu     sync.Mutex
		events []any
	}
	app.testEmitHook = func(name string, data any) {
		if name != "provider:usage" {
			return
		}
		captured.mu.Lock()
		captured.events = append(captured.events, data)
		captured.mu.Unlock()
	}

	app.probeClaudeRateLimits(context.Background())

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.events) != 1 {
		t.Fatalf("expected 1 provider:usage emit, got %d", len(captured.events))
	}
	usage, ok := captured.events[0].(provider.UsageEvent)
	if !ok {
		t.Fatalf("emit payload type = %T, want provider.UsageEvent", captured.events[0])
	}
	if usage.Action != "rate_limits" {
		t.Errorf("Action = %q, want rate_limits", usage.Action)
	}
	if usage.ThreadID != "" {
		t.Errorf("ThreadID = %q, want empty (probe is account-wide)", usage.ThreadID)
	}
	if usage.RateLimits == nil {
		t.Fatalf("RateLimits = nil")
	}
	if len(usage.RateLimits.Limits) != 2 {
		t.Errorf("Limits len = %d, want 2", len(usage.RateLimits.Limits))
	}
}

// TestProbeClaudeRateLimits_SwallowsMissingCredentials pins the
// silent-no-op behavior when the user hasn't authenticated yet.
// Without this contract every probe cadence tick would log noise on a
// freshly installed system.
func TestProbeClaudeRateLimits_SwallowsMissingCredentials(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	// Deliberately do NOT create ~/.claude/.credentials.json.

	app := newTestAppWithStore(t)
	var emitted atomic.Int32
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:usage" {
			emitted.Add(1)
		}
	}

	app.probeClaudeRateLimits(context.Background())

	if emitted.Load() != 0 {
		t.Errorf("expected 0 emits on missing credentials, got %d", emitted.Load())
	}
}

// Before the first account is saved, the canonical login is probed directly
// and its 429s are keyed in the backoff ledger under the empty account ID.
// The hold must be recorded AND enforced: the usage endpoint's throttle is
// per-bearer and shared across every machine on the account, so a fresh
// install that retries into a live window extends the penalty for all of
// them — the "rate limits never update anymore" symptom.
func TestProbeClaudeRateLimits_UnmanagedProbe429RecordsAndEnforcesItsHold(t *testing.T) {
	app := newTestAppWithStore(t)
	// Seed the canonical credential AFTER the fixture's HOME detach so the
	// probe finds it under the home this test controls.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(credsDir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"bearer-test"}}`),
		0o600,
	); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	app.rateLimitProbeClientOverride = rateLimitedUsageClient(t, "45")

	err := app.probeClaudeRateLimits(context.Background())
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("probe error = %v, want the 429 surfaced", err)
	}
	remaining := app.usageBackoff.Remaining(string(provider.Claude), "")
	if remaining <= 0 || remaining > 45*time.Second {
		t.Fatalf("Remaining(\"\") = %v, want the Retry-After hold recorded", remaining)
	}
	// The hold is scoped to the unmanaged key, not smeared across accounts.
	if got := app.usageBackoff.Remaining(string(provider.Claude), "some-account"); got != 0 {
		t.Fatalf("Remaining(some-account) = %v, want the hold scoped to the unmanaged key", got)
	}

	// While the hold runs, the next tick is refused before anything is sent.
	app.rateLimitProbeClientOverride = &http.Client{Transport: tripwireRoundTripper{t: t}}
	err = app.probeClaudeRateLimits(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limited this login") {
		t.Fatalf("probe error during hold = %v, want the backoff refusal", err)
	}
}

// TestProbeClaudeRateLimits_RespectsShuttingDownGate ensures the probe
// short-circuits if the app is already in shutdown — no HTTP call,
// no emit. Protects against late ticker firings after Shutdown begins.
func TestProbeClaudeRateLimits_RespectsShuttingDownGate(t *testing.T) {
	app := newTestAppWithStore(t)
	app.shuttingDown.Store(true)

	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: srvURL, inner: http.DefaultTransport},
	}

	emits := atomic.Int32{}
	app.testEmitHook = func(name string, _ any) {
		if name == "provider:usage" {
			emits.Add(1)
		}
	}

	app.probeClaudeRateLimits(context.Background())

	if hits.Load() != 0 {
		t.Errorf("expected 0 HTTP hits with shutdown set, got %d", hits.Load())
	}
	if emits.Load() != 0 {
		t.Errorf("expected 0 emits with shutdown set, got %d", emits.Load())
	}
}

func TestProbeClaudeRateLimits_StopsWhenExternalAccountIdentityFails(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	installIdentityTestAccount(
		t,
		app,
		string(provider.Claude),
		"first",
		"first@example.com",
		[]byte(`{"claudeAiOauth":{"accessToken":"first"}}`),
	)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": filepath.Join(t.TempDir(), "missing-claude"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.providerCredentials.WriteNativeCredentialForTest(
		string(provider.Claude),
		[]byte(`{"claudeAiOauth":{"accessToken":"unknown"}}`),
	); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	app.rateLimitProbeClientOverride = &http.Client{
		Transport: redirectRoundTripper{target: srvURL, inner: http.DefaultTransport},
	}

	app.probeClaudeRateLimits(context.Background())

	if hits.Load() != 0 {
		t.Fatalf("usage endpoint hits = %d, want 0 after identity failure", hits.Load())
	}
}

// redirectRoundTripper rewrites every outbound request to point at the
// test server, then delegates to the underlying RoundTripper. Mirrors
// the helper in ratelimits_probe_test.go but lives at the app package
// level so this file is self-contained.
type redirectRoundTripper struct {
	target *url.URL
	inner  http.RoundTripper
}

func (rt redirectRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = rt.target.Host
	return rt.inner.RoundTrip(clone)
}
