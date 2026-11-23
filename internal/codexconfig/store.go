package codexconfig

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"

	"agent-overflow/internal/atomicfile"

	"github.com/BurntSushi/toml"
)

// Transport mirrors Codex's discriminator on RawMcpServerConfig — the
// presence of `command` selects stdio, presence of `url` selects
// streamable_http. AO normalises to a single Transport field on the
// way out.
const (
	TransportStdio      = "stdio"
	TransportStreamable = "streamable_http"
)

// Server is the unified view AO renders for the composer popup and
// Settings library. Enabled defaults to true (Codex's own default),
// so AO only writes the key when the user toggled it off.
type Server struct {
	Name           string            `json:"name"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	HTTPHeaders    map[string]string `json:"httpHeaders,omitempty"`
	BearerTokenEnv string            `json:"bearerTokenEnv,omitempty"`
	Enabled        bool              `json:"enabled"`
}

var (
	ErrNotFound        = errors.New("codexconfig: server not found")
	ErrConcurrentWrite = errors.New("codexconfig: concurrent write detected")
	ErrInvalidName     = errors.New("codexconfig: server name must match [A-Za-z0-9_-]+")
)

// bareKey limits the server names accepted by the preference writer.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Store reads and writes ~/.codex/config.toml. Tests inject a temp
// path via the constructor — the package never reads $HOME directly.
type Store struct {
	path string
}

// New returns a Store bound to the given file path. A non-existent
// file is treated as empty on Load; the first Save creates the file
// (and its parent directory).
func New(path string) *Store {
	return &Store{path: path}
}

// PathForHome returns `<home>/.codex/config.toml`. The home is INJECTED,
// never resolved here — see claudeconfig.PathForHome for why this package
// has no $HOME-reading variant.
func PathForHome(home string) string {
	return filepath.Join(home, ".codex", "config.toml")
}

// ListServers returns every entry under [mcp_servers.*] in stable
// name order. Servers with `enabled = false` are returned with
// Enabled=false; servers without the key are Enabled=true (Codex's
// own default).
func (s *Store) ListServers() ([]Server, error) {
	snap, err := s.load()
	if err != nil {
		return nil, err
	}
	mcps := decodeMcpServers(snap.tree)
	if len(mcps) == 0 {
		return nil, nil
	}
	out := make([]Server, 0, len(mcps))
	for name, raw := range mcps {
		srv, err := serverFromRaw(name, raw)
		if err != nil {
			return nil, fmt.Errorf("decode %q: %w", name, err)
		}
		out = append(out, srv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var enabledAssignment = regexp.MustCompile(`(?m)^[ \t]*(?:enabled|"enabled"|'enabled')[ \t]*=[ \t]*(true|false)`)

// SetEnabled flips the `enabled` key for the given server. Codex's
// `enabled` is global (not per-thread), and so is this method.
// Missing servers return ErrNotFound. Preserve provider fields that AO
// does not project into Server.
func (s *Store) SetEnabled(name string, enabled bool) error {
	if !bareKey.MatchString(name) {
		return ErrInvalidName
	}
	return s.modify(func(data []byte) ([]byte, error) {
		var tree map[string]any
		if _, err := toml.Decode(string(data), &tree); err != nil {
			return nil, err
		}
		raw := decodeMcpServers(tree)[name]
		if raw == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		previous, present := raw["enabled"]
		if previous == enabled || (!present && enabled) {
			return data, nil
		}
		raw["enabled"] = enabled
		// Verify the entire parsed document after each candidate edit. A
		// matching line can belong to a nested table or a multiline string.
		// Only the edit that changes this preference alone may be committed.
		accept := func(start, end int, replacement []byte) ([]byte, bool) {
			next := make([]byte, 0, len(data)+len(replacement))
			next = append(next, data[:start]...)
			next = append(next, replacement...)
			next = append(next, data[end:]...)
			var checked map[string]any
			if _, err := toml.Decode(string(next), &checked); err != nil {
				return nil, false
			}
			return next, reflect.DeepEqual(tree, checked)
		}
		value := []byte(fmt.Sprint(enabled))
		if present {
			for _, match := range enabledAssignment.FindAllSubmatchIndex(data, -1) {
				if next, ok := accept(match[2], match[3], value); ok {
					return next, nil
				}
			}
		} else {
			header := regexp.MustCompile(`(?m)^[ \t]*\[[ \t]*mcp_servers\.` + regexp.QuoteMeta(name) + `[ \t]*\][^\n]*(?:\n|$)`)
			for _, match := range header.FindAllIndex(data, -1) {
				assignment := []byte("\nenabled = " + string(value) + "\n")
				if next, ok := accept(match[1], match[1], assignment); ok {
					return next, nil
				}
			}
		}
		return nil, fmt.Errorf("set mcp enabled: cannot locate the enabled field for %q without changing other configuration", name)

	})
}

// snapshot captures the file bytes + stat at read time so save can
// detect a concurrent writer.
type snapshot struct {
	data []byte
	tree map[string]any
	stat os.FileInfo
}

func (s *Store) load() (*snapshot, error) {
	data, info, err := atomicfile.ReadWithStat(s.path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &snapshot{tree: map[string]any{}}, nil
	}
	var tree map[string]any
	if _, err := toml.Decode(string(data), &tree); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return &snapshot{data: data, tree: tree, stat: info}, nil
}

func decodeMcpServers(tree map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	mcpsAny, ok := tree["mcp_servers"]
	if !ok {
		return out
	}
	mcps, ok := mcpsAny.(map[string]any)
	if !ok {
		return out
	}
	for name, v := range mcps {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out[name] = entry
	}
	return out
}

func serverFromRaw(name string, raw map[string]any) (Server, error) {
	srv := Server{Name: name, Enabled: true}
	if v, ok := raw["enabled"].(bool); ok {
		srv.Enabled = v
	}
	if v, ok := raw["command"].(string); ok && v != "" {
		srv.Transport = TransportStdio
		srv.Command = v
		if args, ok := raw["args"].([]any); ok {
			srv.Args = stringSlice(args)
		}
		if envMap, ok := raw["env"].(map[string]any); ok {
			srv.Env = stringMap(envMap)
		}
		return srv, nil
	}
	if v, ok := raw["url"].(string); ok && v != "" {
		srv.Transport = TransportStreamable
		srv.URL = v
		if headers, ok := raw["http_headers"].(map[string]any); ok {
			srv.HTTPHeaders = stringMap(headers)
		}
		if env, ok := raw["bearer_token_env_var"].(string); ok {
			srv.BearerTokenEnv = env
		}
		return srv, nil
	}
	return Server{}, fmt.Errorf("server %q has neither command nor url", name)
}

func stringSlice(in []any) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMap(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// modify runs fn against the current file bytes, then atomically
// writes the result back with a concurrent-write retry. The retry
// covers exactly one external write between read and rename.
func (s *Store) modify(fn func([]byte) ([]byte, error)) error {
	for attempt := 0; attempt < 2; attempt++ {
		snap, err := s.load()
		if err != nil {
			return err
		}
		nextData, err := fn(snap.data)
		if err != nil {
			return err
		}
		committed, err := atomicfile.WriteIfUnchanged(s.path, nextData, snap.stat)
		if committed {
			if err != nil {
				// The new config is in place; only its directory entry may
				// not survive a power loss. The caller goes on as written.
				log.Printf("codexconfig: %v", err)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
	return ErrConcurrentWrite
}
