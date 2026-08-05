package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/claudeconfig"
	"agent-overflow/internal/slicesx"
)

// GetClaudeSkills enumerates the Claude skills a session started in
// workspacePath would load — user tier (~/.claude/skills), project tier
// (<workspace>/.claude/skills), and enabled plugins' skills — straight
// from the filesystem, without spawning anything.
//
// This exists because the zero-token account probe runs --safe-mode,
// whose initialize response deliberately omits skills: without this
// read a cold thread's composer menu cannot list them until a session's
// `system/init` frame arrives. The frontend unions this list under the
// same rule as the probe commands — a live session's name set stays
// authoritative once it exists, and this list only fills in before that
// or enriches names with descriptions.
//
// workspacePath must be ABSOLUTE, like GetCodexSkills: skills are
// directory-scoped and a relative path would silently resolve against
// the app's own cwd.
//
// LocalOnly on the wire: it reads the user's home directory and the
// workspace tree, and its rows name what is installed on the host.
func (a *App) GetClaudeSkills(workspacePath string) ([]claudeconfig.Skill, error) {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return nil, errors.New("claude skills: workspace path required")
	}
	if !filepath.IsAbs(workspacePath) {
		return nil, fmt.Errorf("claude skills: workspace path %q must be absolute", workspacePath)
	}
	store, err := a.claudeConfig()
	if err != nil {
		return nil, err
	}
	skills, err := store.ListSkills(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("claude skills: %w", err)
	}
	return slicesx.OrEmpty(skills), nil
}
