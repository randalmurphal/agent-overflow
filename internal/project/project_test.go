package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEnsureForWorkspaceRejectsNilStore(t *testing.T) {
	_, err := EnsureForWorkspace(nil, nil, "/tmp/anywhere")
	if err == nil {
		t.Fatal("nil store: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("nil store: unexpected error: %v", err)
	}
}

func TestEnsureForWorkspaceRejectsEmptyPath(t *testing.T) {
	s := newTestStore(t)
	_, err := EnsureForWorkspace(s, nil, "")
	if err == nil {
		t.Fatal("empty path: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "workspace path is required") {
		t.Fatalf("empty path: unexpected error: %v", err)
	}

	_, err = EnsureForWorkspace(s, nil, "   ")
	if err == nil {
		t.Fatal("whitespace path: expected error, got nil")
	}
}

func TestEnsureForWorkspaceCreatesWhenAbsent(t *testing.T) {
	s := newTestStore(t)
	got, err := EnsureForWorkspace(s, nil, "/tmp/repo-a")
	if err != nil {
		t.Fatalf("EnsureForWorkspace: %v", err)
	}
	if got.Path != "/tmp/repo-a" {
		t.Fatalf("Path = %q, want %q", got.Path, "/tmp/repo-a")
	}
	if got.Name != "repo-a" {
		t.Fatalf("Name = %q, want %q (filepath.Base)", got.Name, "repo-a")
	}
	if got.Slug != "repo-a" {
		t.Fatalf("Slug = %q, want %q", got.Slug, "repo-a")
	}
	if got.ID == "" {
		t.Fatal("ID empty")
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Fatalf("timestamps unset: %+v", got)
	}
}

func TestEnsureForWorkspaceReturnsExistingByPath(t *testing.T) {
	s := newTestStore(t)
	first, err := EnsureForWorkspace(s, nil, "/tmp/repo-b")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	second, err := EnsureForWorkspace(s, nil, "/tmp/repo-b")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second call returned a new project: first.ID=%q second.ID=%q", first.ID, second.ID)
	}
}

func TestEnsureForWorkspaceTrimsWhitespace(t *testing.T) {
	s := newTestStore(t)
	first, err := EnsureForWorkspace(s, nil, "/tmp/repo-c")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Whitespace around the same path should resolve to the same row,
	// not create a duplicate.
	second, err := EnsureForWorkspace(s, nil, "  /tmp/repo-c  ")
	if err != nil {
		t.Fatalf("whitespace call: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("whitespace call returned a new project: first.ID=%q second.ID=%q", first.ID, second.ID)
	}
}

func TestConfigDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	want := filepath.Join(root, "projects", "my-project")
	if got := ConfigDir(root, "my-project"); got != want {
		t.Fatalf("ConfigDir = %q, want %q", got, want)
	}
}

func TestEnsureConfigDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	want := filepath.Join(root, "projects", "my-project")
	got, err := EnsureConfigDir(root, "my-project")
	if err != nil {
		t.Fatalf("EnsureConfigDir: %v", err)
	}
	if got != want {
		t.Fatalf("EnsureConfigDir path = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("config path is not a directory: %s", got)
	}
}

func TestEnsureConfigDirReturnsMkdirError(t *testing.T) {
	root := t.TempDir()
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	if _, err := EnsureConfigDir(blockingFile, "my-project"); err == nil {
		t.Fatal("EnsureConfigDir error = nil, want MkdirAll error")
	}
}
