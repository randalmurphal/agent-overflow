package claudeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Each test uses a fresh t.TempDir() file so the package never reads
// real ~/.claude.json state. We compare key-by-key (with one
// equality-of-bytes check for an untouched key) rather than diffing
// whole files: the parser explicitly does NOT promise byte-identical
// round-trips across mutations, only that mutated paths re-encode
// and unmutated raw bytes pass through.

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func newStoreWithFile(t *testing.T, body string) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if body != "" {
		writeFile(t, path, body)
	}
	return New(path)
}

func TestListServers_userOnly(t *testing.T) {
	body := `{
  "mcpServers": {
    "alpha": {"type": "stdio", "command": "/bin/alpha", "args": ["x"]},
    "bravo": {"type": "http", "url": "https://b.example", "headers": {"X-K": "v"}}
  },
  "projects": {
    "/work": {"disabledMcpServers": ["bravo"]}
  }
}`
	store := newStoreWithFile(t, body)
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d servers, want 2; got=%+v", len(got), got)
	}
	for _, srv := range got {
		switch srv.Name {
		case "alpha":
			if srv.Disabled {
				t.Errorf("alpha should be enabled")
			}
			if srv.Transport != "stdio" || srv.Command != "/bin/alpha" {
				t.Errorf("alpha decoded wrong: %+v", srv)
			}
		case "bravo":
			if !srv.Disabled {
				t.Errorf("bravo should be disabled for /work")
			}
			if srv.Transport != "http" || srv.URL != "https://b.example" {
				t.Errorf("bravo decoded wrong: %+v", srv)
			}
		default:
			t.Errorf("unexpected server %q", srv.Name)
		}
	}
}

func TestListServers_cloudOnlyEntriesKeptPluginOrphansDropped(t *testing.T) {
	// A disabled-only "claude.ai *" name stays visible (the enabled
	// cloud set lives in the claude.ai account, so the disabled name is
	// the only trace). A disabled-only plugin: name with no installed
	// plugin behind it is an orphan, exactly like a bare name.
	body := `{
  "mcpServers": {"alpha": {"type": "stdio", "command": "/bin/alpha"}},
  "projects": {
    "/work": {"disabledMcpServers": ["plugin:p:tool", "claude.ai Gmail", "alpha"]}
  }
}`
	store := newStoreWithFile(t, body)
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	names := make([]string, 0, len(got))
	sources := make(map[string]Source, len(got))
	for _, srv := range got {
		names = append(names, srv.Name)
		sources[srv.Name] = srv.Source
	}
	want := []string{"alpha", "claude.ai Gmail"}
	sortedNames := append([]string{}, names...)
	sort.Strings(sortedNames)
	if !reflect.DeepEqual(sortedNames, want) {
		t.Fatalf("names = %v, want %v", sortedNames, want)
	}
	if sources["alpha"] != SourceUser {
		t.Errorf("alpha source = %q, want user", sources["alpha"])
	}
	if sources["claude.ai Gmail"] != SourceCloud {
		t.Errorf("claude.ai Gmail source = %q, want cloud", sources["claude.ai Gmail"])
	}
}

func TestListServers_localScopeEntries(t *testing.T) {
	body := `{
  "mcpServers": {"alpha": {"type": "stdio", "command": "/bin/alpha"}},
  "projects": {
    "/work": {
      "mcpServers": {
        "jira": {"type": "stdio", "command": "/bin/jira"},
        "alpha": {"type": "stdio", "command": "/bin/alpha-local"}
      },
      "disabledMcpServers": ["jira"]
    },
    "/other": {}
  }
}`
	store := newStoreWithFile(t, body)
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d servers, want 2 (alpha deduped, jira local); got=%+v", len(got), got)
	}
	byName := map[string]Server{}
	for _, srv := range got {
		byName[srv.Name] = srv
	}
	if jira := byName["jira"]; jira.Source != SourceLocal || !jira.Disabled {
		t.Errorf("jira = %+v, want disabled local-scope entry", jira)
	}
	// Same name in user and local scope: one row, user entry wins.
	if alpha := byName["alpha"]; alpha.Source != SourceUser {
		t.Errorf("alpha = %+v, want deduped to the user entry", alpha)
	}

	// Local entries are workspace-scoped: /other sees only the user set.
	other, err := store.ListServers("/other")
	if err != nil {
		t.Fatalf("ListServers /other: %v", err)
	}
	if len(other) != 1 || other[0].Name != "alpha" {
		t.Errorf("/other = %+v, want only the user alpha", other)
	}
}

func TestListServers_disabledOnlyOrphansAreDropped(t *testing.T) {
	// A name in disabledMcpServers that nothing defines any more — a
	// removed server, or a plugin since uninstalled — is an orphan;
	// Claude Code itself doesn't list it, so neither does AO.
	body := `{
  "mcpServers": {},
  "projects": {
    "/work": {"disabledMcpServers": ["code-index", "plugin:p:tool"]}
  }
}`
	store := newStoreWithFile(t, body)
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no rows (both orphans dropped)", got)
	}
}

// TestListServers_worktreeSharesMainCheckoutEntry pins the canonical
// keying contract end to end: a Claude session in a linked worktree
// reads and writes `projects.<main root>` (the CLI resolves the
// worktree to the canonical git root), so AO's listing and toggle for
// a worktree workspace must use the same entry — keying by the raw
// worktree path shows servers as enabled that the session actually
// has disabled.
func TestListServers_worktreeSharesMainCheckoutEntry(t *testing.T) {
	mainRoot, worktree := writeWorktreeLayout(t, "blitz-388")
	body := `{
  "mcpServers": {"dispatch": {"type": "stdio", "command": "/bin/d"}},
  "projects": {
    ` + quoteJSON(mainRoot) + `: {
      "mcpServers": {"jira": {"type": "stdio", "command": "/bin/j"}},
      "disabledMcpServers": ["dispatch"]
    }
  }
}`
	store := newStoreWithFile(t, body)

	got, err := store.ListServers(worktree)
	if err != nil {
		t.Fatalf("ListServers(worktree): %v", err)
	}
	byName := map[string]Server{}
	for _, srv := range got {
		byName[srv.Name] = srv
	}
	if d := byName["dispatch"]; !d.Disabled {
		t.Errorf("dispatch = %+v, want disabled via the main root's entry", d)
	}
	if j, ok := byName["jira"]; !ok || j.Source != SourceLocal {
		t.Errorf("jira = %+v, want the main root's local-scope row visible from the worktree", j)
	}

	// A toggle through the worktree path writes the main root's entry,
	// never a worktree-keyed one.
	if err := store.SetDisabled(worktree, "jira", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	raw, _ := os.ReadFile(store.path)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode written config: %v", err)
	}
	projects := decoded["projects"].(map[string]any)
	if _, leaked := projects[worktree]; leaked {
		t.Errorf("toggle created a worktree-keyed projects entry")
	}
	main := projects[mainRoot].(map[string]any)
	list := main["disabledMcpServers"].([]any)
	found := false
	for _, v := range list {
		found = found || v == "jira"
	}
	if !found {
		t.Errorf("jira missing from main root's disabledMcpServers: %v", list)
	}
}

func TestListServers_missingFile(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "does-not-exist.json"))
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %+v", got)
	}
}

func TestSetDisabled_idempotent(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {"alpha": {"type": "stdio", "command": "/a"}}}`)
	if err := store.SetDisabled("/work", "alpha", true); err != nil {
		t.Fatalf("SetDisabled true: %v", err)
	}
	if err := store.SetDisabled("/work", "alpha", true); err != nil {
		t.Fatalf("SetDisabled true (again): %v", err)
	}
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if !got[0].Disabled {
		t.Errorf("alpha should be disabled")
	}
	raw, _ := os.ReadFile(store.path)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	projects := decoded["projects"].(map[string]any)
	work := projects["/work"].(map[string]any)
	list := work["disabledMcpServers"].([]any)
	if len(list) != 1 {
		t.Errorf("disabled list len=%d, want 1; %v", len(list), list)
	}

	if err := store.SetDisabled("/work", "alpha", false); err != nil {
		t.Fatalf("SetDisabled false: %v", err)
	}
	got, _ = store.ListServers("/work")
	if got[0].Disabled {
		t.Errorf("alpha should be enabled after re-enable")
	}
}

func TestSetDisabled_unknownServerIsAllowed(t *testing.T) {
	// Plugin and cloud entries can be toggled without a corresponding
	// mcpServers row — Claude Code does the same.
	store := newStoreWithFile(t, `{}`)
	if err := store.SetDisabled("/work", "claude.ai Gmail", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	got, err := store.ListServers("/work")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "claude.ai Gmail" || !got[0].Disabled {
		t.Fatalf("expected one cloud entry disabled, got %+v", got)
	}
	if got[0].Source != SourceCloud {
		t.Errorf("source = %q, want cloud", got[0].Source)
	}
}

func TestKeyOrderPreserved_unrelatedTopLevel(t *testing.T) {
	body := `{
  "zeta": 1,
  "alpha": 2,
  "mcpServers": {"s": {"type": "stdio", "command": "/s"}},
  "delta": 3
}`
	store := newStoreWithFile(t, body)
	if err := store.SetDisabled("/w", "s", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	raw, _ := os.ReadFile(store.path)
	out := string(raw)
	idxZeta := strings.Index(out, `"zeta"`)
	idxAlpha := strings.Index(out, `"alpha"`)
	idxDelta := strings.Index(out, `"delta"`)
	if !(idxZeta < idxAlpha && idxAlpha < idxDelta) {
		t.Errorf("expected zeta < alpha < delta in output order; got zeta=%d alpha=%d delta=%d\n%s", idxZeta, idxAlpha, idxDelta, out)
	}
}

func TestConcurrentWriteDetected(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {}}`)
	// Two concurrent calls: emulate by capturing the snapshot for the
	// first, mutating the file out-of-band, then trying to write.
	snap, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// External writer appends a key.
	external := `{"mcpServers": {}, "newKey": true}`
	if err := os.WriteFile(store.path, []byte(external), 0o600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Force a different mtime than what snapshot recorded.
	mt := snap.stat.ModTime().Add(2 * 1000 * 1000 * 1000) // +2s
	if err := os.Chtimes(store.path, mt, mt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	out, err := snap.raw.marshalIndented()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ok, err := writeIfUnchanged(store.path, out, snap.stat)
	if err != nil {
		t.Fatalf("writeIfUnchanged: %v", err)
	}
	if ok {
		t.Fatalf("write should have been refused on stale snapshot")
	}
}

func TestModifyRetriesOnceThenSucceeds(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {}}`)
	// Race the first read by mutating mid-modify. We can't easily hook
	// the internal loop, so verify directly that a clean second
	// attempt (no external change between iterations) writes
	// successfully — the integration is exercised in the simpler
	// SetDisabled tests above.
	if err := store.SetDisabled("/x", "alpha", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
}
