package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// claudePromptDir holds the prompt files Claude and claude-tui sessions
// spawn with (claude.WriteSystemPromptFile). Close removes a session's
// files, but a backend that is killed, crashes or abandons a session at
// shutdown runs no Close, so the directory lives under the data root and
// boot empties it. Only the backend holding the data root's lock writes
// here, so every file present at boot belongs to a process that has exited.
const claudePromptDir = "claude-prompts"

// claudePromptFileDir returns the directory session prompt files are
// created in, creating it when needed.
func (a *App) claudePromptFileDir() (string, error) {
	if a.configDir == "" {
		return "", errors.New("the app data directory is not set")
	}
	dir := filepath.Join(a.configDir, claudePromptDir)
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create Claude prompt file directory: %w", err)
	}
	return dir, nil
}

// sweepClaudePromptFiles removes the prompt files an earlier process left
// in dataDir when it exited without closing its sessions. Runs at boot,
// before any session can start.
func sweepClaudePromptFiles(dataDir string) error {
	if err := os.RemoveAll(filepath.Join(dataDir, claudePromptDir)); err != nil {
		return fmt.Errorf("remove Claude prompt files from the last run: %w", err)
	}
	return nil
}
