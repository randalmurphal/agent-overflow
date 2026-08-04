package claudeconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pluginServers enumerates the MCP servers that enabled plugins
// contribute to a session started in workspacePath, without spawning
// anything: `enabledPlugins` from the merged settings files gates which
// installed plugins load, `plugins/installed_plugins.json` locates each
// installation, and the plugin's own manifest names its servers. Rows
// use Claude's qualified `plugin:<plugin>:<server>` naming — the same
// name disabledMcpServers and mcp_toggle use.
//
// Missing files are normal empty answers; malformed ones are errors —
// a broken manifest also breaks the plugin in Claude Code itself, so
// surfacing the file name beats silently hiding servers.
func (s *Store) pluginServers(workspacePath string) ([]Server, error) {
	enabled, err := s.enabledPlugins(workspacePath)
	if err != nil {
		return nil, err
	}
	if len(enabled) == 0 {
		return nil, nil
	}
	installs, err := s.installedPlugins()
	if err != nil {
		return nil, err
	}
	var out []Server
	for pluginID, entries := range installs {
		if !enabled[pluginID] {
			continue
		}
		pluginName, _, ok := strings.Cut(pluginID, "@")
		if !ok || pluginName == "" {
			continue
		}
		installPath := ""
		for _, entry := range entries {
			if entry.relevantTo(workspacePath) {
				installPath = entry.InstallPath
				break
			}
		}
		if installPath == "" {
			continue
		}
		names, err := pluginServerNames(installPath)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", pluginID, err)
		}
		for _, name := range names {
			out = append(out, Server{
				Name:   "plugin:" + pluginName + ":" + name,
				Source: SourcePlugin,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// enabledPlugins merges the `enabledPlugins` map across the settings
// files Claude Code consults, in ascending precedence: user
// (<claude home>/settings.json), project
// (<workspace>/.claude/settings.json), then local
// (<workspace>/.claude/settings.local.json). A later source overrides
// per plugin id; `false` means installed-but-disabled.
func (s *Store) enabledPlugins(workspacePath string) (map[string]bool, error) {
	paths := []string{filepath.Join(s.home, "settings.json")}
	if workspacePath != "" {
		paths = append(paths,
			filepath.Join(workspacePath, ".claude", "settings.json"),
			filepath.Join(workspacePath, ".claude", "settings.local.json"),
		)
	}
	merged := make(map[string]bool)
	for _, path := range paths {
		var settings struct {
			EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
		}
		ok, err := readOptionalJSON(path, &settings)
		if err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		for id, raw := range settings.EnabledPlugins {
			// bool or a version-constraint array; only literal false disables.
			merged[id] = string(raw) != "false"
		}
	}
	return merged, nil
}

// pluginInstallEntry is one installation record in
// plugins/installed_plugins.json (v2 shape).
type pluginInstallEntry struct {
	Scope       string `json:"scope"`
	ProjectPath string `json:"projectPath"`
	InstallPath string `json:"installPath"`
}

// relevantTo mirrors the CLI's isInstallationRelevantToCurrentProject:
// user/managed installs apply everywhere; project/local installs only
// to their exact recorded path (the raw session cwd, not the canonical
// git root).
func (e pluginInstallEntry) relevantTo(workspacePath string) bool {
	return e.Scope == "user" || e.Scope == "managed" ||
		(e.ProjectPath != "" && e.ProjectPath == workspacePath)
}

func (s *Store) installedPlugins() (map[string][]pluginInstallEntry, error) {
	var file struct {
		Plugins map[string][]pluginInstallEntry `json:"plugins"`
	}
	if _, err := readOptionalJSON(filepath.Join(s.home, "plugins", "installed_plugins.json"), &file); err != nil {
		return nil, err
	}
	return file.Plugins, nil
}

// pluginServerNames reads the server names a plugin declares: its
// `.mcp.json` (either `{mcpServers: {...}}` or a bare name→config map)
// plus any `mcpServers` declaration in `.claude-plugin/plugin.json`
// (inline map, or path(s) to .mcp.json-style files, with
// ${CLAUDE_PLUGIN_ROOT} resolving to the install dir). `.mcpb` bundle
// references cannot be enumerated without unpacking the bundle and are
// skipped; their servers surface through live-session truth.
func pluginServerNames(installPath string) ([]string, error) {
	seen := make(map[string]struct{})
	if err := addMCPFileNames(filepath.Join(installPath, ".mcp.json"), seen); err != nil {
		return nil, err
	}

	var manifest struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	if _, err := readOptionalJSON(filepath.Join(installPath, ".claude-plugin", "plugin.json"), &manifest); err != nil {
		return nil, err
	}
	if len(manifest.MCPServers) > 0 {
		if err := addManifestMCPNames(installPath, manifest.MCPServers, seen); err != nil {
			return nil, err
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func addManifestMCPNames(installPath string, spec json.RawMessage, seen map[string]struct{}) error {
	switch {
	case len(spec) > 0 && spec[0] == '{':
		var inline map[string]json.RawMessage
		if err := json.Unmarshal(spec, &inline); err != nil {
			return fmt.Errorf("decode plugin.json mcpServers: %w", err)
		}
		for name := range inline {
			seen[name] = struct{}{}
		}
		return nil
	case len(spec) > 0 && spec[0] == '"':
		var path string
		if err := json.Unmarshal(spec, &path); err != nil {
			return fmt.Errorf("decode plugin.json mcpServers: %w", err)
		}
		return addManifestMCPPath(installPath, path, seen)
	case len(spec) > 0 && spec[0] == '[':
		var paths []string
		if err := json.Unmarshal(spec, &paths); err != nil {
			return fmt.Errorf("decode plugin.json mcpServers: %w", err)
		}
		for _, path := range paths {
			if err := addManifestMCPPath(installPath, path, seen); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("decode plugin.json mcpServers: unsupported shape %.20s", string(spec))
	}
}

func addManifestMCPPath(installPath, path string, seen map[string]struct{}) error {
	if strings.HasSuffix(path, ".mcpb") {
		return nil
	}
	path = strings.ReplaceAll(path, "${CLAUDE_PLUGIN_ROOT}", installPath)
	if !filepath.IsAbs(path) {
		path = filepath.Join(installPath, path)
	}
	return addMCPFileNames(path, seen)
}

// addMCPFileNames collects server names from a .mcp.json-style file:
// the keys of `mcpServers` when that wrapper is present, else the
// top-level keys. Missing file is a normal empty answer.
func addMCPFileNames(path string, seen map[string]struct{}) error {
	var parsed map[string]json.RawMessage
	ok, err := readOptionalJSON(path, &parsed)
	if err != nil || !ok {
		return err
	}
	if wrapped, has := parsed["mcpServers"]; has {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(wrapped, &inner); err != nil {
			return fmt.Errorf("decode %s: mcpServers: %w", path, err)
		}
		for name := range inner {
			seen[name] = struct{}{}
		}
		return nil
	}
	for name := range parsed {
		seen[name] = struct{}{}
	}
	return nil
}

// readOptionalJSON decodes path into v, reporting (false, nil) when
// the file does not exist and a path-naming error when it cannot be
// read or parsed.
func readOptionalJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}
