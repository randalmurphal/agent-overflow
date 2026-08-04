package codexconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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

// bareKey matches the TOML bare-key pattern (the only names AO will
// emit unquoted in a `[mcp_servers.<name>]` header). Names with other
// characters need quoting, which AO refuses on create/update so the
// header round-trip stays simple.
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

// DefaultPath returns ~/.codex/config.toml based on the current user.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
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

// SetEnabled flips the `enabled` key for the given server. Codex's
// `enabled` is global (not per-thread), and so is this method.
// Missing servers return ErrNotFound. enabled=true with no existing
// key is a no-op (default is true).
func (s *Store) SetEnabled(name string, enabled bool) error {
	if !bareKey.MatchString(name) {
		return ErrInvalidName
	}
	return s.modify(func(data []byte) ([]byte, error) {
		start, end, _ := findSectionByName(data, name)
		if start < 0 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		// Re-render the existing section with the flipped enabled
		// flag. Reuse the parsed view so all other fields survive.
		current, err := loadServerByName(data, name)
		if err != nil {
			return nil, err
		}
		current.Enabled = enabled
		body, err := renderSection(current)
		if err != nil {
			return nil, err
		}
		return spliceReplace(data, start, end, body), nil
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
	data, info, err := readFileWithStat(s.path)
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

// readFileWithStat returns (contents, info, nil) when the file exists.
// A missing file returns (nil, nil, nil).
//
// The TOCTOU-safe ordering is stat-read-stat: a writer who atomically
// renames a new version over ours between the read and the post-read
// stat would otherwise hand us v1 bytes paired with v2 metadata, which
// would silently pass writeIfUnchanged on save and clobber the
// concurrent write. When the bracketing stats don't match we retry up
// to a small bounded number of times.
func readFileWithStat(path string) ([]byte, os.FileInfo, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		before, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("stat %s: %w", path, statErr)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		after, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("re-stat %s: %w", path, statErr)
		}
		if before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) {
			return data, before, nil
		}
	}
	return nil, nil, fmt.Errorf("read %s: file changed during read across %d attempts", path, maxAttempts)
}

func loadServerByName(data []byte, name string) (Server, error) {
	var tree map[string]any
	if _, err := toml.Decode(string(data), &tree); err != nil {
		return Server{}, fmt.Errorf("parse: %w", err)
	}
	mcps := decodeMcpServers(tree)
	raw, ok := mcps[name]
	if !ok {
		return Server{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return serverFromRaw(name, raw)
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
		ok, err := writeIfUnchanged(s.path, nextData, snap.stat)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return ErrConcurrentWrite
}

func writeIfUnchanged(path string, data []byte, before os.FileInfo) (bool, error) {
	if before != nil {
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("recheck %s: %w", path, err)
		}
		if err == nil && (current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime())) {
			return false, nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create parent: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config.toml.tmp.*")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}

// renderSection produces the TOML bytes for one [mcp_servers.<name>]
// table. The output always ends in a single newline so consecutive
// sections separate cleanly when stitched. Bare-key names are
// validated up front so the header is `[mcp_servers.<name>]` without
// quoting. Field order is fixed for determinism.
func renderSection(srv Server) ([]byte, error) {
	if !bareKey.MatchString(srv.Name) {
		return nil, ErrInvalidName
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", srv.Name)
	switch srv.Transport {
	case TransportStdio:
		writeKV(&b, "command", srv.Command)
		if len(srv.Args) > 0 {
			writeArr(&b, "args", srv.Args)
		}
		if len(srv.Env) > 0 {
			writeTableInline(&b, "env", srv.Env)
		}
	case TransportStreamable:
		writeKV(&b, "url", srv.URL)
		if len(srv.HTTPHeaders) > 0 {
			writeTableInline(&b, "http_headers", srv.HTTPHeaders)
		}
		if srv.BearerTokenEnv != "" {
			writeKV(&b, "bearer_token_env_var", srv.BearerTokenEnv)
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q", srv.Transport)
	}
	if !srv.Enabled {
		fmt.Fprintln(&b, "enabled = false")
	}
	return b.Bytes(), nil
}

func writeKV(b *bytes.Buffer, key, value string) {
	fmt.Fprintf(b, "%s = %s\n", key, tomlQuote(value))
}

func writeArr(b *bytes.Buffer, key string, values []string) {
	b.WriteString(key)
	b.WriteString(" = [")
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlQuote(v))
	}
	b.WriteString("]\n")
}

// writeTableInline emits one `key.subkey = "value"` line per entry so
// the parent table header stays the [mcp_servers.<name>] one (Codex
// expects fields directly under that header, not under a nested
// `[mcp_servers.<name>.env]` table — both decode the same shape but
// the dotted-key form keeps the section a single logical block).
func writeTableInline(b *bytes.Buffer, key string, values map[string]string) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		left := key + "." + tomlKey(k)
		fmt.Fprintf(b, "%s = %s\n", left, tomlQuote(values[k]))
	}
}

// tomlKey returns key quoted as a basic-string TOML key if it
// contains characters outside [A-Za-z0-9_-]; otherwise the bare form.
func tomlKey(key string) string {
	if bareKey.MatchString(key) {
		return key
	}
	return tomlQuote(key)
}

// tomlQuote emits a TOML basic string with backslash escapes for the
// characters TOML requires. We use basic strings everywhere so values
// containing ${VAR} expand naturally — TOML literal strings (single
// quotes) would not.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
