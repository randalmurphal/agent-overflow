// Package agyconfig reads and writes Antigravity CLI's user MCP configuration
// (`~/.gemini/config/mcp_config.json`). Callers always inject the file path
// rather than reading $HOME inside this package so tests stay isolated.
package agyconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"agent-overflow/internal/atomicfile"
)

var (
	pathLocksMu sync.Mutex
	pathLocks   = map[string]*sync.Mutex{}
)

func lockPath(path string) func() {
	clean := filepath.Clean(path)
	pathLocksMu.Lock()
	mu := pathLocks[clean]
	if mu == nil {
		mu = &sync.Mutex{}
		pathLocks[clean] = mu
	}
	pathLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Server is the normalized view of one Antigravity MCP server entry.
type Server struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
	Disabled  bool     `json:"disabled"`
}

var (
	ErrNotFound        = errors.New("agyconfig: server not found")
	ErrConcurrentWrite = errors.New("agyconfig: concurrent write detected")
	ErrInvalidName     = errors.New("agyconfig: server name is required")
)

// Store reads and writes `<home>/.gemini/config/mcp_config.json`.
type Store struct {
	path string
}

// New returns a Store bound to path.
func New(path string) *Store {
	return &Store{path: path}
}

// PathForHome returns `<home>/.gemini/config/mcp_config.json`.
func PathForHome(home string) string {
	return filepath.Join(home, ".gemini", "config", "mcp_config.json")
}

// ListServers returns every configured MCP server in sorted name order.
// A missing config file yields an empty list and a nil error.
func (s *Store) ListServers() ([]Server, error) {
	data, _, err := s.readFile()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("agyconfig: parse %s: %w", s.path, err)
	}
	if len(doc.MCPServers) == 0 {
		return nil, nil
	}
	out := make([]Server, 0, len(doc.MCPServers))
	for name, raw := range doc.MCPServers {
		var entry struct {
			Command   string   `json:"command"`
			Args      []string `json:"args"`
			ServerURL string   `json:"serverUrl"`
			URL       string   `json:"url"`
			Disabled  bool     `json:"disabled"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("agyconfig: decode server %q: %w", name, err)
		}
		url := strings.TrimSpace(entry.ServerURL)
		if url == "" {
			url = strings.TrimSpace(entry.URL)
		}
		transport := TransportStdio
		if url != "" && strings.TrimSpace(entry.Command) == "" {
			transport = TransportHTTP
		}
		out = append(out, Server{
			Name:      name,
			Transport: transport,
			Command:   strings.TrimSpace(entry.Command),
			Args:      entry.Args,
			URL:       url,
			Disabled:  entry.Disabled,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetDisabled updates the `disabled` flag of one server entry in place while
// preserving all unknown top-level and server-level JSON fields.
func (s *Store) SetDisabled(name string, disabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrInvalidName
	}
	unlock := lockPath(s.path)
	defer unlock()
	data, statBefore, err := s.readFile()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("agyconfig: parse %s: %w", s.path, err)
	}
	rawServers, ok := root["mcpServers"]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return fmt.Errorf("agyconfig: parse mcpServers: %w", err)
	}
	rawEntry, exists := servers[name]
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	var entry map[string]any
	if err := json.Unmarshal(rawEntry, &entry); err != nil {
		return fmt.Errorf("agyconfig: parse server %q: %w", name, err)
	}
	if entry == nil {
		entry = map[string]any{}
	}
	if prev, has := entry["disabled"].(bool); has && prev == disabled {
		return nil
	} else if !has && !disabled {
		return nil
	}
	entry["disabled"] = disabled

	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("agyconfig: encode server %q: %w", name, err)
	}
	servers[name] = encodedEntry
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("agyconfig: encode mcpServers: %w", err)
	}
	root["mcpServers"] = encodedServers
	encodedRoot, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("agyconfig: encode config: %w", err)
	}
	encodedRoot = append(encodedRoot, '\n')

	if statAfter, statErr := os.Stat(s.path); statErr == nil && statBefore != nil {
		if !statAfter.ModTime().Equal(statBefore.ModTime()) || statAfter.Size() != statBefore.Size() {
			return ErrConcurrentWrite
		}
	}
	return atomicfile.Write(s.path, encodedRoot)
}

func (s *Store) readFile() ([]byte, os.FileInfo, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}
