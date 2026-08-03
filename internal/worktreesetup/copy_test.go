package worktreesetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyEntriesCopiesMatchesAndRefusesUnsafePatterns(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "config", "local.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyEntries(context.Background(), project, worktree, []string{"config/*.env"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "config", "local.env")); err != nil || string(data) != "x" {
		t.Fatalf("copied setup file = %q, %v", data, err)
	}
	for _, pattern := range []string{"../outside", "/absolute", ".git/config", "  "} {
		if err := copyEntries(context.Background(), project, worktree, []string{pattern}); err == nil {
			t.Fatalf("unsafe pattern %q succeeded", pattern)
		}
	}
}

// A wildcard sweeping the project root must not carry the repository's `.git`
// over the worktree's — that file is what makes the checkout a worktree.
func TestCopyEntriesNeverReplacesTheWorktreeGitLink(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("source-git"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("worktree-git"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyEntries(context.Background(), project, worktree, []string{"*"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, ".git")); err != nil || string(data) != "worktree-git" {
		t.Fatalf("wildcard replaced worktree .git = %q, %v", data, err)
	}
}

// A glob naming nothing is a broken recipe, not a no-op: the worktree would be
// missing exactly the file the recipe exists to bring across.
func TestCopyEntriesRefusesZeroMatchGlobs(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	err := copyEntries(context.Background(), project, worktree, []string{"missing/*.env"})
	if err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("zero-match glob error = %v", err)
	}
	// A glob whose only matches are unsafe is refused with its own message,
	// so "nothing was copied" is never silent.
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = copyEntries(context.Background(), project, worktree, []string{".g*"})
	if err == nil || !strings.Contains(err.Error(), "matched no safe files") {
		t.Fatalf("only-unsafe-match glob error = %v", err)
	}
}

// A symlink inside the project root can point anywhere on the host, so it is
// refused at every level rather than followed.
func TestCopyEntriesRefusesSymlinkedSources(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "outside.env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(project, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := copyEntries(context.Background(), project, worktree, []string{"linked/*.env"}); err == nil {
		t.Fatal("symlinked source ancestor succeeded")
	}
	if err := os.Symlink(filepath.Join(external, "outside.env"), filepath.Join(project, "direct.env")); err != nil {
		t.Fatal(err)
	}
	if err := copyEntries(context.Background(), project, worktree, []string{"direct.env"}); err == nil {
		t.Fatal("symlinked source file succeeded")
	}
	if _, err := os.Lstat(filepath.Join(worktree, "direct.env")); !os.IsNotExist(err) {
		t.Fatalf("refused copy left a destination behind: %v", err)
	}
}

func TestCopyEntriesHonoursCancellation(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.env"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := copyEntries(ctx, project, worktree, []string{"a.env"})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled copy error = %v", err)
	}
}
