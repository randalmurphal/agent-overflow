package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestParseRateLimitsFromHeaders_BothWindows pins the success path
// where the API returns both the 5h and 7d windows.
func TestParseRateLimitsFromHeaders_BothWindows(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.19")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.45")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778814000")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")

	now := time.UnixMilli(1_700_000_000_000)
	snap, err := parseRateLimitsFromHeaders(headers, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap.Provider != string(provider.Claude) {
		t.Errorf("Provider: got %q, want %q", snap.Provider, string(provider.Claude))
	}
	if snap.UpdatedAt != now.UnixMilli() {
		t.Errorf("UpdatedAt: got %d, want %d", snap.UpdatedAt, now.UnixMilli())
	}
	if len(snap.Limits) != 2 {
		t.Fatalf("Limits len: got %d, want 2", len(snap.Limits))
	}

	byWindow := map[int]provider.RateLimitEntry{}
	for _, l := range snap.Limits {
		byWindow[l.WindowMins] = l
	}
	five, ok := byWindow[300]
	if !ok {
		t.Fatalf("missing 5h window in %+v", snap.Limits)
	}
	if five.UsedPercent != 19 {
		t.Errorf("5h UsedPercent: got %v, want 19", five.UsedPercent)
	}
	if five.ResetsAt != 1778479200 {
		t.Errorf("5h ResetsAt: got %d, want 1778479200", five.ResetsAt)
	}
	if five.LimitID != "five_hour" {
		t.Errorf("5h LimitID: got %q, want five_hour", five.LimitID)
	}

	seven, ok := byWindow[10080]
	if !ok {
		t.Fatalf("missing 7d window in %+v", snap.Limits)
	}
	if seven.UsedPercent != 45 {
		t.Errorf("7d UsedPercent: got %v, want 45", seven.UsedPercent)
	}
	if seven.ResetsAt != 1778814000 {
		t.Errorf("7d ResetsAt: got %d, want 1778814000", seven.ResetsAt)
	}
	if seven.LimitID != "seven_day" {
		t.Errorf("7d LimitID: got %q, want seven_day", seven.LimitID)
	}
}

// TestParseRateLimitsFromHeaders_SingleWindow confirms a response with
// only one window's headers still yields a usable snapshot (partial
// fills are normal during steady-state evolution of the API).
func TestParseRateLimitsFromHeaders_SingleWindow(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")

	snap, err := parseRateLimitsFromHeaders(headers, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(snap.Limits))
	}
	if snap.Limits[0].WindowMins != 300 {
		t.Errorf("WindowMins: got %d, want 300", snap.Limits[0].WindowMins)
	}
	if snap.Limits[0].UsedPercent != 42 {
		t.Errorf("UsedPercent: got %v, want 42", snap.Limits[0].UsedPercent)
	}
}

// TestParseRateLimitsFromHeaders_PartialWindowDropped ensures a window
// with one valid header and one missing/malformed header is dropped
// entirely rather than emitted with a 0 fallback that would visibly
// reset the ring.
func TestParseRateLimitsFromHeaders_PartialWindowDropped(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	// Missing 5h-Reset: window should be dropped.
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.45")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778814000")

	snap, err := parseRateLimitsFromHeaders(headers, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("expected 1 entry (partial 5h dropped), got %d: %+v", len(snap.Limits), snap.Limits)
	}
	if snap.Limits[0].WindowMins != 10080 {
		t.Errorf("WindowMins: got %d, want 10080", snap.Limits[0].WindowMins)
	}
}

// TestParseRateLimitsFromHeaders_MalformedUtilizationDropped pins the
// "header present but unparseable" case — silently dropping that
// window is better than crashing or emitting a 0 fallback.
func TestParseRateLimitsFromHeaders_MalformedUtilizationDropped(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "not-a-number")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.45")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778814000")

	snap, err := parseRateLimitsFromHeaders(headers, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("expected 1 entry (malformed 5h dropped), got %d", len(snap.Limits))
	}
	if snap.Limits[0].WindowMins != 10080 {
		t.Errorf("WindowMins: got %d, want 10080", snap.Limits[0].WindowMins)
	}
}

// TestParseRateLimitsFromHeaders_NoHeadersError confirms we return an
// error when the API returns no rate-limit headers at all — the
// contract may have shifted upstream and the caller should log it
// loudly rather than emit an empty snapshot.
func TestParseRateLimitsFromHeaders_NoHeadersError(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")

	_, err := parseRateLimitsFromHeaders(headers, time.Now())
	if err == nil {
		t.Fatalf("expected error for missing headers, got nil")
	}
}

// TestProbeRateLimits_EndToEnd exercises the full HTTP path using a
// local httptest server. This is the only test that covers
// loadOAuthBearer + executeRateLimitsProbe + parseRateLimitsFromHeaders
// composed together.
func TestProbeRateLimits_EndToEnd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir credsDir: %v", err)
	}
	credsBody := []byte(`{"claudeAiOauth":{"accessToken":"test-bearer-xyz","refreshToken":"r","expiresAt":99999999999}}`)
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), credsBody, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	var receivedAuth string
	var receivedBeta string
	var receivedVersion string
	var receivedMethod string
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedBeta = r.Header.Get("anthropic-beta")
		receivedVersion = r.Header.Get("anthropic-version")
		receivedMethod = r.Method
		receivedPath = r.URL.Path

		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.19")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1778479200")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.45")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1778814000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","type":"message","content":[]}`))
	}))
	defer srv.Close()

	// Build a client that rewrites every outbound request to the test
	// server. Lets us reuse the production ProbeRateLimits code path
	// (which targets api.anthropic.com) without touching the real
	// endpoint.
	srvURL, _ := url.Parse(srv.URL)
	client := &http.Client{
		Transport: redirectTransport{target: srvURL, inner: http.DefaultTransport},
	}

	snap, err := ProbeRateLimits(context.Background(), client)
	if err != nil {
		t.Fatalf("ProbeRateLimits: %v", err)
	}
	if len(snap.Limits) != 2 {
		t.Fatalf("Limits len: got %d, want 2", len(snap.Limits))
	}
	if receivedMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", receivedMethod)
	}
	if receivedPath != "/v1/messages" {
		t.Errorf("path: got %q, want /v1/messages", receivedPath)
	}
	if receivedAuth != "Bearer test-bearer-xyz" {
		t.Errorf("Authorization: got %q, want %q", receivedAuth, "Bearer test-bearer-xyz")
	}
	if receivedBeta != oauthBetaHeader {
		t.Errorf("anthropic-beta: got %q, want %q", receivedBeta, oauthBetaHeader)
	}
	if receivedVersion != anthropicVersion {
		t.Errorf("anthropic-version: got %q, want %q", receivedVersion, anthropicVersion)
	}
}

// TestProbeRateLimits_MissingCredentials returns the sentinel so
// callers can log at debug level rather than treat as a hard failure.
func TestProbeRateLimits_MissingCredentials(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	_, err := ProbeRateLimits(context.Background(), http.DefaultClient)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err: got %v, want ErrNoCredentials", err)
	}
}

// TestProbeRateLimits_EmptyToken treats an empty accessToken as
// missing credentials (the file exists but the user is signed out).
func TestProbeRateLimits_EmptyToken(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	credsDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":""}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := ProbeRateLimits(context.Background(), http.DefaultClient)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err: got %v, want ErrNoCredentials", err)
	}
}

// TestProbeRateLimits_Unauthorized surfaces a clear error when the
// bearer is dead so the app can stop firing the probe instead of
// looping every 2 minutes against a 401.
func TestProbeRateLimits_Unauthorized(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	credsDir := filepath.Join(tmpHome, ".claude")
	_ = os.MkdirAll(credsDir, 0o700)
	_ = os.WriteFile(filepath.Join(credsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"dead-token"}}`), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	srvURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: redirectTransport{target: srvURL, inner: http.DefaultTransport}}

	_, err := ProbeRateLimits(context.Background(), client)
	if err == nil {
		t.Fatalf("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("err: got %v, want one containing 'unauthorized'", err)
	}
}

// redirectTransport rewrites every outbound request to point at the
// httptest server, then delegates to the underlying RoundTripper.
// Keeps tests honest about the request path / headers / method while
// letting them target a local server.
type redirectTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = rt.target.Host
	return rt.inner.RoundTrip(clone)
}
