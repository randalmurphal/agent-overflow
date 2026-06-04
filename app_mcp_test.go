package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/codexconfig"
	"agent-overflow/internal/mcpstatus"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
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

func TestNewThreadMCPDefaultsOverrideFutureThreadsWithoutMutatingProviderConfig(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, fmt.Sprintf(`{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin", "args": ["--root", "/tmp"]}
  },
  "projects": {
    %q: {"disabledMcpServers": ["fs"]}
  }
}`, workspace))

	initial, err := app.ListMcpServersForNewThread("claude", workspace)
	if err != nil {
		t.Fatalf("ListMcpServersForNewThread initial: %v", err)
	}
	if !findServer(initial, "fs").Disabled {
		t.Fatal("initial new-thread list should fall back to provider config disabled state")
	}

	if err := app.SetNewThreadMcpServerEnabled("claude", workspace, "fs", true); err != nil {
		t.Fatalf("SetNewThreadMcpServerEnabled: %v", err)
	}
	next, err := app.ListMcpServersForNewThread("claude", workspace)
	if err != nil {
		t.Fatalf("ListMcpServersForNewThread next: %v", err)
	}
	if findServer(next, "fs").Disabled {
		t.Fatal("new-thread default override should enable fs")
	}

	providerScoped, err := app.ListMcpServers("claude", workspace)
	if err != nil {
		t.Fatalf("ListMcpServers provider-scoped: %v", err)
	}
	if !findServer(providerScoped, "fs").Disabled {
		t.Fatal("provider config should remain disabled after new-thread override")
	}

	project, err := app.ensureProjectForWorkspace(workspace)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID:         project.ID,
		Provider:          "claude",
		Model:             "claude-sonnet-4-6",
		WorkspaceOverride: workspace,
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	disabled, snapshotted, err := app.store.GetDisabledMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetDisabledMcpServers: %v", err)
	}
	if !snapshotted {
		t.Fatal("created thread should snapshot MCP defaults")
	}
	if len(disabled) != 0 {
		t.Fatalf("created thread disabled MCP servers = %v, want empty", disabled)
	}
}

func TestNewThreadMCPDefaultsAreWorkspaceScopedForClaude(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeClaudeConfig(t, claudePath, fmt.Sprintf(`{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  },
  "projects": {
    %q: {"disabledMcpServers": ["fs"]},
    %q: {}
  }
}`, workspaceA, workspaceB))

	if err := app.SetNewThreadMcpServerEnabled("claude", workspaceA, "fs", true); err != nil {
		t.Fatalf("SetNewThreadMcpServerEnabled workspaceA: %v", err)
	}
	listA, err := app.ListMcpServersForNewThread("claude", workspaceA)
	if err != nil {
		t.Fatalf("ListMcpServersForNewThread workspaceA: %v", err)
	}
	if findServer(listA, "fs").Disabled {
		t.Fatal("workspace A override should enable fs")
	}
	listB, err := app.ListMcpServersForNewThread("claude", workspaceB)
	if err != nil {
		t.Fatalf("ListMcpServersForNewThread workspaceB: %v", err)
	}
	if findServer(listB, "fs").Disabled {
		t.Fatal("workspace A override leaked into workspace B")
	}
}

func TestSetMcpServerEnabled_ClaudeValidatesBeforeMutatingConfig(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, fmt.Sprintf(`{
  "projects": {
    %q: {"disabledMcpServers": ["plugin:foo:bar"]}
  }
}`, workspace))
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-sonnet-4-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if err := app.SetMcpServerEnabled(thread.ID, "missing", false); err == nil {
		t.Fatal("SetMcpServerEnabled missing server error = nil, want validation error")
	}
	raw, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude config: %v", err)
	}
	if strings.Contains(string(raw), "missing") {
		t.Fatalf("missing server was written before validation:\n%s", raw)
	}

	if err := app.SetMcpServerEnabled(thread.ID, "plugin:foo:bar", true); err != nil {
		t.Fatalf("SetMcpServerEnabled enable plugin: %v", err)
	}
	defaults, found, err := app.store.GetNewThreadDisabledMCPServers("claude", workspace)
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers: %v", err)
	}
	if !found {
		t.Fatal("plugin toggle should persist a new-thread defaults row")
	}
	if len(defaults) != 0 {
		t.Fatalf("plugin defaults = %v, want empty enabled set", defaults)
	}
	list, err := app.ListMcpServers("claude", workspace)
	if err != nil {
		t.Fatalf("ListMcpServers: %v", err)
	}
	if found := findServer(list, "plugin:foo:bar"); found.Name != "" {
		t.Fatalf("enabled disabled-only plugin should disappear from native disabled-only list: %#v", found)
	}
}

func TestBuildCodexMCPServersForThreadHonorsExplicitEmptyOverlays(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"

[mcp_servers.linear]
url = "https://mcp.linear.app/api"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if err := app.store.SetDisabledMcpServers(thread.ID, nil); err != nil {
		t.Fatalf("SetDisabledMcpServers empty: %v", err)
	}
	allEnabled, err := app.buildCodexMCPServersForThread(thread)
	if err != nil {
		t.Fatalf("buildCodexMCPServersForThread all enabled: %v", err)
	}
	if len(allEnabled) != 2 {
		t.Fatalf("all-enabled overlay len = %d, want 2 (%#v)", len(allEnabled), allEnabled)
	}

	if err := app.store.SetDisabledMcpServers(thread.ID, []string{"github", "linear"}); err != nil {
		t.Fatalf("SetDisabledMcpServers all disabled: %v", err)
	}
	allDisabled, err := app.buildCodexMCPServersForThread(thread)
	if err != nil {
		t.Fatalf("buildCodexMCPServersForThread all disabled: %v", err)
	}
	if allDisabled == nil {
		t.Fatal("all-disabled overlay is nil, want explicit empty map")
	}
	if len(allDisabled) != 0 {
		t.Fatalf("all-disabled overlay len = %d, want 0 (%#v)", len(allDisabled), allDisabled)
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

// TestUpdateMcpServer_InvalidatesStatusCache pins the contract: after
// editing an entry, the matching status-cache slot is dropped so the
// UI doesn't show a stale "connected" against a now-broken config.
// Seeds the cache via Put then asserts the update path Invalidates.
func TestUpdateMcpServer_InvalidatesStatusCache(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)
	key := mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "fs"}
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    key,
		Status: mcpstatus.StatusConnected,
		Source: mcpstatus.SourceLiveSession,
	})
	if _, ok := app.mcpStatus().Get(key); !ok {
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
	if _, ok := app.mcpStatus().Get(key); ok {
		t.Errorf("cache should be invalidated for %+v after update", key)
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

// TestStatusCache_CrossProviderNamesDoNotCollide pins the cache-key
// scheme. Same-name servers across providers must not share a cache
// slot — otherwise a Claude failure would shadow a Codex connected
// status and vice versa.
func TestStatusCache_CrossProviderNamesDoNotCollide(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	cache := app.mcpStatus()
	claudeKey := mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "common"}
	codexKey := mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "common"}
	if claudeKey == codexKey {
		t.Fatalf("expected distinct cache keys")
	}
	cache.Put(mcpstatus.ServerStatus{Key: claudeKey, Status: mcpstatus.StatusConnected, Source: mcpstatus.SourceLiveSession})
	cache.Put(mcpstatus.ServerStatus{Key: codexKey, Status: mcpstatus.StatusFailed, Source: mcpstatus.SourceLiveSession})

	cache.Invalidate(claudeKey)
	if _, ok := cache.Get(claudeKey); ok {
		t.Errorf("claude key should be invalidated")
	}
	if _, ok := cache.Get(codexKey); !ok {
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

// TestHandleCodexMCPOAuthCompleted_SuccessInvalidatesAndEmits pins the
// happy-path contract: a successful OAuth completion drops the cached
// entry for that key (so the popup shows "Not checked" until the
// frontend's mcp:oauth-completed listener kicks off a refresh), emits
// exactly one mcp:status (the Invalidate sentinel) and one
// mcp:oauth-completed wire payload, and does NOT surface an
// error-toast event. The failure-path test below pins the opposite
// behavior for errMsg != "".
func TestHandleCodexMCPOAuthCompleted_SuccessInvalidatesAndEmits(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	thread, err := createTestThread(t, app, string(provider.Codex), "/workspace/a", "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	key := mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "linear"}
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    key,
		Status: mcpstatus.StatusNeedsAuth,
		Source: mcpstatus.SourceLiveSession,
	})

	type captured struct {
		name string
		data any
	}
	var (
		mu     sync.Mutex
		events []captured
	)
	app.testEmitHook = func(name string, data any) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, captured{name: name, data: data})
	}

	app.handleCodexMCPOAuthCompleted(thread.ID, "linear", true, "")

	if _, ok := app.mcpStatus().Get(key); ok {
		t.Errorf("cache should be invalidated after OAuth completion")
	}

	mu.Lock()
	defer mu.Unlock()
	var oauthCount, statusCount, errorCount int
	for _, e := range events {
		switch e.name {
		case "mcp:oauth-completed":
			oauthCount++
			payload, ok := e.data.(map[string]any)
			if !ok {
				t.Fatalf("oauth-completed payload type %T", e.data)
			}
			if payload["serverName"] != "linear" || payload["success"] != true {
				t.Errorf("unexpected payload: %+v", payload)
			}
		case "mcp:status":
			statusCount++
		case "error":
			errorCount++
		}
	}
	if oauthCount != 1 {
		t.Errorf("expected 1 mcp:oauth-completed emission, got %d", oauthCount)
	}
	if statusCount != 1 {
		t.Errorf("expected 1 mcp:status emission (the invalidate sentinel), got %d", statusCount)
	}
	if errorCount != 0 {
		t.Errorf("expected no error emissions on success path, got %d", errorCount)
	}
}

// TestHandleCodexMCPOAuthCompleted_FailurePayloadCarriesError pins
// the failure path: cache is still invalidated (the stale needs-auth
// entry no longer reflects truth), and the mcp:oauth-completed wire
// payload carries success=false plus the verbatim error string the
// frontend renders. The per-thread error toast emitted via
// emitErrorToThread routes through triage; in tests triage is nil so
// that branch logs instead of emitting, which is verified by
// triage-wired integration coverage elsewhere.
func TestHandleCodexMCPOAuthCompleted_FailurePayloadCarriesError(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	thread, err := createTestThread(t, app, string(provider.Codex), "/workspace/a", "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	key := mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "linear"}
	app.mcpStatus().Put(mcpstatus.ServerStatus{Key: key, Status: mcpstatus.StatusNeedsAuth, Source: mcpstatus.SourceLiveSession})

	var (
		mu       sync.Mutex
		captured map[string]any
	)
	app.testEmitHook = func(name string, data any) {
		if name != "mcp:oauth-completed" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if payload, ok := data.(map[string]any); ok {
			captured = payload
		}
	}

	app.handleCodexMCPOAuthCompleted(thread.ID, "linear", false, "browser closed before redirect")

	if _, ok := app.mcpStatus().Get(key); ok {
		t.Errorf("cache should be invalidated even on OAuth failure")
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("expected an mcp:oauth-completed emission")
	}
	if captured["success"] != false {
		t.Errorf("expected success=false in payload, got %v", captured["success"])
	}
	if captured["error"] != "browser closed before redirect" {
		t.Errorf("expected verbatim error message, got %q", captured["error"])
	}
	if captured["serverName"] != "linear" {
		t.Errorf("expected serverName=linear, got %v", captured["serverName"])
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

// scriptedQuerier returns a claudeMCPStatusQuerier whose i-th call
// returns the i-th element of `responses`. Callers exceeding the
// length get the last element repeated (so a steady-state response
// can be expressed as `[1]response`).
func scriptedQuerier(responses [][]claude.MCPServerStatus, errs []error) func() claudeMCPStatusQuerier {
	var calls int
	return func() claudeMCPStatusQuerier {
		return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
			i := calls
			if i >= len(responses) {
				i = len(responses) - 1
			}
			calls++
			var err error
			if i < len(errs) {
				err = errs[i]
			}
			return responses[i], err
		}
	}
}

// zeroIntervals returns n zero-duration sleeps so the poll loop runs
// instantly without changing its tick count.
func zeroIntervals(n int) []time.Duration {
	out := make([]time.Duration, n)
	return out
}

// captureOrderedEmissions installs a testEmitHook that filters for the given
// event names and returns a snapshot fn for the test to read.
func captureOrderedEmissions(a *App, names ...string) func() []capturedEmission {
	wanted := map[string]struct{}{}
	for _, n := range names {
		wanted[n] = struct{}{}
	}
	var (
		mu       sync.Mutex
		captured []capturedEmission
	)
	a.testEmitHook = func(name string, data any) {
		if _, ok := wanted[name]; !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, capturedEmission{name: name, data: data})
	}
	return func() []capturedEmission {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedEmission, len(captured))
		copy(out, captured)
		return out
	}
}

type capturedEmission struct {
	name string
	data any
}

// TestPollClaudeMCPAfterOAuth_ConnectedFlipPutsCacheAndEmits asserts
// the happy path: the poller sees needs-auth on tick 1 and connected
// on tick 2, then Puts an authoritative live-session entry, emits
// mcp:oauth-completed{success:true}, and exits before the remaining
// intervals fire.
func TestPollClaudeMCPAfterOAuth_ConnectedFlipPutsCacheAndEmits(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:status", "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "sentry", Status: "needs-auth"}},
			{{Name: "sentry", Status: "connected"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"sentry",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "sentry"})
	if !ok {
		t.Fatal("expected sentry entry in cache after connected flip")
	}
	if cached.Status != mcpstatus.StatusConnected {
		t.Errorf("status = %q, want %q", cached.Status, mcpstatus.StatusConnected)
	}
	if cached.Source != mcpstatus.SourceLiveSession {
		t.Errorf("source = %q, want %q", cached.Source, mcpstatus.SourceLiveSession)
	}

	emissions := snapshot()
	var oauthEvent map[string]any
	for _, e := range emissions {
		if e.name == "mcp:oauth-completed" {
			oauthEvent, _ = e.data.(map[string]any)
		}
	}
	if oauthEvent == nil {
		t.Fatal("expected mcp:oauth-completed emission")
	}
	if oauthEvent["success"] != true {
		t.Errorf("success = %v, want true", oauthEvent["success"])
	}
	if oauthEvent["provider"] != "claude" {
		t.Errorf("provider = %v, want claude", oauthEvent["provider"])
	}
	if oauthEvent["serverName"] != "sentry" {
		t.Errorf("serverName = %v, want sentry", oauthEvent["serverName"])
	}
	if oauthEvent["threadId"] != "thread-1" {
		t.Errorf("threadId = %v, want thread-1", oauthEvent["threadId"])
	}
}

// TestPollClaudeMCPAfterOAuth_FailedFlipEmitsFailure asserts the
// failure path: the poller sees failed on the first tick, Puts the
// failure into the cache, and emits mcp:oauth-completed{success:false}
// carrying the verbatim error string from the provider.
// emitErrorToThread routes through triage (nil in this test) and is
// asserted separately by triage-wired coverage; here we pin the wire
// payload only.
func TestPollClaudeMCPAfterOAuth_FailedFlipEmitsFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "broken", Status: "failed", Error: "connection refused"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"broken",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "broken"})
	if !ok {
		t.Fatal("expected broken entry in cache after failed flip")
	}
	if cached.Status != mcpstatus.StatusFailed {
		t.Errorf("status = %q, want %q", cached.Status, mcpstatus.StatusFailed)
	}
	if cached.Error != "connection refused" {
		t.Errorf("error = %q, want %q", cached.Error, "connection refused")
	}

	emissions := snapshot()
	if len(emissions) != 1 {
		t.Fatalf("expected 1 mcp:oauth-completed, got %d: %+v", len(emissions), emissions)
	}
	payload, _ := emissions[0].data.(map[string]any)
	if payload["success"] != false {
		t.Errorf("success = %v, want false", payload["success"])
	}
	if payload["error"] != "connection refused" {
		t.Errorf("error = %v, want connection refused", payload["error"])
	}
}

// TestPollClaudeMCPAfterOAuth_PendingThenConnected confirms that
// pending/starting states are intermediate (keep polling) — not
// terminal. The CLI may briefly emit "pending" after OAuth completes
// while the now-credentialed client warms up; an over-eager exit
// condition (anything except needs-auth) would miss the actual
// connected flip.
func TestPollClaudeMCPAfterOAuth_PendingThenConnected(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "github", Status: "pending"}},
			{{Name: "github", Status: "starting"}},
			{{Name: "github", Status: "connected"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"github",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "github"})
	if !ok || cached.Status != mcpstatus.StatusConnected {
		t.Fatalf("expected connected, got cached=%+v ok=%v", cached, ok)
	}
	if len(snapshot()) != 1 {
		t.Errorf("expected one mcp:oauth-completed emission, got %d", len(snapshot()))
	}
}

// TestPollClaudeMCPAfterOAuth_MissingEntryKeepsPolling asserts that a
// server absent from the response is treated as "wait and retry," not
// a terminal state. The Claude CLI builds mcp_status from three
// in-memory client pools; a server configured but not yet attempted
// can be missing.
func TestPollClaudeMCPAfterOAuth_MissingEntryKeepsPolling(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{},                                     // empty
			{{Name: "other", Status: "connected"}}, // ours still absent
			{{Name: "linear", Status: "connected"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"linear",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "linear"})
	if !ok || cached.Status != mcpstatus.StatusConnected {
		t.Fatalf("expected connected on third tick, got cached=%+v ok=%v", cached, ok)
	}
	if len(snapshot()) != 1 {
		t.Errorf("expected one mcp:oauth-completed emission, got %d", len(snapshot()))
	}
}

// TestPollClaudeMCPAfterOAuth_TimeoutNoEmission asserts the budget-
// exhausted path: every tick returns needs-auth, the loop walks the
// full interval list, no mcp:oauth-completed fires, no cache write.
// The prior cache entry (whatever it was) stays intact — the user can
// hit Refresh manually.
func TestPollClaudeMCPAfterOAuth_TimeoutNoEmission(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed", "mcp:status")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "stuck", Status: "needs-auth"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"stuck",
		zeroIntervals(6),
		getQuerier,
	)

	if _, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "stuck"}); ok {
		t.Error("cache should not be populated when poll budget exhausts on needs-auth")
	}
	var oauthEvents int
	for _, e := range snapshot() {
		if e.name == "mcp:oauth-completed" {
			oauthEvents++
		}
	}
	if oauthEvents != 0 {
		t.Errorf("expected no mcp:oauth-completed on timeout, got %d", oauthEvents)
	}
}

// TestPollClaudeMCPAfterOAuth_QueryErrorKeepsPolling asserts that a
// transient query error (CLI unreachable, decode failure, timeout
// mid-poll) is non-fatal: the loop continues to the next tick and
// can still observe the flip if it succeeds.
func TestPollClaudeMCPAfterOAuth_QueryErrorKeepsPolling(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			nil,
			{{Name: "github", Status: "connected"}},
		},
		[]error{errors.New("transient: control_request timeout"), nil},
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"github",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "github"})
	if !ok || cached.Status != mcpstatus.StatusConnected {
		t.Fatalf("expected connected after recovering from transient error, got cached=%+v ok=%v", cached, ok)
	}
	if len(snapshot()) != 1 {
		t.Errorf("expected one mcp:oauth-completed, got %d", len(snapshot()))
	}
}

// TestPollClaudeMCPAfterOAuth_SessionGoneExitsCleanly asserts that the
// getQuerier closure returning nil (live session torn down between
// TriggerMcpAuth and the first poll tick) terminates the loop without
// emissions or cache writes.
func TestPollClaudeMCPAfterOAuth_SessionGoneExitsCleanly(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed", "mcp:status")

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"sentry",
		zeroIntervals(6),
		func() claudeMCPStatusQuerier { return nil },
	)

	if _, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "sentry"}); ok {
		t.Error("cache should not be touched when session is gone")
	}
	if got := len(snapshot()); got != 0 {
		t.Errorf("expected no emissions, got %d", got)
	}
}

// TestPollClaudeMCPAfterOAuth_UnknownRawStatusKeepsPolling pins
// future-CLI-status-string drift behavior: if the projector returns
// StatusUnknown (a raw status the projector doesn't recognise),
// the loop must NOT treat that as terminal — keep polling. A
// regression that added StatusUnknown to the terminal switch would
// silently break OAuth detection any time the CLI added a new
// raw status value.
func TestPollClaudeMCPAfterOAuth_UnknownRawStatusKeepsPolling(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "future", Status: "weird-future-state"}},
			{{Name: "future", Status: "connected"}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"future",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "future"})
	if !ok || cached.Status != mcpstatus.StatusConnected {
		t.Fatalf("expected connected on tick 2, got cached=%+v ok=%v", cached, ok)
	}
	if len(snapshot()) != 1 {
		t.Errorf("expected one mcp:oauth-completed (only on connected tick), got %d", len(snapshot()))
	}
}

// TestPollClaudeMCPAfterOAuth_QuerierRebindsBetweenTicks pins the
// closure contract: getQuerier is re-invoked every tick so a
// session that dies AND a new session that takes its place between
// ticks can each be observed. A regression that captured the
// querier once at entry would not survive a mid-poll session swap.
func TestPollClaudeMCPAfterOAuth_QuerierRebindsBetweenTicks(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	// First call returns an "old session that errored." Second call
	// (after the rebind) returns a "fresh session that sees
	// connected." If the closure captured only the first querier,
	// the second tick would still error and the poll would never
	// detect the flip.
	var calls int
	getQuerier := func() claudeMCPStatusQuerier {
		calls++
		switch calls {
		case 1:
			return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
				return nil, errors.New("old session reset by peer")
			}
		default:
			return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
				return []claude.MCPServerStatus{{Name: "rebound", Status: "connected"}}, nil
			}
		}
	}

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"rebound",
		zeroIntervals(6),
		getQuerier,
	)

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "rebound"})
	if !ok || cached.Status != mcpstatus.StatusConnected {
		t.Fatalf("expected connected after session rebind, got cached=%+v ok=%v", cached, ok)
	}
	if len(snapshot()) != 1 {
		t.Errorf("expected one mcp:oauth-completed, got %d", len(snapshot()))
	}
	if calls < 2 {
		t.Errorf("expected getQuerier called at least twice, got %d", calls)
	}
}

// TestPollClaudeMCPAfterOAuth_ShutdownGuardSuppressesTerminal pins
// the drainTriage race fix: appCancel runs in Shutdown step 1b
// BEFORE drainTriage. If ctx flipped to Done() between the query
// returning a terminal status and the side effects running, the
// terminal branch must NOT Put into the cache, emit
// mcp:oauth-completed, or call emitErrorToThread — those routes
// touch a SQLite store that's about to close. Both Connected and
// Failed branches share the guard at the top of the case; cover
// both via subtests so a regression that scoped the guard to one
// branch wouldn't slip past. Failed additionally exercises
// emitErrorToThread, the most dangerous triage-touching path.
func TestPollClaudeMCPAfterOAuth_ShutdownGuardSuppressesTerminal(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{name: "Connected", status: "connected"},
		{name: "Failed", status: "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			snapshot := captureOrderedEmissions(app, "mcp:oauth-completed", "mcp:status")

			ctx, cancel := context.WithCancel(context.Background())

			app.pollClaudeMCPAfterOAuth(
				ctx,
				"thread-1",
				"late",
				zeroIntervals(6),
				func() claudeMCPStatusQuerier {
					return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
						// Simulate Shutdown step 1b running mid-query: by
						// the time the querier returns, ctx is canceled
						// and the terminal-status branch must observe
						// ctx.Err().
						cancel()
						return []claude.MCPServerStatus{{Name: "late", Status: tc.status}}, nil
					}
				},
			)

			if _, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "late"}); ok {
				t.Error("cache must not be populated after ctx is canceled")
			}
			if got := len(snapshot()); got != 0 {
				t.Errorf("expected zero emissions after ctx cancel, got %d", got)
			}
		})
	}
}

// TestPollClaudeMCPAfterOAuth_ErrorIsSanitized pins the security
// fix: a CLI error string longer than the 256B limit, or one
// carrying embedded newlines, is bounded + flattened before it
// reaches the wire payload. A regression that removed
// sanitizeMCPError would expose unfiltered child-process output
// (potentially leaking env / credentials in a CLI panic) to
// LAN-attached subscribers since mcp:oauth-completed is not in the
// loopback-only channel list.
func TestPollClaudeMCPAfterOAuth_ErrorIsSanitized(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	longErr := "boom\nline2\rline3 " + strings.Repeat("x", 300)
	getQuerier := scriptedQuerier(
		[][]claude.MCPServerStatus{
			{{Name: "noisy", Status: "failed", Error: longErr}},
		},
		nil,
	)

	app.pollClaudeMCPAfterOAuth(
		context.Background(),
		"thread-1",
		"noisy",
		zeroIntervals(6),
		getQuerier,
	)

	events := snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 mcp:oauth-completed, got %d", len(events))
	}
	payload, _ := events[0].data.(map[string]any)
	gotErr, _ := payload["error"].(string)
	if strings.ContainsAny(gotErr, "\n\r") {
		t.Errorf("sanitized error must not contain newlines, got %q", gotErr)
	}
	if len(gotErr) > 300 {
		t.Errorf("sanitized error must be bounded (~256 + ellipsis), got %d bytes", len(gotErr))
	}
	if !strings.HasSuffix(gotErr, "…(truncated)") {
		t.Errorf("sanitized long error must end with truncation marker, got %q", gotErr)
	}

	cached, ok := app.mcpStatus().Get(mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "noisy"})
	if !ok {
		t.Fatal("expected cache entry after failed terminal")
	}
	if cached.Error != gotErr {
		t.Errorf("cache Error must match wire payload: cache=%q wire=%q", cached.Error, gotErr)
	}
}

// TestStartClaudeMCPOAuthPoll_DedupCancelsPrior pins the dedup
// contract: spam-clicking Sign In on the same server only ever
// keeps the most recent poller alive. The prior call's goroutine
// is cancelled — verified via its registered cancel func — and
// only the latest goroutine remains registered in the dedup map.
// Without the dedup, two concurrent polls would each Put + emit
// independently when the OAuth eventually flips, producing dupe
// events on the mcp:oauth-completed channel.
func TestStartClaudeMCPOAuthPoll_DedupCancelsPrior(t *testing.T) {
	app := newTestAppWithStore(t)
	// Sentinel session that never returns a terminal status — the
	// session has no live claude session so getQuerier returns nil
	// and the poll exits on the first tick. We don't care about the
	// poll result here; we care about the cancellation of the prior
	// registration. To get a long-running poll, install a stub:
	// since we can't easily fake the closure path through
	// startClaudeMCPOAuthPoll, drive pollClaudeMCPAfterOAuth
	// directly through the registration helper by calling
	// startClaudeMCPOAuthPoll twice in quick succession against
	// a never-completing thread.

	peek := func(name string) *claudeMCPOAuthPoll {
		app.claudeMCPOAuthPollsMu.Lock()
		defer app.claudeMCPOAuthPollsMu.Unlock()
		return app.claudeMCPOAuthPolls[name]
	}

	// Twice for "linear":
	app.startClaudeMCPOAuthPoll("thread-1", "linear")
	first := peek("linear")
	if first == nil {
		t.Fatal("first call must register the poll")
	}
	firstCancelFired := make(chan struct{})
	originalCancel := first.cancel
	// Wrapping the cancel func under the same lock guards the read
	// the second startClaudeMCPOAuthPoll will perform a moment later.
	app.claudeMCPOAuthPollsMu.Lock()
	first.cancel = func() {
		close(firstCancelFired)
		originalCancel()
	}
	app.claudeMCPOAuthPollsMu.Unlock()

	app.startClaudeMCPOAuthPoll("thread-1", "linear")

	select {
	case <-firstCancelFired:
	case <-time.After(time.Second):
		t.Fatal("prior poll's cancel must fire when a second startClaudeMCPOAuthPoll lands for the same name")
	}

	second := peek("linear")
	if second == nil {
		t.Fatal("second call must register a fresh poll under the same key")
	}
	if second == first {
		t.Error("second registration must be a fresh poll instance, not the prior one")
	}
}

// TestPollClaudeMCPAfterOAuth_ContextCancelExitsImmediately asserts
// that a canceled ctx is honored across the sleepCtx tick — important
// for app shutdown where the goroutine should not block on the last
// 13s interval.
func TestPollClaudeMCPAfterOAuth_ContextCancelExitsImmediately(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	app.pollClaudeMCPAfterOAuth(
		ctx,
		"thread-1",
		"any",
		// Non-zero intervals so a missing ctx-check would block here.
		[]time.Duration{500 * time.Millisecond, 500 * time.Millisecond},
		func() claudeMCPStatusQuerier {
			return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
				t.Fatal("query should not be called after ctx cancel")
				return nil, nil
			}
		},
	)

	if got := len(snapshot()); got != 0 {
		t.Errorf("expected no emissions on canceled ctx, got %d", got)
	}
}
