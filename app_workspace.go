package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	normalizedPath, err := normalizeWorkspaceRelativePath(relativePath)
	if err != nil {
		return "", err
	}

	absolutePath := filepath.Join(thread.WorkspacePath, normalizedPath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return "", fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(absolutePath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write workspace file: %w", err)
	}
	return normalizedPath, nil
}

func normalizeWorkspaceRelativePath(relativePath string) (string, error) {
	trimmed := strings.TrimSpace(relativePath)
	if trimmed == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("workspace path must be relative")
	}

	normalized := filepath.Clean(trimmed)
	parentPrefix := ".." + string(filepath.Separator)
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, parentPrefix) {
		return "", fmt.Errorf("workspace path must stay within the workspace root")
	}
	return normalized, nil
}
