package claudeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluginFixture wires a Store whose home dir carries a settings.json,
// an installed_plugins.json, and per-plugin manifest files.
type pluginFixture struct {
	store *Store
	home  string
}

func newPluginFixture(t *testing.T, settingsJSON, installedJSON string) pluginFixture {
	t.Helper()
	dir := t.TempDir()
	store := New(filepath.Join(dir, "claude.json"))
	home := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(filepath.Join(home, "plugins"), 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	if settingsJSON != "" {
		writeFile(t, filepath.Join(home, "settings.json"), settingsJSON)
	}
	if installedJSON != "" {
		writeFile(t, filepath.Join(home, "plugins", "installed_plugins.json"), installedJSON)
	}
	return pluginFixture{store: store, home: home}
}

func (f pluginFixture) installPlugin(t *testing.T, name, mcpJSON string) string {
	t.Helper()
	dir := filepath.Join(f.home, "plugins", "cache", "market", name, "1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	if mcpJSON != "" {
		writeFile(t, filepath.Join(dir, ".mcp.json"), mcpJSON)
	}
	return dir
}

func pluginNames(servers []Server) []string {
	names := make([]string, 0, len(servers))
	for _, srv := range servers {
		names = append(names, srv.Name)
	}
	return names
}

func TestPluginServers_enabledPluginsGateManifests(t *testing.T) {
	f := newPluginFixture(t, `{
  "enabledPlugins": {
    "pw@market": true,
    "ctx@market": false,
    "ghost@market": true
  }
}`, "")
	pwDir := f.installPlugin(t, "pw", `{"playwright": {"command": "npx", "args": ["secret-arg"]}}`)
	ctxDir := f.installPlugin(t, "ctx", `{"context7": {"command": "npx"}}`)
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {
    "pw@market": [{"scope": "user", "installPath": `+quoteJSON(pwDir)+`}],
    "ctx@market": [{"scope": "user", "installPath": `+quoteJSON(ctxDir)+`}]
  }
}`)

	got, err := f.store.pluginServers("/anywhere")
	if err != nil {
		t.Fatalf("pluginServers: %v", err)
	}
	names := pluginNames(got)
	if len(names) != 1 || names[0] != "plugin:pw:playwright" {
		t.Fatalf("names = %v, want only the enabled plugin's qualified server (ctx disabled, ghost not installed)", names)
	}
	if got[0].Source != SourcePlugin {
		t.Errorf("source = %q, want plugin", got[0].Source)
	}
	// Names only — a manifest's command/args never flow out.
	if got[0].Command != "" || len(got[0].Args) != 0 {
		t.Errorf("plugin row leaked config: %#v", got[0])
	}
}

func TestPluginServers_projectScopedInstallMatchesExactPath(t *testing.T) {
	f := newPluginFixture(t, `{"enabledPlugins": {"ralph@market": true}}`, "")
	dir := f.installPlugin(t, "ralph", `{"mcpServers": {"loop": {"command": "x"}}}`)
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {
    "ralph@market": [{"scope": "project", "projectPath": "/repos/orc", "installPath": `+quoteJSON(dir)+`}]
  }
}`)

	match, err := f.store.pluginServers("/repos/orc")
	if err != nil {
		t.Fatalf("pluginServers match: %v", err)
	}
	if names := pluginNames(match); len(names) != 1 || names[0] != "plugin:ralph:loop" {
		t.Fatalf("names = %v, want the project-scoped plugin for its own path (mcpServers wrapper honored)", names)
	}
	other, err := f.store.pluginServers("/repos/elsewhere")
	if err != nil {
		t.Fatalf("pluginServers other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("project-scoped plugin leaked to another workspace: %v", pluginNames(other))
	}
}

func TestPluginServers_workspaceSettingsOverrideUserSettings(t *testing.T) {
	f := newPluginFixture(t, `{"enabledPlugins": {"pw@market": false}}`, "")
	dir := f.installPlugin(t, "pw", `{"playwright": {"command": "npx"}}`)
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {"pw@market": [{"scope": "user", "installPath": `+quoteJSON(dir)+`}]}
}`)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(workspace, ".claude", "settings.local.json"), `{"enabledPlugins": {"pw@market": true}}`)

	got, err := f.store.pluginServers(workspace)
	if err != nil {
		t.Fatalf("pluginServers: %v", err)
	}
	if names := pluginNames(got); len(names) != 1 || names[0] != "plugin:pw:playwright" {
		t.Fatalf("names = %v, want local settings to re-enable the plugin", names)
	}
}

func TestPluginServers_manifestMcpServersForms(t *testing.T) {
	f := newPluginFixture(t, `{"enabledPlugins": {"multi@market": true}}`, "")
	dir := f.installPlugin(t, "multi", `{"from-mcp-json": {"command": "a"}}`)
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "extra.json"), `{"mcpServers": {"from-path": {"command": "b"}}}`)
	writeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{
  "name": "multi",
  "mcpServers": ["${CLAUDE_PLUGIN_ROOT}/extra.json", "bundle.mcpb"]
}`)
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {"multi@market": [{"scope": "user", "installPath": `+quoteJSON(dir)+`}]}
}`)

	got, err := f.store.pluginServers("/w")
	if err != nil {
		t.Fatalf("pluginServers: %v", err)
	}
	names := pluginNames(got)
	want := []string{"plugin:multi:from-mcp-json", "plugin:multi:from-path"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v (.mcpb reference skipped, path form resolved)", names, want)
	}
}

func TestPluginServers_malformedManifestFailsLoudly(t *testing.T) {
	f := newPluginFixture(t, `{"enabledPlugins": {"bad@market": true}}`, "")
	dir := f.installPlugin(t, "bad", `{not json`)
	writeFile(t, filepath.Join(f.home, "plugins", "installed_plugins.json"), `{
  "version": 2,
  "plugins": {"bad@market": [{"scope": "user", "installPath": `+quoteJSON(dir)+`}]}
}`)
	_, err := f.store.pluginServers("/w")
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("want a parse error naming the file, got %v", err)
	}
}

func quoteJSON(s string) string {
	// Test paths contain no characters needing JSON escaping beyond
	// quotes; keep the fixture readable.
	return `"` + s + `"`
}
