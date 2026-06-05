package git

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestWatchRootsReturnsCwdOnlyForNonRepo(t *testing.T) {
	dir := t.TempDir()
	roots, err := NewCore().WatchRoots(dir)
	if err != nil {
		t.Fatalf("WatchRoots(non-repo): %v", err)
	}
	if len(roots) != 1 || !containsWatchRoot(roots, dir, true) {
		t.Fatalf("roots = %#v, want only recursive %q", roots, dir)
	}
}

func TestWatchRootsIncludesNarrowGitMetadataForLinkedWorktree(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/watch-roots")
	worktreePath := filepath.Join(t.TempDir(), "feature-watch-roots")
	testutil.RunGit(t, repo, "worktree", "add", worktreePath, "feature/watch-roots")

	core := NewCore()
	roots, err := core.WatchRoots(worktreePath)
	if err != nil {
		t.Fatalf("WatchRoots(worktree): %v", err)
	}
	gitDir, ok, err := core.revParsePath(worktreePath, "--absolute-git-dir")
	if err != nil || !ok {
		t.Fatalf("revParsePath(--absolute-git-dir) = %q, %v, %v", gitDir, ok, err)
	}
	commonDir, ok, err := core.revParsePath(worktreePath, "--git-common-dir")
	if err != nil || !ok {
		t.Fatalf("revParsePath(--git-common-dir) = %q, %v, %v", commonDir, ok, err)
	}
	refsDir := filepath.Join(commonDir, "refs")

	if !containsWatchRoot(roots, worktreePath, true) {
		t.Fatalf("roots = %#v, want recursive worktree path %q", roots, worktreePath)
	}
	if !containsWatchRoot(roots, gitDir, true) {
		t.Fatalf("roots = %#v, want recursive worktree gitdir %q", roots, gitDir)
	}
	if !containsWatchRoot(roots, commonDir, false) {
		t.Fatalf("roots = %#v, want non-recursive common gitdir %q", roots, commonDir)
	}
	if !containsWatchRoot(roots, refsDir, true) {
		t.Fatalf("roots = %#v, want recursive common refs dir %q", roots, refsDir)
	}
}

func containsWatchRoot(roots []WatchRoot, target string, recursive bool) bool {
	for _, root := range roots {
		if root.Recursive == recursive && SameFilesystemPath(root.Path, target) {
			return true
		}
	}
	return false
}
