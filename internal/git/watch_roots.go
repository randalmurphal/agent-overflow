package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WatchRoot is a filesystem path that should trigger live git-status refreshes.
// Recursive roots watch every descendant; non-recursive roots watch only the
// directory itself.
type WatchRoot struct {
	Path      string
	Recursive bool
}

// WatchRoots returns the filesystem roots a live git-status watcher should
// observe for cwd. Linked worktrees keep the important commit/index/ref
// metadata outside the worktree directory, so watching cwd alone misses
// external commits made from a terminal in that worktree.
func (c *Core) WatchRoots(cwd string) ([]WatchRoot, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("git watch roots: cwd is required")
	}

	roots := []WatchRoot{{Path: cwd, Recursive: true}}

	gitDir, ok, err := c.revParsePath(cwd, "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	if !ok {
		return roots, nil
	}
	roots = append(roots, WatchRoot{Path: gitDir, Recursive: true})

	commonDir, ok, err := c.revParsePath(cwd, "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if ok {
		// Common metadata can be much larger than the linked worktree gitdir.
		// Watch the common dir itself for packed-refs/config updates, then the
		// refs subtree recursively for branch/remote ref movement. Avoid
		// recursively watching objects/, packs, hooks, logs, etc.
		roots = append(roots, WatchRoot{Path: commonDir, Recursive: false})
		refsDir := filepath.Join(commonDir, "refs")
		if isExistingDirectory(refsDir) {
			roots = append(roots, WatchRoot{Path: refsDir, Recursive: true})
		}
	}

	return roots, nil
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
