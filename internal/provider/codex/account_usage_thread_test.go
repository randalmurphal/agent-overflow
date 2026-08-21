package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"strings"

	"agent-overflow/internal/provider"
)

// threadUsageFakeScript builds a fake app-server that answers initialize with
// the given userAgent (which is where a live process states its build), starts
// a thread with the given id, and answers `account/usage/read` with
// usageResult — capturing the request frame so a test can assert the params.
//
// usageResult is spliced in verbatim so a test can produce a result body, an
// error body, or nothing at all.
func threadUsageFakeScript(t *testing.T, userAgent, threadID, usageReply, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"account/usage/read"'; then
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,%s}"
    fi
done
`,
		bashJSON(fmt.Sprintf(`{"userAgent":%q}`, userAgent)),
		bashJSON(fmt.Sprintf(`{"thread":{"id":%q}}`, threadID)),
		capturePath,
		bashJSON(usageReply),
	)

	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// bashJSON escapes a JSON literal for embedding inside the fake app-server's
// double-quoted `echo`. Written once rather than by hand at each call site:
// a mis-escaped brace turns into a bash syntax error whose only symptom is an
// initialize timeout, which reads as a bug in the code under test.
func bashJSON(literal string) string {
	return strings.ReplaceAll(literal, `"`, `\"`)
}

func newThreadUsageSession(t *testing.T, binary string) *Session {
	t.Helper()
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  binary,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSession_ReadThreadUsage_PresentEstimate is the whole feature on the
// happy path: a 0.149 app-server, a thread-scoped request carrying the ROOT
// codex thread id, and a USD figure that comes back verbatim in micros.
func TestSession_ReadThreadUsage_PresentEstimate(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{},"dailyUsageBuckets":null,"threadUsage":{` +
		`"threadId":"codex-thread-usage","estimatedUsageCreditsMicros":4200000,` +
		`"estimatedUsageUsdMicros":137500,"groups":[{"model":"gpt-5.6-codex",` +
		`"reasoningEffort":"high","estimatedUsageCreditsMicros":4200000,"outputTokens":9001}]}}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	if got := s.AppServerVersion(); got != "0.149.0" {
		t.Fatalf("AppServerVersion() = %q, want 0.149.0", got)
	}

	usage, err := s.ReadThreadUsage(context.Background())
	if err != nil {
		t.Fatalf("ReadThreadUsage: %v", err)
	}
	usd, ok := usage.USD()
	if !ok {
		t.Fatal("USD() reported no figure, want one")
	}
	if usd != 0.1375 {
		t.Fatalf("USD() = %v, want 0.1375", usd)
	}
	if usage.CreditsMicros != 4200000 {
		t.Fatalf("CreditsMicros = %d, want 4200000", usage.CreditsMicros)
	}
	if usage.ThreadID != "codex-thread-usage" {
		t.Fatalf("ThreadID = %q, want the thread that was asked about", usage.ThreadID)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode captured request %q: %v", raw, err)
	}
	if frame.Method != accountUsageMethod {
		t.Fatalf("request method = %q, want %q", frame.Method, accountUsageMethod)
	}
	if frame.Params.ThreadID != "codex-thread-usage" {
		t.Fatalf("request threadId = %q, want the root codex thread id", frame.Params.ThreadID)
	}
}

// TestSession_ReadThreadUsage_NullThreadUsage covers the answer upstream gives
// when the backend refuses the thread with 403/404: a successful response whose
// `threadUsage` is null. It is a STATE answer — the caller keeps its fallback —
// so it must resolve to ErrThreadUsageUnavailable rather than an error a caller
// would log every turn.
func TestSession_ReadThreadUsage_NullThreadUsage(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{},"dailyUsageBuckets":null,"threadUsage":null}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.148.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
}

// TestSession_ReadThreadUsage_AbsentThreadUsage is the same conclusion reached
// from a response that omits the key entirely — the shape a future codex could
// answer with, since upstream marks the field `#[serde(default)]`.
func TestSession_ReadThreadUsage_AbsentThreadUsage(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{},"dailyUsageBuckets":null}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.148.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
}

// TestSession_ReadThreadUsage_CreditsOnly pins the third absence: the backend
// priced the thread, but only in credits. `estimatedUsageUsdMicros` is
// `Option<i64>` upstream, and a credit figure is not a dollar figure — so this
// is unavailable too, and the rate table stays.
func TestSession_ReadThreadUsage_CreditsOnly(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{},"threadUsage":{"threadId":"codex-thread-usage",` +
		`"estimatedUsageCreditsMicros":900000,"estimatedUsageUsdMicros":null,"groups":[]}}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.148.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
}

// TestSession_ReadThreadUsage_OldVersionSendsNothing is the version gate. On
// 0.147 the method's params are `Option<()>`, so a `{threadId}` request is a
// guaranteed invalid-params error — the gate has to refuse BEFORE the write,
// not learn from the failure. The absent capture file is the assertion: no
// request left the client.
func TestSession_ReadThreadUsage_OldVersionSendsNothing(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{}}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.147.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Fatal("a request reached the app-server on a pre-0.148 build; the gate must refuse before writing")
	}
}

// TestSession_ReadThreadUsage_UnknownVersionSendsNothing pins the fail-closed
// direction for a handshake that stated no parseable version at all — every
// existing fake app-server in this package answers initialize with `{}`, so
// this is not a hypothetical shape.
func TestSession_ReadThreadUsage_UnknownVersionSendsNothing(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"summary":{}}`
	binary := threadUsageFakeScript(t, "", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	if got := s.AppServerVersion(); got != "" {
		t.Fatalf("AppServerVersion() = %q, want empty", got)
	}
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Fatal("a request reached the app-server on an unknown build; the gate must fail closed")
	}
}

// TestSession_ReadThreadUsage_RPCError covers a real JSON-RPC failure. The
// backend errored (not a 403/404, which upstream converts to a null field), so
// this must NOT resolve to ErrThreadUsageUnavailable — a caller should see it.
func TestSession_ReadThreadUsage_RPCError(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"error":{"code":-32603,"message":"failed to fetch thread usage: upstream 500"}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.149.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if err == nil {
		t.Fatal("ReadThreadUsage returned no error for a JSON-RPC failure")
	}
	if errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("a backend failure was classified as a state answer: %v", err)
	}
}

// TestSession_ReadThreadUsage_AuthRefusalIsAState pins the one RPC error that
// IS a state answer: an API-key login has no billing route at all, and
// upstream refuses it with a generic invalid_request whose message is the only
// distinguishing mark.
func TestSession_ReadThreadUsage_AuthRefusalIsAState(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"error":{"code":-32602,"message":"chatgpt authentication required to read token usage"}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.149.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if !errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("ReadThreadUsage error = %v, want ErrThreadUsageUnavailable", err)
	}
}

// TestSession_ReadThreadUsage_ThreadIDMismatchIsAFault guards the one way an
// estimate could be attributed to the wrong thread. The echoed id is the only
// evidence available, so a mismatch is a wire fault the caller must see — NOT
// a state answer that silently keeps the fallback forever.
func TestSession_ReadThreadUsage_ThreadIDMismatchIsAFault(t *testing.T) {
	capture := t.TempDir() + "/usage-request.json"
	reply := `"result":{"threadUsage":{"threadId":"some-other-thread",` +
		`"estimatedUsageCreditsMicros":1,"estimatedUsageUsdMicros":1,"groups":[]}}`
	binary := threadUsageFakeScript(t, "codex_cli_rs/0.149.0 (Linux)", "codex-thread-usage", reply, capture)

	s := newThreadUsageSession(t, binary)
	_, err := s.ReadThreadUsage(context.Background())
	if err == nil {
		t.Fatal("ReadThreadUsage accepted an estimate for a different thread")
	}
	if errors.Is(err, ErrThreadUsageUnavailable) {
		t.Fatalf("a thread-id mismatch was classified as a state answer: %v", err)
	}
}

// TestParseAppServerVersion pins the userAgent parse. The version is the token
// between the first `/` and the following space; the OS name, OS version and
// architecture that follow all carry digits, so a loose first-semver-token
// scan over the whole string is exactly what this must not do.
func TestParseAppServerVersion(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"real 0.149", "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) codex_cli_rs/0.149.0", "0.149.0"},
		{"two-segment version normalizes", "codex_cli_rs/0.149 (Linux)", "0.149.0"},
		{"prerelease survives", "codex_cli_rs/0.150.0-alpha.3 (Linux)", "0.150.0-alpha.3"},
		{"different originator", "codex_vscode/0.148.0 (Darwin 15.1; arm64)", "0.148.0"},
		{"no slash", "codex_cli_rs 0.149.0", ""},
		{"no version token", "codex_cli_rs/ (Linux 6.6)", ""},
		{"empty", "", ""},
		// The failure this parser exists to prevent: an OS version leading the
		// string must never be mistaken for the build.
		{"os version after a versionless originator", "codex/x (Ubuntu 24.04; x86_64)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAppServerVersion(tc.userAgent); got != tc.want {
				t.Fatalf("parseAppServerVersion(%q) = %q, want %q", tc.userAgent, got, tc.want)
			}
		})
	}
}

// TestThreadUsageFloorIsAboveTheProviderFloor states the relationship between
// the two version numbers in play: the package-wide launch floor admits
// binaries this method does not exist on, which is exactly why the per-method
// gate has to exist. If a future release raises the provider floor past
// 0.148, this gate becomes dead code and should be deleted rather than left to
// rot.
func TestThreadUsageFloorIsAboveTheProviderFloor(t *testing.T) {
	if provider.CodexCLIVersionAtLeast("0.143.0", threadUsageMinimumCodexVersion) {
		t.Fatalf("the provider launch floor now covers %s; the per-method gate is dead code",
			threadUsageMinimumCodexVersion)
	}
}
