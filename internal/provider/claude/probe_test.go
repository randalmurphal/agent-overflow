package claude

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

// --- Gap 2: zero-token subscription probe ---

// writeMockClaudeInitScript writes a shell script to tmpDir that mimics the
// Claude CLI during a `--max-turns 0` probe: it emits one system/init line
// to stdout carrying the provided account JSON, reads one line from stdin
// (the probe's user message), then exits cleanly.
func writeMockClaudeInitScript(t *testing.T, tmpDir, accountJSON string) string {
	t.Helper()
	path := filepath.Join(tmpDir, "mock-claude")
	script := "#!/bin/bash\n" +
		"printf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"probe-s1\",\"model\":\"claude-opus-4-7\",\"cwd\":\"/tmp\",\"tools\":[],\"claude_code_version\":\"2.0.0\"" +
		func() string {
			if accountJSON == "" {
				return ""
			}
			return ",\"account\":" + accountJSON
		}() + "}\\n'\n" +
		"read -r _ || true\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func TestProbeAccountExtractsSubscriptionType(t *testing.T) {
	binary := writeMockClaudeInitScript(t, t.TempDir(),
		`{"subscriptionType":"max_20x","tokenSource":"oauth","apiProvider":"anthropic"}`)

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "max_20x" {
		t.Errorf("SubscriptionType: got %q, want %q", info.SubscriptionType, "max_20x")
	}
	if info.TokenSource != "oauth" {
		t.Errorf("TokenSource: got %q, want %q", info.TokenSource, "oauth")
	}
	if info.APIProvider != "anthropic" {
		t.Errorf("APIProvider: got %q, want %q", info.APIProvider, "anthropic")
	}
	if info.Model != "claude-opus-4-7" {
		t.Errorf("Model: got %q, want %q", info.Model, "claude-opus-4-7")
	}
	if info.Version != "2.0.0" {
		t.Errorf("Version: got %q, want %q", info.Version, "2.0.0")
	}
}

func TestProbeAccountMissingAccountReturnsZero(t *testing.T) {
	// Non-Max accounts (and older CLIs) omit the account field entirely.
	// Probe must return a zero-value AccountInfo, not error.
	binary := writeMockClaudeInitScript(t, t.TempDir(), "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "" {
		t.Errorf("SubscriptionType should be empty, got %q", info.SubscriptionType)
	}
	// Session metadata still flows through.
	if info.Model != "claude-opus-4-7" {
		t.Errorf("Model: got %q, want claude-opus-4-7", info.Model)
	}
}

func TestProbeAccountBuildsMaxTurnsZeroArgs(t *testing.T) {
	// Zero-token guarantee depends on --max-turns 0 being passed to the CLI.
	args := buildProbeArgs()

	var hasMaxTurnsZero bool
	for i, arg := range args {
		if arg == "--max-turns" && i+1 < len(args) && args[i+1] == "0" {
			hasMaxTurnsZero = true
		}
	}
	if !hasMaxTurnsZero {
		t.Fatalf("probe args missing --max-turns 0: %v", args)
	}

	// Must include the minimal stream-json scaffolding.
	wantFlags := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	for _, want := range wantFlags {
		var found bool
		for _, arg := range args {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("probe args missing %q: %v", want, args)
		}
	}
}

func TestProbeAccountReturnsErrorOnSpawnFailure(t *testing.T) {
	info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: "/nonexistent/path/to/claude-12345"})
	if err == nil {
		t.Fatalf("expected spawn error, got info=%+v", info)
	}
	if !strings.Contains(err.Error(), "claude:") {
		t.Errorf("error should mention claude: got %v", err)
	}
}

func TestProbeAccountReturnsErrorWhenInitMissing(t *testing.T) {
	// Simulate a binary that exits before emitting a system/init message.
	// The probe must surface the EOF via a structured error — not hit the
	// configured Timeout fallback. Assert elapsed < Timeout so we're
	// explicitly verifying the EOF path beat the timeout path, rather than
	// hard-coding a wall-clock number that flakes on loaded runners.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "silent")
	// Read stdin and exit with no stdout output.
	if err := os.WriteFile(path, []byte("#!/bin/bash\nread -r _ || true\nexit 0\n"), 0755); err != nil {
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
		t.Fatal("expected error when CLI exits without init")
	}
	if !strings.Contains(err.Error(), "claude:") {
		t.Errorf("error should mention claude: got %v", err)
	}
	// If the EOF path worked, elapsed is bounded by process-spawn latency,
	// not the probe Timeout. Allow a generous margin for slow CI while
	// still catching the bug where Timeout becomes the backstop.
	if elapsed >= probeTimeout {
		t.Errorf("probe hit timeout path (%v) instead of EOF path", elapsed)
	}
}

func TestProbeAccountRespectsConfigTimeout(t *testing.T) {
	// Simulate a binary that blocks on stdin with no output. The probe's
	// internal Timeout must be the thing that unblocks readInitFromProc.
	// The script exits cleanly on stdin close, so no hard-kill is needed —
	// this keeps the test cooperative with Go's exec WaitDelay tracking.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "slow")
	// Read one line, wait, then exit. The probe's Timeout will fire
	// before the read completes most of the time — but if the read
	// completes, the script sleeps briefly without emitting anything.
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
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
	// After the ProbeAccount returns with a context-deadline error, the
	// defer proc.Close() closes stdin. The script exits cleanly.
	if elapsed > 8*time.Second {
		t.Errorf("probe took too long: %v", elapsed)
	}
}

// --- AccountInfo cache ---

func TestAccountInfoCacheReturnsStored(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	info := provider.AccountInfo{SubscriptionType: "max_5x"}

	cache.Set("/usr/bin/claude", info)

	got, ok := cache.Get("/usr/bin/claude")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.SubscriptionType != "max_5x" {
		t.Errorf("SubscriptionType: got %q, want max_5x", got.SubscriptionType)
	}
}

func TestAccountInfoCacheExpires(t *testing.T) {
	cache := NewProbeCache(10 * time.Millisecond)
	cache.Set("/usr/bin/claude", provider.AccountInfo{SubscriptionType: "team"})

	// Shortly before expiration — still a hit.
	if _, ok := cache.Get("/usr/bin/claude"); !ok {
		t.Fatal("expected immediate cache hit")
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := cache.Get("/usr/bin/claude"); ok {
		t.Fatal("expected cache miss after TTL expired")
	}
}

func TestAccountInfoCacheScopedPerBinary(t *testing.T) {
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

// --- extractAccountInfo helper ---

func TestExtractAccountInfoFromInitLine(t *testing.T) {
	init := map[string]any{
		"type":                "system",
		"subtype":             "init",
		"session_id":          "s1",
		"model":               "claude-opus-4-6",
		"cwd":                 "/tmp",
		"claude_code_version": "1.2.3",
		"account": map[string]any{
			"subscriptionType": "pro",
			"tokenSource":      "oauth",
			"apiProvider":      "anthropic",
		},
	}
	data, _ := json.Marshal(init)

	info, err := extractAccountInfoFromInit(data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if info.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType: got %q, want pro", info.SubscriptionType)
	}
	if info.TokenSource != "oauth" {
		t.Errorf("TokenSource: got %q, want oauth", info.TokenSource)
	}
	if info.APIProvider != "anthropic" {
		t.Errorf("APIProvider: got %q, want anthropic", info.APIProvider)
	}
	if info.Model != "claude-opus-4-6" {
		t.Errorf("Model: got %q, want claude-opus-4-6", info.Model)
	}
	if info.Version != "1.2.3" {
		t.Errorf("Version: got %q, want 1.2.3", info.Version)
	}
}
