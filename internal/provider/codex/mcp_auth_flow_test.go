package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

func TestMCPAuthFlowUsesThreadlessAppServerThroughCompletion(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	tracePath := filepath.Join(t.TempDir(), "requests.ndjson")
	const script = `#!/usr/bin/env bash
set -u
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$TRACE_PATH"
  case "$line" in
    *'"initialize"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{}}}\n' "$id"
      ;;
    *'"mcpServer/oauth/login"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"authorizationUrl":"https://example.test/oauth"}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"name":"atlassian","success":true}}\n'
      ;;
  esac
done
`
	binary := writeMockCodexAppServer(t, t.TempDir(), script)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flow, result, err := StartMCPAuth(ctx, MCPAuthConfig{
		Reaper:         procutiltest.Reaper(),
		Binary:         binary,
		WorkDir:        t.TempDir(),
		Env:            provider.EnvInput{Custom: map[string]string{"TRACE_PATH": tracePath}},
		RequestTimeout: time.Second,
	}, "atlassian")
	if err != nil {
		t.Fatalf("StartMCPAuth: %v", err)
	}
	defer flow.Close()
	if result.AuthorizationURL != "https://example.test/oauth" {
		t.Fatalf("authorization URL = %q", result.AuthorizationURL)
	}
	success, errMessage, err := flow.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !success || errMessage != "" {
		t.Fatalf("completion = success %v, error %q", success, errMessage)
	}
	flow.Close()

	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read request trace: %v", err)
	}
	wire := string(trace)
	if strings.Contains(wire, `"thread/start"`) {
		t.Fatalf("temporary OAuth process created a provider thread:\n%s", wire)
	}
	if !strings.Contains(wire, `"mcpServer/oauth/login"`) || !strings.Contains(wire, `"name":"atlassian"`) {
		t.Fatalf("OAuth request missing from wire:\n%s", wire)
	}
}

func TestMCPAuthFlowRefusesAnInvalidConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, workDir, want string }{
		{"relative workspace", "relative", "must be absolute"},
		{"unbounded request", t.TempDir(), "timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := StartMCPAuth(context.Background(), MCPAuthConfig{Reaper: procutiltest.Reaper(), WorkDir: tc.workDir}, "srv")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}
