package projectapp

import (
	"fmt"
	"strings"

	"agent-overflow/internal/worktreesetup"
)

func (s *Service) GetWorktreeSetup(projectID string) (worktreesetup.Config, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return worktreesetup.Config{}, fmt.Errorf("project id is required")
	}
	database, err := s.database("get project worktree setup")
	if err != nil {
		return worktreesetup.Config{}, err
	}
	config, _, err := database.ProjectWorktreeSetup(projectID)
	return config, err
}

// SetWorktreeSetup returns the recipe as stored alongside the project row the
// write touched. The recipe is the editor's answer; the row is what gets
// broadcast, because the recipe is not part of the project row other clients
// hold (see store.UpdateProjectWorktreeSetup).
func (s *Service) SetWorktreeSetup(projectID string, config worktreesetup.Config) (worktreesetup.Config, Write, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return worktreesetup.Config{}, Write{}, fmt.Errorf("project id is required")
	}
	config.Timeout = strings.TrimSpace(config.Timeout)
	if err := worktreesetup.Validate(config); err != nil {
		return worktreesetup.Config{}, Write{}, err
	}
	database, err := s.database("set project worktree setup")
	if err != nil {
		return worktreesetup.Config{}, Write{}, err
	}
	row, changed, err := database.UpdateProjectWorktreeSetup(projectID, &config)
	write, err := s.writeResult(database, projectID, row, changed, err)
	if err != nil {
		return worktreesetup.Config{}, Write{}, err
	}
	stored, _, err := database.ProjectWorktreeSetup(projectID)
	if err != nil {
		return worktreesetup.Config{}, Write{}, err
	}
	return stored, write, nil
}
