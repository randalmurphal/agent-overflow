package git

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WatchRootKind classifies a watch root by which events under it can
// invalidate the root set itself, so the watcher knows when to
// recompute. Kinds are ordered by trigger surface — each kind's
// triggers are a superset of the previous one's — which lets root
// normalization merge duplicate paths by taking the larger kind
// without losing any trigger.
type WatchRootKind uint8

const (
	// KindSubtree is plain workspace content (a recursive pruned
	// subtree, or the whole-cwd fallback). No event under it changes
	// the root set.
	KindSubtree WatchRootKind = iota

	// KindAncestor is a non-recursive ancestor of a pruned ignored
	// boundary: a directory appearing directly under it is covered by
	// no existing root, so the watcher must recompute.
	KindAncestor

	// KindGitMeta is a git metadata directory (git dir, refs/, info/,
	// a linked worktree's private gitdir). New child directories
	// trigger like KindAncestor (a late-created info/ must become a
	// root), and writes to index / exclude / config additionally
	// trigger: `git add -f` moves ignored boundaries via the index,
	// info/exclude edits change ignore rules, and config edits can
	// re-point core.excludesFile.
	KindGitMeta
)

// WatchRoot is a filesystem path that should trigger live git-status refreshes.
// Recursive roots watch every descendant; non-recursive roots watch only the
// directory itself.
type WatchRoot struct {
	Path      string
	Recursive bool
	Kind      WatchRootKind

	// TriggerFile, when non-empty, names one direct child whose events
	// flag a root recompute regardless of Kind. It carries the global
	// ignore file (core.excludesFile): the file is watched via its
	// parent directory — watching the file itself would die on the
	// editor write-temp-then-rename pattern — but that directory can be
	// busy (core.excludesFile in $HOME is common), so only events for
	// exactly this basename may trigger. Orthogonal to Kind on purpose:
	// folding it into the kind ladder would break the superset ordering
	// the normalize merge relies on.
	TriggerFile string
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
// ignored boundary non-recursively (KindAncestor, so the watcher
// recomputes roots when a new directory appears there). The transitions
// that could re-legitimize an ignored path — .gitignore edits,
// info/exclude edits, index writes (`git add -f`), global-ignore edits
// — all happen inside watched locations whose kinds tell the watcher
// to rebuild.
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
		return []WatchRoot{{Path: cwd, Recursive: true, Kind: KindSubtree}}, nil
	}

	roots := c.workspaceWatchRoots(cwd, gitDir)
	if excludes, ok := c.globalExcludesRoot(cwd); ok {
		roots = append(roots, excludes)
	}

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
	roots = append(roots, WatchRoot{Path: gitDir, Recursive: true, Kind: KindGitMeta})
	roots = append(roots, gitMetadataRoots(commonDir)...)
	return roots, nil
}

// globalExcludesRoot returns a watch root for the directory holding the
// user's global ignore file (core.excludesFile, defaulting to git's
// $XDG_CONFIG_HOME/git/ignore convention when unset). Editing that file
// moves prunable boundaries exactly like a .gitignore edit, but it
// lives outside the workspace — without this root a tree un-ignored by
// a global-ignore edit would stay unwatched forever. The file's
// basename rides along as TriggerFile so only its own events recompute
// roots (the parent dir can be as busy as $HOME). Absent config and a
// missing default directory disable the root (watching a nonexistent
// dir would fail the whole install).
func (c *Core) globalExcludesRoot(cwd string) (WatchRoot, bool) {
	var path string
	result, err := c.run(cwd, "config", "--path", "--get", "core.excludesFile")
	if err == nil && result.exitCode == 0 {
		// Trim only the terminator: a quoted config value may carry
		// legitimate leading/trailing spaces in the filename.
		path = strings.TrimRight(result.stdout, "\r\n")
	}
	if path == "" {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			path = filepath.Join(xdg, "git", "ignore")
		} else if home, homeErr := os.UserHomeDir(); homeErr == nil {
			path = filepath.Join(home, ".config", "git", "ignore")
		} else {
			return WatchRoot{}, false
		}
	}
	// --path expands only ~; a relative value is resolved by git against
	// its process cwd — the repo here, NOT this app's own cwd.
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	dir := filepath.Dir(path)
	if !isExistingDirectory(dir) {
		return WatchRoot{}, false
	}
	return WatchRoot{Path: dir, Recursive: false, Kind: KindSubtree, TriggerFile: filepath.Base(path)}, true
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
	roots := []WatchRoot{{Path: gitDir, Recursive: false, Kind: KindGitMeta}}
	if refsDir := filepath.Join(gitDir, "refs"); isExistingDirectory(refsDir) {
		roots = append(roots, WatchRoot{Path: refsDir, Recursive: true, Kind: KindGitMeta})
	}
	if infoDir := filepath.Join(gitDir, "info"); isExistingDirectory(infoDir) {
		roots = append(roots, WatchRoot{Path: infoDir, Recursive: false, Kind: KindGitMeta})
	}
	return roots
}

// workspaceWatchRoots computes the pruned workspace-side roots for cwd.
// Any failure (git listing, readdir, cap overflow) degrades to the
// single recursive root — always correct, just heavier. The degrade is
// logged: silently re-inflating to tens of thousands of inotify
// watches is exactly the regression the pruning exists to prevent, and
// it must be diagnosable when it recurs.
func (c *Core) workspaceWatchRoots(cwd, gitDir string) []WatchRoot {
	fallback := []WatchRoot{{Path: cwd, Recursive: true, Kind: KindSubtree}}
	blocked, err := c.ignoredBoundaryDirs(cwd)
	if err != nil {
		log.Printf("git: watch-root pruning disabled for %s (single recursive watch): %v", cwd, err)
		return fallback
	}
	// The git dir is metadata, not workspace: its watch policy is owned
	// by gitMetadataRoots. For a primary checkout it sits under cwd and
	// must be pruned here or the recursive workspace root would drag
	// objects/ back in.
	if rel, err := filepath.Rel(cwd, gitDir); err == nil &&
		rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		blocked = append(blocked, rel)
	}
	roots, err := pruneIgnoredSubtrees(cwd, blocked)
	if err != nil {
		log.Printf("git: watch-root pruning disabled for %s (single recursive watch): %v", cwd, err)
		return fallback
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
// (KindAncestor), and each of its child directories that is neither
// blocked nor itself an ancestor becomes a recursive root. A non-nil
// error means the caller should fall back to the single recursive root
// (no blocked dirs is success with that same single root; failure
// modes are malformed boundaries, readdir errors, and the root cap).
//
// Boundary validation stays inline rather than reusing
// workspacepath.NormalizeRelative: that helper trims whitespace, which
// would corrupt legitimately space-padded directory names arriving
// verbatim from git's NUL-delimited listing.
func pruneIgnoredSubtrees(cwd string, blocked []string) ([]WatchRoot, error) {
	cleaned := make([]string, 0, len(blocked))
	for _, b := range blocked {
		b = filepath.Clean(b)
		if b == "." || b == "" || b == ".." || strings.HasPrefix(b, ".."+string(filepath.Separator)) || filepath.IsAbs(b) {
			// A boundary that isn't a proper relative subtree means the
			// listing wasn't what we expected; don't guess.
			return nil, fmt.Errorf("ignored boundary %q is not a workspace-relative subtree", b)
		}
		cleaned = append(cleaned, b)
	}
	if len(cleaned) == 0 {
		return []WatchRoot{{Path: cwd, Recursive: true, Kind: KindSubtree}}, nil
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
		roots = append(roots, WatchRoot{Path: abs, Recursive: false, Kind: KindAncestor})
		if len(roots) > maxPrunedWatchRoots {
			return nil, fmt.Errorf("pruned roots exceed cap %d", maxPrunedWatchRoots)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, fmt.Errorf("listing ancestor %s: %w", abs, err)
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
			roots = append(roots, WatchRoot{Path: filepath.Join(cwd, rel), Recursive: true, Kind: KindSubtree})
			if len(roots) > maxPrunedWatchRoots {
				return nil, fmt.Errorf("pruned roots exceed cap %d", maxPrunedWatchRoots)
			}
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	return roots, nil
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
