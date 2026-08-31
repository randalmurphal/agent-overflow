package app

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/workspacepath"
)

// WriteThreadWorkspaceFile writes content to a path relative to the thread's
// active workspace and returns the normalized relative path.
func (a *App) WriteThreadWorkspaceFile(threadID, relativePath, content string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	if thread.WorkspacePath == "" {
		return "", fmt.Errorf("thread %s does not have a workspace path", threadID)
	}

	normalizedPath, err := workspacepath.NormalizeRelative(relativePath)
	if err != nil {
		return "", err
	}

	absolutePath := filepath.Join(thread.WorkspacePath, normalizedPath)
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
