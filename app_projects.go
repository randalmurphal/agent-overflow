package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ListProjects returns projects with a lightweight thread count per
// project for the sidebar.
func (a *App) ListProjects() ([]store.ProjectWithCounts, error) {
	if a.store == nil {
		return nil, fmt.Errorf("list projects: store unavailable")
	}
	return a.store.ListProjectsWithThreadCounts()
}

// CreateProject validates that path exists, is a directory, and is not
// already backing another project. Returns ErrProjectPathInUse when the
// path already has a project row — the frontend interprets that as
// "redirect to the existing project" rather than a failure.
func (a *App) CreateProject(path string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("create project: store unavailable")
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

	now := time.Now().UnixMilli()
	p := store.Project{
		ID:        uuid.NewString(),
		Path:      abs,
		Name:      filepath.Base(abs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.store.CreateProject(p); err != nil {
		return store.Project{}, err
	}
	return p, nil
}

// RenameProject updates the display name. Path is immutable.
func (a *App) RenameProject(id, name string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("rename project: store unavailable")
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("rename project: name is required")
	}
	if err := a.store.UpdateProjectName(id, trimmed); err != nil {
		return store.Project{}, err
	}
	return a.store.GetProject(id)
}

// ArchiveProject hides the project without deleting it.
func (a *App) ArchiveProject(id string) error {
	if a.store == nil {
		return fmt.Errorf("archive project: store unavailable")
	}
	return a.store.ArchiveProject(id)
}

// UnarchiveProject reverses ArchiveProject and returns the refreshed row.
func (a *App) UnarchiveProject(id string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("unarchive project: store unavailable")
	}
	if err := a.store.UnarchiveProject(id); err != nil {
		return store.Project{}, err
	}
	return a.store.GetProject(id)
}

// DeleteProject cascades through the threads FK and returns the list of
// thread ids that were dropped so the frontend can purge pane state.
func (a *App) DeleteProject(id string) ([]string, error) {
	if a.store == nil {
		return nil, fmt.Errorf("delete project: store unavailable")
	}
	ids, err := a.store.ListThreadIDsForProject(id)
	if err != nil {
		return nil, err
	}
	if err := a.store.DeleteProject(id); err != nil {
		return nil, err
	}
	return ids, nil
}

// ensureProjectForWorkspace finds or creates a project row for the given
// workspace path. Used by flows (CreateThreadFromPR, Wave 2+ auto-import)
// that need a project implicitly before a Thread can be inserted.
//
// Lookup precedence:
//  1. Project whose path exactly matches the resolved git repository root.
//  2. Project whose path matches the workspace path verbatim.
//  3. Create a new project at whichever path is a git root, or fall back
//     to the workspace path.
//
// Returns an error if the workspace path is empty (the caller must
// provide something to anchor the project on).
func (a *App) ensureProjectForWorkspace(workspacePath string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("resolve project: store unavailable")
	}
	trimmed := strings.TrimSpace(workspacePath)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("resolve project: workspace path is required")
	}

	// Prefer the git repo root when detectable — two threads in
	// sibling checkouts should share the same project row.
	candidatePath := trimmed
	if root, err := a.gitCore().RepositoryRoot(trimmed); err == nil {
		if r := strings.TrimSpace(root); r != "" {
			candidatePath = r
		}
	}

	if existing, err := a.store.GetProjectByPath(candidatePath); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.Project{}, err
	}
	if candidatePath != trimmed {
		if existing, err := a.store.GetProjectByPath(trimmed); err == nil {
			return existing, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return store.Project{}, err
		}
	}

	now := time.Now().UnixMilli()
	p := store.Project{
		ID:        uuid.NewString(),
		Path:      candidatePath,
		Name:      filepath.Base(candidatePath),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.store.CreateProject(p); err != nil {
		return store.Project{}, err
	}
	return p, nil
}
