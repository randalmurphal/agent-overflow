package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// TestSetMcpServerEnabled_Claude_LiveSession_FiresSetServersRPC pins the
// load-bearing rebuild contract: when a chat-mode Claude thread has a
// live session and the user toggles a server, AO MUST send
// `control_request{subtype:"mcp_set_servers"}` (no session respawn).
// The mock binary captures the wire payload to disk; the test reads the
// captured envelope, walks the JSON shape, and asserts the rendered
// server map carries the expected `disabled=false` server and excludes
// the toggled-off one.
func TestSetMcpServerEnabled_Claude_LiveSession_FiresSetServersRPC(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)

	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"},
    "github": {"type": "stdio", "command": "gh-mcp"}
  }
}`)

	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeClaudeMcpSetServersCaptureBinary(t, captureDir)
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  binary,
			WorkDir: workspace,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "claude-live-reconcile-token",
		claude:   sess,
	}

	if err := app.SetMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetMcpServerEnabled: %v", err)
	}

	envelope := readMcpSetServersCapture(t, captureDir, 3*time.Second)
	subtype, _ := envelope.Request["subtype"].(string)
	if subtype != "mcp_set_servers" {
		t.Fatalf("captured request subtype = %q, want mcp_set_servers", subtype)
	}
	servers, ok := envelope.Request["servers"].(map[string]any)
	if !ok {
		t.Fatalf("captured request missing servers map: %#v", envelope.Request)
	}
	if _, present := servers["github"]; present {
		t.Errorf("toggled-off server github should be absent from mcp_set_servers payload: %#v", servers)
	}
	fs, ok := servers["fs"].(map[string]any)
	if !ok {
		t.Fatalf("captured request missing fs server entry: %#v", servers)
	}
	if fs["command"] != "fs-bin" {
		t.Errorf("fs server entry command = %v, want fs-bin", fs["command"])
	}
	// The `type` discriminator is backfilled by withClaudeTransportType
	// — pin the contract that the live-reconcile path runs the same
	// stamping the launch path does.
	if fs["type"] != "stdio" {
		t.Errorf("fs server entry type = %v, want stdio (withClaudeTransportType backfill missing)", fs["type"])
	}
}

// TestSetMcpServerEnabled_Claude_DesignThread_SkipsLiveReconcile guards
// the load-bearing skip: design-mode Claude sessions launch with
// --strict-mcp-config, so user-MCP entries are invisible to them
// regardless of the workspace disabled-list — sending mcp_set_servers
// would either no-op or surface as confusing CLI churn.
func TestSetMcpServerEnabled_Claude_DesignThread_SkipsLiveReconcile(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)

	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)

	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "design")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeClaudeMcpSetServersCaptureBinary(t, captureDir)
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  binary,
			WorkDir: workspace,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "claude-design-token",
		claude:   sess,
	}

	if err := app.SetMcpServerEnabled(thread.ID, "fs", false); err != nil {
		t.Fatalf("SetMcpServerEnabled design thread: %v", err)
	}

	if waitForMcpSetServersCapture(t, captureDir, 500*time.Millisecond) {
		t.Fatalf("design thread should NOT trigger mcp_set_servers; capture observed at %s", captureDir)
	}
}

// TestSetMcpServerEnabled_Codex_LiveSession_FiresRefreshRPC pins the
// Codex side of the same rebuild contract: a toggle on a live Codex
// session sends `config/mcpServer/reload`, no session respawn.
func TestSetMcpServerEnabled_Codex_LiveSession_FiresRefreshRPC(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)

	workspace := t.TempDir()
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
`)

	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeCodexRefreshCaptureBinary(t, captureDir, "codex-thread-mcp")
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			Model:   "gpt-5",
			WorkDir: workspace,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "codex-live-reconcile-token",
		codex:    sess,
	}

	if err := app.SetMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetMcpServerEnabled: %v", err)
	}

	method := readCodexReloadCapture(t, captureDir, 3*time.Second)
	if method != "config/mcpServer/reload" {
		t.Fatalf("captured Codex method = %q, want config/mcpServer/reload", method)
	}

	// File write must commit even though the live RPC fires async.
	raw, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("expected `enabled = false` on disk after toggle, got:\n%s", raw)
	}
	listed, err := app.ListMcpServers("codex", workspace)
	if err != nil {
		t.Fatalf("ListMcpServers: %v", err)
	}
	if !findServer(listed, "github").Disabled {
		t.Errorf("github: expected Disabled=true after toggle")
	}
	// Sanity check: the streamable_http transport native to Codex is
	// preserved through the list path.
	if findServer(listed, "github").Transport != codexconfig.TransportStdio {
		t.Errorf("github transport = %q, want stdio", findServer(listed, "github").Transport)
	}
}

// claudeControlRequestEnvelope mirrors the wire shape this test pins.
// Kept local — production code stays in internal/provider/claude.
type claudeControlRequestEnvelope struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

func writeClaudeMcpSetServersCaptureBinary(t *testing.T, captureDir string) string {
	t.Helper()
	// The script appends every line that looks like a control_request
	// to capture.jsonl and synthesises a success control_response so
	// the Claude session client doesn't time out. The mcp_set_servers
	// success payload echoes a minimal diff so the SetMCPServers
	// caller observes a well-formed result.
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"mcp_set_servers"'* | *'"subtype":"mcp_set_servers"'*'"type":"control_request"'*)
            printf '%s\n' "$line" >> ` + shellQuote(filepath.Join(captureDir, "capture.jsonl")) + `
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"added":[],"removed":[]}}}\n' "$reqid"
            ;;
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "claude-mcp-capture.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write claude mcp capture binary: %v", err)
	}
	return path
}

func writeCodexRefreshCaptureBinary(t *testing.T, captureDir, threadID string) string {
	t.Helper()
	// Captures every JSON-RPC request whose method matches the live-
	// reload path. initialize / thread/start get the canonical-shape
	// success responses Codex's NewSession expects.
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"config/mcpServer/reload"'; then
        printf '%s\n' "$line" >> ` + shellQuote(filepath.Join(captureDir, "capture.jsonl")) + `
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"` + threadID + `","turns":[]}}}\n' "$id"
        continue
    fi
done
`
	path := filepath.Join(t.TempDir(), "codex-mcp-refresh-capture.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex refresh capture binary: %v", err)
	}
	return path
}

func readMcpSetServersCapture(t *testing.T, captureDir string, deadline time.Duration) claudeControlRequestEnvelope {
	t.Helper()
	path := filepath.Join(captureDir, "capture.jsonl")
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			var env claudeControlRequestEnvelope
			if jerr := json.Unmarshal(trimToFirstLine(raw), &env); jerr != nil {
				t.Fatalf("unmarshal capture %s: %v\n%s", path, jerr, raw)
			}
			return env
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no mcp_set_servers capture observed at %s within %s", path, deadline)
	return claudeControlRequestEnvelope{}
}

func waitForMcpSetServersCapture(t *testing.T, captureDir string, window time.Duration) bool {
	t.Helper()
	path := filepath.Join(captureDir, "capture.jsonl")
	end := time.Now().Add(window)
	for time.Now().Before(end) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readCodexReloadCapture(t *testing.T, captureDir string, deadline time.Duration) string {
	t.Helper()
	path := filepath.Join(captureDir, "capture.jsonl")
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			var env struct {
				Method string `json:"method"`
			}
			if jerr := json.Unmarshal(trimToFirstLine(raw), &env); jerr != nil {
				t.Fatalf("unmarshal codex capture %s: %v\n%s", path, jerr, raw)
			}
			return env.Method
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no config/mcpServer/reload capture observed at %s within %s", path, deadline)
	return ""
}

func trimToFirstLine(raw []byte) []byte {
	for i, b := range raw {
		if b == '\n' {
			return raw[:i]
		}
	}
	return raw
}

