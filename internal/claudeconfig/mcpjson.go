package claudeconfig

import (
	"path/filepath"
	"sort"
)

// projectServers enumerates project-scope MCP servers: the `.mcp.json`
// files Claude Code merges for a session cwd of workspacePath — every
// ancestor directory up to the filesystem root, closer files winning
// name collisions (which for a name-only view is a plain union).
//
// Approval: interactively, a `.mcp.json` server needs per-project
// approval (enabledMcpjsonServers / enableAllProjectMcpServers), but
// the non-interactive sessions AO spawns auto-approve everything not
// explicitly rejected. So the honest render for AO is
// enabled-unless-rejected: only `disabledMcpjsonServers` from the
// merged settings marks a row Disabled here (the caller additionally
// applies the workspace's disabledMcpServers list, which gates every
// scope by name).
func (s *Store) projectServers(workspacePath string) ([]Server, error) {
	if workspacePath == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	dir := filepath.Clean(workspacePath)
	for {
		if err := addMCPFileNames(filepath.Join(dir, ".mcp.json"), seen); err != nil {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(seen) == 0 {
		return nil, nil
	}

	rejected, err := s.disabledMcpjsonServers(workspacePath)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(seen))
	for name := range seen {
		out = append(out, Server{
			Name:     name,
			Source:   SourceProject,
			Disabled: rejected[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// disabledMcpjsonServers returns the rejected .mcp.json server names
// from the merged settings files, ascending precedence user → project
// → local; the highest-precedence source that defines the key wins
// wholesale (settings merge replaces per key, it does not union).
func (s *Store) disabledMcpjsonServers(workspacePath string) (map[string]bool, error) {
	paths := []string{
		filepath.Join(s.home, "settings.json"),
		filepath.Join(workspacePath, ".claude", "settings.json"),
		filepath.Join(workspacePath, ".claude", "settings.local.json"),
	}
	var winner []string
	for _, path := range paths {
		var settings struct {
			DisabledMcpjsonServers *[]string `json:"disabledMcpjsonServers"`
		}
		if _, err := readOptionalJSON(path, &settings); err != nil {
			return nil, err
		}
		if settings.DisabledMcpjsonServers != nil {
			winner = *settings.DisabledMcpjsonServers
		}
	}
	if len(winner) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(winner))
	for _, name := range winner {
		out[name] = true
	}
	return out, nil
}
