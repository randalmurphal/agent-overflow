package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/workspacefiles"
)

func newWorkspaceFilesApp(t *testing.T) (*App, WorkspaceRef, string) {
	t.Helper()
	app := newTestAppWithStore(t)
	app.workspaceFiles = workspacefiles.NewSearcher(workspacefiles.Config{})

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		"README.md":     "readme",
		"src/main.ts":   "main",
		"src/helper.ts": "helper",
		"package.json":  "{}",
	}
	for path, body := range files {
		full := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	return app, testWorkspaceRef(t, app, workspace), workspace
}

func TestSearchWorkspaceFilesReturnsMatches(t *testing.T) {
	app, ref, workspace := newWorkspaceFilesApp(t)

	got, err := app.SearchWorkspaceFiles(ref, "main", 10)
	if err != nil {
		t.Fatalf("SearchWorkspaceFiles: %v", err)
	}
	if got.Root != workspace {
		t.Fatalf("Root: got %q want %q", got.Root, workspace)
	}
	if len(got.Files) == 0 {
		t.Fatal("expected at least one file match")
	}
	if !strings.HasSuffix(got.Files[0].Path, "main.ts") {
		t.Fatalf("best match should be main.ts, got %q", got.Files[0].Path)
	}
}

func TestSearchWorkspaceFilesEmptyQueryReturnsAll(t *testing.T) {
	app, ref, _ := newWorkspaceFilesApp(t)
	got, err := app.SearchWorkspaceFiles(ref, "", 50)
	if err != nil {
		t.Fatalf("SearchWorkspaceFiles: %v", err)
	}
	if len(got.Files) == 0 {
		t.Fatal("expected workspace contents on empty query")
	}
}

func TestSearchWorkspaceFilesUnresolvableWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	app.workspaceFiles = workspacefiles.NewSearcher(workspacefiles.Config{})

	_, err := app.SearchWorkspaceFiles(WorkspaceRef{ProjectID: "nope"}, "foo", 10)
	if err == nil {
		t.Fatal("expected error for unknown project")
	}
}

func TestSearchWorkspaceFilesUnsetSearcher(t *testing.T) {
	app := newTestAppWithStore(t)
	_, err := app.SearchWorkspaceFiles(WorkspaceRef{ProjectID: "x"}, "foo", 10)
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected init error, got %v", err)
	}
}
