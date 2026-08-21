package codex

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestParseCodexMCPList_ServerInfoWithZeroTools pins the primary
// liveness signal: a server that initialized but exposes no tools (a
// resources-only server, or one whose tool list is empty) is connected.
// The old tool-count-only rule called this "starting" forever.
func TestParseCodexMCPList_ServerInfoWithZeroTools(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"name":       "resources-only",
				"authStatus": "oAuth",
				"serverInfo": map[string]any{"name": "resources-only", "version": "1.2.3"},
				"tools":      map[string]any{},
			},
		},
	})
	results, err := parseMCPList(body, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if results[0].Status != mcpstatus.StatusConnected {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusConnected)
	}
}

// TestParseCodexMCPList_NoServerInfoZeroToolsIsFailed is the headline
// regression: the list response describes a SETTLED connection attempt,
// so an oAuth server that came back with neither serverInfo nor tools
// failed to initialize. It used to render as "starting" indefinitely.
func TestParseCodexMCPList_NoServerInfoZeroToolsIsFailed(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"name": "atlassian", "authStatus": "oAuth", "tools": map[string]any{}},
			{"name": "bearer-dead", "authStatus": "bearerToken", "tools": map[string]any{}},
			{"name": "mystery", "authStatus": "unsupported", "tools": map[string]any{}},
		},
	})
	results, err := parseMCPList(body, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range results {
		if r.Status != mcpstatus.StatusFailed {
			t.Errorf("%s: status = %q, want %q", r.Name, r.Status, mcpstatus.StatusFailed)
		}
	}
}

func TestParseCodexMCPList_ServerInfoAbsentButToolsPresent(t *testing.T) {
	// Safety net: tools can only exist past a completed initialize, so a
	// response that somehow omits serverInfo while listing tools is still
	// connected.
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{"name": "toolsy", "authStatus": "unsupported", "tools": map[string]any{"ping": map[string]any{}}},
		},
	})
	results, _ := parseMCPList(body, time.Now())
	if results[0].Status != mcpstatus.StatusConnected {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusConnected)
	}
}

// TestParseCodexMCPList_NotLoggedInIgnoresServerInfo: a login
// requirement outranks any liveness evidence — the row's action is a
// sign-in either way.
func TestParseCodexMCPList_NotLoggedInIgnoresServerInfo(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"name":       "linear",
				"authStatus": "notLoggedIn",
				"serverInfo": map[string]any{"name": "linear", "version": "1"},
				"tools":      map[string]any{"issue_read": map[string]any{}},
			},
		},
	})
	results, _ := parseMCPList(body, time.Now())
	if results[0].Status != mcpstatus.StatusNeedsAuth {
		t.Errorf("status = %q, want %q", results[0].Status, mcpstatus.StatusNeedsAuth)
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

// TestMCPStatusFromList walks the whole matrix. StatusStarting must not
// appear anywhere in it: a list response only describes settled
// connection attempts, so "still booting" is not an answer this
// projector can give — only a startup notification can.
func TestMCPStatusFromList(t *testing.T) {
	cases := []struct {
		auth          string
		hasServerInfo bool
		toolCount     int
		want          mcpstatus.Status
	}{
		{"notLoggedIn", false, 0, mcpstatus.StatusNeedsAuth},
		{"notLoggedIn", true, 5, mcpstatus.StatusNeedsAuth}, // login required wins over liveness
		{"unsupported", true, 0, mcpstatus.StatusConnected},
		{"unsupported", false, 3, mcpstatus.StatusConnected}, // tools also prove initialize
		{"unsupported", false, 0, mcpstatus.StatusFailed},
		{"bearerToken", true, 0, mcpstatus.StatusConnected},
		{"bearerToken", false, 4, mcpstatus.StatusConnected},
		{"bearerToken", false, 0, mcpstatus.StatusFailed},
		{"oAuth", true, 7, mcpstatus.StatusConnected},
		{"oAuth", true, 0, mcpstatus.StatusConnected},
		{"oAuth", false, 0, mcpstatus.StatusFailed},   // the invalid_grant incident shape
		{" oAuth ", false, 0, mcpstatus.StatusFailed}, // auth enum is trimmed before matching
		// codex 0.147 `unknown`: OAuth discovery failed, which says nothing
		// about whether the server connected. A plain HTTP server with no
		// `.well-known` metadata reports it and still serves tools; 0.146
		// called the same server `unsupported`, so evidence — not the auth
		// axis — has to keep deciding or the upgrade greys out healthy rows.
		{"unknown", true, 0, mcpstatus.StatusConnected},
		{"unknown", false, 2, mcpstatus.StatusConnected},
		{"unknown", false, 0, mcpstatus.StatusFailed},
		{" unknown ", true, 0, mcpstatus.StatusConnected},
		{"future-state", true, 9, mcpstatus.StatusUnknown},
		{"", false, 0, mcpstatus.StatusUnknown},
	}
	for _, tc := range cases {
		entry := MCPServerStatus{Name: "srv", AuthStatus: tc.auth}
		if tc.hasServerInfo {
			entry.ServerInfo = &MCPServerInfo{Name: "srv", Version: "1.0"}
		}
		if tc.toolCount > 0 {
			entry.Tools = map[string]json.RawMessage{}
			for i := 0; i < tc.toolCount; i++ {
				entry.Tools[fmt.Sprintf("tool-%d", i)] = json.RawMessage(`{}`)
			}
		}
		got := MCPStatusFromList(entry)
		if got != tc.want {
			t.Errorf("MCPStatusFromList(%q, %v, %d) = %q, want %q", tc.auth, tc.hasServerInfo, tc.toolCount, got, tc.want)
		}
		if got == mcpstatus.StatusStarting {
			t.Errorf("MCPStatusFromList(%q, %v, %d) returned StatusStarting; a settled probe can never report it", tc.auth, tc.hasServerInfo, tc.toolCount)
		}
	}
}

// TestMCPStartupUpdateTerminalFailure pins which retained states may
// outrank a settled list answer in the app-layer merge: terminal,
// unrecovered outcomes only. "starting" defers — a settled probe is by
// construction newer, and letting a retained starting win would latch
// "Starting…" whenever the terminal notification was lost.
func TestMCPStartupUpdateTerminalFailure(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"failed", true},
		{"cancelled", true},
		{" failed ", true}, // wire strings are trimmed
		{"starting", false},
		{"ready", false},
		{"", false},
		{"some-future-state", false},
	}
	for _, tc := range cases {
		if got := (MCPStartupUpdate{State: tc.state}).TerminalFailure(); got != tc.want {
			t.Errorf("TerminalFailure(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestMCPStatusFromNotif(t *testing.T) {
	cases := []struct {
		name   string
		update MCPStartupUpdate
		want   mcpstatus.Status
	}{
		{name: "starting", update: MCPStartupUpdate{State: "starting"}, want: mcpstatus.StatusStarting},
		{name: "ready", update: MCPStartupUpdate{State: "ready"}, want: mcpstatus.StatusConnected},
		{name: "failed", update: MCPStartupUpdate{State: "failed"}, want: mcpstatus.StatusFailed},
		{name: "cancelled", update: MCPStartupUpdate{State: "cancelled"}, want: mcpstatus.StatusFailed},
		{name: "empty", update: MCPStartupUpdate{}, want: mcpstatus.StatusUnknown},
		{name: "unknown state", update: MCPStartupUpdate{State: "new-state"}, want: mcpstatus.StatusUnknown},
		{
			// The point of item 3.7: an expired OAuth grant used to render
			// as a dead "failed" row. needs-auth is what puts the existing
			// Sign in action on it.
			name: "failed with reauth required",
			update: MCPStartupUpdate{
				State:         "failed",
				Error:         "token expired",
				FailureReason: MCPFailureReasonReauthRequired,
			},
			want: mcpstatus.StatusNeedsAuth,
		},
		{
			// A future McpStartupFailureReason variant must not be guessed
			// into a sign-in prompt; it falls back to the state mapping.
			name: "failed with unknown reason",
			update: MCPStartupUpdate{
				State:         "failed",
				FailureReason: "someFutureReason",
			},
			want: mcpstatus.StatusFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MCPStatusFromNotif(tc.update); got != tc.want {
				t.Errorf("MCPStatusFromNotif(%+v) = %q, want %q", tc.update, got, tc.want)
			}
		})
	}
}
