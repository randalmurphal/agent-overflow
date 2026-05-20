package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/mcp"
	"agent-overflow/internal/mcpprobe"
	"agent-overflow/internal/provider"
)

// newMCPTestApp returns an App wired to temp Claude/Codex config
// files. The test injects the per-provider stores directly so the
// lazy default-path fallback in app_mcp.go never reads $HOME.
func newMCPTestApp(t *testing.T) (*App, string, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	claudePath := filepath.Join(t.TempDir(), "claude.json")
	codexPath := filepath.Join(t.TempDir(), "config.toml")
	app.claudeConfigStore = claudeconfig.New(claudePath)
	app.codexConfigStore = codexconfig.New(codexPath)
	return app, claudePath, codexPath
}

func writeClaudeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write claude config: %v", err)
	}
}

func writeCodexConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write codex config: %v", err)
	}
}

// TestListMcpServers_Claude_UserAndPluginWithWorkspaceDisabledFlag
// asserts that user + plugin entries both surface to the UI, the
// workspace's disabledMcpServers list flows through to the unified
// `Disabled` flag, and the Source discriminator carries plugin lines
// untouched.
func TestListMcpServers_Claude_UserAndPluginWithWorkspaceDisabledFlag(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	// Plugin entries live only in a workspace's disabledMcpServers
	// list (Claude itself owns the plugin definitions). Place the
	// plugin name in /workspace/a so the listing surfaces it; the fs
	// user entry is in workspace A's disabled list too so we can
	// assert the unified Disabled flag flows through workspace-scoped.
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin", "args": ["--root", "/tmp"]}
  },
  "projects": {
    "/workspace/a": {"disabledMcpServers": ["fs", "plugin:foo:bar"]},
    "/workspace/b": {}
  }
}`)

	gotA, err := app.ListMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListMcpServers /workspace/a: %v", err)
	}
	if len(gotA) != 2 {
		t.Fatalf("workspace A: want 2 entries (fs + plugin), got %d (%#v)", len(gotA), gotA)
	}
	byName := map[string]MCPServer{}
	for _, srv := range gotA {
		byName[srv.Name] = srv
	}
	if fs := byName["fs"]; !fs.Disabled || fs.Source != string(claudeconfig.SourceUser) {
		t.Errorf("workspace A fs: want disabled+user, got disabled=%v source=%q", fs.Disabled, fs.Source)
	}
	if pl := byName["plugin:foo:bar"]; pl.Source == string(claudeconfig.SourceUser) {
		t.Errorf("workspace A plugin entry surfaced as user-source: %#v", pl)
	}

	gotB, err := app.ListMcpServers("claude", "/workspace/b")
	if err != nil {
		t.Fatalf("ListMcpServers /workspace/b: %v", err)
	}
	bByName := map[string]MCPServer{}
	for _, srv := range gotB {
		bByName[srv.Name] = srv
	}
	if fs := bByName["fs"]; fs.Disabled {
		t.Errorf("workspace B fs: want enabled, got disabled (disabled flag leaked across workspaces)")
	}
	if _, ok := bByName["plugin:foo:bar"]; ok {
		t.Errorf("workspace B should not surface plugin entry disabled only in workspace A: %#v", bByName)
	}
}

// TestListMcpServers_Codex_ReadsGlobalEnabledFlag confirms the Codex
// `enabled = false` global flag flows through to the unified Disabled
// field, and workspacePath is ignored (Codex's flag is not workspace-
// scoped).
func TestListMcpServers_Codex_ReadsGlobalEnabledFlag(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
args = ["serve"]
enabled = false

[mcp_servers.linear]
url = "https://mcp.linear.app/api"
`)
	got, err := app.ListMcpServers("codex", "/any/workspace/ignored")
	if err != nil {
		t.Fatalf("ListMcpServers codex: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	byName := map[string]MCPServer{}
	for _, srv := range got {
		byName[srv.Name] = srv
	}
	if !byName["github"].Disabled {
		t.Errorf("github: want disabled, got %#v", byName["github"])
	}
	if byName["linear"].Disabled {
		t.Errorf("linear: want enabled, got disabled")
	}
	if byName["linear"].Transport != codexconfig.TransportStreamable {
		t.Errorf("linear transport: want streamable_http, got %q", byName["linear"].Transport)
	}
}

// TestListMcpServers_UnsupportedProvider returns ErrMCPProviderUnsupported.
func TestListMcpServers_UnsupportedProvider(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	_, err := app.ListMcpServers("gemini", "")
	if !errors.Is(err, ErrMCPProviderUnsupported) {
		t.Fatalf("want ErrMCPProviderUnsupported, got %v", err)
	}
}

// TestCreateMcpServer_Claude_WritesToFile confirms the binding wires
// through to the Claude adapter on input.Provider=="claude" and
// produces a parseable on-disk entry.
func TestCreateMcpServer_Claude_WritesToFile(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	created, err := app.CreateMcpServer(MCPServer{
		Provider:  "claude",
		Name:      "everything",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-everything"},
	})
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	if created.Provider != "claude" || created.Name != "everything" {
		t.Errorf("unexpected returned shape: %#v", created)
	}
	raw, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude.json: %v", err)
	}
	if !strings.Contains(string(raw), `"everything"`) {
		t.Errorf("expected entry on disk, got:\n%s", raw)
	}
}

// TestCreateMcpServer_Codex_WritesTomlSection confirms the binding
// emits a `[mcp_servers.<name>]` section into the TOML file when the
// provider discriminator is "codex".
func TestCreateMcpServer_Codex_WritesTomlSection(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	if _, err := app.CreateMcpServer(MCPServer{
		Provider:  "codex",
		Name:      "github",
		Transport: codexconfig.TransportStdio,
		Command:   "gh-mcp",
		Args:      []string{"serve"},
	}); err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex toml: %v", err)
	}
	if !strings.Contains(string(raw), "[mcp_servers.github]") {
		t.Errorf("expected section header on disk, got:\n%s", raw)
	}
}

// TestCreateMcpServer_RejectsPluginSource asserts that AO refuses to
// create a Claude entry tagged plugin/cloud — only user-source entries
// live in the top-level mcpServers map.
func TestCreateMcpServer_RejectsPluginSource(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	_, err := app.CreateMcpServer(MCPServer{
		Provider:  "claude",
		Name:      "plugin:foo:bar",
		Source:    string(claudeconfig.SourcePlugin),
		Transport: "stdio",
		Command:   "x",
	})
	if !errors.Is(err, ErrMCPReadOnlyEntry) {
		t.Fatalf("want ErrMCPReadOnlyEntry, got %v", err)
	}
}

// TestCreateMcpServer_UnsupportedProvider returns the sentinel.
func TestCreateMcpServer_UnsupportedProvider(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	_, err := app.CreateMcpServer(MCPServer{Provider: "gemini", Name: "x"})
	if !errors.Is(err, ErrMCPProviderUnsupported) {
		t.Fatalf("want ErrMCPProviderUnsupported, got %v", err)
	}
}

// TestUpdateMcpServer_InvalidatesProbeCache pins the contract: after
// editing an entry, the matching probe-cache slot is dropped so the
// UI doesn't show stale "ready" against a now-broken config. Uses the
// mcpprobe SeedForTest seam to pre-populate the cache without
// running a real probe.
func TestUpdateMcpServer_InvalidatesProbeCache(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)
	spec := mcp.Spec{Provider: "claude", Name: "fs", Transport: mcp.TransportStdio}
	app.mcpProbe().SeedForTest(spec, mcpprobe.Result{
		CacheKey:   spec.CacheKey(),
		Provider:   spec.Provider,
		ServerName: spec.Name,
		Status:     mcp.StatusReady,
	})
	if _, ok := app.mcpProbe().Snapshot()[spec.CacheKey()]; !ok {
		t.Fatalf("preflight: cache should contain seed")
	}
	if _, err := app.UpdateMcpServer(MCPServer{
		Provider:  "claude",
		Name:      "fs",
		Transport: "stdio",
		Command:   "fs-bin-v2",
	}); err != nil {
		t.Fatalf("UpdateMcpServer: %v", err)
	}
	if _, ok := app.mcpProbe().Snapshot()[spec.CacheKey()]; ok {
		t.Errorf("cache should be invalidated for %s after update", spec.CacheKey())
	}
}

// TestDeleteMcpServer_Claude_StripsDisabledList confirms that deleting
// a Claude entry also removes its name from every workspace's
// disabledMcpServers list — re-adding the server later must not
// silently surface as disabled.
func TestDeleteMcpServer_Claude_StripsDisabledList(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  },
  "projects": {
    "/workspace/a": {"disabledMcpServers": ["fs"]},
    "/workspace/b": {"disabledMcpServers": ["fs"]}
  }
}`)
	if err := app.DeleteMcpServer("claude", "fs"); err != nil {
		t.Fatalf("DeleteMcpServer: %v", err)
	}
	raw, _ := os.ReadFile(claudePath)
	if strings.Contains(string(raw), `"fs"`) {
		t.Errorf("expected fs to be stripped everywhere; got:\n%s", raw)
	}
}

// TestSetMcpServerEnabled_Claude_TogglesWorkspaceDisabledList exercises
// the load-bearing semantic: Disabled is workspace-scoped for Claude,
// so the toggle writes to the calling thread's workspace projects
// entry — not the top-level mcpServers definition.
func TestSetMcpServerEnabled_Claude_TogglesWorkspaceDisabledList(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)
	thread, err := createTestThread(t, app, string(provider.Claude), "/workspace/a", "claude-sonnet-4-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if err := app.SetMcpServerEnabled(thread.ID, "fs", false); err != nil {
		t.Fatalf("SetMcpServerEnabled disable: %v", err)
	}
	got, err := app.ListMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListMcpServers post-disable: %v", err)
	}
	if !findServer(got, "fs").Disabled {
		t.Errorf("workspace /workspace/a fs: want disabled after toggle")
	}

	otherWorkspace, err := app.ListMcpServers("claude", "/workspace/b")
	if err != nil {
		t.Fatalf("ListMcpServers /workspace/b: %v", err)
	}
	if findServer(otherWorkspace, "fs").Disabled {
		t.Errorf("workspace /workspace/b fs: want enabled (disable leaked across workspaces)")
	}

	if err := app.SetMcpServerEnabled(thread.ID, "fs", true); err != nil {
		t.Fatalf("SetMcpServerEnabled enable: %v", err)
	}
	got2, _ := app.ListMcpServers("claude", "/workspace/a")
	if findServer(got2, "fs").Disabled {
		t.Errorf("workspace /workspace/a fs: want enabled after re-toggle")
	}
}

// TestSetMcpServerEnabled_Codex_TogglesGlobalFlag ensures the binding
// writes the global `enabled` field rather than any per-workspace
// state (Codex doesn't have per-workspace MCP scoping).
func TestSetMcpServerEnabled_Codex_TogglesGlobalFlag(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), "/workspace/a", "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if err := app.SetMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetMcpServerEnabled disable: %v", err)
	}
	raw, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("expected `enabled = false` in toml after disable, got:\n%s", raw)
	}

	listed, err := app.ListMcpServers("codex", "")
	if err != nil {
		t.Fatalf("ListMcpServers: %v", err)
	}
	if !findServer(listed, "github").Disabled {
		t.Errorf("github: want disabled after toggle")
	}
}

// TestProbeCache_CrossProviderNamesDoNotCollide pins the cache-key
// scheme (`provider:name`). Same-name servers across providers must
// not share a cache slot — otherwise a Claude probe failure would
// shadow a Codex success and vice versa.
func TestProbeCache_CrossProviderNamesDoNotCollide(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	cache := app.mcpProbe()
	claudeSpec := mcp.Spec{Provider: "claude", Name: "common", Transport: mcp.TransportStdio}
	codexSpec := mcp.Spec{Provider: "codex", Name: "common", Transport: mcp.TransportStdio}
	if claudeSpec.CacheKey() == codexSpec.CacheKey() {
		t.Fatalf("expected distinct cache keys")
	}
	cache.SeedForTest(claudeSpec, mcpprobe.Result{CacheKey: claudeSpec.CacheKey(), Status: mcp.StatusReady})
	cache.SeedForTest(codexSpec, mcpprobe.Result{CacheKey: codexSpec.CacheKey(), Status: mcp.StatusFailed})

	cache.Invalidate(claudeSpec.CacheKey())
	snap := cache.Snapshot()
	if _, ok := snap[claudeSpec.CacheKey()]; ok {
		t.Errorf("claude key should be invalidated")
	}
	if _, ok := snap[codexSpec.CacheKey()]; !ok {
		t.Errorf("codex key should survive a claude-scoped Invalidate")
	}
}

// TestSetMcpServerEnabled_NoSession_Succeeds confirms the binding
// completes cleanly when the calling thread has no live provider
// session — the file write must commit and no live-reconcile error
// should bubble up.
func TestSetMcpServerEnabled_NoSession_Succeeds(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, "[mcp_servers.github]\ncommand = \"gh-mcp\"\n")
	thread, err := createTestThread(t, app, string(provider.Codex), "/workspace/a", "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if app.hasActiveSession(thread.ID) {
		t.Fatalf("precondition: no session expected")
	}
	if err := app.SetMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetMcpServerEnabled with no session: %v", err)
	}
}

// findServer is a tiny test helper that scans for an MCPServer by
// name. Returns the zero value if missing.
func findServer(in []MCPServer, name string) MCPServer {
	for _, s := range in {
		if s.Name == name {
			return s
		}
	}
	return MCPServer{}
}
