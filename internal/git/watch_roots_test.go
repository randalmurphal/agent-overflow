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

// isolateGlobalGitConfig keeps the runner's global/system git config —
// most importantly a core.excludesFile that may ignore fixture dirs
// like node_modules — out of pruning-geometry fixtures.
func isolateGlobalGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

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
// non-recursive KindAncestor roots, their surviving child dirs are
// recursive roots, and the git metadata roots are the narrow
// non-recursive-dir + refs + info set (never a recursive git dir, which
// would drag objects/ in).
func TestWatchRootsPrunesIgnoredSubtrees(t *testing.T) {
	isolateGlobalGitConfig(t)
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

	// Ancestors of pruned subtrees: non-recursive, KindAncestor.
	for _, ancestor := range []string{repo, filepath.Join(repo, "frontend")} {
		root, ok := findWatchRoot(roots, ancestor)
		if !ok || root.Recursive || root.Kind != KindAncestor {
			t.Fatalf("ancestor %s: got %+v ok=%v, want non-recursive KindAncestor root", ancestor, root, ok)
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
	if root, ok := findWatchRoot(roots, gitDir); !ok || root.Recursive || root.Kind != KindGitMeta {
		t.Fatalf("git dir root: got %+v ok=%v, want non-recursive KindGitMeta", root, ok)
	}
	if !containsWatchRoot(roots, filepath.Join(gitDir, "refs"), true) {
		t.Fatalf("roots = %+v, want recursive refs root", roots)
	}
	if !containsWatchRoot(roots, filepath.Join(gitDir, "info"), false) {
		t.Fatalf("roots = %+v, want non-recursive info root (exclude file)", roots)
	}
}

// TestWatchRootsIgnoredDirWithForcedTrackedFileNotPruned pins the git
// property the whole pruning design leans on: `ls-files --directory`
// never collapses a directory that holds an index-tracked file. After
// `git add -f` inside an ignored tree, the boundary must move below the
// tracked file's directory so edits to it stay watched.
func TestWatchRootsIgnoredDirWithForcedTrackedFileNotPruned(t *testing.T) {
	isolateGlobalGitConfig(t)
	repo := testutil.InitGitRepo(t)
	writeRepoFile(t, repo, ".gitignore", "node_modules/\n")
	writeRepoFile(t, repo, filepath.Join("node_modules", "pkg", "f.js"), "x\n")
	writeRepoFile(t, repo, filepath.Join("node_modules", "other", "g.js"), "y\n")
	testutil.RunGit(t, repo, "add", "-f", filepath.Join("node_modules", "pkg", "f.js"))

	roots, err := NewCore().WatchRoots(repo)
	if err != nil {
		t.Fatalf("WatchRoots: %v", err)
	}
	nm := filepath.Join(repo, "node_modules")
	// node_modules now holds a tracked file: it must be an ancestor
	// (the still-fully-ignored "other" collapses below it), with the
	// tracked file's own directory watched recursively.
	if root, ok := findWatchRoot(roots, nm); !ok || root.Recursive || root.Kind != KindAncestor {
		t.Fatalf("node_modules root: got %+v ok=%v, want non-recursive KindAncestor (not pruned)", root, ok)
	}
	if !containsWatchRoot(roots, filepath.Join(nm, "pkg"), true) {
		t.Fatalf("roots = %+v, want recursive root at node_modules/pkg (holds a tracked file)", roots)
	}
	if _, ok := findWatchRoot(roots, filepath.Join(nm, "other")); ok {
		t.Fatalf("roots = %+v, fully-ignored node_modules/other must stay pruned", roots)
	}
}

// TestWatchRootsIncludesGlobalExcludesDir proves the directory holding
// core.excludesFile becomes a watch root carrying the file's basename
// as TriggerFile: editing the global ignore file changes prunable
// boundaries exactly like a .gitignore edit, but lives outside the
// workspace — and only that one file's events may trigger recomputes
// (the parent dir can be as busy as $HOME).
func TestWatchRootsIncludesGlobalExcludesDir(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	excludesDir := t.TempDir()
	excludesFile := filepath.Join(excludesDir, "my-ignores")
	if err := os.WriteFile(excludesFile, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write excludes: %v", err)
	}
	// Repo-local config wins over any global config on the test runner.
	testutil.RunGit(t, repo, "config", "core.excludesFile", excludesFile)

	roots, err := NewCore().WatchRoots(repo)
	if err != nil {
		t.Fatalf("WatchRoots: %v", err)
	}
	root, ok := findWatchRoot(roots, excludesDir)
	if !ok || root.Recursive || root.TriggerFile != "my-ignores" {
		t.Fatalf("excludes dir root: got %+v ok=%v, want non-recursive root at %s with TriggerFile=my-ignores",
			root, ok, excludesDir)
	}
}

// TestWatchRootsRelativeExcludesFileResolvesAgainstRepo: a relative
// core.excludesFile is resolved by git against the cwd its commands run
// in — the repo — so the watch root must join it there, not against the
// app process's own working directory.
func TestWatchRootsRelativeExcludesFileResolvesAgainstRepo(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	confDir := filepath.Join(repo, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeRepoFile(t, repo, filepath.Join("conf", "ign"), "node_modules/\n")
	testutil.RunGit(t, repo, "config", "core.excludesFile", "conf/ign")

	roots, err := NewCore().WatchRoots(repo)
	if err != nil {
		t.Fatalf("WatchRoots: %v", err)
	}
	// conf/ is also a plain workspace subtree root here; the excludes
	// entry is a separate raw entry (normalization merges them later),
	// so search for the trigger-bearing one specifically.
	for _, root := range roots {
		if SameFilesystemPath(root.Path, confDir) && root.TriggerFile == "ign" {
			return
		}
	}
	t.Fatalf("roots = %+v, want an entry at %s with TriggerFile=ign", roots, confDir)
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

	// No boundaries: single recursive root, no error.
	roots, err := pruneIgnoredSubtrees(dir, nil)
	if err != nil || len(roots) != 1 || !roots[0].Recursive || roots[0].Path != dir {
		t.Fatalf("no boundaries: got %+v err=%v, want single recursive root", roots, err)
	}

	// One nested boundary: cwd and "a" become rebuild ancestors, "a/c"
	// and "d" recursive subtrees, "a/b" absent, "e.txt" not a root.
	roots, err = pruneIgnoredSubtrees(dir, []string{filepath.Join("a", "b")})
	if err != nil {
		t.Fatalf("nested boundary: %v", err)
	}
	want := []WatchRoot{
		{Path: dir, Recursive: false, Kind: KindAncestor},
		{Path: filepath.Join(dir, "a"), Recursive: false, Kind: KindAncestor},
		{Path: filepath.Join(dir, "a", "c"), Recursive: true, Kind: KindSubtree},
		{Path: filepath.Join(dir, "d"), Recursive: true, Kind: KindSubtree},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Path < want[j].Path })
	if !slices.Equal(roots, want) {
		t.Fatalf("nested boundary:\n got %+v\nwant %+v", roots, want)
	}

	// A boundary nested inside another boundary defers to the outer one.
	roots, err = pruneIgnoredSubtrees(dir, []string{"a", filepath.Join("a", "b")})
	if err != nil {
		t.Fatalf("nested-in-blocked: %v", err)
	}
	if _, found := findWatchRoot(roots, filepath.Join(dir, "a")); found {
		t.Fatalf("nested-in-blocked: got %+v, blocked dir 'a' must not be a root", roots)
	}
	if !containsWatchRoot(roots, filepath.Join(dir, "d"), true) {
		t.Fatalf("nested-in-blocked: got %+v, want recursive root at d", roots)
	}

	// Malformed boundaries mean the listing wasn't understood: fall back.
	for _, bad := range [][]string{{".."}, {"."}, {string(filepath.Separator) + "abs"}} {
		if _, err := pruneIgnoredSubtrees(dir, bad); err == nil {
			t.Fatalf("malformed boundary %v: want error", bad)
		}
	}

	// Root cap: more sibling dirs than maxPrunedWatchRoots forces fallback.
	// The boundary is already depth 1, so the depth ladder has nowhere to
	// degrade to — the overflow must surface as an error.
	capDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(capDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked: %v", err)
	}
	for i := 0; i <= maxPrunedWatchRoots; i++ {
		if err := os.Mkdir(filepath.Join(capDir, fmt.Sprintf("d%04d", i)), 0o755); err != nil {
			t.Fatalf("mkdir cap fixture: %v", err)
		}
	}
	if _, err := pruneIgnoredSubtrees(capDir, []string{"blocked"}); err == nil {
		t.Fatalf("cap overflow: want error signalling fallback to single recursive root")
	}
}

// TestPruneIgnoredSubtreesDepthLadderDegrades covers the overflow path
// that must NOT give up pruning: a __pycache__-shaped tree whose full
// boundary set needs more than maxPrunedWatchRoots roots (one ancestor
// per package dir) degrades to pruning only the shallow boundaries.
// The shallow boundary (the big tree worth pruning) stays pruned; the
// deep scattered boundaries stay watched via their recursive top-level
// subtree roots.
func TestPruneIgnoredSubtreesDepthLadderDegrades(t *testing.T) {
	dir := t.TempDir()
	// 40 top-level dirs x 30 subdirs, each holding a depth-3 ignored
	// boundary: pruning all of them needs 1 + 40 + 1200 ancestor roots,
	// over the 1024 cap. Plus one depth-1 boundary that survives the
	// depth-2 rung.
	const tops, subs = 40, 30
	boundaries := make([]string, 0, tops*subs+1)
	for ti := range tops {
		for si := range subs {
			b := filepath.Join(fmt.Sprintf("t%02d", ti), fmt.Sprintf("s%02d", si), "ig")
			if err := os.MkdirAll(filepath.Join(dir, b), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", b, err)
			}
			boundaries = append(boundaries, b)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "topig"), 0o755); err != nil {
		t.Fatalf("mkdir topig: %v", err)
	}
	boundaries = append(boundaries, "topig")

	roots, err := pruneIgnoredSubtrees(dir, boundaries)
	if err != nil {
		t.Fatalf("depth ladder must degrade, not fail: %v", err)
	}
	if len(roots) > maxPrunedWatchRoots {
		t.Fatalf("got %d roots, want <= %d", len(roots), maxPrunedWatchRoots)
	}
	// topig (depth 1) survives the degraded rung: still pruned.
	if _, found := findWatchRoot(roots, filepath.Join(dir, "topig")); found {
		t.Fatalf("got %+v, depth-1 boundary topig must stay pruned", roots)
	}
	// The deep boundaries' top-level dirs collapse back to plain
	// recursive subtree roots — watched, not exploded into ancestors.
	if !containsWatchRoot(roots, filepath.Join(dir, "t00"), true) {
		t.Fatalf("got %d roots, want recursive subtree root at t00", len(roots))
	}
	if _, found := findWatchRoot(roots, filepath.Join(dir, "t00", "s00")); found {
		t.Fatalf("s00 must not be a root: its parent t00 is already recursive")
	}
	if root, _ := findWatchRoot(roots, dir); root.Kind != KindAncestor || root.Recursive {
		t.Fatalf("cwd root = %+v, want non-recursive KindAncestor (topig still pruned)", root)
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
