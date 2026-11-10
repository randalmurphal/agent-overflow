package git

import (
	"os"
	"path/filepath"
	"strings"
)

// pendingOperation detects an in-progress merge, cherry-pick, revert, rebase,
// or bisect by inspecting well-known files under the repository's git directory.
// Returns "" when the repo is idle.
//
// We use `git rev-parse --git-dir` to resolve the correct git-dir because it
// handles both plain repos (`.git` is a dir) and linked worktrees (`.git` is a
// file pointing into a `worktrees/<name>` dir). Checking for a literal `.git`
// folder under `cwd` would miss ongoing ops in worktrees.
func (c *Core) pendingOperation(cwd string) string {
	gitDir := c.resolveGitDir(cwd)
	if gitDir == "" {
		return ""
	}

	// MERGE_HEAD is present during an in-progress merge (including merge
	// conflicts waiting for resolution).
	if fileExists(filepath.Join(gitDir, "MERGE_HEAD")) {
		return "merge"
	}
	// CHERRY_PICK_HEAD is present while a cherry-pick waits on its conflict.
	if fileExists(filepath.Join(gitDir, "CHERRY_PICK_HEAD")) {
		return "cherry-pick"
	}
	// REVERT_HEAD is present while a revert waits on its conflict.
	if fileExists(filepath.Join(gitDir, "REVERT_HEAD")) {
		return "revert"
	}
	// rebase-merge / rebase-apply cover both interactive rebases and the
	// legacy format-patch style rebases (`rebase-apply`).
	if dirExists(filepath.Join(gitDir, "rebase-merge")) || dirExists(filepath.Join(gitDir, "rebase-apply")) {
		return "rebase"
	}
	// BISECT_LOG is present during a `git bisect` session.
	if fileExists(filepath.Join(gitDir, "BISECT_LOG")) {
		return "bisect"
	}
	return ""
}

// pendingOperationCommitsWhole reports whether a commit during op has to
// take every change: a merge, a cherry-pick and a revert each record their
// whole result. Git refuses selected paths during the first two; during a
// revert it would commit them and end the revert half recorded.
func pendingOperationCommitsWhole(op string) bool {
	return op == "merge" || op == "cherry-pick" || op == "revert"
}

// CommitsWholeOnly reports whether a commit in cwd has to be
// CommitSelection.All right now.
func (c *Core) CommitsWholeOnly(cwd string) bool {
	return pendingOperationCommitsWhole(c.pendingOperation(cwd))
}

// resolveGitDir runs `git rev-parse --git-dir` and returns the absolute git
// directory for the repository containing cwd. Returns "" if the directory
// isn't a git repo or rev-parse fails; callers treat "" as "no pending op"
// which is the correct fallback in both cases.
//
// Successful resolutions are memoized per cwd on the Core (the git-dir path
// is immutable for a given cwd); failures are re-probed every call so a
// later `git init` — or a transient git error — never sticks.
func (c *Core) resolveGitDir(cwd string) string {
	c.gitDirCacheMu.RLock()
	cached, ok := c.gitDirCache[cwd]
	c.gitDirCacheMu.RUnlock()
	if ok {
		return cached
	}

	result, err := c.run(cwd, "rev-parse", "--git-dir")
	if err != nil || result.exitCode != 0 {
		return ""
	}
	gitDir := strings.TrimSpace(result.stdout)
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(cwd, gitDir)
	}
	c.gitDirCacheMu.Lock()
	c.gitDirCache[cwd] = gitDir
	c.gitDirCacheMu.Unlock()
	return gitDir
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
