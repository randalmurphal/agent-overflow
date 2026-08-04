package codexconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func newStoreWithFile(t *testing.T, body string) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	return New(path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

const userLikeConfig = `model = "gpt-5.5"
model_reasoning_effort = "high"
plan_mode_reasoning_effort = "xhigh"
personality = "pragmatic"
project_doc_fallback_filenames = ["CLAUDE.md"]
service_tier = "fast"
[features]
prevent_idle_sleep = true
image_detail_original = true
terminal_resize_reflow = true
goals = true

[tui]
status_line = ["current-dir", "git-branch", "model-with-reasoning"]

[tui.model_availability_nux]
"gpt-5.5" = 4

[projects."/home/rmurphy/repos/ai-foundations"]
trust_level = "trusted"

[projects."/home/rmurphy/repos/agent-overflow"]
trust_level = "trusted"

[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/mcp"

[mcp_servers.openaiDeveloperDocs]
url = "https://developers.openai.com/mcp"

[notice]
hide_rate_limit_model_nudge = true
fast_default_opt_out = true
`

func TestListServers(t *testing.T) {
	store := newStoreWithFile(t, userLikeConfig)
	got, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2; got=%+v", len(got), got)
	}
	if got[0].Name != "atlassian" || got[1].Name != "openaiDeveloperDocs" {
		t.Errorf("order/names wrong: %+v", got)
	}
	for _, srv := range got {
		if srv.Transport != TransportStreamable {
			t.Errorf("%s transport = %q, want streamable_http", srv.Name, srv.Transport)
		}
		if !srv.Enabled {
			t.Errorf("%s should default to enabled", srv.Name)
		}
	}
}

func TestListServers_missingFile(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "absent.toml"))
	got, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestSetEnabled_togglesPreservingOtherFields(t *testing.T) {
	store := newStoreWithFile(t, `[mcp_servers.foo]
command = "/bin/foo"
args = ["x", "y"]
env.A = "1"
env.B = "2"
`)
	if err := store.SetEnabled("foo", false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	got, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 1 || got[0].Enabled {
		t.Fatalf("expected enabled=false; got=%+v", got)
	}
	if got[0].Command != "/bin/foo" {
		t.Errorf("command lost: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].Args, []string{"x", "y"}) {
		t.Errorf("args lost: %+v", got[0])
	}
	if got[0].Env["A"] != "1" || got[0].Env["B"] != "2" {
		t.Errorf("env lost: %+v", got[0].Env)
	}

	if err := store.SetEnabled("foo", true); err != nil {
		t.Fatalf("SetEnabled true: %v", err)
	}
	got, _ = store.ListServers()
	if !got[0].Enabled {
		t.Errorf("expected enabled=true; got=%+v", got)
	}
}

func TestConcurrentWriteDetected(t *testing.T) {
	store := newStoreWithFile(t, "[mcp_servers.foo]\ncommand = \"/bin/foo\"\n")
	snap, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// External writer mutates the file out-of-band.
	if err := os.WriteFile(store.path, []byte("[mcp_servers.foo]\ncommand = \"/bin/foo\"\n[notice]\nfoo = true\n"), 0o600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Bump mtime so the stat detects the change.
	mt := snap.stat.ModTime().Add(2 * 1000 * 1000 * 1000)
	if err := os.Chtimes(store.path, mt, mt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ok, err := writeIfUnchanged(store.path, []byte("x"), snap.stat)
	if err != nil {
		t.Fatalf("writeIfUnchanged: %v", err)
	}
	if ok {
		t.Fatalf("write should have been refused on stale snapshot")
	}
}

func TestRender_stdioWithEnvUsesDottedKeys(t *testing.T) {
	out, err := renderSection(Server{
		Name:      "x",
		Transport: TransportStdio,
		Command:   "/bin/x",
		Env:       map[string]string{"A": "1", "B-key": "2"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("renderSection: %v", err)
	}
	str := string(out)
	if !strings.Contains(str, `env.A = "1"`) {
		t.Errorf("expected dotted env.A; got=%s", str)
	}
	if !strings.Contains(str, `env."B-key"`) && !strings.Contains(str, `env.B-key`) {
		// Either bare or quoted is fine since '-' is a bare-key
		// character; assert at least one form is present.
		t.Errorf("expected env.B-key entry; got=%s", str)
	}
}

func TestRender_orderDeterministic(t *testing.T) {
	srv := Server{
		Name:      "x",
		Transport: TransportStdio,
		Command:   "/bin/x",
		Env:       map[string]string{"Z": "1", "A": "2", "M": "3"},
		Enabled:   true,
	}
	first, err := renderSection(srv)
	if err != nil {
		t.Fatalf("render 1: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := renderSection(srv)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if string(first) != string(next) {
			t.Fatalf("non-deterministic render:\nfirst=%s\nnext=%s", first, next)
		}
	}
	// Sanity: alphabetical
	str := string(first)
	idxA := strings.Index(str, "env.A")
	idxM := strings.Index(str, "env.M")
	idxZ := strings.Index(str, "env.Z")
	if !(idxA > 0 && idxA < idxM && idxM < idxZ) {
		t.Errorf("env keys not alphabetical; A=%d M=%d Z=%d", idxA, idxM, idxZ)
	}
}

func TestFindSectionByName_dottedSubsectionsBelong(t *testing.T) {
	body := `[features]
foo = true

[mcp_servers.alpha]
command = "/bin/alpha"

[mcp_servers.alpha.http_headers]
X = "1"

[mcp_servers.beta]
command = "/bin/beta"
`
	start, end, _ := findSectionByName([]byte(body), "alpha")
	chunk := body[start:end]
	if !strings.Contains(chunk, "[mcp_servers.alpha]") {
		t.Errorf("missing alpha header in chunk:\n%s", chunk)
	}
	if !strings.Contains(chunk, "[mcp_servers.alpha.http_headers]") {
		t.Errorf("dotted subsection not absorbed:\n%s", chunk)
	}
	if strings.Contains(chunk, "[mcp_servers.beta]") {
		t.Errorf("beta leaked into alpha chunk:\n%s", chunk)
	}
}

func TestFindSectionByName_missing(t *testing.T) {
	start, end, _ := findSectionByName([]byte(`[features]
foo = true
`), "alpha")
	if start != -1 || end != -1 {
		t.Errorf("expected (-1,-1); got (%d,%d)", start, end)
	}
}

func TestListServers_includesDisabled(t *testing.T) {
	store := newStoreWithFile(t, `[mcp_servers.foo]
command = "/bin/foo"
enabled = false
`)
	got, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 1 || got[0].Enabled {
		t.Fatalf("expected disabled foo; got=%+v", got)
	}
}

func TestTomlQuote_escapesBackslashesAndQuotes(t *testing.T) {
	got := tomlQuote(`a"b\c`)
	want := `"a\"b\\c"`
	if got != want {
		t.Errorf("tomlQuote got=%q want=%q", got, want)
	}
}

func TestTomlQuote_preservesDollarBrace(t *testing.T) {
	// We use basic strings so ${VAR} substitutions reach Codex
	// unchanged. Regression: literal strings (single quotes) would
	// also be fine — but $ must NOT be escaped.
	got := tomlQuote(`Bearer ${TOK}`)
	if !strings.Contains(got, `${TOK}`) {
		t.Errorf("dollar-brace pattern altered: %q", got)
	}
}

func TestSortedNamesStable(t *testing.T) {
	// Defensive: sort behaviour can drift across Go versions when
	// values include equal-precedence strings. Codex names are
	// distinct in our package — just verify the sort path is
	// stable order for the public list method.
	store := newStoreWithFile(t, `[mcp_servers.beta]
command = "/b"

[mcp_servers.alpha]
command = "/a"

[mcp_servers.charlie]
command = "/c"
`)
	got, _ := store.ListServers()
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	want := []string{"alpha", "beta", "charlie"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
	// Reverse-sanity: confirm sort.Strings agrees.
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("post-sort order = %v, want %v", names, want)
	}
}
