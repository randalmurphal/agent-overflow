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

func (s *Service) SetWorktreeSetup(projectID string, config worktreesetup.Config) (worktreesetup.Config, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return worktreesetup.Config{}, fmt.Errorf("project id is required")
	}
	config.Timeout = strings.TrimSpace(config.Timeout)
	if err := worktreesetup.Validate(config); err != nil {
		return worktreesetup.Config{}, err
	}
	database, err := s.database("set project worktree setup")
	if err != nil {
		return worktreesetup.Config{}, err
	}
	if err := database.UpdateProjectWorktreeSetup(projectID, &config); err != nil {
		return worktreesetup.Config{}, err
	}
	stored, _, err := database.ProjectWorktreeSetup(projectID)
	return stored, err
}
