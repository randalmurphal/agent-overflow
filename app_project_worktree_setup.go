package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/worktreesetup"
)

// WorktreeSetupConfig is the wire shape of a project's worktree setup recipe.
// It mirrors worktreesetup.Config with the slices always materialised, so the
// editor binds against `[]` rather than having to treat null as empty.
type WorktreeSetupConfig struct {
	Copy    []string   `json:"copy"`
	Run     [][]string `json:"run"`
	Timeout string     `json:"timeout"`
}

// GetProjectWorktreeSetup returns the project's recipe. An unconfigured project
// returns the empty recipe — the editor's starting state — rather than an
// error, because "not configured yet" is the normal case, not a fault.
func (a *App) GetProjectWorktreeSetup(projectID string) (WorktreeSetupConfig, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return WorktreeSetupConfig{}, fmt.Errorf("project id is required")
	}
	config, _, err := a.store.ProjectWorktreeSetup(projectID)
	if err != nil {
		return WorktreeSetupConfig{}, err
	}
	return toWireWorktreeSetup(config), nil
}

// SetProjectWorktreeSetup validates and persists the project's recipe, and
// returns the stored result so the editor re-seeds from what was actually
// saved. An invalid recipe is a save error and is NEVER persisted: these argv
// commands run unattended on every worktree this project cuts.
//
// A recipe that asks for nothing clears the row, so "remove everything" and
// "never configured" are the same state.
func (a *App) SetProjectWorktreeSetup(projectID string, config WorktreeSetupConfig) (WorktreeSetupConfig, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return WorktreeSetupConfig{}, fmt.Errorf("project id is required")
	}
	stored := worktreesetup.Config{Copy: config.Copy, Run: config.Run, Timeout: strings.TrimSpace(config.Timeout)}
	if err := worktreesetup.Validate(stored); err != nil {
		return WorktreeSetupConfig{}, err
	}
	if err := a.store.UpdateProjectWorktreeSetup(projectID, &stored); err != nil {
		return WorktreeSetupConfig{}, err
	}
	saved, _, err := a.store.ProjectWorktreeSetup(projectID)
	if err != nil {
		return WorktreeSetupConfig{}, err
	}
	return toWireWorktreeSetup(saved), nil
}

func toWireWorktreeSetup(config worktreesetup.Config) WorktreeSetupConfig {
	return WorktreeSetupConfig{
		Copy:    slicesx.OrEmpty(config.Copy),
		Run:     slicesx.OrEmpty(config.Run),
		Timeout: config.Timeout,
	}
}
