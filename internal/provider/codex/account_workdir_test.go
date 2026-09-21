package codex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider/codex"
)

func TestAccountProcessesIsolateProjectConfiguration(t *testing.T) {
	for _, operation := range []string{"usage", "identity", "account-usage", "transfer", "login", "login-handshake-failure", "spawn-failure", "cancelled"} {
		t.Run(operation, func(t *testing.T) {
			kerneltest.IsolateSpawns(t)
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			canonical := filepath.Join(parent, ".codex")
			selected := t.TempDir()
			for _, dir := range []string{canonical, selected} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("notify = [\"user-setting\"]\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(t.TempDir(), "cwd")
			binary := filepath.Join(t.TempDir(), "mock-account")
			script := `#!/bin/bash
printf '%s' "$PWD" > "$AO_TEST_ACCOUNT_CWD"
case "$*" in *'project_root_markers=[]'*) ;; *) exit 7 ;; esac
if [ -e .codex/config.toml ]; then exit 8; fi
if ! grep -q 'user-setting' "$CODEX_HOME/config.toml"; then exit 9; fi
while IFS= read -r line; do
 case "$line" in
  *'"method":"initialize"'*)
    if [ "$AO_TEST_REJECT_INITIALIZE" = 1 ]; then
      printf '%s\n' '{"id":1,"error":{"code":-32603,"message":"test handshake rejection"}}'
    else
      printf '%s\n' '{"id":1,"result":{"userAgent":"codex_cli_rs/0.151.0 (fake)"}}'
    fi ;;

  *'"method":"account/read"'*) printf '{"id":%s,"result":{"account":{"type":"chatgpt","email":"person@example.test"}}}\n' "${AO_TEST_ACCOUNT_READ_ID:-3}" ;;
  *'"method":"account/usage/read"'*) printf '%s\n' '{"id":2,"result":{"summary":{"lifetimeTokens":123},"dailyUsageBuckets":[]}}' ;;
  *'"method":"account/rateLimits/read"'*) printf '%s\n' '{"id":2,"result":{"rateLimits":{"planType":"pro"}}}' ;;
 esac
done
`
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := codex.ProbeConfig{Binary: binary, WorkDir: parent, Env: map[string]string{"CODEX_HOME": selected, "AO_TEST_ACCOUNT_CWD": logPath}}
			switch operation {
			case "usage":
				_, err = codex.ProbeAccount(t.Context(), cfg)
			case "identity":
				_, err = codex.ProbeIdentity(t.Context(), cfg)
			case "account-usage":
				fetcher := codex.AccountUsageFetcher{Binary: binary, WorkDir: parent, Env: cfg.Env}
				var usage codex.AccountUsage
				usage, err = fetcher.Fetch(t.Context())
				if err == nil && (usage.LifetimeTokens == nil || *usage.LifetimeTokens != 123) {
					t.Fatalf("usage response lost: %+v", usage)
				}
			case "transfer":
				cfg.Env["AO_TEST_ACCOUNT_READ_ID"] = "2"
				err = codex.CheckTransferAccount(t.Context(), cfg)
			case "login", "login-handshake-failure":
				if operation == "login-handshake-failure" {
					cfg.Env["AO_TEST_REJECT_INITIALIZE"] = "1"
				}
				var session *codex.LoginSession
				session, err = codex.StartLogin(t.Context(), codex.LoginConfig{Binary: binary, WorkDir: parent, Env: cfg.Env})
				if err == nil {
					t.Cleanup(func() {
						if err := session.Close(); err != nil {
							t.Errorf("close login: %v", err)
						}
					})
					cwd, readErr := os.ReadFile(logPath)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if _, statErr := os.Stat(string(cwd)); statErr != nil {
						t.Fatalf("login cwd removed before close: %v", statErr)
					}
					err = session.Close()
					if again := session.Close(); again != nil {
						t.Fatal(again)
					}
				}
			case "spawn-failure":
				cfg.Binary += "-absent"
				_, err = codex.ProbeAccount(t.Context(), cfg)
			case "cancelled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				_, err = codex.ProbeIdentity(ctx, cfg)
			}
			failed := operation == "spawn-failure" || operation == "cancelled" || operation == "login-handshake-failure"
			if (err != nil) != failed {
				t.Fatalf("unexpected result: %v", err)
			}
			if operation == "login-handshake-failure" && !strings.Contains(err.Error(), "sign-in error -32603") {
				t.Fatalf("unexpected handshake failure: %v", err)
			}
			if !failed {
				cwd, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				if filepath.Dir(string(cwd)) != parent || !strings.HasPrefix(filepath.Base(string(cwd)), ".ao-codex-account-") {
					t.Fatalf("uncontrolled cwd: %q", cwd)
				}
				if _, err := os.Stat(string(cwd)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cwd outlived operation: %v", err)
				}
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != ".codex" {
				t.Fatalf("left temporary directories: %v", entries)
			}
			data, err := os.ReadFile(filepath.Join(canonical, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "notify = [\"user-setting\"]\n" {
				t.Fatal("changed canonical config")
			}
		})
	}
}
