package claude

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartMCPAuthRequiresCredentialReaderAndBoundedRequest(t *testing.T) {
	workDir := t.TempDir()
	_, _, err := StartMCPAuth(context.Background(), MCPAuthConfig{
		Config:         Config{WorkDir: workDir},
		RequestTimeout: time.Second,
	}, "srv")
	if err == nil || !strings.Contains(err.Error(), "credential reader required") {
		t.Fatalf("missing-reader error = %v", err)
	}

	_, _, err = StartMCPAuth(context.Background(), MCPAuthConfig{
		Config:         Config{WorkDir: workDir},
		ReadCredential: func() ([]byte, error) { return nil, nil },
	}, "srv")
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("missing-timeout error = %v", err)
	}
}

func TestNewSessionRejectsMissingEventCallbackBeforeSpawn(t *testing.T) {
	_, err := NewSession(
		context.Background(),
		"thread",
		Config{Binary: filepath.Join(t.TempDir(), "must-not-spawn")},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "onEvent callback is required") {
		t.Fatalf("error = %v, want missing-callback refusal", err)
	}
}

func TestMCPAuthFlowClosesThreadlessSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "mock-claude")
	const script = `#!/bin/sh
while IFS= read -r line; do
  reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"mcp_authenticate"'*)
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"authUrl":"https://example.test/oauth","requiresUserAction":true}}}\n' "$reqid"
      ;;
    *'"mcp_status"'*)
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[{"name":"srv","status":"connected"}]}}}\n' "$reqid"
      ;;
  esac
done
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock Claude: %v", err)
	}
	// This checks session cleanup, not response latency. Leave the scripted
	// subprocess time to be scheduled alongside the full provider suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flow, result, err := StartMCPAuth(ctx, MCPAuthConfig{
		Config:         Config{Binary: binary, WorkDir: dir},
		ReadCredential: func() ([]byte, error) { return nil, fs.ErrNotExist },
		RequestTimeout: 5 * time.Second,
	}, "srv")
	if err != nil {
		t.Fatalf("StartMCPAuth: %v", err)
	}
	if result.AuthURL != "https://example.test/oauth" {
		t.Fatalf("auth URL = %q", result.AuthURL)
	}
	statuses, err := flow.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != "connected" {
		t.Fatalf("statuses = %+v", statuses)
	}
	if err := flow.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
