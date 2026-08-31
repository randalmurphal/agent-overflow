package projectapp

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
)

// EnsureForWorkspace applies repository-root project identity while retaining
// the caller's workspace separately on its thread.
func (s *Service) EnsureForWorkspace(workspacePath string) (store.Project, error) {
	database, err := s.database("ensure project for workspace")
	if err != nil {
		return store.Project{}, err
	}
	return project.EnsureForWorkspace(database, workspacePath)
}

// ProjectForWorkspaceOperation resolves and validates the project row used by
// project-scoped git operations.
func (s *Service) ProjectForWorkspaceOperation(projectID string) (store.Project, error) {
	if s == nil || s.deps.Store == nil {
		return store.Project{}, fmt.Errorf("store unavailable")
	}
	database := s.deps.Store
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return store.Project{}, fmt.Errorf("projectId is required")
	}
	row, err := database.GetProject(projectID)
	if err != nil {
		return store.Project{}, fmt.Errorf("resolve project %s: %w", projectID, err)
	}
	if strings.TrimSpace(row.Path) == "" {
		return store.Project{}, fmt.Errorf("project %s has no path", projectID)
	}
	return row, nil
}

// ResolveSourceWorkspace validates the checkout used as a destructive git cwd.
func (s *Service) ResolveSourceWorkspace(row store.Project, sourceWorkspace string) (string, error) {
	sourceWorkspace = strings.TrimSpace(sourceWorkspace)
	if sourceWorkspace == "" || gitops.SameFilesystemPath(row.Path, sourceWorkspace) {
		return row.Path, nil
	}
	if s == nil || s.deps.Workspace == nil {
		return "", fmt.Errorf("validate source workspace: workspace resolver unavailable")
	}
	resolved, ok, err := s.deps.Workspace.FindWorktree(row.Path, sourceWorkspace)
	if err != nil {
		return "", fmt.Errorf("validate source workspace: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("%s is not a workspace of project %s", sourceWorkspace, row.Path)
	}
	return resolved, nil
}
