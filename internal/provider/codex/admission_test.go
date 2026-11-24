package codex

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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
// completes its own initialize against a stock app-server below the
// minimum, which answers with the client's own name. Each must return the
// shared refusal, naming the executable and the version, and send nothing
// after initialize.
func TestEveryHandshakeRefusesAnUnsupportedCodex(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	clients := map[string]struct {
		name string
		run  func(ctx context.Context, binary, workDir string) error
	}{
		"sign-in": {provider.CodexLoginClientName, func(ctx context.Context, binary, workDir string) error {
			session, err := StartLogin(ctx, LoginConfig{
				Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir,
				Env: map[string]string{"CODEX_HOME": filepath.Join(workDir, "home")},
			})
			if session != nil {
				_ = session.Close()
			}
			return err
		}},
		"account probe": {provider.CodexProbeClientName, func(ctx context.Context, binary, workDir string) error {
			_, err := ProbeAccount(ctx, ProbeConfig{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir})
			return err
		}},
		"reset spend": {provider.CodexProbeClientName, func(ctx context.Context, binary, workDir string) error {
			cfg := spendConfig(binary, "key", "credit")
			cfg.WorkDir = workDir
			_, _, err := SpendReset(ctx, cfg)
			return err
		}},
		"identity probe": {provider.CodexProbeClientName, func(ctx context.Context, binary, workDir string) error {
			_, err := ProbeIdentity(ctx, ProbeConfig{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir})
			return err
		}},
		"account usage": {provider.CodexUsageClientName, func(ctx context.Context, binary, workDir string) error {
			_, err := (&AccountUsageFetcher{Reaper: procutiltest.Reaper(), Binary: binary, WorkDir: workDir}).Fetch(ctx)
			return err
		}},
		"mcp status": {provider.CodexMCPStatusClientName, func(ctx context.Context, binary, workDir string) error {
			_, err := (&MCPStatusFetcher{Binary: binary, Cwd: workDir}).Fetch(ctx, mcpstatus.ProviderCodex)
			return err
		}},
		"session": {provider.CodexClientOrigin, func(ctx context.Context, binary, workDir string) error {
			conn, err := Dial(ctx, ConnConfig{Binary: binary, WorkDir: workDir, Reaper: procutiltest.Reaper(), ThreadID: "t"})
			if conn != nil {
				_ = conn.Close()
			}
			return err
		}},
	}
	for label, client := range clients {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			log := filepath.Join(dir, "methods")
			binary := filepath.Join(dir, "codex")
			userAgent := providertest.FormatCodexUserAgent("0.153.4", client.name, client.name, provider.CodexClientVersion)
			mockexec.Write(t, binary, admissionFakeScript(userAgent, log))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := client.run(ctx, binary, dir)

			var refusal *provider.CodexVersionError
			if !errors.As(err, &refusal) {
				t.Fatalf("error = %v, want a *provider.CodexVersionError", err)
			}
			want := "Codex v0.153.4 (" + binary + ") is installed; Agent Overflow needs v0.154.0 or newer. Update Codex."
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

// A stock reply is read exactly: the build right after the originator this
// side launched the process with, checked against the minimum. Below it is
// refused naming the executable; at and above it, and with build metadata,
// it is admitted with the build recorded. A prerelease of the minimum orders
// below it. An originator that holds a slash, parentheses or a version of
// its own is read the same way, because the reader knows it.
func TestAdmitInitializeReadsTheBuildAfterTheOriginator(t *testing.T) {
	t.Parallel()
	stock := func(originator, build string) string {
		return providertest.FormatCodexUserAgent(build, originator, provider.CodexClientOrigin, provider.CodexClientVersion)
	}
	for _, tc := range []struct{ originator, userAgent, want string }{
		{"agent_overflow", stock("agent_overflow", "0.154.0"), "0.154.0"},
		{"agent_overflow", stock("agent_overflow", "0.160.2"), "0.160.2"},
		{"agent_overflow", stock("agent_overflow", "0.154.0+company.1"), "0.154.0+company.1"},
		{"Team/2.0.0 (CI)", stock("Team/2.0.0 (CI)", "0.154.0"), "0.154.0"},
		{"Engineering (nightly)", stock("Engineering (nightly)", "0.155.1"), "0.155.1"},
		{"agent_overflow", "agent_overflow/0.154.0 (Linux image/1.2.3 (OEM); x86_64) xterm", "0.154.0"},
		// A launcher that names itself AO's client name and a version: its
		// version is what this reader reads, as on main.
		{"agent_overflow", "agent_overflow/1.0.0 (CI)/0.153.4 (Linux 6.6; x86_64) dumb (agent_overflow; 0.1.0)", "1.0.0"},
	} {
		got, err := admitReply(t, tc.originator, tc.userAgent)
		if err != nil || got != tc.want {
			t.Errorf("%q with originator %q = (%q, %v), want %q", tc.userAgent, tc.originator, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ originator, build string }{
		{"agent_overflow", "0.153.4"},
		{"agent_overflow", "0.154.0-alpha.2"},
		{"agent_overflow", "0.154.0-rc.1"},
		{"Team/2.0.0 (CI)", "0.153.4"},
	} {
		_, err := admitReply(t, tc.originator, stock(tc.originator, tc.build))
		var refusal *provider.CodexVersionError
		if !errors.As(err, &refusal) || refusal.Version != tc.build || refusal.Path != "/opt/codex" {
			t.Errorf("build %s with originator %q = %v, want refused below the minimum, naming /opt/codex", tc.build, tc.originator, err)
		}
	}
}

// A reply that does not start with the originator this side launched,
// then a whole build and " (", is not read: the process is admitted, as
// main admits it, with no recorded build, and the reply is logged once,
// bounded, with the executable. Serial: it captures the global logger.
func TestAdmitInitializeAdmitsAReplyItCannotRead(t *testing.T) {
	var logged strings.Builder
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)
	long := "codex_cli_rs/0.153.4 (Linux " + strings.Repeat("x", 4096) + "; x86_64) dumb"
	for _, userAgent := range []string{
		// A launcher's own originator after AO's name, or in its place.
		"agent_overflow/team/0.154.0 (Linux 6.6; x86_64) dumb (agent_overflow; 0.1.0)",
		"team/0.154.0 (Linux 6.6; x86_64) iTerm.app/3.5.0 (agent_overflow; 0.1.0)",
		"wrapper/0.154.0 (Mac OS 15.1; arm64) iTerm.app/3.5.0 (agent_overflow; 0.1.0)",
		// A stock Codex old enough to start with its default originator.
		"codex_cli_rs/0.153.4 (Ubuntu 24.04; x86_64) dumb",
		// Upstream's last fallback, and no user agent at all.
		"agent_overflow",
		"",
		// The originator, then a word that is not a whole build.
		"agent_overflow/0.154 (Linux 6.6; x86_64) dumb (agent_overflow; 0.1.0)",
		"agent_overflow/v0.154.0 (Linux 6.6; x86_64) dumb (agent_overflow; 0.1.0)",
		"agent_overflow/0.154.0-. (Linux 6.6; x86_64) dumb (agent_overflow; 0.1.0)",
		"agent_overflow/0.154.0",
		"agent_overflow/0.153.4 dumb",
		long,
	} {
		logged.Reset()
		got, err := admitReply(t, "agent_overflow", userAgent)
		if err != nil || got != "" {
			t.Errorf("%.80q = (%q, %v), want admitted with no build", userAgent, got, err)
			continue
		}
		var lines []string
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, "admitted without a version check") {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "/opt/codex") {
			t.Errorf("%.80q logged %q, want one line naming /opt/codex", userAgent, logged.String())
			continue
		}
		if len(lines[0]) > 400 {
			t.Errorf("%.80q logged %d bytes, want the reply bounded", userAgent, len(lines[0]))
		}
	}
}

// The admitted build is an owned copy: a connection keeps it for its life
// and must not keep the reply it was cut from.
func TestAdmitInitializeKeepsAnOwnedBuild(t *testing.T) {
	t.Parallel()
	userAgent := providertest.FormatCodexUserAgent("0.154.0", "agent_overflow", "agent_overflow", "0.1.0") + strings.Repeat("x", 1<<16)
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
	if providertest.CodexUserAgent != providertest.FormatCodexUserAgent(provider.MinimumCodexCLIVersion,
		provider.CodexClientOrigin, provider.CodexClientOrigin, provider.CodexClientVersion) {
		t.Fatalf("CodexUserAgent = %q, not the formatter's shape", providertest.CodexUserAgent)
	}
}

// A process launched with an originator override answers with it. The
// handshake reads the override from the environment the process was
// started with, so a session reads exactly the build that follows it,
// including after an override that holds " (": a supported one is admitted
// with its build and an older one is refused. The fake answers with the
// override only when its launch carries it, and as a stock 0.153.4 session
// otherwise.
func TestDialReadsTheOriginatorOverrideFromItsLaunch(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	const override = "Team/2.0.0 (CI)"
	for _, build := range []string{"0.154.0", "0.153.4"} {
		t.Run(build, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			binary := filepath.Join(dir, "codex")
			overridden := providertest.FormatCodexUserAgent(build, override, provider.CodexClientOrigin, provider.CodexClientVersion)
			stock := providertest.FormatCodexUserAgent("0.153.4", provider.CodexClientOrigin, provider.CodexClientOrigin, provider.CodexClientVersion)
			mockexec.Write(t, binary, `#!/bin/bash
ua='`+stock+`'
if [ "$CODEX_INTERNAL_ORIGINATOR_OVERRIDE" = '`+override+`' ]; then ua='`+overridden+`'; fi
while IFS= read -r line; do
  if [[ $line =~ \"id\":([0-9]+) ]]; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"%s"}}\n' "${BASH_REMATCH[1]}" "$ua"
  fi
done
`)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := Dial(ctx, ConnConfig{
				Binary: binary, WorkDir: dir, Reaper: procutiltest.Reaper(), ThreadID: "t",
				Env: map[string]string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE": override},
			})
			if build == "0.153.4" {
				var refusal *provider.CodexVersionError
				if !errors.As(err, &refusal) || refusal.Version != build || refusal.Path != binary {
					t.Fatalf("Dial = %v, want the version refusal naming %s", err, binary)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dial with an originator override: %v", err)
			}
			defer func() { _ = conn.Close() }()
			if got := conn.AppServerVersion(); got != build {
				t.Fatalf("version = %q, want %s", got, build)
			}
		})
	}
}
