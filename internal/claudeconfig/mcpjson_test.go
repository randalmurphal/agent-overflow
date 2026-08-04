package claudeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func newMcpjsonStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "claude.json"))
}

func TestProjectServers_walksAncestorsCloserWins(t *testing.T) {
	store := newMcpjsonStore(t)
	base := t.TempDir()
	workspace := filepath.Join(base, "org", "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(workspace, ".mcp.json"), `{"mcpServers": {"context7": {"command": "npx"}}}`)
	writeFile(t, filepath.Join(base, "org", ".mcp.json"), `{"mcpServers": {"org-wide": {"command": "x"}}}`)

	got, err := store.projectServers(workspace)
	if err != nil {
		t.Fatalf("projectServers: %v", err)
	}
	byName := map[string]Server{}
	for _, srv := range got {
		byName[srv.Name] = srv
	}
	for _, name := range []string{"context7", "org-wide"} {
		srv, ok := byName[name]
		if !ok || srv.Source != SourceProject || srv.Disabled {
			t.Errorf("%s = %#v, want enabled project-source row", name, srv)
		}
	}
	// Names only — .mcp.json config never flows out.
	if srv := byName["context7"]; srv.Command != "" || len(srv.Args) != 0 {
		t.Errorf("project row leaked config: %#v", srv)
	}
}

func TestProjectServers_disabledMcpjsonServersRejects(t *testing.T) {
	store := newMcpjsonStore(t)
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, ".mcp.json"), `{"mcpServers": {"context7": {"command": "npx"}, "other": {"command": "y"}}}`)
	if err := os.MkdirAll(filepath.Join(workspace, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(workspace, ".claude", "settings.local.json"), `{"disabledMcpjsonServers": ["context7"]}`)

	got, err := store.projectServers(workspace)
	if err != nil {
		t.Fatalf("projectServers: %v", err)
	}
	for _, srv := range got {
		wantDisabled := srv.Name == "context7"
		if srv.Disabled != wantDisabled {
			t.Errorf("%s disabled = %v, want %v", srv.Name, srv.Disabled, wantDisabled)
		}
	}
}

func TestProjectServers_bareTopLevelShapeAccepted(t *testing.T) {
	store := newMcpjsonStore(t)
	workspace := t.TempDir()
	// Plugin-style bare map, no mcpServers wrapper.
	writeFile(t, filepath.Join(workspace, ".mcp.json"), `{"tool": {"command": "t"}}`)
	got, err := store.projectServers(workspace)
	if err != nil {
		t.Fatalf("projectServers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "tool" {
		t.Fatalf("got %v, want the bare-shape server", got)
	}
}
