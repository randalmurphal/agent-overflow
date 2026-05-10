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
// mimics `codex app-server` during a probe. The probe sends three
// NDJSON lines (initialize request, initialized notification,
// account/rateLimits/read request); the script reads them, emits a
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
