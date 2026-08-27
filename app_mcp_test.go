package main

import (
	"context"
	"encoding/json"
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
	"agent-overflow/internal/provider/codex"
)

// newMCPTestApp returns an App wired to temp Claude/Codex config
// files. The test injects the per-provider stores directly so the
// lazy default-path fallback in app_mcp.go never reads $HOME.
func newMCPTestApp(t *testing.T) (*App, string, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	claudePath := filepath.Join(t.TempDir(), "claude.json")
	codexPath := filepath.Join(t.TempDir(), "config.toml")
	app.mcp.claudeConfigStore = claudeconfig.New(claudePath)
	app.mcp.codexConfigStore = codexconfig.New(codexPath)
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

// writeClaudePluginFixture installs a fake enabled plugin with one MCP
// server into the claude home next to claudePath, so config listings
// can enumerate `plugin:<plugin>:<server>` without spawning anything.
// Safe to call repeatedly: settings.json and installed_plugins.json
// accumulate entries.
func writeClaudePluginFixture(t *testing.T, claudePath, plugin, server string) {
	t.Helper()
	home := filepath.Join(filepath.Dir(claudePath), ".claude")
	install := filepath.Join(home, "plugins", "cache", "market", plugin, "1.0.0")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir plugin install: %v", err)
	}
	writeJSONFixture(t, filepath.Join(install, ".mcp.json"), map[string]any{
		server: map[string]any{"command": "stub"},
	})

	pluginID := plugin + "@market"
	mergeJSONFixture(t, filepath.Join(home, "settings.json"), func(doc map[string]any) {
		enabled, _ := doc["enabledPlugins"].(map[string]any)
		if enabled == nil {
			enabled = map[string]any{}
		}
		enabled[pluginID] = true
		doc["enabledPlugins"] = enabled
	})
	mergeJSONFixture(t, filepath.Join(home, "plugins", "installed_plugins.json"), func(doc map[string]any) {
		doc["version"] = 2
		plugins, _ := doc["plugins"].(map[string]any)
		if plugins == nil {
			plugins = map[string]any{}
		}
		plugins[pluginID] = []any{map[string]any{"scope": "user", "installPath": install}}
		doc["plugins"] = plugins
	})
}

func writeJSONFixture(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func mergeJSONFixture(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse fixture %s: %v", path, err)
		}
	}
	mutate(doc)
	writeJSONFixture(t, path, doc)
}

// TestListWorkspaceMcpServers_Claude_UserAndPluginWithWorkspaceDisabledFlag
// asserts the config listing: user entries and installed-plugin
// servers both surface, the workspace's disabledMcpServers list flows
// through to Disabled (and the "disabled" status), claude.ai cloud
// connectors are filtered out, and rows are labeled Source "config".
// The disable is workspace-scoped: another workspace sees the same
// membership fully enabled.
func TestListWorkspaceMcpServers_Claude_UserAndPluginWithWorkspaceDisabledFlag(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudePluginFixture(t, claudePath, "foo", "bar")
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin", "args": ["--root", "/tmp"]}
  },
  "projects": {
    "/workspace/a": {"disabledMcpServers": ["fs", "plugin:foo:bar", "claude.ai Gmail"]},
    "/workspace/b": {}
  }
}`)

	gotA, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers /workspace/a: %v", err)
	}
	if len(gotA) != 2 {
		t.Fatalf("workspace A: want 2 entries (fs + plugin, claude.ai filtered), got %d (%#v)", len(gotA), gotA)
	}
	fs := findServer(gotA, "fs")
	if !fs.Disabled || fs.Scope != string(claudeconfig.SourceUser) {
		t.Errorf("workspace A fs: want disabled+user, got disabled=%v scope=%q", fs.Disabled, fs.Scope)
	}
	if fs.Status != string(mcpstatus.StatusDisabled) {
		t.Errorf("workspace A fs status = %q, want disabled", fs.Status)
	}
	if fs.Source != "config" {
		t.Errorf("workspace A fs source = %q, want config", fs.Source)
	}
	pl := findServer(gotA, "plugin:foo:bar")
	if pl.Name == "" || !pl.Disabled || pl.Scope != string(claudeconfig.SourcePlugin) {
		t.Errorf("workspace A plugin row = %#v, want disabled plugin-scope row", pl)
	}

	gotB, err := app.ListWorkspaceMcpServers("claude", "/workspace/b")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers /workspace/b: %v", err)
	}
	if fs := findServer(gotB, "fs"); fs.Disabled {
		t.Errorf("workspace B fs: want enabled, got disabled (disabled flag leaked across workspaces)")
	}
	if pl := findServer(gotB, "plugin:foo:bar"); pl.Name == "" || pl.Disabled {
		t.Errorf("workspace B plugin row = %#v, want enabled (plugins are installation-global; only A disabled it)", pl)
	}
}

// TestListWorkspaceMcpServers_Codex_ReadsGlobalEnabledFlag confirms the
// Codex `enabled = false` global flag flows through to Disabled, and
// workspacePath is ignored (Codex's flag is not workspace-scoped).
func TestListWorkspaceMcpServers_Codex_ReadsGlobalEnabledFlag(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
args = ["serve"]
enabled = false

[mcp_servers.linear]
url = "https://mcp.linear.app/api"
`)
	got, err := app.ListWorkspaceMcpServers("codex", "/any/workspace/ignored")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers codex: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if gh := findServer(got, "github"); !gh.Disabled || gh.Status != string(mcpstatus.StatusDisabled) {
		t.Errorf("github: want disabled, got %#v", gh)
	}
	if findServer(got, "linear").Disabled {
		t.Errorf("linear: want enabled, got disabled")
	}
}

func TestCodexSessionMCPRowsPrefersExplicitRuntimeOverRetainedStartupFailure(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, "[mcp_servers.github]\ncommand = \"gh-mcp\"\n")

	script := `#!/bin/bash
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
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-mcp\"}}}"
        echo '{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"failed","error":"stale failure"}}'
        continue
    fi
    if echo "$line" | grep -q '"method":"mcpServerStatus/list"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"data\":[{\"name\":\"github\",\"runtimeStatus\":\"connected\",\"authStatus\":\"oAuth\",\"serverInfo\":{\"name\":\"github\",\"version\":\"1\"},\"tools\":{\"issues_list\":{}}}],\"nextCursor\":null}}"
    fi
done
`
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex: %v", err)
	}
	sess, err := codex.NewSession(context.Background(), "ao-thread", codex.Config{
		Binary: binary, Model: "test-model", WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	deadline := time.Now().Add(time.Second)
	for sess.MCPStartupStates()["github"].State == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sess.MCPStartupStates()["github"].State; got != "failed" {
		t.Fatalf("retained startup state = %q, want failed", got)
	}

	rows, err := app.codexSessionMCPRows(context.Background(), sess)
	if err != nil {
		t.Fatalf("codexSessionMCPRows: %v", err)
	}
	github := findServer(rows, "github")
	if github.Status != string(mcpstatus.StatusConnected) || github.Error != "" {
		t.Fatalf("github row = %+v, want explicit connected runtime without stale error", github)
	}
}

// TestListWorkspaceMcpServers_UnsupportedProvider returns ErrMCPProviderUnsupported.
func TestListWorkspaceMcpServers_UnsupportedProvider(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	_, err := app.ListWorkspaceMcpServers("gemini", "")
	if !errors.Is(err, ErrMCPProviderUnsupported) {
		t.Fatalf("want ErrMCPProviderUnsupported, got %v", err)
	}
}

// TestListWorkspaceMcpServers_CacheStatusOverlay pins the fallback row
// shaping: an enabled server picks up the cached connection status and
// tool names, while a disabled server reports "disabled" even when a
// stale cache entry claims it was connected.
func TestListWorkspaceMcpServers_CacheStatusOverlay(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"

[mcp_servers.linear]
url = "https://mcp.linear.app/api"
enabled = false
`)
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "github"},
		Status: mcpstatus.StatusConnected,
		Tools:  []string{"issues_list", "pr_read"},
		Source: mcpstatus.SourceLiveSession,
	})
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "linear"},
		Status: mcpstatus.StatusConnected,
		Source: mcpstatus.SourceLiveSession,
	})

	got, err := app.ListWorkspaceMcpServers("codex", "")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	gh := findServer(got, "github")
	if gh.Status != string(mcpstatus.StatusConnected) {
		t.Errorf("github status = %q, want connected from cache", gh.Status)
	}
	if len(gh.Tools) != 2 {
		t.Errorf("github tools = %v, want cached tool names", gh.Tools)
	}
	if ln := findServer(got, "linear"); ln.Status != string(mcpstatus.StatusDisabled) {
		t.Errorf("linear status = %q, want disabled (config wins over stale cache)", ln.Status)
	}
}

// TestListWorkspaceMcpServers_Claude_PluginRowsFromManifests pins the
// plugin-visibility contract for the no-session view: membership comes
// from plugin manifests (enabledPlugins + installed_plugins.json), so
// the rows exist with zero spawns and no cache warm-up; the status
// cache only overlays connection state. A cross-workspace "disabled"
// cache entry degrades to unknown instead of contradicting the enabled
// config, and a config-disabled plugin keeps its disabled row no
// matter what the cache claims.
func TestListWorkspaceMcpServers_Claude_PluginRowsFromManifests(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudePluginFixture(t, claudePath, "playwright", "playwright")
	writeClaudePluginFixture(t, claudePath, "other", "x")
	writeClaudePluginFixture(t, claudePath, "off", "svc")
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {},
  "projects": {
    "/workspace/a": {"disabledMcpServers": ["plugin:off:svc"]}
  }
}`)

	// No cache at all: rows still exist (the startup-badge case).
	cold, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers cold: %v", err)
	}
	if len(cold) != 3 {
		t.Fatalf("cold: want 3 manifest rows, got %d (%#v)", len(cold), cold)
	}
	if pw := findServer(cold, "plugin:playwright:playwright"); pw.Disabled || pw.Status != string(mcpstatus.StatusUnknown) {
		t.Errorf("cold playwright row = %#v, want enabled with unknown status", pw)
	}

	put := func(name string, status mcpstatus.Status, tools ...string) {
		app.mcpStatus().Put(mcpstatus.ServerStatus{
			Key:    mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name},
			Status: status,
			Tools:  tools,
			Source: mcpstatus.SourceEphemeralFetch,
		})
	}
	put("plugin:playwright:playwright", mcpstatus.StatusConnected, "browser_click")
	put("plugin:other:x", mcpstatus.StatusDisabled)
	put("plugin:off:svc", mcpstatus.StatusConnected)

	got, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	pw := findServer(got, "plugin:playwright:playwright")
	if pw.Disabled || pw.Source != "config" || pw.Scope != string(claudeconfig.SourcePlugin) {
		t.Fatalf("playwright row = %#v, want enabled config-sourced plugin row", pw)
	}
	if pw.Status != string(mcpstatus.StatusConnected) || len(pw.Tools) != 1 {
		t.Errorf("playwright row = %#v, want cached connected status + tools", pw)
	}
	if other := findServer(got, "plugin:other:x"); other.Disabled || other.Status != string(mcpstatus.StatusUnknown) {
		t.Errorf("plugin:other:x = %#v, want enabled row with unknown status (cross-workspace disabled cache entry)", other)
	}
	if off := findServer(got, "plugin:off:svc"); !off.Disabled || off.Status != string(mcpstatus.StatusDisabled) {
		t.Errorf("plugin:off:svc = %#v, want config-disabled row (cache must not resurrect it)", off)
	}
}

// TestListWorkspaceMcpServers_Claude_CacheOnlyNamesNeverCreateRows pins
// the no-leak contract: the status cache is a status overlay, never
// membership. Cached names with no config/manifest definition — bare
// names from another workspace's .mcp.json, plugin-qualified names
// from an uninstalled plugin, cloud connectors — must not appear.
func TestListWorkspaceMcpServers_Claude_CacheOnlyNamesNeverCreateRows(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{"mcpServers": {}, "projects": {}}`)
	for _, name := range []string{"context7", "plugin:playwright:playwright", "claude.ai Gmail"} {
		app.mcpStatus().Put(mcpstatus.ServerStatus{
			Key:    mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: name},
			Status: mcpstatus.StatusConnected,
			Source: mcpstatus.SourceEphemeralFetch,
		})
	}
	got, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cache-only names leaked into the listing: %#v", got)
	}
}

// TestListWorkspaceMcpServers_Claude_HidesOrphanDisabledOnlyNames pins
// the orphan filter: a disabledMcpServers name with no definition
// anywhere (no user/local entry, no installed plugin) is a leftover
// Claude Code itself doesn't list — AO must not invent a row for it,
// and a cache entry for the name (another workspace's project-scope
// server) must not resurrect it either.
func TestListWorkspaceMcpServers_Claude_HidesOrphanDisabledOnlyNames(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "projects": {
    "/workspace/a": {"disabledMcpServers": ["code-index", "plugin:foo:bar"]}
  }
}`)
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "code-index"},
		Status: mcpstatus.StatusConnected,
		Source: mcpstatus.SourceEphemeralFetch,
	})

	got, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("orphan disabled-only names should be hidden, got %#v", got)
	}
}

// TestListWorkspaceMcpServers_Claude_ListsLocalScopeServers pins the
// local-scope surface: servers added with `claude mcp add --scope
// local` live under projects.<workspace>.mcpServers and belong only to
// that workspace's listing.
func TestListWorkspaceMcpServers_Claude_ListsLocalScopeServers(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {},
  "projects": {
    "/workspace/a": {
      "mcpServers": {"jira": {"type": "stdio", "command": "jira-bin"}},
      "disabledMcpServers": []
    },
    "/workspace/b": {}
  }
}`)

	gotA, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers /workspace/a: %v", err)
	}
	jira := findServer(gotA, "jira")
	if jira.Name == "" || jira.Disabled || jira.Scope != string(claudeconfig.SourceLocal) {
		t.Fatalf("jira = %#v, want enabled local-scope row", jira)
	}

	gotB, err := app.ListWorkspaceMcpServers("claude", "/workspace/b")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers /workspace/b: %v", err)
	}
	if leaked := findServer(gotB, "jira"); leaked.Name != "" {
		t.Errorf("local-scope jira leaked into workspace B: %#v", leaked)
	}
}

// TestListWorkspaceMcpServers_StaleStatusOverlayFlagged pins the
// staleness contract: when a status entry expires past the TTL, the
// row keeps its last-known status marked Stale=true (membership is
// config-derived and never lapses), so the frontend chains a
// background refresh instead of trusting or dropping it.
func TestListWorkspaceMcpServers_StaleStatusOverlayFlagged(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudePluginFixture(t, claudePath, "pw", "pw")
	writeClaudeConfig(t, claudePath, `{"mcpServers": {}, "projects": {}}`)
	now := time.Now()
	app.mcp.statusCacheOnce.Do(func() {})
	app.mcp.statusCache = mcpstatus.NewWith(30*time.Second, nil, func() time.Time { return now })
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    mcpstatus.Key{Provider: mcpstatus.ProviderClaude, Name: "plugin:pw:pw"},
		Status: mcpstatus.StatusConnected,
		Tools:  []string{"browser_click"},
		Source: mcpstatus.SourceEphemeralFetch,
	})

	fresh, err := app.ListWorkspaceMcpServers("claude", "/w")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers fresh: %v", err)
	}
	if row := findServer(fresh, "plugin:pw:pw"); row.Name == "" || row.Stale {
		t.Fatalf("fresh row = %#v, want present and not stale", row)
	}

	now = now.Add(2 * time.Minute) // expire the entry
	stale, err := app.ListWorkspaceMcpServers("claude", "/w")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers stale: %v", err)
	}
	row := findServer(stale, "plugin:pw:pw")
	if row.Name == "" {
		t.Fatalf("row vanished — membership is config-derived and must survive the status TTL")
	}
	if !row.Stale || row.Status != string(mcpstatus.StatusConnected) {
		t.Errorf("stale row = %#v, want Stale=true with last-known connected status", row)
	}
}

// TestListWorkspaceMcpServers_Claude_ProjectScopeFromMcpJSON pins the
// .mcp.json surface: a workspace's .mcp.json names project-scope
// servers that AO's non-interactive sessions load, so they render as
// enabled project rows — unless the workspace's disabledMcpServers
// names them.
func TestListWorkspaceMcpServers_Claude_ProjectScopeFromMcpJSON(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(`{"mcpServers": {"context7": {"command": "npx"}, "gone": {"command": "x"}}}`), 0o644); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}
	writeClaudeConfig(t, claudePath, fmt.Sprintf(`{
  "mcpServers": {},
  "projects": {%q: {"disabledMcpServers": ["gone"]}}
}`, workspace))

	got, err := app.ListWorkspaceMcpServers("claude", workspace)
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	c7 := findServer(got, "context7")
	if c7.Name == "" || c7.Disabled || c7.Scope != string(claudeconfig.SourceProject) {
		t.Errorf("context7 = %#v, want enabled project-scope row", c7)
	}
	if gone := findServer(got, "gone"); gone.Name == "" || !gone.Disabled {
		t.Errorf("gone = %#v, want disabled via disabledMcpServers", gone)
	}
}

// TestListWorkspaceMcpServers_Codex_IgnoresCacheOnlyNames pins the
// no-leak contract for Codex: config.toml enumerates the global server
// set completely, so a cached name it doesn't carry is a project-layer
// server from whatever cwd fed the cache — it must not appear in a
// workspace config listing.
func TestListWorkspaceMcpServers_Codex_IgnoresCacheOnlyNames(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
`)
	app.mcpStatus().Put(mcpstatus.ServerStatus{
		Key:    mcpstatus.Key{Provider: mcpstatus.ProviderCodex, Name: "project-layer"},
		Status: mcpstatus.StatusConnected,
		Tools:  []string{"do_thing"},
		Source: mcpstatus.SourceLiveSession,
	})

	got, err := app.ListWorkspaceMcpServers("codex", "")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "github" {
		t.Fatalf("want only the config-named github row, got %#v", got)
	}
}

// TestListThreadMcpServers_NoSession_FallsBackToWorkspaceConfig pins
// the fallback contract: with no live session, the thread listing is
// exactly the workspace config view for the thread's provider and
// workspace path.
func TestListThreadMcpServers_NoSession_FallsBackToWorkspaceConfig(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, fmt.Sprintf(`{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  },
  "projects": {
    %q: {"disabledMcpServers": ["fs"]}
  }
}`, workspace))
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-sonnet-4-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if app.hasActiveSession(thread.ID) {
		t.Fatalf("precondition: no session expected")
	}

	got, err := app.ListThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadMcpServers: %v", err)
	}
	fs := findServer(got, "fs")
	if fs.Name == "" || !fs.Disabled || fs.Source != "config" {
		t.Fatalf("fs row = %#v, want disabled config-sourced row", fs)
	}
}

// TestSetWorkspaceMcpServerEnabled_Claude_TogglesWorkspaceDisabledList
// exercises the load-bearing semantic: Disabled is workspace-scoped for
// Claude, so the toggle writes to that workspace's projects entry — the
// same `disabledMcpServers` list the CLI's own mcp_toggle persists to.
func TestSetWorkspaceMcpServerEnabled_Claude_TogglesWorkspaceDisabledList(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)

	if err := app.SetWorkspaceMcpServerEnabled("claude", "/workspace/a", "fs", false); err != nil {
		t.Fatalf("SetWorkspaceMcpServerEnabled disable: %v", err)
	}
	got, err := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers post-disable: %v", err)
	}
	if !findServer(got, "fs").Disabled {
		t.Errorf("workspace /workspace/a fs: want disabled after toggle")
	}

	otherWorkspace, err := app.ListWorkspaceMcpServers("claude", "/workspace/b")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers /workspace/b: %v", err)
	}
	if findServer(otherWorkspace, "fs").Disabled {
		t.Errorf("workspace /workspace/b fs: want enabled (disable leaked across workspaces)")
	}

	if err := app.SetWorkspaceMcpServerEnabled("claude", "/workspace/a", "fs", true); err != nil {
		t.Fatalf("SetWorkspaceMcpServerEnabled enable: %v", err)
	}
	got2, _ := app.ListWorkspaceMcpServers("claude", "/workspace/a")
	if findServer(got2, "fs").Disabled {
		t.Errorf("workspace /workspace/a fs: want enabled after re-toggle")
	}
}

// TestSetWorkspaceMcpServerEnabled_Claude_RequiresWorkspacePath: the
// disabledMcpServers list is keyed by workspace path — a blank key
// would write toggle state nowhere any session reads from.
func TestSetWorkspaceMcpServerEnabled_Claude_RequiresWorkspacePath(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	if err := app.SetWorkspaceMcpServerEnabled("claude", "  ", "fs", false); err == nil {
		t.Fatal("expected error for blank workspace path")
	}
}

// TestSetThreadMcpServerEnabled_Claude_NoSession_WritesWorkspaceConfig
// confirms the no-live-session Claude path writes the thread's
// workspace-scoped disabled list directly.
func TestSetThreadMcpServerEnabled_Claude_NoSession_WritesWorkspaceConfig(t *testing.T) {
	app, claudePath, _ := newMCPTestApp(t)
	workspace := t.TempDir()
	writeClaudeConfig(t, claudePath, `{
  "mcpServers": {
    "fs": {"type": "stdio", "command": "fs-bin"}
  }
}`)
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-sonnet-4-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	if err := app.SetThreadMcpServerEnabled(thread.ID, "fs", false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled disable: %v", err)
	}
	got, err := app.ListWorkspaceMcpServers("claude", workspace)
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
	}
	if !findServer(got, "fs").Disabled {
		t.Errorf("fs: want disabled in thread workspace after toggle")
	}
}

// TestSetThreadMcpServerEnabled_Codex_TogglesGlobalFlag ensures the
// binding writes the global `enabled` field (Codex has no per-workspace
// MCP scoping) and completes cleanly with no live session.
func TestSetThreadMcpServerEnabled_Codex_TogglesGlobalFlag(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.github]
command = "gh-mcp"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), "/workspace/a", "gpt-5.2", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if app.hasActiveSession(thread.ID) {
		t.Fatalf("precondition: no session expected")
	}

	if err := app.SetThreadMcpServerEnabled(thread.ID, "github", false); err != nil {
		t.Fatalf("SetThreadMcpServerEnabled disable: %v", err)
	}
	raw, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("expected `enabled = false` in toml after disable, got:\n%s", raw)
	}

	listed, err := app.ListWorkspaceMcpServers("codex", "")
	if err != nil {
		t.Fatalf("ListWorkspaceMcpServers: %v", err)
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

// findServer is a tiny test helper that scans for a ThreadMCPServer by
// name. Returns the zero value if missing.
func findServer(in []ThreadMCPServer, name string) ThreadMCPServer {
	for _, s := range in {
		if s.Name == name {
			return s
		}
	}
	return ThreadMCPServer{}
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

// TestPollClaudeMCPAfterOAuth_TimeoutEmitsNotConfirmed asserts the
// budget-exhausted path: every tick returns needs-auth, the loop walks
// the full interval list, and the tail then (1) invalidates the cache
// key — the unknown sentinel is what makes every attached pane re-list,
// so a sign-in that completed after the poll gave up still flips the
// row via the live-session re-list — and (2) emits
// mcp:oauth-completed{success:false, timedOut:true}. No cache WRITE
// happens: the poll has no provider truth to record, so it must not
// manufacture an entry. Before the tail existed the poll ended in
// silence and a slow success left "Needs sign-in" rendering over a
// connected server.
func TestPollClaudeMCPAfterOAuth_TimeoutEmitsNotConfirmed(t *testing.T) {
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
	var oauth []capturedEmission
	var statusSentinels int
	for _, e := range snapshot() {
		switch e.name {
		case "mcp:oauth-completed":
			oauth = append(oauth, e)
		case "mcp:status":
			statusSentinels++
		}
	}
	if len(oauth) != 1 {
		t.Fatalf("expected 1 mcp:oauth-completed on timeout, got %d", len(oauth))
	}
	payload, _ := oauth[0].data.(map[string]any)
	if payload["success"] != false {
		t.Errorf("success = %v, want false", payload["success"])
	}
	if payload["timedOut"] != true {
		t.Errorf("timedOut = %v, want true", payload["timedOut"])
	}
	if payload["serverName"] != "stuck" {
		t.Errorf("serverName = %v, want stuck", payload["serverName"])
	}
	if statusSentinels != 1 {
		t.Errorf("expected 1 mcp:status invalidation sentinel, got %d", statusSentinels)
	}
}

// TestPollClaudeMCPAfterOAuth_ShutdownGuardSuppressesTimeout is the
// exhaustion-tail sibling of ShutdownGuardSuppressesTerminal: if ctx
// flips to Done() between the last tick's query and the tail, nothing
// may emit or touch triage — and a canceled poll must never report a
// timeout verdict, because cancellation means a superseding Sign In
// click or shutdown owns the flow now.
func TestPollClaudeMCPAfterOAuth_ShutdownGuardSuppressesTimeout(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, "mcp:oauth-completed", "mcp:status")

	ctx, cancel := context.WithCancel(context.Background())
	ticks := 0
	app.pollClaudeMCPAfterOAuth(
		ctx,
		"thread-1",
		"slow",
		zeroIntervals(3),
		func() claudeMCPStatusQuerier {
			return func(ctx context.Context) ([]claude.MCPServerStatus, error) {
				ticks++
				if ticks == 3 {
					// Cancel lands while the LAST query is in flight, so
					// the loop still exits by exhaustion (ctxutil.Sleep
					// never sees the cancel) and only the tail's own
					// guard stands between it and the emissions.
					cancel()
				}
				return []claude.MCPServerStatus{{Name: "slow", Status: "needs-auth"}}, nil
			}
		},
	)

	if ticks != 3 {
		t.Fatalf("expected the loop to exhaust all 3 ticks, got %d", ticks)
	}
	if got := len(snapshot()); got != 0 {
		t.Errorf("expected zero emissions from a canceled poll, got %d", got)
	}
}

// TestDefaultClaudeMCPOAuthIntervals_Shape pins the schedule: the fib
// ramp keeps fast flows fast, and the trailing cadence extends the
// total budget to at least 5 minutes so a slow IdP hop is still
// confirmed. A regression back to the original ~32s budget recreated
// exactly the stale-"Needs sign-in" gap the exhaustion tail closed.
func TestDefaultClaudeMCPOAuthIntervals_Shape(t *testing.T) {
	ramp := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
		13 * time.Second,
	}
	if len(defaultClaudeMCPOAuthIntervals) < len(ramp) {
		t.Fatalf("schedule shorter than the ramp: %d ticks", len(defaultClaudeMCPOAuthIntervals))
	}
	for i, want := range ramp {
		if defaultClaudeMCPOAuthIntervals[i] != want {
			t.Errorf("tick %d = %s, want %s", i, defaultClaudeMCPOAuthIntervals[i], want)
		}
	}
	var total time.Duration
	for _, d := range defaultClaudeMCPOAuthIntervals {
		total += d
	}
	if total < 5*time.Minute {
		t.Errorf("total budget = %s, want >= 5m", total)
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
		app.mcp.claudeOAuthPollsMu.Lock()
		defer app.mcp.claudeOAuthPollsMu.Unlock()
		return app.mcp.claudeOAuthPolls[name]
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
	app.mcp.claudeOAuthPollsMu.Lock()
	first.cancel = func() {
		close(firstCancelFired)
		originalCancel()
	}
	app.mcp.claudeOAuthPollsMu.Unlock()

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
