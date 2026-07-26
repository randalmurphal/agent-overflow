package claudeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Source identifies where Claude Code learns about an MCP server. Only
// user-source entries can be created/edited/deleted through AO — the
// plugin and cloud variants are managed by Claude itself.
type Source string

const (
	SourceUser   Source = "user"   // top-level mcpServers entry
	SourcePlugin Source = "plugin" // plugin:<plugin>:<name>; lives in disabledMcpServers only
	SourceCloud  Source = "cloud"  // "claude.ai *"; lives in disabledMcpServers only
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

// ErrReadOnly reports an attempted mutation against a plugin- or
// cloud-source entry (those aren't owned by AO).
var ErrReadOnly = errors.New("claudeconfig: server is not user-managed")

// ListServers returns every MCP server visible from the given
// workspace path, in stable order: user-source entries first (by
// name), followed by plugin and cloud entries that appear only in
// disabledMcpServers. workspacePath should be the absolute path of
// the project root used as the key under `projects.<path>`.
func (s *Store) ListServers(workspacePath string) ([]Server, error) {
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	disabledSet, err := readDisabledSet(snap.raw, workspacePath)
	if err != nil {
		return nil, err
	}
	userServers, err := readUserServers(snap.raw)
	if err != nil {
		return nil, err
	}

	out := make([]Server, 0, len(userServers)+len(disabledSet))
	seen := make(map[string]struct{}, len(userServers))
	for _, srv := range userServers {
		srv.Disabled = containsName(disabledSet, srv.Name)
		out = append(out, srv)
		seen[srv.Name] = struct{}{}
	}

	// Plugin / cloud entries appear only in disabledMcpServers — AO
	// can toggle them but can't edit/delete them.
	extras := make([]Server, 0)
	for _, name := range disabledSet {
		if _, ok := seen[name]; ok {
			continue
		}
		extras = append(extras, Server{
			Name:     name,
			Source:   classifyDisabledOnlyName(name),
			Disabled: true,
		})
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	out = append(out, extras...)
	return out, nil
}

func classifyDisabledOnlyName(name string) Source {
	if len(name) >= len("plugin:") && name[:len("plugin:")] == "plugin:" {
		return SourcePlugin
	}
	if len(name) >= len("claude.ai ") && name[:len("claude.ai ")] == "claude.ai " {
		return SourceCloud
	}
	// Unrecognized disabled-only entries default to plugin so they
	// stay edit/delete-disabled in the UI. Adding a new category
	// later is preferable to silently treating an unknown name as
	// user-owned.
	return SourcePlugin
}

// CreateServer adds a new entry to the top-level mcpServers map. It
// returns an error if the name already exists. Validation of the
// transport-specific fields runs before any IO.
func (s *Store) CreateServer(srv Server) error {
	if err := validateUserServer(srv); err != nil {
		return err
	}
	return s.modify(func(root *orderedJSON) error {
		mcps, err := ensureMcpServersObject(root)
		if err != nil {
			return err
		}
		if mcps.has(srv.Name) {
			return fmt.Errorf("claudeconfig: server %q already exists", srv.Name)
		}
		entryRaw, err := encodeServerEntry(srv)
		if err != nil {
			return err
		}
		mcps.set(srv.Name, entryRaw)
		return nil
	})
}

// UpdateServer replaces the existing entry at srv.Name. Renaming is
// not supported in v1 — the UI surfaces name as an immutable
// identifier so its semantics line up with Claude's own /mcp output.
func (s *Store) UpdateServer(srv Server) error {
	if err := validateUserServer(srv); err != nil {
		return err
	}
	return s.modify(func(root *orderedJSON) error {
		mcps, err := root.getObject("mcpServers")
		if err != nil {
			return err
		}
		if mcps == nil || !mcps.has(srv.Name) {
			return fmt.Errorf("%w: %s", ErrNotFound, srv.Name)
		}
		entryRaw, err := encodeServerEntry(srv)
		if err != nil {
			return err
		}
		mcps.set(srv.Name, entryRaw)
		return nil
	})
}

// DeleteServer removes a user-source entry from the top-level
// mcpServers map. It also strips the name from every
// `projects.<path>.disabledMcpServers` array so a re-add doesn't
// silently surface as disabled.
func (s *Store) DeleteServer(name string) error {
	if name == "" {
		return errors.New("claudeconfig: server name is required")
	}
	return s.modify(func(root *orderedJSON) error {
		mcps, err := root.getObject("mcpServers")
		if err != nil {
			return err
		}
		if mcps == nil || !mcps.has(name) {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		mcps.delete(name)
		return stripDisabledNameEverywhere(root, name)
	})
}

// SetDisabled toggles whether the named server is in the
// disabledMcpServers list for the given workspace. Works for any
// source (user, plugin, cloud) because the list is what Claude uses
// to gate every kind of server. Idempotent.
func (s *Store) SetDisabled(workspacePath, name string, disabled bool) error {
	if workspacePath == "" {
		return errors.New("claudeconfig: workspace path is required")
	}
	if name == "" {
		return errors.New("claudeconfig: server name is required")
	}
	return s.modify(func(root *orderedJSON) error {
		project, err := ensureProjectObject(root, workspacePath)
		if err != nil {
			return err
		}
		return setDisabledFlag(project, name, disabled)
	})
}

// readDisabledSet returns the array of names from
// `projects.<workspace>.disabledMcpServers`. Missing workspace or
// missing array yields an empty slice — both are normal states.
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
		out = append(out, srv)
	}
	return out, nil
}

// claudeServerJSON is the wire shape AO emits for an mcpServers entry.
// Stdio has command/args/env; HTTP+SSE have url/headers. The "type"
// discriminator is always written explicitly so a downstream Claude
// version that flips the default can't silently re-route the entry.
type claudeServerJSON struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func encodeServerEntry(srv Server) (json.RawMessage, error) {
	entry := claudeServerJSON{Type: srv.Transport}
	switch srv.Transport {
	case TransportStdio:
		entry.Command = srv.Command
		entry.Args = srv.Args
		entry.Env = srv.Env
	case TransportHTTP, TransportSSE:
		entry.URL = srv.URL
		entry.Headers = srv.Headers
	default:
		return nil, fmt.Errorf("claudeconfig: unsupported transport %q", srv.Transport)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
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

func validateUserServer(srv Server) error {
	if srv.Name == "" {
		return errors.New("claudeconfig: server name is required")
	}
	switch srv.Transport {
	case TransportStdio:
		if srv.Command == "" {
			return errors.New("claudeconfig: stdio server requires a command")
		}
	case TransportHTTP, TransportSSE:
		if srv.URL == "" {
			return fmt.Errorf("claudeconfig: %s server requires a url", srv.Transport)
		}
	default:
		return fmt.Errorf("claudeconfig: unsupported transport %q", srv.Transport)
	}
	return nil
}

func ensureMcpServersObject(root *orderedJSON) (*orderedJSON, error) {
	existing, err := root.getObject("mcpServers")
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	fresh := newOrderedJSON()
	root.set("mcpServers", fresh)
	return fresh, nil
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

func stripDisabledNameEverywhere(root *orderedJSON, name string) error {
	projects, err := root.getObject("projects")
	if err != nil {
		return err
	}
	if projects == nil {
		return nil
	}
	for _, ws := range projects.keys {
		proj, err := projects.getObject(ws)
		if err != nil {
			return err
		}
		if proj == nil {
			continue
		}
		if err := setDisabledFlag(proj, name, false); err != nil {
			return err
		}
	}
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
