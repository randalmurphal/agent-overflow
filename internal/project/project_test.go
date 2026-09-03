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
	_, _, err := EnsureForWorkspace(nil, "/tmp/anywhere")
	if err == nil {
		t.Fatal("nil store: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("nil store: unexpected error: %v", err)
	}
}

func TestEnsureForWorkspaceRejectsEmptyPath(t *testing.T) {
	s := newTestStore(t)
	_, _, err := EnsureForWorkspace(s, "")
	if err == nil {
		t.Fatal("empty path: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "workspace path is required") {
		t.Fatalf("empty path: unexpected error: %v", err)
	}

	_, _, err = EnsureForWorkspace(s, "   ")
	if err == nil {
		t.Fatal("whitespace path: expected error, got nil")
	}
}

func TestEnsureForWorkspaceCreatesWhenAbsent(t *testing.T) {
	s := newTestStore(t)
	got, _, err := EnsureForWorkspace(s, "/tmp/repo-a")
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
	first, _, err := EnsureForWorkspace(s, "/tmp/repo-b")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	second, _, err := EnsureForWorkspace(s, "/tmp/repo-b")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second call returned a new project: first.ID=%q second.ID=%q", first.ID, second.ID)
	}
}

func TestEnsureForWorkspaceTrimsWhitespace(t *testing.T) {
	s := newTestStore(t)
	first, _, err := EnsureForWorkspace(s, "/tmp/repo-c")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Whitespace around the same path should resolve to the same row,
	// not create a duplicate.
	second, _, err := EnsureForWorkspace(s, "  /tmp/repo-c  ")
	if err != nil {
		t.Fatalf("whitespace call: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("whitespace call returned a new project: first.ID=%q second.ID=%q", first.ID, second.ID)
	}
}

// A workspace that is a LINKED WORKTREE resolves to the repository it was cut
// from, so importing (or creating) a thread there lands in the real project
// instead of minting one named after the branch. The worktree lives OUTSIDE
// the repository, exactly where AO parks its own, so nothing about this can
// fall out of path containment.
func TestEnsureForWorkspaceLandsAWorktreeInItsRepository(t *testing.T) {
	s := newTestStore(t)
	tmp := t.TempDir()

	repo := filepath.Join(tmp, "fixture-repo")
	private := filepath.Join(repo, ".git", "worktrees", "BLITZ-188")
	worktree := filepath.Join(tmp, "worktrees", "fixture-repo", "BLITZ-188")
	for _, dir := range []string{private, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(worktree, ".git"):     "gitdir: " + private + "\n",
		filepath.Join(private, "gitdir"):    filepath.Join(worktree, ".git") + "\n",
		filepath.Join(private, "commondir"): "../..\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	fromRepo, _, err := EnsureForWorkspace(s, repo)
	if err != nil {
		t.Fatalf("EnsureForWorkspace(repo): %v", err)
	}
	fromWorktree, _, err := EnsureForWorkspace(s, worktree)
	if err != nil {
		t.Fatalf("EnsureForWorkspace(worktree): %v", err)
	}
	if fromWorktree.ID != fromRepo.ID {
		t.Fatalf("worktree project = %+v, want the repository's row %+v", fromWorktree, fromRepo)
	}

	// And in the other order: a worktree seen FIRST must create the project
	// at the repository root, not at its own path — from a subdirectory of
	// the worktree too, which is what a session started deeper records.
	other := newTestStore(t)
	subdir := filepath.Join(worktree, "internal")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", subdir, err)
	}
	created, _, err := EnsureForWorkspace(other, subdir)
	if err != nil {
		t.Fatalf("EnsureForWorkspace(worktree subdir): %v", err)
	}
	if created.Name != filepath.Base(repo) {
		t.Fatalf("created project = %+v, want one named after the repository %q", created, filepath.Base(repo))
	}
	if resolved, err := filepath.EvalSymlinks(repo); err != nil || created.Path != resolved {
		t.Fatalf("created project path = %q, want the repository root %q (err %v)", created.Path, resolved, err)
	}
}

func TestConfigDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	want := filepath.Join(root, "projects", "my-project")
	if got := ConfigDir(root, "my-project"); got != want {
		t.Fatalf("ConfigDir = %q, want %q", got, want)
	}
}
