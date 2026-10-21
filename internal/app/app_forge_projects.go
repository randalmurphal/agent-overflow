package app

import (
	"context"
	"errors"
	"fmt"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// ForgeProject is one of this backend's projects whose origin names a
// repository on a supported forge: the Pull requests page lists its PRs
// and pipelines. Error is set, with Repo zero, when the origin could not
// be read for a reason other than naming no forge repository.
type ForgeProject struct {
	ProjectID string               `json:"projectId"`
	Name      string               `json:"name"`
	Path      string               `json:"path"`
	Repo      gitops.RepoReference `json:"repo"`
	Error     string               `json:"error,omitempty"`
}

// ListForgeProjects lists this backend's projects (the non-archived ones
// the sidebar shows) whose origin is a known forge repository, in the
// sidebar's name order. A project with another origin, or none, is
// omitted.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ListForgeProjects(ctx context.Context) ([]ForgeProject, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	return a.forgeProjects(ctx)
}

func (a *App) forgeProjects(ctx context.Context) ([]ForgeProject, error) {
	if a.prUpdates.listProjectsFn != nil {
		return a.prUpdates.listProjectsFn(ctx)
	}
	projects, err := a.store.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list forge projects: %w", err)
	}
	out := make([]ForgeProject, 0, len(projects))
	for _, project := range projects {
		row, ok, err := a.forgeProject(ctx, project)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// forgeProject reads one project's forge repository from its origin
// (cached by forge detection). ok is false for an origin that names no
// forge repository; err only for a cancelled ctx.
func (a *App) forgeProject(ctx context.Context, project store.Project) (ForgeProject, bool, error) {
	row := ForgeProject{ProjectID: project.ID, Name: project.Name, Path: project.Path}
	repo, err := a.gitCore().OriginRepository(ctx, project.Path)
	switch {
	case err == nil:
		row.Repo = repo
	case errors.Is(err, gitops.ErrOriginNotForge):
		return ForgeProject{}, false, nil
	case ctx.Err() != nil:
		return ForgeProject{}, false, ctx.Err()
	default:
		row.Error = err.Error()
	}
	return row, true, nil
}

// projectForgeRepository is the forge repository of projectID's origin,
// for a read about the project as a whole.
func (a *App) projectForgeRepository(ctx context.Context, projectID string) (gitops.RepoReference, error) {
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return gitops.RepoReference{}, fmt.Errorf("project %s: %w", projectID, err)
	}
	row, ok, err := a.forgeProject(ctx, project)
	switch {
	case err != nil:
		return gitops.RepoReference{}, err
	case !ok:
		return gitops.RepoReference{}, fmt.Errorf("project %s: %w", project.Name, gitops.ErrOriginNotForge)
	case row.Error != "":
		return gitops.RepoReference{}, fmt.Errorf("project %s: %s", project.Name, row.Error)
	}
	return row.Repo, nil
}
