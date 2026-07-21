package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WatchRoot is a filesystem path that should trigger live git-status refreshes.
// Recursive roots watch every descendant; non-recursive roots watch only the
// directory itself.
type WatchRoot struct {
	Path      string
	Recursive bool

	// RebuildOnChildDir marks workspace-side non-recursive roots (the
	// ancestors of pruned ignored subtrees): a directory appearing
	// directly under one of these is not covered by any existing root,
	// so the watcher must recompute its roots. Git metadata roots leave
	// it false — objects/ fan-out and worktree bookkeeping create
	// directories routinely and never invalidate the root set.
	RebuildOnChildDir bool
}

// maxPrunedWatchRoots caps how many roots ignored-subtree pruning may
// produce. A pathological layout (thousands of scattered ignored dirs)
// degrades to the single recursive root rather than exploding the root
// list — correct either way; pruning is an optimization.
const maxPrunedWatchRoots = 256

// WatchRoots returns the filesystem roots a live git-status watcher should
// observe for cwd. Linked worktrees keep the important commit/index/ref
// metadata outside the worktree directory, so watching cwd alone misses
// external commits made from a terminal in that worktree.
//
// The workspace side prunes git-ignored subtrees: content inside a
// fully-ignored directory can never change `git status` output, while
// watching it costs one inotify watch per directory (node_modules alone
// is routinely thousands) and turns every build/install into a refresh
// storm. Instead of one recursive root at cwd, the pruned form watches
// each maximal non-ignored subtree recursively and every ancestor of an
// ignored boundary non-recursively (flagged RebuildOnChildDir so the
// watcher recomputes roots when a new directory appears there). The
// transitions that could re-legitimize an ignored path — .gitignore
// edits, `git add -f`, index writes — all happen inside watched
// locations, so the watcher can react by rebuilding.
//
// The git metadata side is deliberately narrow: for a primary checkout
// the git dir is watched non-recursively (HEAD, index, MERGE_HEAD,
// packed-refs, and pending-op markers are all directly in it) plus
// refs/ and info/ — never objects/, whose loose-object fan-out both
// explodes the watch count and fires on every object write. A linked
// worktree's private gitdir is small and watched recursively, with the
// same narrow treatment for the shared common dir.
func (c *Core) WatchRoots(cwd string) ([]WatchRoot, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("git watch roots: cwd is required")
	}

	gitDir, ok, err := c.revParsePath(cwd, "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	if !ok {
		return []WatchRoot{{Path: cwd, Recursive: true}}, nil
	}

	roots := c.workspaceWatchRoots(cwd, gitDir)

	commonDir, ok, err := c.revParsePath(cwd, "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if gitDir == commonDir || !ok {
		// Primary checkout: gitDir IS the common dir.
		roots = append(roots, gitMetadataRoots(gitDir)...)
		return roots, nil
	}

	// Linked worktree: the private gitdir (worktrees/<name>) is small —
	// gitdir file, HEAD, index, pending-op state — and safe to watch
	// whole; the shared common dir gets the narrow treatment.
	roots = append(roots, WatchRoot{Path: gitDir, Recursive: true})
	roots = append(roots, gitMetadataRoots(commonDir)...)
	return roots, nil
}

// gitMetadataRoots returns the narrow watch set for a git directory:
// the dir itself non-recursively (top-level state files like HEAD,
// index, MERGE_HEAD, packed-refs, and the pending-op marker dirs whose
// creation is itself a top-level event), refs/ recursively
// (branch/remote/stash movement), and info/ non-recursively (the
// exclude file, which changes ignore rules exactly like a .gitignore
// edit). Never objects/ — loose-object fan-out explodes the watch
// count and no object write changes status without a ref/index write
// beside it.
func gitMetadataRoots(gitDir string) []WatchRoot {
	roots := []WatchRoot{{Path: gitDir, Recursive: false}}
	if refsDir := filepath.Join(gitDir, "refs"); isExistingDirectory(refsDir) {
		roots = append(roots, WatchRoot{Path: refsDir, Recursive: true})
	}
	if infoDir := filepath.Join(gitDir, "info"); isExistingDirectory(infoDir) {
		roots = append(roots, WatchRoot{Path: infoDir, Recursive: false})
	}
	return roots
}

// workspaceWatchRoots computes the pruned workspace-side roots for cwd.
// Any failure (git listing, readdir, cap overflow) degrades to the
// single recursive root — always correct, just heavier.
func (c *Core) workspaceWatchRoots(cwd, gitDir string) []WatchRoot {
	blocked, err := c.ignoredBoundaryDirs(cwd)
	if err != nil {
		return []WatchRoot{{Path: cwd, Recursive: true}}
	}
	// The git dir is metadata, not workspace: its watch policy is owned
	// by gitMetadataRoots. For a primary checkout it sits under cwd and
	// must be pruned here or the recursive workspace root would drag
	// objects/ back in.
	if rel, err := filepath.Rel(cwd, gitDir); err == nil &&
		rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		blocked = append(blocked, rel)
	}
	roots, ok := pruneIgnoredSubtrees(cwd, blocked)
	if !ok {
		return []WatchRoot{{Path: cwd, Recursive: true}}
	}
	return roots
}

// ignoredBoundaryDirs lists the directories git collapses as fully
// ignored, relative to cwd (e.g. "node_modules", "frontend/dist").
// --directory makes git report a wholly-ignored tree as one entry
// instead of enumerating its files, so the output is small and each
// entry is exactly a subtree the watcher can skip.
func (c *Core) ignoredBoundaryDirs(cwd string) ([]string, error) {
	result, err := c.run(cwd, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git watch roots: ls-files --ignored failed: %s", strings.TrimSpace(result.stderr))
	}
	var dirs []string
	for entry := range strings.SplitSeq(result.stdout, "\x00") {
		// Ignored files that don't live in a fully-ignored directory are
		// listed individually without the trailing separator; they stay
		// watched (their parent is part of a watched subtree).
		if rel, isDir := strings.CutSuffix(entry, "/"); isDir && rel != "" {
			dirs = append(dirs, filepath.FromSlash(rel))
		}
	}
	return dirs, nil
}

// pruneIgnoredSubtrees turns (cwd, blocked subtree list) into watch
// roots: every ancestor of a blocked dir is watched non-recursively
// (with RebuildOnChildDir set), and each of its child directories that
// is neither blocked nor itself an ancestor becomes a recursive root.
// Returns ok=false when the caller should fall back to the single
// recursive root (no blocked dirs is ok=true with that same single
// root; failure modes are readdir errors and the root cap).
func pruneIgnoredSubtrees(cwd string, blocked []string) ([]WatchRoot, bool) {
	cleaned := make([]string, 0, len(blocked))
	for _, b := range blocked {
		b = filepath.Clean(b)
		if b == "." || b == "" || b == ".." || strings.HasPrefix(b, ".."+string(filepath.Separator)) || filepath.IsAbs(b) {
			// A boundary that isn't a proper relative subtree means the
			// listing wasn't what we expected; don't guess.
			return nil, false
		}
		cleaned = append(cleaned, b)
	}
	if len(cleaned) == 0 {
		return []WatchRoot{{Path: cwd, Recursive: true}}, true
	}

	// Drop boundaries nested inside other boundaries (defensive: git's
	// --directory collapse shouldn't emit them, but the gitDir append in
	// workspaceWatchRoots could nest if a repo ignored its own .git
	// parent chain in a weird layout).
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) < len(cleaned[j]) })
	blockedSet := make(map[string]struct{}, len(cleaned))
	for _, b := range cleaned {
		if hasBlockedAncestor(blockedSet, b) {
			continue
		}
		blockedSet[b] = struct{}{}
	}

	ancestors := make(map[string]struct{})
	for b := range blockedSet {
		for dir := filepath.Dir(b); ; dir = filepath.Dir(dir) {
			ancestors[dir] = struct{}{}
			if dir == "." {
				break
			}
		}
	}

	var roots []WatchRoot
	for ancestor := range ancestors {
		abs := cwd
		if ancestor != "." {
			abs = filepath.Join(cwd, ancestor)
		}
		roots = append(roots, WatchRoot{Path: abs, Recursive: false, RebuildOnChildDir: true})
		if len(roots) > maxPrunedWatchRoots {
			return nil, false
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, false
		}
		for _, entry := range entries {
			// IsDir is false for symlinks: a recursive watch never
			// follows a symlink off the workspace, matching notify.
			if !entry.IsDir() {
				continue
			}
			rel := entry.Name()
			if ancestor != "." {
				rel = filepath.Join(ancestor, entry.Name())
			}
			if _, isBlocked := blockedSet[rel]; isBlocked {
				continue
			}
			if _, isAncestor := ancestors[rel]; isAncestor {
				continue
			}
			roots = append(roots, WatchRoot{Path: filepath.Join(cwd, rel), Recursive: true})
			if len(roots) > maxPrunedWatchRoots {
				return nil, false
			}
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	return roots, true
}

func hasBlockedAncestor(blockedSet map[string]struct{}, rel string) bool {
	for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
		if _, ok := blockedSet[dir]; ok {
			return true
		}
	}
	return false
}

func isExistingDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (c *Core) revParsePath(cwd string, arg string) (string, bool, error) {
	result, err := c.run(cwd, "rev-parse", arg)
	if err != nil {
		return "", false, fmt.Errorf("git watch roots: rev-parse %s: %w", arg, err)
	}
	if result.exitCode != 0 {
		stderr := strings.TrimSpace(result.stderr)
		if strings.Contains(strings.ToLower(stderr), "not a git repository") {
			return "", false, nil
		}
		if stderr == "" {
			stderr = strings.TrimSpace(result.stdout)
		}
		return "", false, fmt.Errorf("git watch roots: rev-parse %s failed: %s", arg, stderr)
	}

	path := strings.TrimSpace(result.stdout)
	if path == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), true, nil
}
