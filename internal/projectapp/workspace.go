package projectapp

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/project"
	"agent-overflow/internal/store"
)

// EnsureForWorkspace applies repository-root project identity while retaining
// the caller's workspace separately on its thread. The Write's Changed reports
// whether a project row was CREATED — resolving to an existing one moves
// nothing and announces nothing.
func (s *Service) EnsureForWorkspace(workspacePath string) (Write, error) {
	database, err := s.database("ensure project for workspace")
	if err != nil {
		return Write{}, err
	}
	row, created, err := project.EnsureForWorkspace(database, workspacePath)
	if err != nil {
		return Write{}, err
	}
	if !created {
		return Write{Project: row}, nil
	}
	// Only a freshly created row: an existing project already carries an
	// identity (or is waiting for the boot backfill), and re-deriving it on
	// every thread creation would spend a git subprocess to learn nothing.
	//
	// `internal/project` does not derive it itself because that package is
	// pure store-and-filesystem; the git port lives here.
	if remoteURL, rootCommit := s.repoIdentity(row.Path); remoteURL != "" || rootCommit != "" {
		identified, changed, err := database.UpdateProjectIdentity(row.ID, remoteURL, rootCommit)
		if err != nil {
			return Write{}, err
		}
		if changed {
			row = identified
		}
	}
	return Write{Project: row, Changed: true}, nil
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
