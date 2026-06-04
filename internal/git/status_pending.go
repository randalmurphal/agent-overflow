package git

import (
	"os"
	"path/filepath"
	"strings"
)

// pendingOperation detects an in-progress merge, rebase, or bisect by
// inspecting well-known files under the repository's git directory. Returns
// "" when the repo is idle.
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

// resolveGitDir runs `git rev-parse --git-dir` and returns the absolute git
// directory for the repository containing cwd. Returns "" if the directory
// isn't a git repo or rev-parse fails; callers treat "" as "no pending op"
// which is the correct fallback in both cases.
func (c *Core) resolveGitDir(cwd string) string {
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
