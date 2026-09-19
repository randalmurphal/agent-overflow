package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Isolated-boot workspace boundary.
//
// An isolated boot (--harness / --soak) pins every provider spawn to
// ao-mockprovider, but the mock still runs with the thread's workspace as its
// cwd: a scenario's writeFile step edits whatever directory that is, and the
// app's own git status/diff/checkpoint logic runs against the same checkout.
// So a workspace is admitted only when it resolves inside the harness data
// root (IsolationConfig.WorkspaceRoot). The check is applied where a path
// first enters the app (CreateProject) and again where a session is spawned
// (startSessionNowWithClaudeResumeAt), so neither a seeded path nor a
// restored thread row can reach a real repository.

// requireIsolatedWorkspace refuses a path outside the isolated boot's
// workspace root. No-op when no root is configured (unit tests).
func (a *App) requireIsolatedWorkspace(path string) error {
	root := a.isolatedWorkspaceRoot
	if root == "" {
		return nil
	}
	resolvedRoot, err := resolveWorkspaceBoundary(root)
	if err != nil {
		return fmt.Errorf("harness mode: resolve harness data root %s: %w", root, err)
	}
	resolved, err := resolveWorkspaceBoundary(path)
	if err != nil {
		return fmt.Errorf("harness mode: resolve workspace %s: %w", path, err)
	}
	if resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf(
		"harness mode: workspace %s is outside the harness data root %s; isolated boots run only in fixture workspaces (HarnessSeed a repo, or ao-harness clone, which relocates cloned workspaces)",
		path, root)
}

// resolveWorkspaceBoundary canonicalizes a path for prefix comparison.
// Symlinks are resolved on both sides so a data root reached through a link
// (macOS /tmp -> /private/tmp) still accepts the real paths under it. A path
// that does not exist yet keeps its cleaned spelling below the deepest
// existing ancestor, which is what lets a worktree be checked before it is
// created.
func resolveWorkspaceBoundary(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	remainder := ""
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
