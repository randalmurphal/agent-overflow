package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// TestHasActiveClaudeSession_Empty returns false when no sessions are
// registered. Locks the contract for the probe loop's idle-skip gate.
func TestHasActiveClaudeSession_Empty(t *testing.T) {
	app := newTestAppWithStore(t)
	if app.hasActiveClaudeSession() {
		t.Errorf("expected false on empty session map")
	}
}

// TestHasActiveClaudeSession_ClaudeOnly returns true with one Claude
// session.
func TestHasActiveClaudeSession_ClaudeOnly(t *testing.T) {
	app := newTestAppWithStore(t)
	app.sessions["t1"] = session{provider: string(provider.Claude), token: "tok"}
	if !app.hasActiveClaudeSession() {
		t.Errorf("expected true with one claude session")
	}
}

// TestHasActiveClaudeSession_CodexOnly returns false when only Codex
// sessions exist — the probe targets Anthropic, so Codex-only setups
// shouldn't fire it.
func TestHasActiveClaudeSession_CodexOnly(t *testing.T) {
	app := newTestAppWithStore(t)
	app.sessions["t1"] = session{provider: string(provider.Codex), token: "tok"}
	if app.hasActiveClaudeSession() {
		t.Errorf("expected false with only codex sessions")
	}
}

// TestHasActiveClaudeSession_Mixed returns true when at least one
// Claude session exists alongside other providers.
func TestHasActiveClaudeSession_Mixed(t *testing.T) {
	app := newTestAppWithStore(t)
	app.sessions["t1"] = session{provider: string(provider.Codex), token: "tok-c"}
	app.sessions["t2"] = session{provider: string(provider.Claude), token: "tok-cl"}
	if !app.hasActiveClaudeSession() {
		t.Errorf("expected true with mixed providers including claude")
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
