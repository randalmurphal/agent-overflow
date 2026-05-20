package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/mcpstatus"
)

func TestParseCodexMCPList_OAuthWithTools(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"name":       "atlassian",
				"authStatus": "oAuth",
				"tools": map[string]any{
					"fetchTicket":   map[string]any{},
					"searchTickets": map[string]any{},
					"writeComment":  map[string]any{},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	results, err := parseMCPList(body, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}
	r := results[0]
	if r.Status != mcpstatus.StatusConnected {
		t.Errorf("status = %q, want %q", r.Status, mcpstatus.StatusConnected)
	}
	if r.ToolCount != 3 {
		t.Errorf("tool count = %d, want 3", r.ToolCount)
	}
	if r.AuthStatus != "oAuth" {
		t.Errorf("auth status = %q, want oAuth", r.AuthStatus)
	}
}

func TestParseCodexMCPList_NotLoggedIn(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"name": "linear", "authStatus": "notLoggedIn", "tools": map[string]any{}},
		},
	})
	results, err := parseMCPList(body, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if results[0].Status != mcpstatus.StatusNeedsAuth {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusNeedsAuth)
	}
}

func TestParseCodexMCPList_EmptyToolsWithBearerToken(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"name": "still-starting", "authStatus": "bearerToken", "tools": map[string]any{}},
		},
	})
	results, _ := parseMCPList(body, time.Now())
	if results[0].Status != mcpstatus.StatusStarting {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusStarting)
	}
}

func TestParseCodexMCPList_UnsupportedNoTools(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"name": "mystery", "authStatus": "unsupported", "tools": map[string]any{}},
		},
	})
	results, _ := parseMCPList(body, time.Now())
	if results[0].Status != mcpstatus.StatusUnknown {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusUnknown)
	}
}

func TestParseCodexMCPList_EmptyResponse(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte{},
		[]byte(`{"data":[]}`),
	}
	for _, c := range cases {
		results, err := parseMCPList(c, time.Now())
		if err != nil {
			t.Errorf("input %q: unexpected error %v", c, err)
		}
		if len(results) != 0 {
			t.Errorf("input %q: expected empty results, got %+v", c, results)
		}
	}
}

func TestParseCodexMCPList_MalformedReturnsError(t *testing.T) {
	if _, err := parseMCPList([]byte(`not json`), time.Now()); err == nil {
		t.Fatal("expected decode error")
	}
}

func writeMockCodexAppServer(t *testing.T, dir, scriptBody string) string {
	t.Helper()
	binPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(binPath, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write mock codex: %v", err)
	}
	return binPath
}

// mockCodexAppServerScript writes a bash script that reads stdin
// JSON-RPC requests line by line and emits canned responses for
// initialize and mcpServerStatus/list. Other request IDs are ignored.
//
// The script extracts the request id with a tiny jq-free regex so we
// don't depend on jq being installed.
const mockCodexAppServerScript = `#!/usr/bin/env bash
set -u
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"capabilities":{},"protocolVersion":"2025-06-18","serverInfo":{"name":"mock-codex","version":"0.0.0"}}}\n' "$id"
      ;;
    *'"mcpServerStatus/list"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"data":[{"name":"github","authStatus":"oAuth","tools":{"a":{},"b":{},"c":{}}},{"name":"linear","authStatus":"notLoggedIn","tools":{}}]}}\n' "$id"
      ;;
  esac
done
`

func TestMCPStatusFetcher_Fetch_UsesMockBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	binPath := writeMockCodexAppServer(t, t.TempDir(), mockCodexAppServerScript)

	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	results, err := f.Fetch(context.Background(), mcpstatus.ProviderCodex)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d (%+v)", len(results), results)
	}

	want := map[string]mcpstatus.Status{
		"github": mcpstatus.StatusConnected,
		"linear": mcpstatus.StatusNeedsAuth,
	}
	for _, r := range results {
		expected, ok := want[r.Name]
		if !ok {
			t.Errorf("unexpected server: %q", r.Name)
			continue
		}
		if r.Status != expected {
			t.Errorf("%q: status = %q, want %q", r.Name, r.Status, expected)
		}
		if r.Key.Provider != mcpstatus.ProviderCodex {
			t.Errorf("%q: provider = %q, want codex", r.Name, r.Key.Provider)
		}
		if r.Source != mcpstatus.SourceEphemeralFetch {
			t.Errorf("%q: source = %q, want ephemeral-fetch", r.Name, r.Source)
		}
	}
}

func TestMCPStatusFetcher_Fetch_InitializeError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	const script = `#!/usr/bin/env bash
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"internal: bootstrap failed"}}\n' "$id"
      ;;
  esac
done
`
	binPath := writeMockCodexAppServer(t, t.TempDir(), script)
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	_, err := f.Fetch(context.Background(), mcpstatus.ProviderCodex)
	if err == nil {
		t.Fatal("expected error from initialize failure")
	}
	if !strings.Contains(err.Error(), "bootstrap failed") {
		t.Errorf("expected error to surface RPC message, got %v", err)
	}
}

func TestMCPStatusFetcher_Fetch_ListError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	const script = `#!/usr/bin/env bash
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"mcpServerStatus/list"'*)
      id=$(printf '%s' "$line" | grep -oE '"id"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+')
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      ;;
  esac
done
`
	binPath := writeMockCodexAppServer(t, t.TempDir(), script)
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 5 * time.Second}
	_, err := f.Fetch(context.Background(), mcpstatus.ProviderCodex)
	if err == nil {
		t.Fatal("expected error from list failure")
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Errorf("expected error to surface RPC message, got %v", err)
	}
}

func TestMCPStatusFetcher_Fetch_MissingBinary(t *testing.T) {
	f := &MCPStatusFetcher{Binary: ""}
	if _, err := f.Fetch(context.Background(), mcpstatus.ProviderCodex); err == nil {
		t.Fatal("expected error for missing binary path")
	}
}

func TestMCPStatusFetcher_Fetch_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock shell binaries are POSIX-only")
	}
	const script = `#!/usr/bin/env bash
sleep 5
`
	binPath := writeMockCodexAppServer(t, t.TempDir(), script)
	f := &MCPStatusFetcher{Binary: binPath, Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := f.Fetch(context.Background(), mcpstatus.ProviderCodex)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestMCPStatusFromList(t *testing.T) {
	cases := []struct {
		auth      string
		toolCount int
		want      mcpstatus.Status
	}{
		{"notLoggedIn", 0, mcpstatus.StatusNeedsAuth},
		{"notLoggedIn", 5, mcpstatus.StatusNeedsAuth}, // login required wins over tools
		{"unsupported", 3, mcpstatus.StatusConnected},
		{"unsupported", 0, mcpstatus.StatusUnknown}, // configured but never observed
		{"bearerToken", 4, mcpstatus.StatusConnected},
		{"bearerToken", 0, mcpstatus.StatusStarting}, // auth configured, tools pending
		{"oAuth", 7, mcpstatus.StatusConnected},
		{"oAuth", 0, mcpstatus.StatusStarting},
		{"future-state", 0, mcpstatus.StatusUnknown},
		{"", 0, mcpstatus.StatusUnknown},
	}
	for _, tc := range cases {
		if got := MCPStatusFromList(tc.auth, tc.toolCount); got != tc.want {
			t.Errorf("MCPStatusFromList(%q, %d) = %q, want %q", tc.auth, tc.toolCount, got, tc.want)
		}
	}
}

func TestMCPStatusFromNotif(t *testing.T) {
	cases := []struct {
		state string
		want  mcpstatus.Status
	}{
		{"starting", mcpstatus.StatusStarting},
		{"ready", mcpstatus.StatusConnected},
		{"failed", mcpstatus.StatusFailed},
		{"cancelled", mcpstatus.StatusFailed},
		{"", mcpstatus.StatusUnknown},
		{"new-state", mcpstatus.StatusUnknown},
	}
	for _, tc := range cases {
		if got := MCPStatusFromNotif(tc.state); got != tc.want {
			t.Errorf("MCPStatusFromNotif(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}
