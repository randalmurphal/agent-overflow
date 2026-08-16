package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// TestSetThreadMcpServerEnabled_Claude_LiveSession_FiresToggleRPC pins
// the live-toggle contract: when a chat-mode Claude thread has a live
// session, a toggle MUST go through
// `control_request{subtype:"mcp_toggle"}` — the CLI disconnects the
// server and persists the workspace `disabledMcpServers` entry itself —
// and AO must NOT double-write the config file (the CLI's write is
// debounced; a concurrent AO write could clobber it).
func TestSetThreadMcpServerEnabled_Claude_LiveSession_FiresToggleRPC(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)

	workspace := t.TempDir()
	configBody := `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"},
    "github": {"type": "stdio", "command": "gh-mcp"}
  }
}`
	writeClaudeConfig(t, claudePath, configBody)

	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeClaudeMcpToggleCaptureBinary(t, captureDir)
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
		token:    "claude-live-toggle-token",
		claude:   sess,
	}

	if err := app.SetThreadMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled: %v", err)
	}

	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if subtype, _ := envelope.Request["subtype"].(string); subtype != "mcp_toggle" {
		t.Fatalf("captured request subtype = %q, want mcp_toggle", subtype)
	}
	if name, _ := envelope.Request["serverName"].(string); name != "github" {
		t.Errorf("captured serverName = %q, want github", name)
	}
	if enabled, ok := envelope.Request["enabled"].(bool); !ok || enabled {
		t.Errorf("captured enabled = %v, want false", envelope.Request["enabled"])
	}

	// The CLI owns the config persist on this path — AO must not have
	// touched the file.
	raw, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude config: %v", err)
	}
	if string(raw) != configBody {
		t.Errorf("claude config was rewritten by the live toggle path:\n%s", raw)
	}
}

// TestSetThreadMcpServerEnabled_Claude_DesignThread_WritesConfigNotRPC
// guards the design carve-out: design-mode Claude sessions launch with
// --strict-mcp-config, so user MCP is invisible to them and a live
// mcp_toggle would be a no-op against the wrong server set. The toggle
// must land in the workspace config (for future non-design sessions)
// and no control_request may reach the live session.
func TestSetThreadMcpServerEnabled_Claude_DesignThread_WritesConfigNotRPC(t *testing.T) {
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
	binary := writeClaudeMcpToggleCaptureBinary(t, captureDir)
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

	if err := app.SetThreadMcpServerEnabled(thread.ID, "fs", false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled design thread: %v", err)
	}

	if waitForMcpCapture(captureDir, 500*time.Millisecond) {
		t.Fatalf("design thread should NOT send mcp_toggle; capture observed at %s", captureDir)
	}
	listed, err := app.ListWorkspaceMcpServers("claude", workspace)
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if !findServer(listed, "fs").Disabled {
		t.Errorf("fs: want disabled in workspace config after design-thread toggle")
	}
}

// TestSetThreadMcpServerEnabled_Codex_LiveSession_FiresRefreshRPC pins
// the Codex side: a toggle on a live Codex session writes the global
// `enabled` flag and hot-reloads via `config/mcpServer/reload` — no
// session respawn.
func TestSetThreadMcpServerEnabled_Codex_LiveSession_FiresRefreshRPC(t *testing.T) {
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
	binary := writeCodexRefreshCaptureBinary(t, captureDir, "codex-thread-mcp", "")
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
		token:    "codex-live-toggle-token",
		codex:    sess,
	}

	if err := app.SetThreadMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled: %v", err)
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
	listed, err := app.ListWorkspaceMcpServers("codex", workspace)
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if !findServer(listed, "github").Disabled {
		t.Errorf("github: expected Disabled=true after toggle")
	}
}

// TestListThreadMcpServers_Claude_LiveSession_UsesSessionTruth pins the
// session-truth listing: a live Claude session answers via
// `control_request{subtype:"mcp_status"}`, plugin servers surface under
// their qualified name with scope "dynamic", a disabled server maps to
// Disabled=true, claude.ai connectors are filtered, tool names flow
// through, and rows are labeled Source "session".
func TestListThreadMcpServers_Claude_LiveSession_UsesSessionTruth(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	// Deliberately different from the session's answer — the listing
	// must NOT read this file while a session is live.
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "config-only-server": {"type": "stdio", "command": "x"}
  }
}`)

	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	statusPayload := `{"mcpServers":[` +
		`{"name":"github","status":"connected","scope":"user","tools":[{"name":"issues_list"},{"name":"pr_read"}]},` +
		`{"name":"plugin:foo:bar","status":"disabled","scope":"dynamic"},` +
		`{"name":"claude.ai Gmail","status":"connected","scope":"claudeai"}` +
		`]}`
	binary := writeClaudeMcpStatusResponderBinary(t, statusPayload)
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
		token:    "claude-live-list-token",
		claude:   sess,
	}

	got, err := app.ListThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadMcpServers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows (github + plugin, claude.ai filtered), got %d (%#v)", len(got), got)
	}
	gh := findServer(got, "github")
	if gh.Status != "connected" || gh.Source != "session" || gh.Scope != "user" {
		t.Errorf("github row = %#v, want connected session-sourced user row", gh)
	}
	if len(gh.Tools) != 2 || gh.Tools[0] != "issues_list" {
		t.Errorf("github tools = %v, want [issues_list pr_read]", gh.Tools)
	}
	pl := findServer(got, "plugin:foo:bar")
	if !pl.Disabled || pl.Status != "disabled" || pl.Scope != "dynamic" {
		t.Errorf("plugin row = %#v, want disabled dynamic row", pl)
	}
	if cfg := findServer(got, "config-only-server"); cfg.Name != "" {
		t.Errorf("config file must not leak into the session-truth listing: %#v", cfg)
	}
}

// claudeControlRequestEnvelope mirrors the wire shape this test pins.
// Kept local — production code stays in internal/provider/claude.
type claudeControlRequestEnvelope struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

// writeClaudeMcpToggleCaptureBinary emits a fake Claude CLI that
// appends every mcp_toggle control_request to capture.jsonl and
// synthesises a success control_response so the session client doesn't
// time out.
func writeClaudeMcpToggleCaptureBinary(t *testing.T, captureDir string) string {
	t.Helper()
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"mcp_toggle"'* | *'"subtype":"mcp_toggle"'*'"type":"control_request"'*)
            printf '%s\n' "$line" >> ` + shellQuote(filepath.Join(captureDir, "capture.jsonl")) + `
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "claude-mcp-toggle-capture.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write claude mcp capture binary: %v", err)
	}
	return path
}

// writeClaudeMcpStatusResponderBinary emits a fake Claude CLI that
// answers `mcp_status` control_requests with the given payload.
func writeClaudeMcpStatusResponderBinary(t *testing.T, payload string) string {
	t.Helper()
	payloadPath := filepath.Join(t.TempDir(), "mcp-status.json")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write status payload: %v", err)
	}
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"mcp_status"'* | *'"subtype":"mcp_status"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":%s}}\n' "$reqid" "$(cat ` + shellQuote(payloadPath) + `)"
            ;;
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "claude-mcp-status-responder.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write claude mcp status responder: %v", err)
	}
	return path
}

func writeCodexRefreshCaptureBinary(t *testing.T, captureDir, threadID, gateFile string) string {
	t.Helper()
	// Captures every JSON-RPC request whose method matches the live-
	// reload path. initialize / thread/start get the canonical-shape
	// success responses Codex's NewSession expects. A non-empty gateFile
	// makes the reload arm BLOCK (after capturing) until that file
	// exists, so a test can hold a reload deterministically in flight.
	gateWait := ""
	if gateFile != "" {
		gateWait = `        while [ ! -f ` + shellQuote(gateFile) + ` ]; do sleep 0.02; done
`
	}
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"config/mcpServer/reload"'; then
        printf '%s\n' "$line" >> ` + shellQuote(filepath.Join(captureDir, "capture.jsonl")) + `
` + gateWait + `        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
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

func readClaudeMcpToggleCapture(t *testing.T, captureDir string, deadline time.Duration) claudeControlRequestEnvelope {
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
	t.Fatalf("no mcp_toggle capture observed at %s within %s", path, deadline)
	return claudeControlRequestEnvelope{}
}

func waitForMcpCapture(captureDir string, window time.Duration) bool {
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
