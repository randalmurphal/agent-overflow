package claudeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Source identifies where Claude Code learns about an MCP server.
type Source string

const (
	SourceUser    Source = "user"    // top-level mcpServers entry
	SourceLocal   Source = "local"   // projects.<key>.mcpServers entry (claude mcp add --scope local)
	SourceProject Source = "project" // a .mcp.json in the workspace or an ancestor directory
	SourcePlugin  Source = "plugin"  // plugin:<plugin>:<name>, enumerated from plugin manifests
	SourceCloud   Source = "cloud"   // "claude.ai *"; lives in disabledMcpServers only
)

// Transport mirrors Claude's transport discriminator on each
// mcpServers entry. The CLI defaults to "stdio" when the key is
// missing; AO stores the implicit default explicitly so the wire
// shape is clear on read.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// Server is the unified view AO renders for the composer popup and
// Settings library. Source distinguishes the three places an entry
// can come from. Disabled is per-workspace and reflects the value of
// `projects.<workspace>.disabledMcpServers` at Load time.
type Server struct {
	Name      string            `json:"name"`
	Source    Source            `json:"source"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Disabled  bool              `json:"disabled"`
}

// ErrNotFound reports that the requested user-source server is not
// present in the top-level mcpServers map.
var ErrNotFound = errors.New("claudeconfig: server not found")

// ListServers returns every MCP server a Claude session started in
// workspacePath would consider, in stable order: user-source entries
// first, then local-scope, then plugin-provided servers, then
// project-scope (.mcp.json) servers, then cloud entries that appear
// only in disabledMcpServers. ~/.claude.json state is keyed by
// ProjectKey(workspacePath) — the canonical git root, so every
// worktree of a repo shares the main checkout's entry, exactly like
// the CLI. Duplicate names keep the first (manually-configured)
// occurrence.
//
// A disabled-only name nothing defines any more — a removed server, or
// a plugin since uninstalled — is deliberately DROPPED: Claude Code
// itself doesn't list it.
func (s *Store) ListServers(workspacePath string) ([]Server, error) {
	key := ProjectKey(workspacePath)
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	disabledSet, err := readDisabledSet(snap.raw, key)
	if err != nil {
		return nil, err
	}
	userServers, err := readUserServers(snap.raw)
	if err != nil {
		return nil, err
	}
	localServers, err := readLocalServers(snap.raw, key)
	if err != nil {
		return nil, err
	}
	pluginServers, err := s.pluginServers(workspacePath)
	if err != nil {
		return nil, err
	}
	projectServers, err := s.projectServers(workspacePath)
	if err != nil {
		return nil, err
	}

	combined := append(append(append(userServers, localServers...), pluginServers...), projectServers...)
	out := make([]Server, 0, len(combined)+len(disabledSet))
	seen := make(map[string]struct{}, len(combined))
	for _, srv := range combined {
		if _, ok := seen[srv.Name]; ok {
			continue
		}
		// disabledMcpServers gates every scope by name; a project row
		// can additionally be rejected via disabledMcpjsonServers.
		srv.Disabled = srv.Disabled || containsName(disabledSet, srv.Name)
		out = append(out, srv)
		seen[srv.Name] = struct{}{}
	}

	// Cloud connectors appear only in disabledMcpServers — AO can
	// toggle them but can't enumerate the enabled set (it lives in the
	// claude.ai account). Any other unmatched disabled-only name is an
	// orphan and stays hidden.
	extras := make([]Server, 0)
	for _, name := range disabledSet {
		if _, ok := seen[name]; ok {
			continue
		}
		if !strings.HasPrefix(name, "claude.ai ") {
			continue
		}
		extras = append(extras, Server{
			Name:     name,
			Source:   SourceCloud,
			Disabled: true,
		})
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	out = append(out, extras...)
	return out, nil
}

// SetDisabled toggles whether the named server is in the
// disabledMcpServers list for the workspace's canonical project entry
// (ProjectKey — worktrees write to the main checkout's entry, matching
// where a Claude session in that cwd reads). Works for any source
// (user, local, plugin, project, cloud) because the list gates every
// kind of server by name. Idempotent.
func (s *Store) SetDisabled(workspacePath, name string, disabled bool) error {
	if workspacePath == "" {
		return errors.New("claudeconfig: workspace path is required")
	}
	if name == "" {
		return errors.New("claudeconfig: server name is required")
	}
	key := ProjectKey(workspacePath)
	return s.modify(func(root *orderedJSON) error {
		project, err := ensureProjectObject(root, key)
		if err != nil {
			return err
		}
		return setDisabledFlag(project, name, disabled)
	})
}

// readDisabledSet returns the array of names from
// `projects.<key>.disabledMcpServers`, where key is the canonical
// ProjectKey. Missing project entry or missing array yields an empty
// slice — both are normal states.
func readDisabledSet(root *orderedJSON, workspacePath string) ([]string, error) {
	projects, err := root.getObject("projects")
	if err != nil {
		return nil, err
	}
	if projects == nil {
		return nil, nil
	}
	project, err := projects.getObject(workspacePath)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, nil
	}
	rawAny, ok := project.get("disabledMcpServers")
	if !ok {
		return nil, nil
	}
	raw, ok := rawAny.(json.RawMessage)
	if !ok {
		return nil, fmt.Errorf("claudeconfig: disabledMcpServers under %q is not raw", workspacePath)
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode disabledMcpServers under %q: %w", workspacePath, err)
	}
	return list, nil
}

// readUserServers parses the top-level mcpServers map into Server
// values. Entries without a `type` key default to stdio.
func readUserServers(root *orderedJSON) ([]Server, error) {
	mcps, err := root.getObject("mcpServers")
	if err != nil {
		return nil, err
	}
	return decodeServerMap(mcps, SourceUser)
}

// readLocalServers parses the project entry's local-scope server map
// (`projects.<key>.mcpServers`, written by `claude mcp add
// --scope local`). Missing project entry or missing map is a normal
// empty answer.
func readLocalServers(root *orderedJSON, workspacePath string) ([]Server, error) {
	if workspacePath == "" {
		return nil, nil
	}
	projects, err := root.getObject("projects")
	if err != nil || projects == nil {
		return nil, err
	}
	project, err := projects.getObject(workspacePath)
	if err != nil || project == nil {
		return nil, err
	}
	mcps, err := project.getObject("mcpServers")
	if err != nil {
		return nil, err
	}
	return decodeServerMap(mcps, SourceLocal)
}

func decodeServerMap(mcps *orderedJSON, source Source) ([]Server, error) {
	if mcps == nil {
		return nil, nil
	}
	out := make([]Server, 0, len(mcps.keys))
	for _, name := range mcps.keys {
		v, _ := mcps.get(name)
		raw, ok := v.(json.RawMessage)
		if !ok {
			// already-mutated child; nothing to read here yet
			continue
		}
		srv, err := decodeServerEntry(name, raw)
		if err != nil {
			return nil, err
		}
		srv.Source = source
		out = append(out, srv)
	}
	return out, nil
}

// claudeServerJSON is the wire shape of an mcpServers entry. Stdio has
// command/args/env; HTTP+SSE have url/headers.
type claudeServerJSON struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func decodeServerEntry(name string, raw json.RawMessage) (Server, error) {
	var entry claudeServerJSON
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Server{}, fmt.Errorf("decode server %q: %w", name, err)
	}
	transport := entry.Type
	if transport == "" {
		transport = TransportStdio
	}
	srv := Server{
		Name:      name,
		Source:    SourceUser,
		Transport: transport,
	}
	switch transport {
	case TransportStdio:
		srv.Command = entry.Command
		srv.Args = entry.Args
		srv.Env = entry.Env
	case TransportHTTP, TransportSSE:
		srv.URL = entry.URL
		srv.Headers = entry.Headers
	default:
		return Server{}, fmt.Errorf("decode server %q: unsupported transport %q", name, transport)
	}
	return srv, nil
}

func ensureProjectObject(root *orderedJSON, workspacePath string) (*orderedJSON, error) {
	projects, err := root.getObject("projects")
	if err != nil {
		return nil, err
	}
	if projects == nil {
		projects = newOrderedJSON()
		root.set("projects", projects)
	}
	project, err := projects.getObject(workspacePath)
	if err != nil {
		return nil, err
	}
	if project == nil {
		project = newOrderedJSON()
		projects.set(workspacePath, project)
	}
	return project, nil
}

func setDisabledFlag(project *orderedJSON, name string, disabled bool) error {
	var list []string
	if rawAny, ok := project.get("disabledMcpServers"); ok {
		if raw, ok := rawAny.(json.RawMessage); ok {
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode disabledMcpServers: %w", err)
			}
		}
	}
	idx := indexOfString(list, name)
	if disabled {
		if idx >= 0 {
			return nil
		}
		list = append(list, name)
	} else {
		if idx < 0 {
			return nil
		}
		list = append(list[:idx], list[idx+1:]...)
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return err
	}
	project.set("disabledMcpServers", json.RawMessage(encoded))
	return nil
}

func containsName(list []string, name string) bool {
	return indexOfString(list, name) >= 0
}

func indexOfString(list []string, target string) int {
	for i, s := range list {
		if s == target {
			return i
		}
	}
	return -1
}
