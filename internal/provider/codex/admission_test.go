package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
	"agent-overflow/internal/testutil/mockexec"
)

// admissionFakeScript is an app-server that answers initialize with
// userAgent and every other request with an empty result, and logs each
// method it receives to log. A client that skipped admission would
// therefore carry on past initialize and succeed.
func admissionFakeScript(userAgent, log string) string {
	return `#!/bin/bash
log='` + log + `'
while IFS= read -r line; do
  method=''
  if [[ $line =~ \"method\":\"([^\"]*)\" ]]; then method=${BASH_REMATCH[1]}; fi
  printf '%s\n' "$method" >> "$log"
  if [[ $line =~ \"id\":([0-9]+) ]]; then
    id=${BASH_REMATCH[1]}
    if [ "$method" = initialize ]; then
      printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"%s"}}\n' "$id" '` + userAgent + `'
    else
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
    fi
  fi
done
`
}

// TestEveryHandshakeRefusesAnUnsupportedCodex runs each client that
// completes its own initialize against an app-server below the minimum. Each
// must return the shared refusal, naming the executable and the version, and
// send nothing after initialize.
func TestEveryHandshakeRefusesAnUnsupportedCodex(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	const userAgent = "codex_cli_rs/0.153.4 (Ubuntu 24.04; x86_64) codex_cli_rs/0.153.4"
	clients := map[string]func(ctx context.Context, binary, workDir string) error{
		"sign-in": func(ctx context.Context, binary, workDir string) error {
			session, err := StartLogin(ctx, LoginConfig{
				Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir,
				Env: map[string]string{"CODEX_HOME": filepath.Join(workDir, "home")},
			})
			if session != nil {
				_ = session.Close()
			}
			return err
		},
		"account probe": func(ctx context.Context, binary, workDir string) error {
			_, err := ProbeAccount(ctx, ProbeConfig{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir})
			return err
		},
		"reset spend": func(ctx context.Context, binary, workDir string) error {
			cfg := spendConfig(binary, "key", "credit")
			cfg.WorkDir = workDir
			_, _, err := SpendReset(ctx, cfg)
			return err
		},
		"identity probe": func(ctx context.Context, binary, workDir string) error {
			_, err := ProbeIdentity(ctx, ProbeConfig{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir})
			return err
		},
		"account usage": func(ctx context.Context, binary, workDir string) error {
			_, err := (&AccountUsageFetcher{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir}).Fetch(ctx)
			return err
		},
		"mcp status": func(ctx context.Context, binary, workDir string) error {
			_, err := (&MCPStatusFetcher{Binary: binary, Cwd: workDir}).Fetch(ctx, mcpstatus.ProviderCodex)
			return err
		},
	}
	for name, run := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			log := filepath.Join(dir, "methods")
			binary := filepath.Join(dir, "codex")
			mockexec.Write(t, binary, admissionFakeScript(userAgent, log))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := run(ctx, binary, dir)

			var refusal *provider.CodexVersionError
			if !errors.As(err, &refusal) {
				t.Fatalf("error = %v, want a *provider.CodexVersionError", err)
			}
			want := (&provider.CodexVersionError{Path: binary, Version: "0.153.4", Raw: userAgent}).Error()
			if refusal.Error() != want {
				t.Fatalf("refusal = %q, want %q", refusal.Error(), want)
			}
			data, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "initialize" {
				t.Fatalf("methods sent = %q, want only initialize", got)
			}
		})
	}
}

// admitReply runs admitInitialize on a reply carrying userAgent.
func admitReply(t *testing.T, originator, userAgent string) (string, error) {
	t.Helper()
	result, err := json.Marshal(map[string]string{"userAgent": userAgent})
	if err != nil {
		t.Fatal(err)
	}
	return admitInitialize("/opt/codex", originator, result)
}

// TestAdmitInitializeReadsTheBuildAfterTheOriginator pins where the build
// is read: right after the originator this side started the process with.
// Upstream writes every field unescaped, so an originator, an os version
// or a terminal may each hold a slash, parentheses and a version of its
// own, and none of them is read as the build.
func TestAdmitInitializeReadsTheBuildAfterTheOriginator(t *testing.T) {
	t.Parallel()
	admitted := []struct{ originator, userAgent, want string }{
		{"Team/2.0.0 (CI)", "Team/2.0.0 (CI)/0.154.0 (Ubuntu 24.04; x86_64) xterm", "0.154.0"},
		{"agent_overflow", "agent_overflow/0.154.0 (Linux image/1.2.3 (OEM); x86_64) xterm", "0.154.0"},
		{"Engineering (nightly)", "Engineering (nightly)/0.154.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5.0 (agent_overflow; 0.1.0)", "0.154.0"},
		{"Company (CI)", "Company (CI)/tools 2.0.0/0.154.0 (Ubuntu 24.04; x86_64) dumb", ""},
		{"agent_overflow", providertest.CodexUserAgentFor("agent_overflow", "0.154.0+company.1"), "0.154.0+company.1"},
		{"agent_overflow", providertest.CodexUserAgentFor("agent_overflow", "0.160.2"), "0.160.2"},
		{"", "/0.154.0 (Linux 6.6; x86_64) dumb", "0.154.0"},
	}
	for _, tc := range admitted {
		got, err := admitReply(t, tc.originator, tc.userAgent)
		if tc.want == "" {
			// `tools 2.0.0/0.154.0` is no originator Codex was started with:
			// the reply starts with the originator, and the word after it is
			// not a version.
			if err == nil {
				t.Errorf("%q with originator %q admitted %q, want it unreadable", tc.userAgent, tc.originator, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q with originator %q = (%q, %v), want %q", tc.userAgent, tc.originator, got, err, tc.want)
		}
	}
}

// A reply that does not start with the expected originator came from a
// process whose environment a launcher changed. Its build is read only from
// the one position that is a slash, a version and " ("; two such positions
// are unreadable, refused with the capped reply and the executable named.
func TestAdmitInitializeFallsBackToTheOneCandidate(t *testing.T) {
	t.Parallel()
	got, err := admitReply(t, "agent_overflow", "wrapper/0.154.0 (Linux 6.6; x86_64) xterm (agent_overflow; 0.1.0)")
	if err != nil || got != "0.154.0" {
		t.Fatalf("one candidate = (%q, %v), want 0.154.0", got, err)
	}
	ambiguous := "Team/2.0.0 (CI)/0.154.0 (Ubuntu 24.04; x86_64) xterm" + strings.Repeat(" padding", 30)
	_, err = admitReply(t, "agent_overflow", ambiguous)
	var refusal *provider.CodexVersionError
	if !errors.As(err, &refusal) || refusal.Version != "" {
		t.Fatalf("two candidates = %v, want an unreadable refusal", err)
	}
	if refusal.Path != "/opt/codex" || refusal.Raw != ambiguous[:120]+"..." {
		t.Fatalf("refusal = %+v, want the executable and the reply capped at 120 bytes", refusal)
	}
	if !strings.Contains(refusal.Error(), "/opt/codex") {
		t.Fatalf("refusal %q does not name the executable", refusal.Error())
	}
}

// The build token goes through the one grammar: malformed versions are
// unreadable, a prerelease of the minimum is below it, and a bare
// originator, upstream's last fallback, carries no build.
func TestAdmitInitializeRefusesWhatTheGrammarRefuses(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"0.154.0-.", "0.154.0+", "01.154.0", "0.154", "dev-0.154.0", "v0.154.0"} {
		_, err := admitReply(t, "agent_overflow", providertest.CodexUserAgentFor("agent_overflow", token))
		var refusal *provider.CodexVersionError
		if !errors.As(err, &refusal) || refusal.Version != "" {
			t.Errorf("build %q = %v, want an unreadable refusal", token, err)
		}
	}
	_, err := admitReply(t, "agent_overflow", providertest.CodexUserAgentFor("agent_overflow", "0.154.0-rc.1"))
	var refusal *provider.CodexVersionError
	if !errors.As(err, &refusal) || refusal.Version != "0.154.0-rc.1" {
		t.Fatalf("0.154.0-rc.1 = %v, want refused as below the minimum", err)
	}
	for _, bare := range []string{"agent_overflow", "", "agent_overflow/0.154.0"} {
		if got, err := admitReply(t, "agent_overflow", bare); err == nil {
			t.Errorf("user agent %q admitted %q, want it unreadable", bare, got)
		}
	}
}

// The admitted build is an owned copy: a connection keeps it for its life
// and must not keep the reply it was cut from.
func TestAdmitInitializeKeepsAnOwnedBuild(t *testing.T) {
	t.Parallel()
	userAgent := providertest.CodexUserAgentFor("agent_overflow", "0.154.0") + strings.Repeat("x", 1<<16)
	got := appServerBuild(userAgent, "agent_overflow")
	if got != "0.154.0" {
		t.Fatalf("build = %q, want 0.154.0", got)
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(userAgent)))
	at := uintptr(unsafe.Pointer(unsafe.StringData(got)))
	if at >= start && at < start+uintptr(len(userAgent)) {
		t.Fatal("the admitted build points into the user agent")
	}
}

// The originator is the launch environment's override when set, otherwise
// the client name the handshake sends; an override no header can carry is
// Codex's default, and one that is not UTF-8 is unset to Codex.
func TestHandshakeOriginator(t *testing.T) {
	t.Parallel()
	params := codexInitializeParams("agent_overflow_probe", nil)
	env := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
	}
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{nil, "agent_overflow_probe"},
		{map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "Team (CI)"}, "Team (CI)"},
		{map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": ""}, ""},
		{map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "bad\x01name"}, "codex_cli_rs"},
		{map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "\xff"}, "agent_overflow_probe"},
	} {
		if got := handshakeOriginator(params, env(tc.env)); got != tc.want {
			t.Errorf("originator with env %q = %q, want %q", tc.env, got, tc.want)
		}
	}
	if providertest.CodexUserAgent != providertest.CodexUserAgentFor(provider.CodexClientOrigin, provider.MinimumCodexCLIVersion) {
		t.Fatalf("CodexUserAgent = %q, not the formatter's shape", providertest.CodexUserAgent)
	}
}

// A process launched with an originator override answers with it. The
// handshake reads the override from the environment the process was
// started with, so a session admits the build that follows it.
func TestDialReadsTheOriginatorOverrideFromItsLaunch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	mockexec.Write(t, binary, `#!/bin/bash
while IFS= read -r line; do
  if [[ $line =~ \"id\":([0-9]+) ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"%s/0.154.0 (Linux 6.6; x86_64) dumb"}}\n' "${BASH_REMATCH[1]}" "$CODEX_INTERNAL_ORIGINATOR_OVERRIDE"
  fi
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, ConnConfig{
		Binary: binary, WorkDir: dir, Reaper: procutiltest.Reaper(), ThreadID: "t",
		Env: map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "Team/2.0.0 (CI)"},
	})
	if err != nil {
		t.Fatalf("Dial with an originator override: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := conn.AppServerVersion(); got != "0.154.0" {
		t.Fatalf("version = %q, want 0.154.0", got)
	}
}
