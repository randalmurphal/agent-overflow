package app

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/workspacepath"
)

// WriteWorkspaceFile writes content to a path relative to the referenced
// checkout and returns the normalized relative path. Workspace-keyed because
// the subject is the directory, not a thread: the thread is only how the
// caller found it. The ref goes through ResolveWorkspace, so an arbitrary
// caller-supplied directory cannot be written to — only the project root or
// one of its registered worktrees.
//
//ao:scope git:operate
func (a *App) WriteWorkspaceFile(ws WorkspaceRef, relativePath, content string) (string, error) {
	_, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return "", fmt.Errorf("write workspace file: %w", err)
	}

	normalizedPath, err := workspacepath.NormalizeRelative(relativePath)
	if err != nil {
		return "", err
	}

	absolutePath := filepath.Join(workspace, normalizedPath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return "", fmt.Errorf("create parent directories: %w", err)
	}
	// 0o600 — workspace files written via this binding originate from
	// thread content (drafts, tool output snippets the UI persists for
	// editing). They can carry secrets or partial provider responses
	// the user wouldn't want world-readable on a shared host. Match the
	// security posture of settings.json and saved payloads rather than
	// leaving the file world-readable by default.
	if err := os.WriteFile(absolutePath, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write workspace file: %w", err)
	}
	return normalizedPath, nil
}
