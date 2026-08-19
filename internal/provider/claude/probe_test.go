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

// probeWorkDir is the absolute, project-free directory the probes run in.
// WorkDir is required and validated, so every ProbeConfig in this file
// carries one — see provider.ValidateProbeWorkDir. os.TempDir rather than
// a literal because filepath.IsAbs is OS-specific.
var probeWorkDir = os.TempDir()

// writeMockClaudeInitScript writes a shell script to tmpDir that mimics
// the Claude CLI during a probe: it reads ONE line from stdin (the
// probe's control_request{subtype:"initialize"}), then prints a
// control_response carrying the supplied accountJSON in
// `response.response.account`. A leading hook envelope is also
// emitted to mirror the live wire ordering observed during the spike,
// where SessionStart hook events arrive BEFORE the control_response.
//
// When accountJSON is empty, the response carries no `account` field —
// modeling the unauthenticated case where the CLI succeeds but has no
// subscription data to share. When `subtype` is non-empty and not
// "success", the response is shaped as an error reply and the probe
// must return a typed error.
func writeMockClaudeInitScript(t *testing.T, tmpDir, accountJSON, subtype, errMsg string) string {
	t.Helper()
	path := filepath.Join(tmpDir, "mock-claude")
	if subtype == "" {
		subtype = "success"
	}
	innerResponse := "{}"
	if subtype == "success" {
		if accountJSON == "" {
			innerResponse = "{}"
		} else {
			innerResponse = `{"account":` + accountJSON + `}`
		}
	}

	// JSON-escape the request_id we expect — the script just echoes the
	// wire shape; the probe matches on `response.request_id`.
	respLine := `{"type":"control_response","response":{"subtype":"` + subtype +
		`","request_id":"` + probeInitRequestID + `"`
	if subtype == "success" {
		respLine += `,"response":` + innerResponse
	} else if errMsg != "" {
		respLine += `,"error":"` + errMsg + `"`
	}
	respLine += `}}`

	script := "#!/bin/bash\n" +
		// Hook noise the live CLI emits before the control_response.
		`printf '{"type":"system","subtype":"hook_started","hook_event":"SessionStart"}\n'` + "\n" +
		`printf '{"type":"system","subtype":"hook_response","hook_event":"SessionStart","exit_code":0}\n'` + "\n" +
		`read -r _ || true` + "\n" +
		`printf '%s\n' '` + respLine + `'` + "\n" +
		`exit 0` + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func TestProbeAccountExtractsSubscriptionType(t *testing.T) {
	binary := writeMockClaudeInitScript(t, t.TempDir(),
		`{"subscriptionType":"Claude Max","tokenSource":"oauth","apiProvider":"firstParty"}`,
		"success", "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,
		Binary:  binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "Claude Max" {
		t.Errorf("SubscriptionType: got %q, want %q", info.SubscriptionType, "Claude Max")
	}
	if info.TokenSource != "oauth" {
		t.Errorf("TokenSource: got %q, want %q", info.TokenSource, "oauth")
	}
	if info.APIProvider != "firstParty" {
		t.Errorf("APIProvider: got %q, want %q", info.APIProvider, "firstParty")
	}
}

// TestProbeAccountExtractsSparseAccountShapes covers the two account
// objects a working CLI can return that carry almost nothing. Both are
// consequences of the CLI's account-metadata builder (verified against
// claude 2.1.219): it returns early unless the resolved apiProvider is
// "firstParty", and a firstParty profile login populates neither
// subscription nor tokenSource.
//
// These are the only signals `providerstatus.ClaudeUnauthenticated` has
// to work with on those setups, so the probe must not drop them.
func TestProbeAccountExtractsSparseAccountShapes(t *testing.T) {
	cases := []struct {
		name        string
		accountJSON string
		want        provider.AccountInfo
	}{
		{
			// Bedrock (and every other non-firstParty backend): the
			// builder bails before setting anything else.
			name:        "external credential backend",
			accountJSON: `{"apiProvider":"bedrock"}`,
			want:        provider.AccountInfo{APIProvider: "bedrock"},
		},
		{
			// firstParty + profile token source: email is the only
			// field that comes back.
			name:        "firstParty profile login",
			accountJSON: `{"email":"user@example.com","apiProvider":"firstParty"}`,
			want:        provider.AccountInfo{Email: "user@example.com", APIProvider: "firstParty"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binary := writeMockClaudeInitScript(t, t.TempDir(), tc.accountJSON, "success", "")

			info, err := ProbeAccount(context.Background(), ProbeConfig{
				WorkDir: probeWorkDir,
				Binary:  binary})
			if err != nil {
				t.Fatalf("ProbeAccount: %v", err)
			}
			if info != tc.want {
				t.Fatalf("AccountInfo = %+v, want %+v", info, tc.want)
			}
		})
	}
}

func TestProbeAccountSkipsHookNoiseBeforeResponse(t *testing.T) {
	// The probe reads the control_response specifically; intervening
	// system events (SessionStart hooks observed in the live spike)
	// must not confuse the matcher.
	binary := writeMockClaudeInitScript(t, t.TempDir(),
		`{"subscriptionType":"Claude Pro"}`,
		"success", "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,
		Binary:  binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "Claude Pro" {
		t.Errorf("SubscriptionType: got %q, want Claude Pro", info.SubscriptionType)
	}
}

func TestProbeAccountMissingAccountReturnsZero(t *testing.T) {
	// Unauthenticated / older CLI: the response succeeds but the inner
	// payload has no `account` field. Probe must return zero-value
	// AccountInfo, not error — `providerstatus.ClaudeUnauthenticated`
	// keys off the empty-fields signal.
	binary := writeMockClaudeInitScript(t, t.TempDir(), "", "success", "")

	info, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,
		Binary:  binary})
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if info.SubscriptionType != "" {
		t.Errorf("SubscriptionType should be empty, got %q", info.SubscriptionType)
	}
	if info.TokenSource != "" {
		t.Errorf("TokenSource should be empty, got %q", info.TokenSource)
	}
}

func TestProbeAccountSurfacesErrorSubtype(t *testing.T) {
	// A control_response with subtype:"error" and an `error` field
	// must propagate as a typed Go error — the probe is the line of
	// defense for "claude is broken at startup", not silent zero.
	binary := writeMockClaudeInitScript(t, t.TempDir(), "", "error", "auth expired")

	_, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,
		Binary:  binary})
	if err == nil {
		t.Fatal("expected error for non-success subtype")
	}
	if !strings.Contains(err.Error(), "auth expired") {
		t.Errorf("expected error message to surface error field; got %v", err)
	}
}

func TestProbeAccountBuildsMaxTurnsZeroArgs(t *testing.T) {
	// Zero-token guarantee depends on --max-turns 0 being passed to the
	// CLI even though the probe never sends a user turn — defense in
	// depth in case the process linger past our control_request reply.
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

	// --safe-mode keeps the probe from firing the user's hooks, plugins,
	// MCP servers, and CLAUDE.md discovery; unlike --bare it leaves OAuth
	// (and so the native token refresh) working normally.
	wantFlags := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--safe-mode"}
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
	info, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,
		Binary:  "/nonexistent/path/to/claude-12345"})
	if err == nil {
		t.Fatalf("expected spawn error, got info=%+v", info)
	}
	if !strings.Contains(err.Error(), "claude:") {
		t.Errorf("error should mention claude: got %v", err)
	}
}

func TestProbeAccountReturnsErrorWhenResponseMissing(t *testing.T) {
	// Simulate a binary that exits before emitting any control_response.
	// The probe must surface the EOF via a structured error well below
	// the configured Timeout so the unauthenticated banner can react
	// immediately on misconfigured environments.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "silent")
	if err := os.WriteFile(path, []byte("#!/bin/bash\nread -r _ || true\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write silent: %v", err)
	}

	const probeTimeout = 3 * time.Second
	start := time.Now()
	_, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,

		Binary:  path,
		Timeout: probeTimeout,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error when CLI exits without response")
	}
	if !strings.Contains(err.Error(), "claude:") {
		t.Errorf("error should mention claude: got %v", err)
	}
	if elapsed >= probeTimeout {
		t.Errorf("probe hit timeout path (%v) instead of EOF path", elapsed)
	}
}

func TestProbeAccountRespectsConfigTimeout(t *testing.T) {
	// Simulate a binary that blocks indefinitely after reading our
	// control_request. The probe's internal Timeout is what unblocks
	// readControlInitResponse; without it, the probe would hang the
	// whole app at startup if Claude misbehaves.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "slow")
	script := "#!/bin/bash\n" +
		"read -r _ || true\n" +
		"sleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write slow: %v", err)
	}

	start := time.Now()
	_, err := ProbeAccount(context.Background(), ProbeConfig{
		WorkDir: probeWorkDir,

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

// --- AccountInfo cache ---

func TestAccountInfoCacheReturnsStored(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	info := provider.AccountInfo{SubscriptionType: "max_5x"}

	cache.Set(provider.ProbeCacheKey{Binary: "/usr/bin/claude", WorkDir: probeWorkDir}, info)

	got, ok := cache.Get(provider.ProbeCacheKey{Binary: "/usr/bin/claude", WorkDir: probeWorkDir})
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.SubscriptionType != "max_5x" {
		t.Errorf("SubscriptionType: got %q, want max_5x", got.SubscriptionType)
	}
}

func TestAccountInfoCacheExpires(t *testing.T) {
	cache := NewProbeCache(10 * time.Millisecond)
	cache.Set(provider.ProbeCacheKey{Binary: "/usr/bin/claude", WorkDir: probeWorkDir}, provider.AccountInfo{SubscriptionType: "team"})

	if _, ok := cache.Get(provider.ProbeCacheKey{Binary: "/usr/bin/claude", WorkDir: probeWorkDir}); !ok {
		t.Fatal("expected immediate cache hit")
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := cache.Get(provider.ProbeCacheKey{Binary: "/usr/bin/claude", WorkDir: probeWorkDir}); ok {
		t.Fatal("expected cache miss after TTL expired")
	}
}

func TestAccountInfoCacheScopedPerBinary(t *testing.T) {
	cache := NewProbeCache(5 * time.Minute)
	cache.Set(provider.ProbeCacheKey{Binary: "/bin/a", WorkDir: probeWorkDir}, provider.AccountInfo{SubscriptionType: "alpha"})
	cache.Set(provider.ProbeCacheKey{Binary: "/bin/b", WorkDir: probeWorkDir}, provider.AccountInfo{SubscriptionType: "beta"})

	a, _ := cache.Get(provider.ProbeCacheKey{Binary: "/bin/a", WorkDir: probeWorkDir})
	b, _ := cache.Get(provider.ProbeCacheKey{Binary: "/bin/b", WorkDir: probeWorkDir})
	if a.SubscriptionType != "alpha" {
		t.Errorf("/bin/a: got %q, want alpha", a.SubscriptionType)
	}
	if b.SubscriptionType != "beta" {
		t.Errorf("/bin/b: got %q, want beta", b.SubscriptionType)
	}
}

// --- extractAccountInfoFromInitResponse helper ---

func TestExtractAccountInfoFromInitResponsePopulatesAllFields(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"commands": []any{},
		"account": map[string]any{
			"email":            "user@example.com",
			"organization":     "User's Org",
			"subscriptionType": "Claude Max",
			"tokenSource":      "oauth",
			"apiProvider":      "firstParty",
		},
		"pid": 12345,
	})

	info, err := extractAccountInfoFromInitResponse(payload)
	if err != nil {
		t.Fatalf("extractAccountInfoFromInitResponse: %v", err)
	}
	if info.SubscriptionType != "Claude Max" {
		t.Errorf("SubscriptionType: got %q, want Claude Max", info.SubscriptionType)
	}
	if info.TokenSource != "oauth" {
		t.Errorf("TokenSource: got %q, want oauth", info.TokenSource)
	}
	if info.APIProvider != "firstParty" {
		t.Errorf("APIProvider: got %q, want firstParty", info.APIProvider)
	}
	if info.OrgName != "User's Org" {
		t.Errorf("OrgName: got %q, want User's Org", info.OrgName)
	}
	if info.OrgID != "" {
		t.Errorf("OrgID: got %q, want blank — the initialize wire carries no uuid", info.OrgID)
	}
}

func TestExtractAccountInfoFromInitResponseTreatsEmptyAsZero(t *testing.T) {
	got, err := extractAccountInfoFromInitResponse(nil)
	if err != nil || got != (provider.AccountInfo{}) {
		t.Errorf("nil payload: got %+v, %v, want zero, nil", got, err)
	}
	got, err = extractAccountInfoFromInitResponse([]byte(`{"commands":[]}`))
	if err != nil || got != (provider.AccountInfo{}) {
		t.Errorf("payload without account: got %+v, %v, want zero, nil", got, err)
	}
}

// TestExtractAccountInfoFromInitResponseFailsLoudOnUnreadablePayload pins the
// one wrong answer this decoder can give. A zero AccountInfo means "nobody is
// logged in" to every consumer — the login banner, the rate-limit refresh, the
// OAuth rotation path — so a payload we could not read must be an error rather
// than a confident denial.
func TestExtractAccountInfoFromInitResponseFailsLoudOnUnreadablePayload(t *testing.T) {
	if _, err := extractAccountInfoFromInitResponse([]byte(`not-json`)); err == nil {
		t.Fatal("non-JSON payload: got nil error, want a decode failure")
	}
	if _, err := extractAccountInfoFromInitResponse([]byte(`{"account":"nope"}`)); err == nil {
		t.Fatal("account of the wrong type: got nil error, want a decode failure")
	}
}

// TestProbeAccountRequiresWorkDir proves the requirement is enforced by the
// probe itself, not by caller discipline: a perfectly good binary that would
// answer the handshake is still refused when the caller did not say which
// directory it is asking about. Before this, every production caller passed
// ProbeConfig{Binary: binary} and silently inherited the app's launch cwd.
func TestProbeAccountRequiresWorkDir(t *testing.T) {
	binary := writeMockClaudeInitScript(t, t.TempDir(),
		`{"subscriptionType":"Claude Max"}`, "success", "")

	for _, workDir := range []string{"", "relative/dir"} {
		info, err := ProbeAccount(context.Background(), ProbeConfig{Binary: binary, WorkDir: workDir})
		if err == nil {
			t.Fatalf("WorkDir %q: expected refusal, got %+v", workDir, info)
		}
		if !strings.Contains(err.Error(), "WorkDir") {
			t.Errorf("WorkDir %q: error %q should name the missing field", workDir, err)
		}
		if info != (provider.AccountInfo{}) {
			t.Errorf("WorkDir %q: refused probe returned %+v, want zero value", workDir, info)
		}
	}
}
