package projectapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

// Deps names the persistence, clock, and registered-workspace dependencies of
// project application policy. Filesystem validation deliberately stays in this
// service because it is part of the CreateProject contract, not store policy.
type Deps struct {
	Store     *store.Store
	Now       func() time.Time
	Workspace WorkspaceResolver
}

// WorkspaceResolver returns git's canonical spelling for a registered
// worktree. `internal/app` supplies the git-backed implementation; project
// policy stays testable without importing App.
type WorkspaceResolver interface {
	FindWorktree(projectPath, candidate string) (path string, found bool, err error)
}

// Service owns project persistence and workspace-membership policy. Live git
// mutation and destructive deletion execution remain explicit `internal/app`
// adapters.
type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{deps: deps}
}

func (s *Service) database(action string) (*store.Store, error) {
	if s == nil || s.deps.Store == nil {
		return nil, fmt.Errorf("%s: store unavailable", action)
	}
	return s.deps.Store, nil
}

func (s *Service) List() ([]store.ProjectWithCounts, error) {
	database, err := s.database("list projects")
	if err != nil {
		return nil, err
	}
	return database.ListProjectsWithThreadCounts()
}

func (s *Service) Create(path string) (store.Project, error) {
	database, err := s.database("create project")
	if err != nil {
		return store.Project{}, err
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("create project: path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return store.Project{}, fmt.Errorf("create project: resolve absolute path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return store.Project{}, fmt.Errorf("create project: stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return store.Project{}, fmt.Errorf("create project: %s is not a directory", abs)
	}

	now := s.deps.Now().UnixMilli()
	project := store.Project{
		// Globally unique by construction: a client attached to more than
		// one backend keys projects by this string (internal/entityid).
		ID:        entityid.New(),
		Path:      abs,
		Name:      filepath.Base(abs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := database.CreateProject(project); err != nil {
		return store.Project{}, err
	}
	return project, nil
}

func (s *Service) Rename(id, name string) (store.Project, error) {
	database, err := s.database("rename project")
	if err != nil {
		return store.Project{}, err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("rename project: name is required")
	}
	if err := database.UpdateProjectName(id, trimmed); err != nil {
		return store.Project{}, err
	}
	return database.GetProject(id)
}

func (s *Service) Archive(id string) error {
	database, err := s.database("archive project")
	if err != nil {
		return err
	}
	return database.ArchiveProject(id)
}

func (s *Service) Unarchive(id string) (store.Project, error) {
	database, err := s.database("unarchive project")
	if err != nil {
		return store.Project{}, err
	}
	if err := database.UnarchiveProject(id); err != nil {
		return store.Project{}, err
	}
	return database.GetProject(id)
}

func (s *Service) UpdateSortPositions(orderedIDs []string) error {
	database, err := s.database("update project sort positions")
	if err != nil {
		return err
	}
	return database.UpdateProjectSortPositions(orderedIDs)
}
