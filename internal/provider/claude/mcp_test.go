package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// mcpStatusResponderScript writes a fake Claude CLI that:
//   - replies to any `"mcp_status"` request with a canned response,
//     parameterised by `mode`,
//   - ignores all other lines and exits on stdin close.
//
// Mirrors the stopTaskResponderScript pattern in session_test.go so
// the round-trip uses the real proc + readLoop + sendControlRequest
// machinery rather than mocking internals.
func mcpStatusResponderScript(mode string) string {
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"mcp_status"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "two-servers":
		// Two-server response: one connected, one needs-auth. Mirrors
		// the spike capture against Claude 2.1.139.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[{"name":"github","status":"connected","serverInfo":{"name":"github","version":"1.0"},"tools":[{"name":"a"}]},{"name":"sentry","status":"needs-auth","config":{"type":"http","url":"https://example"}}]}}}\n' "$reqid"`
	case "with-error":
		// Failed server carries the `error` field.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[{"name":"broken","status":"failed","error":"connection refused"}]}}}\n' "$reqid"`
	case "empty":
		// Empty mcpServers array — the CLI returns the field but no entries.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":[]}}}\n' "$reqid"`
	case "error-subtype":
		// Provider-side error (e.g., feature unavailable).
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"mcp_status unsupported"}}\n' "$reqid"`
	case "silent":
		body = `            : # never respond — exercises the timeout path`
	case "malformed":
		// success subtype, but the inner response.mcpServers field
		// is a string rather than the expected array. Exercises the
		// decode-error branch in QueryMCPStatus.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"mcpServers":"oops"}}}\n' "$reqid"`
	case "success-no-payload":
		// success subtype with no `response` field at all. The CLI's
		// generic sendControlResponseSuccess path can emit this if a
		// future schema change drops the body; the parser must treat
		// it as "no servers" rather than as a malformed envelope.
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s"}}\n' "$reqid"`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

func newMCPStatusResponderSession(t *testing.T, mode string, timeout time.Duration) *Session {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(mcpStatusResponderScript(mode)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              "thread-mcp-test",
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: timeout,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSession_QueryMCPStatus_TwoServers(t *testing.T) {
	s := newMCPStatusResponderSession(t, "two-servers", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	servers, err := s.QueryMCPStatus(ctx)
	if err != nil {
		t.Fatalf("QueryMCPStatus: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(servers), servers)
	}

	byName := map[string]MCPServerStatus{}
	for _, s := range servers {
		byName[s.Name] = s
	}
	if got := byName["github"].Status; got != "connected" {
		t.Errorf("github status = %q, want connected", got)
	}
	if got := byName["sentry"].Status; got != "needs-auth" {
		t.Errorf("sentry status = %q, want needs-auth", got)
	}
	if byName["sentry"].Error != "" {
		t.Errorf("needs-auth entry should not carry an error, got %q", byName["sentry"].Error)
	}
}

func TestSession_QueryMCPStatus_FailedCarriesError(t *testing.T) {
	s := newMCPStatusResponderSession(t, "with-error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	servers, err := s.QueryMCPStatus(ctx)
	if err != nil {
		t.Fatalf("QueryMCPStatus: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(servers))
	}
	if got := servers[0].Status; got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if got := servers[0].Error; got != "connection refused" {
		t.Errorf("error = %q, want %q", got, "connection refused")
	}
}

func TestSession_QueryMCPStatus_Empty(t *testing.T) {
	s := newMCPStatusResponderSession(t, "empty", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	servers, err := s.QueryMCPStatus(ctx)
	if err != nil {
		t.Fatalf("QueryMCPStatus: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("expected 0 entries, got %d: %+v", len(servers), servers)
	}
}

func TestSession_QueryMCPStatus_ErrorSubtype(t *testing.T) {
	s := newMCPStatusResponderSession(t, "error-subtype", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := s.QueryMCPStatus(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mcp_status unsupported") {
		t.Errorf("error message missing provider detail: %v", err)
	}
}

func TestSession_QueryMCPStatus_MalformedPayload(t *testing.T) {
	s := newMCPStatusResponderSession(t, "malformed", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := s.QueryMCPStatus(ctx)
	if err == nil {
		t.Fatal("expected decode error on malformed payload, got nil")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error should wrap %q, got %v", "decode response", err)
	}
}

func TestSession_QueryMCPStatus_SuccessNoPayload(t *testing.T) {
	s := newMCPStatusResponderSession(t, "success-no-payload", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	servers, err := s.QueryMCPStatus(ctx)
	if err != nil {
		t.Fatalf("QueryMCPStatus on success-with-no-payload: %v", err)
	}
	if servers != nil {
		t.Errorf("expected nil servers slice on no-payload, got %+v", servers)
	}
}

func TestSession_QueryMCPStatus_Timeout(t *testing.T) {
	s := newMCPStatusResponderSession(t, "silent", 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := s.QueryMCPStatus(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}
