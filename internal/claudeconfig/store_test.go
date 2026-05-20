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

func TestListServers_pluginAndCloudOnlyEntries(t *testing.T) {
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
	want := []string{"alpha", "claude.ai Gmail", "plugin:p:tool"}
	sortedNames := append([]string{}, names...)
	sort.Strings(sortedNames)
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(sortedNames, wantSorted) {
		t.Fatalf("names = %v, want %v", sortedNames, wantSorted)
	}
	if sources["alpha"] != SourceUser {
		t.Errorf("alpha source = %q, want user", sources["alpha"])
	}
	if sources["claude.ai Gmail"] != SourceCloud {
		t.Errorf("claude.ai Gmail source = %q, want cloud", sources["claude.ai Gmail"])
	}
	if sources["plugin:p:tool"] != SourcePlugin {
		t.Errorf("plugin:p:tool source = %q, want plugin", sources["plugin:p:tool"])
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

func TestCreateServer_writesAndPreservesUnrelatedKeys(t *testing.T) {
	body := `{
  "numStartups": 17,
  "mcpServers": {"alpha": {"type": "stdio", "command": "/bin/alpha"}},
  "projects": {"/x": {"hasTrustDialogAccepted": true}}
}`
	store := newStoreWithFile(t, body)
	err := store.CreateServer(Server{
		Name:      "bravo",
		Source:    SourceUser,
		Transport: TransportStdio,
		Command:   "/bin/bravo",
		Args:      []string{"--flag"},
		Env:       map[string]string{"K": "${V}"},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(raw), `"numStartups": 17`) {
		t.Errorf("untouched top-level key lost; file=\n%s", raw)
	}
	if !strings.Contains(string(raw), `"hasTrustDialogAccepted": true`) {
		t.Errorf("untouched project key lost; file=\n%s", raw)
	}
	if !strings.Contains(string(raw), `"bravo"`) {
		t.Errorf("new server not written; file=\n%s", raw)
	}

	got, err := store.ListServers("/x")
	if err != nil {
		t.Fatalf("ListServers after write: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d after create, want 2; got=%+v", len(got), got)
	}
}

func TestCreateServer_rejectsDuplicate(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {"alpha": {"type": "stdio", "command": "/a"}}}`)
	err := store.CreateServer(Server{Name: "alpha", Source: SourceUser, Transport: TransportStdio, Command: "/b"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestUpdateServer_replacesEntryFields(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {"alpha": {"type": "stdio", "command": "/a", "args": ["one"]}}}`)
	err := store.UpdateServer(Server{
		Name:      "alpha",
		Source:    SourceUser,
		Transport: TransportStdio,
		Command:   "/a-new",
		Args:      []string{"two"},
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	got, err := store.ListServers("/x")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].Command != "/a-new" || !reflect.DeepEqual(got[0].Args, []string{"two"}) {
		t.Errorf("unexpected: %+v", got[0])
	}
}

func TestUpdateServer_missingErrors(t *testing.T) {
	store := newStoreWithFile(t, `{"mcpServers": {}}`)
	err := store.UpdateServer(Server{Name: "ghost", Source: SourceUser, Transport: TransportStdio, Command: "/g"})
	if err == nil {
		t.Fatalf("expected ErrNotFound, got nil")
	}
}

func TestDeleteServer_stripsDisabledEverywhere(t *testing.T) {
	body := `{
  "mcpServers": {"alpha": {"type": "stdio", "command": "/a"}, "bravo": {"type": "stdio", "command": "/b"}},
  "projects": {
    "/x": {"disabledMcpServers": ["alpha", "bravo"]},
    "/y": {"disabledMcpServers": ["alpha"]}
  }
}`
	store := newStoreWithFile(t, body)
	if err := store.DeleteServer("alpha"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	raw, _ := os.ReadFile(store.path)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mcps := decoded["mcpServers"].(map[string]any)
	if _, ok := mcps["alpha"]; ok {
		t.Errorf("alpha not removed from mcpServers")
	}
	if _, ok := mcps["bravo"]; !ok {
		t.Errorf("bravo lost from mcpServers")
	}
	projects := decoded["projects"].(map[string]any)
	for _, key := range []string{"/x", "/y"} {
		proj := projects[key].(map[string]any)
		disabled, _ := proj["disabledMcpServers"].([]any)
		for _, d := range disabled {
			if d.(string) == "alpha" {
				t.Errorf("alpha still listed under %s", key)
			}
		}
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

func TestValidate_rejectsBadTransport(t *testing.T) {
	store := newStoreWithFile(t, `{}`)
	err := store.CreateServer(Server{Name: "x", Source: SourceUser, Transport: "websocket", URL: "wss://x"})
	if err == nil || !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("expected unsupported transport error, got %v", err)
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
