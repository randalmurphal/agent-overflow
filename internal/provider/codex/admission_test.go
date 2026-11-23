package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
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
