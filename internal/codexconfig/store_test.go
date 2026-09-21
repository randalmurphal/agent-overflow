package codexconfig

import (
	"github.com/BurntSushi/toml"
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

[projects."/home/user/repos/sample-project"]
trust_level = "trusted"

[projects."/home/user/repos/agent-overflow"]
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

func TestSetEnabledPreservesProviderFields(t *testing.T) {
	const body = `model = "test"
[mcp_servers.foo]
command = "fake-server"
enabled = false
startup_timeout_sec = 42
required = true
enabled_tools = ["read"]
disabled_tools = ["write"]
future_field = { nested = "keep" }
[mcp_servers.foo.env]
TOKEN = "fixture-only"
[mcp_servers.bar]
url = "https://example.test/mcp"
`
	store := newStoreWithFile(t, body)
	var before map[string]any
	if _, err := toml.Decode(body, &before); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false, false, true} {
		if err := store.SetEnabled("foo", enabled); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(store.path)
		if err != nil {
			t.Fatal(err)
		}
		var after map[string]any
		if _, err := toml.Decode(string(raw), &after); err != nil {
			t.Fatal(err)
		}
		decodeMcpServers(before)["foo"]["enabled"] = enabled
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("toggle changed unrelated config:\n%s", raw)
		}
	}
}

func TestSetEnabledPreservesSeparatedSubtables(t *testing.T) {
	const body = `[mcp_servers.foo]
command = "fixture"
[unrelated]
value = true
[mcp_servers.foo.env]
VALUE = "keep"
`
	store := newStoreWithFile(t, body)
	if err := store.SetEnabled("foo", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(body, "[mcp_servers.foo]\n", "[mcp_servers.foo]\n\nenabled = false\n", 1)
	if string(raw) != want {
		t.Fatalf("changed unrelated configuration: %s", raw)
	}
}

func TestSetEnabledChangesOnlyTheActualPreference(t *testing.T) {
	const body = `[mcp_servers.foo]
command = '''fixture
 enabled = false
fixture'''
# Keep this comment.
"enabled" = false # Keep this one too.
[mcp_servers.foo.env]
enabled = "false"
`
	store := newStoreWithFile(t, body)
	if err := store.SetEnabled("foo", true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(body, `"enabled" = false`, `"enabled" = true`, 1)
	if string(raw) != want {
		t.Fatalf("changed more than the preference:\n%s", raw)
	}
	if err := store.SetEnabled("foo", true); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != want {
		t.Fatal("repeated enable changed the configuration")
	}
}
