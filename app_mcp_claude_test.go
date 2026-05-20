package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// writeClaudeMCPSetServersBinary returns a fake Claude CLI script
// that:
//
//  1. Records its argv to argvPath so tests can confirm
//     `--mcp-config` arrived with the right entries on first spawn.
//  2. Records every stdin line to stdinPath so tests can confirm
//     `mcp_set_servers` control_requests actually flew over the wire
//     — without this the test would still pass if `SetMCPServers`
//     became a no-op (the mock never gets a chance to fail loudly).
//  3. Answers any `control_request{subtype:"mcp_set_servers"}` with a
//     success control_response carrying a canned diff payload.
//  4. Answers other control_requests with a generic success body so
//     the session reader loop never wedges on an unanswered request.
func writeClaudeMCPSetServersBinary(t *testing.T, argvPath, stdinPath string) string {
	t.Helper()
	script := `#!/bin/sh
set -u
for arg in "$@"; do
    printf '%s\n' "$arg" >>"` + argvPath + `"
done
while IFS= read -r line; do
    printf '%s\n' "$line" >>"` + stdinPath + `"
    case "$line" in
        *'"type":"control_request"'*'"subtype":"mcp_set_servers"'* | *'"subtype":"mcp_set_servers"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"added":["alpha"],"removed":[],"errors":{}}}}\n' "$reqid"
            ;;
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "claude-mcp-set-servers.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock claude binary: %v", err)
	}
	return path
}

// httpServerWithBearer returns an HTTP library row whose BearerEnv
// references an env var. Used by the bearer-folds-into-Authorization
// tests so the fixture is centralised.
func httpServerWithBearer(name string) store.MCPServer {
	return store.MCPServer{
		Name:      name,
		Transport: mcp.TransportHTTP,
		URL:       "https://api.example.com/mcp",
		BearerEnv: "GITHUB_TOKEN",
		Enabled:   true,
	}
}

// TestReconcileClaudeMCPLive_SendsMCPSetServersOnUpdate is the
// load-bearing assertion for the Claude live diff-reconcile path:
// `UpdateThreadMcpServers` MUST drive `Session.SetMCPServers` (the
// `mcp_set_servers` control_request). The launch args side of the
// contract is asserted in the same test by inspecting the recorded
// argv for `--mcp-config <json>` carrying the user library entry.
func TestReconcileClaudeMCPLive_SendsMCPSetServersOnUpdate(t *testing.T) {
	app := newTestAppWithStore(t)

	// Create the library row BEFORE the thread so the profile-seed at
	// thread create time picks up the id. createTestThread runs
	// seedThreadMCPServersFromProfile via the same CreateThread path
	// production uses.
	server, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "claude", workspace, "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	capture := t.TempDir()
	argvPath := filepath.Join(capture, "argv.log")
	stdinPath := filepath.Join(capture, "stdin.log")
	if err := os.WriteFile(argvPath, nil, 0o644); err != nil {
		t.Fatalf("seed argv file: %v", err)
	}
	if err := os.WriteFile(stdinPath, nil, 0o644); err != nil {
		t.Fatalf("seed stdin file: %v", err)
	}
	binary := writeClaudeMCPSetServersBinary(t, argvPath, stdinPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	// At this point the profile-seed ran on CreateThread; the new
	// thread has been auto-associated with `alpha`. Confirm the merge
	// already contains the user entry before we send the live update.
	if _, ok := merged["alpha"]; !ok {
		t.Fatalf("merge missing 'alpha' entry, got %v", merged)
	}

	sess, err := claude.NewSession(ctx, thread.ID, claude.Config{
		Binary:     binary,
		WorkDir:    workspace,
		MCPServers: merged,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.mu.Lock()
	app.sessions[thread.ID] = session{provider: string(provider.Claude), claude: sess}
	app.mu.Unlock()

	// Re-set the per-thread selection. Even though the value matches
	// what the seed already wrote, the binding MUST still call
	// SetMCPServers so the live reconcile is exercised — the mock
	// script's response is the contract.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{server.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	// Inspect the recorded argv for the launch-args contract:
	// `--mcp-config <json>` arrived with the user server included and
	// `--strict-mcp-config` is present so .mcp.json discovery is off.
	args, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	mcpConfig, ok := findFlagValue(lines, "--mcp-config")
	if !ok {
		t.Fatalf("recorded argv missing --mcp-config; got %v", lines)
	}
	if !strings.Contains(mcpConfig, `"alpha"`) {
		t.Fatalf("--mcp-config payload missing user server 'alpha': %s", mcpConfig)
	}
	if !strings.Contains(mcpConfig, `"type":"stdio"`) {
		t.Fatalf("--mcp-config payload missing type:stdio discriminator: %s", mcpConfig)
	}
	if !strings.Contains(mcpConfig, `"command":"/bin/echo"`) {
		t.Fatalf("--mcp-config payload missing user-supplied command: %s", mcpConfig)
	}
	if !containsLine(lines, "--strict-mcp-config") {
		t.Fatalf("--strict-mcp-config flag missing from launch args; .mcp.json discovery must be off: %v", lines)
	}

	// Wire-level contract: a `mcp_set_servers` control_request MUST
	// have flown over stdin. Without this assertion the test would
	// still pass if `SetMCPServers` silently became a no-op, because
	// the mock script only *responds* when it sees the frame — it
	// doesn't fail when the frame never arrives.
	stdinBytes, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if !strings.Contains(string(stdinBytes), `"subtype":"mcp_set_servers"`) {
		t.Fatalf("mock script never received mcp_set_servers control_request; captured stdin:\n%s", stdinBytes)
	}
}

// TestReconcileClaudeMCPLive_NoLiveSessionPersistsOnly is the
// "session is not live" branch: `UpdateThreadMcpServers` MUST still
// persist the new selection and update the profile, without spinning
// up a session or calling mcp_set_servers (which would deadlock
// against a nil session). Mirrors the pattern in app_mcp_test.go but
// is specific to Claude — Codex's reconnect-on-change branch is
// exercised in the Codex test file.
func TestReconcileClaudeMCPLive_NoLiveSessionPersistsOnly(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-claude-no-session", "claude-opus-4-7", "")
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

// TestReconcileClaudeMCPLive_EmptySelectionStillReconciles guards the
// "user toggled everything off" branch. The reconcile MUST still
// fire — empty set ≠ skip-update, because Claude needs the explicit
// drop to take previous servers off the tool list. Without this, a
// disabled toggle leaves stale tools registered until the next session
// restart.
func TestReconcileClaudeMCPLive_EmptySelectionStillReconciles(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, "claude", workspace, "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	capture := t.TempDir()
	argvPath := filepath.Join(capture, "argv.log")
	stdinPath := filepath.Join(capture, "stdin.log")
	if err := os.WriteFile(argvPath, nil, 0o644); err != nil {
		t.Fatalf("seed argv file: %v", err)
	}
	if err := os.WriteFile(stdinPath, nil, 0o644); err != nil {
		t.Fatalf("seed stdin file: %v", err)
	}
	binary := writeClaudeMCPSetServersBinary(t, argvPath, stdinPath)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess, err := claude.NewSession(ctx, thread.ID, claude.Config{
		Binary:  binary,
		WorkDir: workspace,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.mu.Lock()
	app.sessions[thread.ID] = session{provider: string(provider.Claude), claude: sess}
	app.mu.Unlock()

	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{}); err != nil {
		t.Fatalf("UpdateThreadMcpServers (empty): %v", err)
	}

	// Same wire-level contract as the SendsMCPSetServersOnUpdate test:
	// the empty-set toggle MUST drive a real mcp_set_servers frame, not
	// a silent "skip the call because the list is empty" branch.
	stdinBytes, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if !strings.Contains(string(stdinBytes), `"subtype":"mcp_set_servers"`) {
		t.Fatalf("mock script never received mcp_set_servers control_request for empty selection; captured stdin:\n%s", stdinBytes)
	}
}

// TestMergeMCPServersForThread_ClaudeBearerEnvFoldsIntoAuthHeader is a
// regression guard for the per-row render contract: an HTTP library
// row with BearerEnv must surface as
// `headers.Authorization = "Bearer ${BEARER_VAR}"`. Claude expands
// `${VAR}` itself, so AO emits the indirection literally and never
// holds the secret. The whole point of "AO does not store secrets" is
// that the rendered spec carries env-var references only.
func TestMergeMCPServersForThread_ClaudeBearerEnvFoldsIntoAuthHeader(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-claude-bearer", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(httpServerWithBearer("remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{server.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	merged, _, err := app.mergeMCPServersForThread(thread.ID, thread.Provider, nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	entry, ok := merged["remote"].(map[string]any)
	if !ok {
		t.Fatalf("merged[remote] = %v, want map", merged["remote"])
	}
	headers, _ := entry["headers"].(map[string]string)
	if got := headers["Authorization"]; got != "Bearer ${GITHUB_TOKEN}" {
		t.Fatalf("Authorization header = %q, want Bearer ${GITHUB_TOKEN}", got)
	}
	// The merged spec must carry no literal secret. Re-marshal and
	// verify by string scan — anything that looks like a token in our
	// render would indicate the env-var indirection had been resolved
	// somewhere it shouldn't have been.
	rawBytes, _ := json.Marshal(entry)
	if strings.Contains(string(rawBytes), "ghp_") {
		t.Fatalf("rendered spec leaked an apparent token: %s", rawBytes)
	}
}

// findFlagValue returns the entry following the named flag in argv.
// Helper for asserting recorded launch args.
func findFlagValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func containsLine(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}
