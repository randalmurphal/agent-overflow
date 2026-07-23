package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// writeMockCodexAppServerScript writes a shell script to tmpDir that
// mimics `codex app-server` during a probe. The probe sends four
// NDJSON lines (initialize request, initialized notification,
// account/read, account/rateLimits/read); the script reads enough to let
// the probe proceed, emits a
// noisy id=1 init reply (which the probe must skip), then emits the
// id=2 response carrying the supplied rate-limits payload.
//
// rateLimitsJSON is the inner `result` object — pass an empty string to
// omit the result field entirely (modeling the wire's absent-data
// case). When errMsg is non-empty, the id=2 frame carries an `error`
// member instead of `result`.
func writeMockCodexAppServerScript(t *testing.T, tmpDir, rateLimitsJSON, errMsg string) string {
	t.Helper()
	path := filepath.Join(tmpDir, "mock-codex")

	var idTwoFrame string
	switch {
	case errMsg != "":
		idTwoFrame = `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"` + errMsg + `"}}`
	case rateLimitsJSON == "":
		idTwoFrame = `{"jsonrpc":"2.0","id":2,"result":{}}`
	default:
		idTwoFrame = `{"jsonrpc":"2.0","id":2,"result":` + rateLimitsJSON + `}`
	}

	script := "#!/bin/bash\n" +
		// Drain the three probe writes (initialize, initialized, rateLimits/read).
		`read -r _ || true` + "\n" +
		`read -r _ || true` + "\n" +
		`read -r _ || true` + "\n" +
		// Init reply (id=1) — probe must skip this and keep reading.
		`printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"v2"}}\n'` + "\n" +
		// A bare notification — probe must skip (no id).
		`printf '{"jsonrpc":"2.0","method":"some/notification","params":{}}\n'` + "\n" +
		// The matching id=2 response.
		`printf '%s\n' '` + idTwoFrame + `'` + "\n" +
		`exit 0` + "\n"

	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func TestProbeAccountWaitsBrieflyForOutOfOrderAccountIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-codex-out-of-order")
	script := "#!/bin/bash\n" +
		"read -r _ || true\nread -r _ || true\nread -r _ || true\nread -r _ || true\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"planType":"pro"}}}'` + "\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"chatgpt","email":"person@example.com","planType":"pro"}}}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: path})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.Email != "person@example.com" {
		t.Fatalf("Email = %q, want person@example.com", info.Email)
	}
}

func TestProbeAccountExtractsPlanType(t *testing.T) {
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{},"secondary":{}}}`,
		"")

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want %q", info.SubscriptionType, "pro")
	}
	if info.APIProvider != "openai" {
		t.Errorf("APIProvider: got %q, want openai", info.APIProvider)
	}
}

func TestProbeAccountSkipsNonMatchingFrames(t *testing.T) {
	// The init reply (id=1) and the bare notification baked into the
	// mock must not confuse the matcher — we read past them until
	// we see id=2.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"planType":"plus"}}`, "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "plus" {
		t.Errorf("SubscriptionType: got %q, want plus", info.SubscriptionType)
	}
}

func TestProbeAccountMissingPlanTypeReturnsZero(t *testing.T) {
	// Authenticated but the backend hasn't yet seen activity / hasn't
	// populated planType. Probe must return AccountInfo with empty
	// SubscriptionType (still APIProvider="openai") and no error so the
	// caller can distinguish "succeeded but no plan info" from a hard
	// failure.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"codex"}}`, "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "" {
		t.Errorf("SubscriptionType should be empty, got %q", info.SubscriptionType)
	}
	if info.APIProvider != "openai" {
		t.Errorf("APIProvider: got %q, want openai", info.APIProvider)
	}
}

func TestProbeAccountEmptyResultReturnsZero(t *testing.T) {
	// The wire could legitimately return result:{} (no rateLimits
	// container at all). Same outcome: zero SubscriptionType, no error.
	binary := writeMockCodexAppServerScript(t, t.TempDir(), "", "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "" {
		t.Errorf("SubscriptionType should be empty, got %q", info.SubscriptionType)
	}
	if info.APIProvider != "openai" {
		t.Errorf("APIProvider: got %q, want openai", info.APIProvider)
	}
}

func TestProbeAccountSurfacesError(t *testing.T) {
	// A JSON-RPC error reply must propagate as a typed Go error rather
	// than silently mapping to a zero-value account.
	binary := writeMockCodexAppServerScript(t, t.TempDir(), "", "auth required")

	_, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err == nil {
		t.Fatal("expected error for JSON-RPC error reply")
	}
	if !strings.Contains(err.Error(), "auth required") {
		t.Errorf("expected error message to surface message field; got %v", err)
	}
}

func TestProbeAccountBuildsAppServerArgs(t *testing.T) {
	args := buildProbeArgs()
	// Pin the exact args. Any future flag added here is suspect — the
	// probe is one-shot, no thread, no inference. A zero-token guarantee
	// equivalent to Claude's `--max-turns 0` requires the args list
	// stay minimal: nothing here should cause a thread to start, a
	// model call to fire, or persistent state to be written.
	want := []string{"app-server"}
	if len(args) != len(want) {
		t.Fatalf("probe args: got %v, want exactly %v", args, want)
	}
	for i, a := range args {
		if a != want[i] {
			t.Errorf("probe args[%d]: got %q, want %q", i, a, want[i])
		}
	}
}

func TestProbeAccountInvokesOnSnapshotCallback(t *testing.T) {
	// The probe must hand the rate-limit snapshot back to the caller via
	// OnSnapshot so app_codex_probe.go can emit it onto provider:usage —
	// otherwise the 5h/7d rings stay empty until the user runs a turn.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":91,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":7,"windowDurationMins":10080,"resetsAt":1776372636}}}`,
		"")

	var got []provider.RateLimitsSnapshot
	info, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary: binary,
		OnSnapshot: func(snap provider.RateLimitsSnapshot) {
			got = append(got, snap)
		},
	})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", info.SubscriptionType)
	}
	if len(got) != 1 {
		t.Fatalf("OnSnapshot fired %d times, want 1", len(got))
	}
	snap := got[0]
	if snap.Provider != string(provider.Codex) {
		t.Errorf("Provider: got %q, want %q", snap.Provider, provider.Codex)
	}
	if len(snap.Limits) != 2 {
		t.Fatalf("Limits len: got %d, want 2 (codex primary + secondary)", len(snap.Limits))
	}
	if snap.Limits[0].WindowMins != 300 || snap.Limits[0].UsedPercent != 91 {
		t.Errorf("Limits[0]: got %+v, want {WindowMins:300, UsedPercent:91}", snap.Limits[0])
	}
	if snap.Limits[1].WindowMins != 10080 || snap.Limits[1].UsedPercent != 7 {
		t.Errorf("Limits[1]: got %+v, want {WindowMins:10080, UsedPercent:7}", snap.Limits[1])
	}
}

// Realistic probe response wire shape: both `rateLimits` and
// `rateLimitsByLimitId` populated, with multiple buckets and distinct
// values across them. Mirrors the bug user's scenario (codex at 100%,
// spark at 46%); proves the probe retains every server-advertised bucket.
func TestProbeAccountInvokesOnSnapshotCallbackWithMultiBucket(t *testing.T) {
	// Mock binary's printf is line-oriented — keep the fixture on one
	// line so the NDJSON frame doesn't span multiple ReadLine calls.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":1,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1776372636}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1775803864},"secondary":{"usedPercent":91,"windowDurationMins":10080,"resetsAt":1776372636}},"spark":{"limitId":"spark","primary":{"usedPercent":46,"windowDurationMins":300,"resetsAt":1775809666},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1776396466}}}}`,
		"")

	var got []provider.RateLimitsSnapshot
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary: binary,
		OnSnapshot: func(snap provider.RateLimitsSnapshot) {
			got = append(got, snap)
		},
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("OnSnapshot fired %d times, want 1", len(got))
	}
	snap := got[0]
	if len(snap.Limits) != 4 {
		t.Fatalf("Limits len: got %d, want 4 (codex and spark buckets)", len(snap.Limits))
	}
	if snap.Limits[0].UsedPercent != 100 {
		t.Errorf("Limits[0].UsedPercent: got %v, want 100 (codex bucket from rateLimitsByLimitId, NOT spark's 46 nor top-level's 1)", snap.Limits[0].UsedPercent)
	}
	if snap.Limits[1].UsedPercent != 91 {
		t.Errorf("Limits[1].UsedPercent: got %v, want 91", snap.Limits[1].UsedPercent)
	}
	if snap.Limits[2].LimitID != "spark" || snap.Limits[2].UsedPercent != 46 {
		t.Errorf("Limits[2]: got %+v, want spark primary at 46", snap.Limits[2])
	}
	if snap.Limits[3].LimitID != "spark" || snap.Limits[3].UsedPercent != 22 {
		t.Errorf("Limits[3]: got %+v, want spark secondary at 22", snap.Limits[3])
	}
}

func TestProbeAccountSkipsOnSnapshotCallbackForEmptyResponse(t *testing.T) {
	// Authenticated but no rate-limit data (e.g. fresh account, backend
	// hasn't yet seen activity). AccountInfo still succeeds; OnSnapshot
	// must NOT fire — emitting an empty snapshot would clobber any
	// fresher value the rate-limit store already has from an active
	// session.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"codex","planType":"pro"}}`,
		"")

	var fired bool
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary: binary,
		OnSnapshot: func(provider.RateLimitsSnapshot) {
			fired = true
		},
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if fired {
		t.Fatal("OnSnapshot fired for a response with no rate-limit windows")
	}
}

func TestProbeAccountRetainsStandaloneDynamicBucket(t *testing.T) {
	// Server-advertised buckets are dynamic. A renamed or newly introduced
	// model allowance must remain visible without an app release.
	binary := writeMockCodexAppServerScript(t, t.TempDir(),
		`{"rateLimits":{"limitId":"spark","primary":{"usedPercent":46,"windowDurationMins":300,"resetsAt":1775809666},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1776396466}}}`,
		"")

	var got provider.RateLimitsSnapshot
	if _, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary: binary,
		OnSnapshot: func(snapshot provider.RateLimitsSnapshot) {
			got = snapshot
		},
	}); err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if len(got.Limits) != 2 {
		t.Fatalf("Limits len: got %d, want 2", len(got.Limits))
	}
	for _, limit := range got.Limits {
		if limit.LimitID != "spark" {
			t.Errorf("LimitID: got %q, want spark", limit.LimitID)
		}
	}
}

func TestProbeAccountReturnsErrorOnSpawnFailure(t *testing.T) {
	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: "/nonexistent/path/to/codex-12345"})
	if err == nil {
		t.Fatalf("expected spawn error, got info=%+v", info)
	}
	if !strings.Contains(err.Error(), "codex:") {
		t.Errorf("error should mention codex: got %v", err)
	}
}

func TestProbeAccountReturnsErrorWhenResponseMissing(t *testing.T) {
	// Simulate an app-server that exits before emitting the rateLimits
	// response. The probe must surface the EOF via a structured error
	// well below the configured Timeout.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "silent")
	script := "#!/bin/bash\nread -r _ || true\nread -r _ || true\nread -r _ || true\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write silent: %v", err)
	}

	const probeTimeout = 3 * time.Second
	start := time.Now()
	_, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:  path,
		Timeout: probeTimeout,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error when app-server exits without response")
	}
	if !strings.Contains(err.Error(), "codex:") {
		t.Errorf("error should mention codex: got %v", err)
	}
	if elapsed >= probeTimeout {
		t.Errorf("probe hit timeout path (%v) instead of EOF path", elapsed)
	}
}

func TestProbeAccountRespectsConfigTimeout(t *testing.T) {
	// Simulate an app-server that hangs after reading our requests.
	// Without the configured Timeout the probe would block app startup
	// indefinitely if codex misbehaves.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "slow")
	script := "#!/bin/bash\n" +
		"read -r _ || true\nread -r _ || true\nread -r _ || true\n" +
		"sleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write slow: %v", err)
	}

	start := time.Now()
	_, err := ProbeAccount(context.Background(), ProbeConfig{
		Binary:  path,
		Timeout: 150 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Tight bound: the configured timeout was 150ms, so anything above
	// ~1s indicates we hit the default-timeout path instead of the
	// configured one — meaning a regression where Timeout is ignored
	// would slip past an 8s assertion.
	if elapsed > time.Second {
		t.Errorf("probe took too long: %v (Timeout=150ms)", elapsed)
	}
}

// --- ProbeCache (mirror of the Claude cache contract) ---

func TestProbeCacheReturnsStored(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	info := provider.AccountInfo{SubscriptionType: "pro", APIProvider: "openai"}

	cache.Set("/usr/bin/codex", info)

	got, ok := cache.Get("/usr/bin/codex")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", got.SubscriptionType)
	}
}

func TestProbeCacheExpires(t *testing.T) {
	cache := NewProbeCache(10 * time.Millisecond)
	cache.Set("/usr/bin/codex", provider.AccountInfo{SubscriptionType: "team"})

	if _, ok := cache.Get("/usr/bin/codex"); !ok {
		t.Fatal("expected immediate cache hit")
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := cache.Get("/usr/bin/codex"); ok {
		t.Fatal("expected cache miss after TTL expired")
	}
}

func TestProbeCacheScopedPerBinary(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	cache.Set("/bin/a", provider.AccountInfo{SubscriptionType: "alpha"})
	cache.Set("/bin/b", provider.AccountInfo{SubscriptionType: "beta"})

	a, _ := cache.Get("/bin/a")
	b, _ := cache.Get("/bin/b")
	if a.SubscriptionType != "alpha" {
		t.Errorf("/bin/a: got %q, want alpha", a.SubscriptionType)
	}
	if b.SubscriptionType != "beta" {
		t.Errorf("/bin/b: got %q, want beta", b.SubscriptionType)
	}
}

// --- extractAccountInfoFromRateLimits unit tests ---

func TestExtractAccountInfoFromRateLimitsPopulatesPlanType(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"rateLimits": map[string]any{
			"limitId":  "codex",
			"planType": "pro",
			"primary":  map[string]any{},
		},
		"rateLimitsByLimitId": map[string]any{
			"codex":           map[string]any{"planType": "pro"},
			"codex_bengalfox": map[string]any{"planType": "spark"},
		},
	})

	info := extractAccountInfoFromRateLimits(payload)
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro (must read top-level rateLimits, NOT codex_bengalfox)", info.SubscriptionType)
	}
	if info.APIProvider != "openai" {
		t.Errorf("APIProvider: got %q, want openai", info.APIProvider)
	}
}

func TestExtractAccountInfoFromRateLimitsTreatsEmptyAsZero(t *testing.T) {
	got := extractAccountInfoFromRateLimits(nil)
	if got.SubscriptionType != "" {
		t.Errorf("nil payload SubscriptionType: got %q, want empty", got.SubscriptionType)
	}
	if got.APIProvider != "openai" {
		t.Errorf("nil payload APIProvider: got %q, want openai", got.APIProvider)
	}

	got = extractAccountInfoFromRateLimits([]byte(`{}`))
	if got.SubscriptionType != "" {
		t.Errorf("empty object SubscriptionType: got %q, want empty", got.SubscriptionType)
	}

	got = extractAccountInfoFromRateLimits([]byte(`not-json`))
	if got.SubscriptionType != "" {
		t.Errorf("non-JSON SubscriptionType: got %q, want empty", got.SubscriptionType)
	}
	if got.APIProvider != "openai" {
		t.Errorf("non-JSON APIProvider: got %q, want openai", got.APIProvider)
	}
}
