package claudeconfig

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ProjectKey returns the key Claude Code uses under `projects` in
// ~/.claude.json for a session whose cwd is workspacePath: the
// canonical git root, with a linked worktree resolved back to the main
// repository's working directory. Not being in a git repository falls
// back to the cleaned workspacePath itself.
//
// This mirrors the CLI's getProjectPathForConfig →
// findCanonicalGitRoot → resolveCanonicalRoot chain (including its two
// structure validations and the bare-repo case), because AO must read
// and write the exact entry a Claude session in that cwd uses — keying
// by the raw worktree path silently splits AO's view of
// disabledMcpServers from the CLI's.
func ProjectKey(workspacePath string) string {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return ""
	}
	root := findGitRoot(workspacePath)
	if root == "" {
		return norm.NFC.String(filepath.Clean(workspacePath))
	}
	return norm.NFC.String(resolveCanonicalRoot(root))
}

// findGitRoot walks up from startPath looking for a `.git` entry
// (directory in a regular checkout, file in a linked worktree or
// submodule) and returns the directory containing it, or "".
func findGitRoot(startPath string) string {
	dir := filepath.Clean(startPath)
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveCanonicalRoot maps a linked worktree's root to the main
// repository root by following `.git` (file) → `gitdir:` → `commondir`.
// A regular checkout (`.git` directory) and a submodule (`.git` file
// but no commondir) resolve to gitRoot unchanged. The two structure
// checks below mirror the CLI's: they reject hand-crafted layouts that
// point commondir at an arbitrary directory, falling back to gitRoot.
func resolveCanonicalRoot(gitRoot string) string {
	gitContent, err := os.ReadFile(filepath.Join(gitRoot, ".git"))
	if err != nil {
		// Directory (EISDIR) or unreadable: a regular checkout.
		return gitRoot
	}
	trimmed := strings.TrimSpace(string(gitContent))
	if !strings.HasPrefix(trimmed, "gitdir:") {
		return gitRoot
	}
	worktreeGitDir := resolveFrom(gitRoot, strings.TrimSpace(strings.TrimPrefix(trimmed, "gitdir:")))

	commonRaw, err := os.ReadFile(filepath.Join(worktreeGitDir, "commondir"))
	if err != nil {
		// Submodules have no commondir; they are separate repos.
		return gitRoot
	}
	commonDir := resolveFrom(worktreeGitDir, strings.TrimSpace(string(commonRaw)))

	// Check 1: worktreeGitDir must be a direct child of
	// <commonDir>/worktrees — the layout `git worktree add` creates.
	if filepath.Dir(worktreeGitDir) != filepath.Join(commonDir, "worktrees") {
		return gitRoot
	}
	// Check 2: <worktreeGitDir>/gitdir must point back at
	// <gitRoot>/.git. Git writes that back-link with symlinks resolved,
	// so realpath the directory (not the .git file itself) before
	// comparing.
	backRaw, err := os.ReadFile(filepath.Join(worktreeGitDir, "gitdir"))
	if err != nil {
		return gitRoot
	}
	backlink, err := filepath.EvalSymlinks(strings.TrimSpace(string(backRaw)))
	if err != nil {
		return gitRoot
	}
	realRoot, err := filepath.EvalSymlinks(gitRoot)
	if err != nil {
		return gitRoot
	}
	if backlink != filepath.Join(realRoot, ".git") {
		return gitRoot
	}
	// Bare-repo worktrees: the common dir is not inside a working
	// directory, so the common dir itself is the stable identity.
	if filepath.Base(commonDir) != ".git" {
		return commonDir
	}
	return filepath.Dir(commonDir)
}

// resolveFrom resolves a possibly-relative path against base, cleaning
// the result — the Go analogue of Node's resolve(base, p).
func resolveFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}
