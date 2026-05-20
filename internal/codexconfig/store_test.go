package codexconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
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

func TestCreateServer_preservesUntouchedSections(t *testing.T) {
	store := newStoreWithFile(t, userLikeConfig)
	err := store.CreateServer(Server{
		Name:      "code_search",
		Transport: TransportStdio,
		Command:   "/usr/bin/code-search",
		Args:      []string{"--mode", "json"},
		Env:       map[string]string{"PATH": "/usr/bin:/bin"},
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	out := readFile(t, store.path)
	for _, want := range []string{
		`model = "gpt-5.5"`,
		`[features]`,
		`prevent_idle_sleep = true`,
		`[tui]`,
		`[tui.model_availability_nux]`,
		`"gpt-5.5" = 4`,
		`[projects."/home/rmurphy/repos/agent-overflow"]`,
		`trust_level = "trusted"`,
		`[mcp_servers.atlassian]`,
		`url = "https://mcp.atlassian.com/v1/mcp"`,
		`[mcp_servers.openaiDeveloperDocs]`,
		`[notice]`,
		`hide_rate_limit_model_nudge = true`,
		`fast_default_opt_out = true`,
		`[mcp_servers.code_search]`,
		`command = "/usr/bin/code-search"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n----\n%s", want, out)
		}
	}

	// New section should land grouped with existing mcp_servers
	// sections, i.e. before [notice].
	idxNew := strings.Index(out, "[mcp_servers.code_search]")
	idxNotice := strings.Index(out, "[notice]")
	if idxNew < 0 || idxNotice < 0 {
		t.Fatalf("missing markers: new=%d notice=%d", idxNew, idxNotice)
	}
	if idxNew > idxNotice {
		t.Errorf("new mcp section landed after [notice]; expected before. output=\n%s", out)
	}

	// Parse the result to confirm it's still valid TOML and the new
	// server has the expected shape.
	var tree map[string]any
	if _, err := toml.Decode(out, &tree); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	mcps := decodeMcpServers(tree)
	if entry := mcps["code_search"]; entry == nil {
		t.Errorf("code_search not parseable; mcps=%+v", mcps)
	}
}

func TestUpdateServer_replacesInPlace(t *testing.T) {
	store := newStoreWithFile(t, userLikeConfig)
	err := store.UpdateServer(Server{
		Name:      "atlassian",
		Transport: TransportStreamable,
		URL:       "https://mcp.atlassian.com/v2/mcp",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	out := readFile(t, store.path)
	if !strings.Contains(out, `url = "https://mcp.atlassian.com/v2/mcp"`) {
		t.Errorf("new url missing: %s", out)
	}
	// Should still have the other section, in order.
	idxAtl := strings.Index(out, "[mcp_servers.atlassian]")
	idxOaiDocs := strings.Index(out, "[mcp_servers.openaiDeveloperDocs]")
	if !(idxAtl >= 0 && idxOaiDocs > idxAtl) {
		t.Errorf("section order broken; atl=%d docs=%d", idxAtl, idxOaiDocs)
	}
	// Should still have [features] etc.
	if !strings.Contains(out, `prevent_idle_sleep = true`) {
		t.Errorf("[features] body lost")
	}
}

func TestUpdateServer_notFound(t *testing.T) {
	store := newStoreWithFile(t, `[mcp_servers.foo]
command = "/bin/foo"
`)
	err := store.UpdateServer(Server{Name: "ghost", Transport: TransportStdio, Command: "/bin/ghost", Enabled: true})
	if err == nil {
		t.Fatalf("expected ErrNotFound, got nil")
	}
}

func TestDeleteServer_removesSection(t *testing.T) {
	store := newStoreWithFile(t, userLikeConfig)
	if err := store.DeleteServer("atlassian"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	out := readFile(t, store.path)
	if strings.Contains(out, "[mcp_servers.atlassian]") {
		t.Errorf("section not removed; out=\n%s", out)
	}
	// Other mcp section + [features] + [notice] should survive.
	for _, want := range []string{
		"[mcp_servers.openaiDeveloperDocs]",
		"[features]",
		"[notice]",
		`hide_rate_limit_model_nudge = true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing after delete: %q\nout=\n%s", want, out)
		}
	}
}

func TestDeleteServer_notFound(t *testing.T) {
	store := newStoreWithFile(t, userLikeConfig)
	err := store.DeleteServer("ghost")
	if err == nil {
		t.Fatalf("expected ErrNotFound, got nil")
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

func TestCreateServer_rejectsInvalidName(t *testing.T) {
	store := newStoreWithFile(t, "")
	err := store.CreateServer(Server{
		Name:      "claude.ai bad",
		Transport: TransportStdio,
		Command:   "/bin/x",
		Enabled:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "match") {
		t.Fatalf("expected invalid-name error, got %v", err)
	}
}

func TestRoundTrip_keysAndSectionOrder(t *testing.T) {
	// Verify ordering invariants relative to the input on a file
	// where we only delete one server.
	store := newStoreWithFile(t, userLikeConfig)
	if err := store.DeleteServer("openaiDeveloperDocs"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	out := readFile(t, store.path)
	// Order check: features < tui < projects < mcp_servers (with one
	// removed) < notice. The exact original positions stay because
	// only the deleted section's bytes were spliced out.
	checkpoints := []string{
		`model = "gpt-5.5"`,
		`[features]`,
		`[tui]`,
		`[projects."/home/rmurphy/repos/ai-foundations"]`,
		`[mcp_servers.atlassian]`,
		`[notice]`,
	}
	last := -1
	for _, marker := range checkpoints {
		idx := strings.Index(out, marker)
		if idx < 0 {
			t.Fatalf("missing %q in output", marker)
		}
		if idx <= last {
			t.Errorf("order broken: %q at %d, previous at %d\nout=\n%s", marker, idx, last, out)
		}
		last = idx
	}
}

func TestCreateServer_intoEmptyFileCreatesSection(t *testing.T) {
	store := newStoreWithFile(t, "")
	err := store.CreateServer(Server{
		Name:      "foo",
		Transport: TransportStdio,
		Command:   "/bin/foo",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateServer into empty: %v", err)
	}
	got, err := store.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "foo" {
		t.Fatalf("want one foo server; got=%+v", got)
	}
}

func TestCreateServer_dedupesAcrossList(t *testing.T) {
	store := newStoreWithFile(t, "")
	err := store.CreateServer(Server{Name: "foo", Transport: TransportStdio, Command: "/bin/foo", Enabled: true})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	err = store.CreateServer(Server{Name: "foo", Transport: TransportStdio, Command: "/bin/foo-2", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate, got %v", err)
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
