package projectapp

import (
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/repoidentity"
	"agent-overflow/internal/store"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// RefreshProject resolves one project's current coordinates without changing
// its identity, placement, or activity timestamp.
func (s *Service) RefreshProject(ctx context.Context, id string) (Write, error) {
	database, err := s.database("refresh project identity")
	if err != nil {
		return Write{}, err
	}
	row, err := database.GetProject(id)
	if err != nil {
		return Write{}, err
	}
	identity, read := s.repoIdentity(ctx, row.Path, projectIdentity(row))
	if !read.ok {
		return Write{}, ctx.Err()
	}
	updated, changed, err := database.UpdateProjectIdentity(id, identity)
	return s.writeResult(database, id, updated, changed, err)
}

// CreateCheckout validates the chosen checkout in the same API operation that
// registers it. Inspection alone cannot authorize a later unchecked create.
func (s *Service) CreateCheckout(ctx context.Context, path string, expected store.ProjectIdentity) (Write, error) {
	database, err := s.database("choose project checkout")
	if err != nil {
		return Write{}, err
	}
	if strings.TrimSpace(path) == "" {
		return Write{}, fmt.Errorf("choose checkout: path is required")
	}
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return Write{}, fmt.Errorf("choose checkout: invalid path")
	}
	if s.deps.Identity == nil {
		return Write{}, fmt.Errorf("choose checkout: repository identity unavailable")
	}
	// InspectFolder validates the directory; all reads use the caller's context.
	folder, err := s.InspectFolder(ctx, abs)
	if err != nil {
		return Write{}, err
	}
	if err := ctx.Err(); err != nil {
		return Write{}, err
	}
	existing, readErr := database.GetProjectByPath(abs)
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return Write{}, readErr
	}
	if readErr == nil && existing.Archived {
		return Write{}, fmt.Errorf("that project is archived; restore it before choosing its checkout")
	}
	hasExpected := expected.RepositoryID != ""
	if !hasExpected && expected.Error != "" {
		return Write{}, fmt.Errorf("cannot verify the selected repository: %s", repoidentity.RedactText(expected.Error))
	}
	if hasExpected && (!folder.Repository || !repoidentity.Matches(expected.RepositoryID, folder.RepositoryID)) {
		if folder.IdentityError != "" {
			return Write{}, fmt.Errorf("cannot verify that checkout: %s", folder.IdentityError)
		}
		return Write{}, fmt.Errorf("that folder is not a checkout of the selected repository")
	}
	identity := store.ProjectIdentity{RepositoryID: folder.RepositoryID, IdentitySource: folder.IdentitySource, Error: folder.IdentityError}
	if readErr == nil {
		row, changed, err := database.UpdateProjectIdentity(existing.ID, identity)
		return s.writeResult(database, existing.ID, row, changed, err)
	}
	now := s.deps.Now().UnixMilli()
	row, err := database.CreateProject(store.Project{ID: entityid.New(), Path: abs, Name: filepath.Base(abs), CreatedAt: now, UpdatedAt: now, IdentitySource: identity.IdentitySource, RepositoryID: identity.RepositoryID, IdentityError: identity.Error})
	return Write{Project: row, Changed: err == nil}, err
}
