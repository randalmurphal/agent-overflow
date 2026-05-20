package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// writeCodexThreadStartBinary returns a fake `codex app-server`
// script that:
//   1. Answers `initialize` with an empty result.
//   2. Captures the `thread/start` request frame to capturePath so the
//      test can assert the configOverrides shape.
//   3. Answers `thread/start` with a synthetic thread id and exits the
//      send/turn loop quietly until stdin closes.
//
// The exact shape mirrors `WriteMockCodexSession` in
// internal/testutil but adds the per-test capture so we can inspect
// the mcp_servers overlay AO emitted.
func writeCodexThreadStartBinary(t *testing.T, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-mcp\"}}}"
        continue
    fi
done
`, capturePath)
	path := filepath.Join(t.TempDir(), "mock-codex-thread-start.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}

// captureCodexThreadStartFrame opens the capture path and decodes its
// `thread/start` envelope into the pieces the tests assert on.
type codexThreadStartFrame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Config map[string]any `json:"config"`
	} `json:"params"`
}

func readCodexThreadStartCapture(t *testing.T, path string) codexThreadStartFrame {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured thread/start frame: %v", err)
	}
	var frame codexThreadStartFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode captured thread/start frame: %v (raw: %s)", err, string(raw))
	}
	if frame.Method != "thread/start" {
		t.Fatalf("frame.method = %q, want thread/start", frame.Method)
	}
	return frame
}

// TestCodexSessionStart_IncludesUserMCPInConfigOverrides verifies the
// plan's launch-args contract for Codex: the per-thread user MCP
// selection lands inside `thread/start.params.config.mcp_servers`
// alongside any design-mode entries. Without this, the agent's first
// turn would never see the user's selected library servers.
func TestCodexSessionStart_IncludesUserMCPInConfigOverrides(t *testing.T) {
	app := newTestAppWithStore(t)

	// Create the library row BEFORE the thread so the profile-seed at
	// thread create time picks up the id and the merge has something
	// to render.
	server, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "codex", workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	if _, ok := merged["alpha"]; !ok {
		t.Fatalf("merge missing 'alpha' entry, got %v", merged)
	}

	capturePath := filepath.Join(t.TempDir(), "thread-start.json")
	binary := writeCodexThreadStartBinary(t, capturePath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:     binary,
		Model:      "gpt-5.4",
		WorkDir:    workspace,
		MCPServers: merged,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	frame := readCodexThreadStartCapture(t, capturePath)
	mcpServers, ok := frame.Params.Config["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("params.config.mcp_servers missing or wrong type: %v", frame.Params.Config["mcp_servers"])
	}
	alpha, ok := mcpServers["alpha"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers[alpha] missing or wrong type: %v", mcpServers["alpha"])
	}
	if cmd, _ := alpha["command"].(string); cmd != "/bin/echo" {
		t.Errorf("mcp_servers[alpha].command = %v, want /bin/echo", alpha["command"])
	}
	if en, _ := alpha["enabled"].(bool); !en {
		t.Errorf("mcp_servers[alpha].enabled = %v, want true (selected server overlay must force-enable)", alpha["enabled"])
	}
	// Codex serde rejects extra fields on stdio: confirm we did not
	// emit a `url` accidentally on a stdio entry.
	if _, hasURL := alpha["url"]; hasURL {
		t.Errorf("mcp_servers[alpha] has 'url' key; stdio overlay must not emit url: %v", alpha)
	}
	// The server has an id; tests pin behaviour, not numbers.
	if server.ID == "" {
		t.Fatal("created server has empty ID; precondition broken")
	}
}

// TestCodexSessionStart_MasksUnselectedLibraryServersAsEnabledFalse is
// the per-thread isolation defense. Codex's per-thread `mcp_servers`
// overlay MERGES with on-disk config — so unselected library entries
// would leak in from `~/.codex/config.toml` (which AO writes during
// OAuth) unless every unselected entry is overlaid as
// `enabled: false` with its full transport spec.
func TestCodexSessionStart_MasksUnselectedLibraryServersAsEnabledFalse(t *testing.T) {
	app := newTestAppWithStore(t)

	keep, err := app.CreateMcpServer(stdioLibraryServer("", "keep"))
	if err != nil {
		t.Fatalf("CreateMcpServer keep: %v", err)
	}
	if _, err := app.CreateMcpServer(stdioLibraryServer("", "mask")); err != nil {
		t.Fatalf("CreateMcpServer mask: %v", err)
	}
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "codex", workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	// The profile-seed added both; explicitly shrink to "keep" only so
	// the masking branch has something to mask.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{keep.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}

	capturePath := filepath.Join(t.TempDir(), "thread-start.json")
	binary := writeCodexThreadStartBinary(t, capturePath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:     binary,
		Model:      "gpt-5.4",
		WorkDir:    workspace,
		MCPServers: merged,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	frame := readCodexThreadStartCapture(t, capturePath)
	mcpServers, ok := frame.Params.Config["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("params.config.mcp_servers missing: %v", frame.Params.Config)
	}

	keepEntry, ok := mcpServers["keep"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers[keep] missing: %v", mcpServers)
	}
	if en, _ := keepEntry["enabled"].(bool); !en {
		t.Errorf("mcp_servers[keep].enabled = %v, want true", keepEntry["enabled"])
	}

	maskEntry, ok := mcpServers["mask"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers[mask] missing — unselected library row must overlay with enabled:false, not be omitted: %v", mcpServers)
	}
	if en, ok := maskEntry["enabled"].(bool); !ok || en {
		t.Errorf("mcp_servers[mask].enabled = %v, want false", maskEntry["enabled"])
	}
	// Codex's `RawMcpServerConfig` serde requires command (stdio) or
	// url (streamable_http) on the variant. A bare {enabled:false}
	// overlay would fail at deserialization.
	if _, hasCommand := maskEntry["command"]; !hasCommand {
		t.Errorf("mcp_servers[mask] missing command field; Codex serde rejects bare {enabled:false} overlays: %v", maskEntry)
	}
}

// TestCodexSessionStart_HTTPBearerEnvRendersAsBearerTokenEnvVar pins
// the Codex-side render contract for HTTP servers with BearerEnv set:
// the key on the wire is `bearer_token_env_var`, NOT folded into
// `http_headers["Authorization"]` (which would be Claude's shape).
// Codex resolves the env-var itself at session-start so AO does not
// touch the secret.
func TestCodexSessionStart_HTTPBearerEnvRendersAsBearerTokenEnvVar(t *testing.T) {
	app := newTestAppWithStore(t)

	server, err := app.CreateMcpServer(httpServerWithBearer("remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "codex", workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{server.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}
	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}

	capturePath := filepath.Join(t.TempDir(), "thread-start.json")
	binary := writeCodexThreadStartBinary(t, capturePath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:     binary,
		Model:      "gpt-5.4",
		WorkDir:    workspace,
		MCPServers: merged,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	frame := readCodexThreadStartCapture(t, capturePath)
	mcpServers, _ := frame.Params.Config["mcp_servers"].(map[string]any)
	entry, ok := mcpServers["remote"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers[remote] missing: %v", mcpServers)
	}
	if got, _ := entry["bearer_token_env_var"].(string); got != "GITHUB_TOKEN" {
		t.Errorf("bearer_token_env_var = %v, want GITHUB_TOKEN", entry["bearer_token_env_var"])
	}
	// Codex must not see the Claude-style folded Authorization header.
	if headers, ok := entry["http_headers"].(map[string]any); ok {
		if _, hasAuth := headers["Authorization"]; hasAuth {
			t.Errorf("http_headers.Authorization present on Codex render; this is Claude's shape and would double-apply the token: %v", headers)
		}
	}
}

// TestUpdateThreadMcpServers_CodexNoSessionPersistsOnly is the
// Codex-side counterpart to the Claude "no live session" branch in
// app_mcp_claude_test.go: persistence-only, no reconnect attempt.
// Codex's reconcile branch fires a goroutine that calls
// ReconnectSession; without a live session that goroutine never
// spawns.
func TestUpdateThreadMcpServers_CodexNoSessionPersistsOnly(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/mcp-codex-no-session", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{server.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}
	got, err := app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers: %v", err)
	}
	if len(got) != 1 || got[0] != server.ID {
		t.Fatalf("GetThreadMcpServers = %v, want [%s]", got, server.ID)
	}
}

// TestCodexSessionStart_EmptyMCPServersOmitsConfigKey verifies the
// inverse: a thread with no MCP selection and no design servers does
// NOT emit a `mcp_servers` key in `config`. Codex's overlay merge
// would otherwise be a no-op, but a stray empty `{}` would still
// surface in logs and snapshot diffs.
func TestCodexSessionStart_EmptyMCPServersOmitsConfigKey(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "codex", workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	// Confirm nothing is selected and no library entries exist.
	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want empty for no-MCP thread", merged)
	}

	capturePath := filepath.Join(t.TempDir(), "thread-start.json")
	binary := writeCodexThreadStartBinary(t, capturePath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:  binary,
		Model:   "gpt-5.4",
		WorkDir: workspace,
		// Intentionally no MCPServers — Config.MCPServers is nil here.
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	frame := readCodexThreadStartCapture(t, capturePath)
	if _, present := frame.Params.Config["mcp_servers"]; present {
		t.Fatalf("params.config.mcp_servers present with no servers selected: %v", frame.Params.Config["mcp_servers"])
	}
}

// TestCodexSessionStart_DesignMCPMergesWithUserSelection covers the
// design + user merge for Codex. A design server name takes precedence
// over a user library entry with the same name, and the user-selected
// entries flow through unchanged.
func TestCodexSessionStart_DesignMCPMergesWithUserSelection(t *testing.T) {
	app := newTestAppWithStore(t)

	user, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer alpha: %v", err)
	}
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "codex", workspace, "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{user.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	designServers := map[string]any{
		"design": map[string]any{
			"command": "/usr/bin/design-bridge",
			"args":    []any{"--mode=design"},
		},
	}
	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, designServers)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}

	capturePath := filepath.Join(t.TempDir(), "thread-start.json")
	binary := writeCodexThreadStartBinary(t, capturePath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess, err := codex.NewSession(ctx, thread.ID, codex.Config{
		Binary:     binary,
		Model:      "gpt-5.4",
		WorkDir:    workspace,
		MCPServers: merged,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	frame := readCodexThreadStartCapture(t, capturePath)
	mcpServers, _ := frame.Params.Config["mcp_servers"].(map[string]any)
	if _, ok := mcpServers["design"]; !ok {
		t.Errorf("mcp_servers missing 'design': %v", mcpServers)
	}
	if _, ok := mcpServers["alpha"]; !ok {
		t.Errorf("mcp_servers missing 'alpha': %v", mcpServers)
	}
	// Sanity: design entry shape passes through.
	if d, ok := mcpServers["design"].(map[string]any); ok {
		if cmd, _ := d["command"].(string); cmd != "/usr/bin/design-bridge" {
			t.Errorf("design command = %v, want /usr/bin/design-bridge", d["command"])
		}
	}
	// Transport constant referenced for readability/cross-link only.
	_ = mcp.TransportStdio
}
