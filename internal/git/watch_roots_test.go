package git

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// TestWatchRootsPrunesIgnoredSubtrees proves the workspace side of
// WatchRoots skips fully-ignored directories (and the git dir) instead
// of one recursive root at cwd: ancestors of pruned subtrees are
// non-recursive RebuildOnChildDir roots, their surviving child dirs are
// recursive roots, and the git metadata roots are the narrow
// non-recursive-dir + refs + info set (never a recursive git dir, which
// would drag objects/ in).
func TestWatchRootsPrunesIgnoredSubtrees(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	writeRepoFile(t, repo, ".gitignore", "node_modules/\nfrontend/dist/\n")
	for _, dir := range []string{"node_modules/pkg", "frontend/dist", "frontend/src", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeRepoFile(t, repo, "node_modules/pkg/f.js", "x\n")

	roots, err := NewCore().WatchRoots(repo)
	if err != nil {
		t.Fatalf("WatchRoots: %v", err)
	}

	// Ancestors of pruned subtrees: non-recursive, rebuild-flagged.
	for _, ancestor := range []string{repo, filepath.Join(repo, "frontend")} {
		root, ok := findWatchRoot(roots, ancestor)
		if !ok || root.Recursive || !root.RebuildOnChildDir {
			t.Fatalf("ancestor %s: got %+v ok=%v, want non-recursive RebuildOnChildDir root", ancestor, root, ok)
		}
	}
	// Surviving subtrees: recursive.
	for _, subtree := range []string{filepath.Join(repo, "src"), filepath.Join(repo, "frontend", "src")} {
		if !containsWatchRoot(roots, subtree, true) {
			t.Fatalf("roots = %+v, want recursive root at %s", roots, subtree)
		}
	}
	// Pruned subtrees: absent entirely.
	for _, pruned := range []string{
		filepath.Join(repo, "node_modules"),
		filepath.Join(repo, "frontend", "dist"),
	} {
		if _, ok := findWatchRoot(roots, pruned); ok {
			t.Fatalf("roots = %+v, must not watch ignored subtree %s", roots, pruned)
		}
	}
	// Git metadata: narrow. The git dir must never be recursive (objects/).
	gitDir := filepath.Join(repo, ".git")
	if root, ok := findWatchRoot(roots, gitDir); !ok || root.Recursive {
		t.Fatalf("git dir root: got %+v ok=%v, want non-recursive", root, ok)
	}
	if !containsWatchRoot(roots, filepath.Join(gitDir, "refs"), true) {
		t.Fatalf("roots = %+v, want recursive refs root", roots)
	}
	if !containsWatchRoot(roots, filepath.Join(gitDir, "info"), false) {
		t.Fatalf("roots = %+v, want non-recursive info root (exclude file)", roots)
	}
}

// TestPruneIgnoredSubtrees covers the pure pruning geometry: the
// no-boundary passthrough, ancestor/subtree splitting, nested-boundary
// filtering, malformed boundaries, non-directory entries, and the root
// cap — each returning either the pruned set or the fallback signal.
func TestPruneIgnoredSubtrees(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"a/b", "a/c", "d"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "e.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// No boundaries: single recursive root, ok.
	roots, ok := pruneIgnoredSubtrees(dir, nil)
	if !ok || len(roots) != 1 || !roots[0].Recursive || roots[0].Path != dir {
		t.Fatalf("no boundaries: got %+v ok=%v, want single recursive root", roots, ok)
	}

	// One nested boundary: cwd and "a" become rebuild ancestors, "a/c"
	// and "d" recursive subtrees, "a/b" absent, "e.txt" not a root.
	roots, ok = pruneIgnoredSubtrees(dir, []string{filepath.Join("a", "b")})
	if !ok {
		t.Fatalf("nested boundary: fallback signalled unexpectedly")
	}
	want := []WatchRoot{
		{Path: dir, Recursive: false, RebuildOnChildDir: true},
		{Path: filepath.Join(dir, "a"), Recursive: false, RebuildOnChildDir: true},
		{Path: filepath.Join(dir, "a", "c"), Recursive: true},
		{Path: filepath.Join(dir, "d"), Recursive: true},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Path < want[j].Path })
	if !slices.Equal(roots, want) {
		t.Fatalf("nested boundary:\n got %+v\nwant %+v", roots, want)
	}

	// A boundary nested inside another boundary defers to the outer one.
	roots, ok = pruneIgnoredSubtrees(dir, []string{"a", filepath.Join("a", "b")})
	if !ok {
		t.Fatalf("nested-in-blocked: fallback signalled unexpectedly")
	}
	if _, found := findWatchRoot(roots, filepath.Join(dir, "a")); found {
		t.Fatalf("nested-in-blocked: got %+v, blocked dir 'a' must not be a root", roots)
	}
	if !containsWatchRoot(roots, filepath.Join(dir, "d"), true) {
		t.Fatalf("nested-in-blocked: got %+v, want recursive root at d", roots)
	}

	// Malformed boundaries mean the listing wasn't understood: fall back.
	for _, bad := range [][]string{{".."}, {"."}, {string(filepath.Separator) + "abs"}} {
		if _, ok := pruneIgnoredSubtrees(dir, bad); ok {
			t.Fatalf("malformed boundary %v: want fallback", bad)
		}
	}

	// Root cap: more sibling dirs than maxPrunedWatchRoots forces fallback.
	capDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(capDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	for i := 0; i <= maxPrunedWatchRoots; i++ {
		if err := os.Mkdir(filepath.Join(capDir, fmt.Sprintf("d%03d", i)), 0o755); err != nil {
			t.Fatalf("mkdir cap fixture: %v", err)
		}
	}
	if _, ok := pruneIgnoredSubtrees(capDir, []string{"blocked"}); ok {
		t.Fatalf("cap overflow: want fallback to single recursive root")
	}
}

func containsWatchRoot(roots []WatchRoot, target string, recursive bool) bool {
	root, ok := findWatchRoot(roots, target)
	return ok && root.Recursive == recursive
}

func findWatchRoot(roots []WatchRoot, target string) (WatchRoot, bool) {
	for _, root := range roots {
		if SameFilesystemPath(root.Path, target) {
			return root, true
		}
	}
	return WatchRoot{}, false
}
