package projectapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/repoidentity"
	"agent-overflow/internal/store"
)

// Deps names the persistence, clock, and registered-workspace dependencies of
// project application policy. Filesystem validation deliberately stays in this
// service because it is part of the CreateProject contract, not store policy.
type Deps struct {
	Store     *store.Store
	Now       func() time.Time
	Workspace WorkspaceResolver
	// Identity resolves a forge ID using this checkout's transient origin.
	// The origin itself never enters project persistence or client metadata.
	Identity func(ctx context.Context, path string) (gitops.RepoIdentity, error)
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

// repoIdentity derives the identity to store for a checkout at path whose row
// currently holds stored. Nil-safe: with no deriver wired the stored identity
// stands. ok is false when ctx ended the read: its error is not the
// checkout's, and nothing may be recorded.
//
// A failed Git read keeps the last verified ID beside the error, so a
// transient failure neither unmerges a project nor hides why it may be stale.
// A path that is not a repository (any longer) keeps it too: a checkout
// that is missing for a while, an unmounted volume say, is still the same
// repository when it returns.
func (s *Service) repoIdentity(ctx context.Context, path string, stored store.ProjectIdentity) (store.ProjectIdentity, bool) {
	if s == nil || s.deps.Identity == nil {
		return stored, true
	}
	identity, err := s.deps.Identity(ctx, path)
	switch {
	case ctx.Err() != nil:
		return stored, false
	case err != nil:
		stored.Error = repoidentity.RedactText(err.Error())
		return stored, true
	case !identity.Repository:
		stored.Error = ""
		return stored, true
	}
	id := identity.RepositoryID
	if id == "" && identity.LookupError != "" && identity.IdentitySource != "" && identity.IdentitySource == stored.IdentitySource {
		id = stored.RepositoryID
	}
	return store.ProjectIdentity{RepositoryID: id, IdentitySource: identity.IdentitySource, Error: identity.LookupError}, true
}

// projectIdentity is the identity stored on row.
func projectIdentity(row store.Project) store.ProjectIdentity {
	return store.ProjectIdentity{Error: row.IdentityError, RepositoryID: row.RepositoryID, IdentitySource: row.IdentitySource}
}

func (s *Service) database(action string) (*store.Store, error) {
	if s == nil || s.deps.Store == nil {
		return nil, fmt.Errorf("%s: store unavailable", action)
	}
	return s.deps.Store, nil
}

// Write is what one project mutation did: the project row as it now stands,
// and whether the write actually moved it.
//
// Both halves are always populated, and they answer different questions. The
// ROW is the mutation's return value, so the calling client can apply it
// without a re-read — it is filled even for a no-op, because a rename to the
// name a project already had must still answer with that project rather than
// a blank one. CHANGED decides whether the mutation is announced on
// `project:updated`: a write that moved nothing has nothing to tell the other
// attached clients.
type Write struct {
	Project store.Project
	Changed bool
}

// writeResult normalizes a store mutation into a Write. The store answers a
// zero row for a no-op (there is no changed row to read back), so the current
// row is fetched here instead of leaving callers to special-case it.
func (s *Service) writeResult(database *store.Store, id string, row store.Project, changed bool, err error) (Write, error) {
	if err != nil {
		return Write{}, err
	}
	if changed {
		return Write{Project: row, Changed: true}, nil
	}
	current, err := database.GetProject(id)
	if err != nil {
		return Write{}, err
	}
	return Write{Project: current}, nil
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
	// Background: the row is written regardless, so the read is bounded by
	// git's own timeout rather than abandoned.
	identity, _ := s.repoIdentity(context.Background(), abs, store.ProjectIdentity{})
	project := store.Project{
		// Globally unique by construction: a client attached to more than
		// one backend keys projects by this string (internal/entityid).
		ID:   entityid.New(),
		Path: abs,
		Name: filepath.Base(abs),
		// Derived at the one moment the row is written, so a project is
		// identified from its first appearance in a client's sidebar
		// rather than only after the next boot's backfill.
		RepositoryID:   identity.RepositoryID,
		IdentitySource: identity.IdentitySource,
		IdentityError:  identity.Error,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// The stored row, not the one built above: the slug is generated inside
	// the insert, so the local copy has an empty one.
	return database.CreateProject(project)
}

func (s *Service) Rename(id, name string) (Write, error) {
	database, err := s.database("rename project")
	if err != nil {
		return Write{}, err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return Write{}, fmt.Errorf("rename project: name is required")
	}
	row, changed, err := database.UpdateProjectName(id, trimmed)
	return s.writeResult(database, id, row, changed, err)
}

func (s *Service) Archive(id string) (Write, error) {
	database, err := s.database("archive project")
	if err != nil {
		return Write{}, err
	}
	row, changed, err := database.ArchiveProject(id)
	return s.writeResult(database, id, row, changed, err)
}

func (s *Service) Unarchive(id string) (Write, error) {
	database, err := s.database("unarchive project")
	if err != nil {
		return Write{}, err
	}
	row, changed, err := database.UnarchiveProject(id)
	return s.writeResult(database, id, row, changed, err)
}

// UpdateSortPositions returns the rows the reorder wrote, which is what the
// caller broadcasts. Ids naming no project are skipped, so the answer can be
// shorter than the request.
func (s *Service) UpdateSortPositions(orderedIDs []string) ([]store.Project, error) {
	database, err := s.database("update project sort positions")
	if err != nil {
		return nil, err
	}
	return database.UpdateProjectSortPositions(orderedIDs)
}

// FolderIdentity is the repository identity of a directory that is not (yet)
// a project, answered before one is created there so a client can check the
// folder is a checkout of the repository it expects.
type FolderIdentity struct {
	RepositoryID   string `json:"repositoryID,omitempty"`
	IdentityError  string `json:"identityError,omitempty"`
	Repository     bool   `json:"repository"`
	IdentitySource string `json:"-"`
}

// InspectFolder reads the repository identity of the directory at path. The
// path must be an existing directory, as for Create. A git failure is
// returned rather than answered as "not a repository": the caller is about to
// decide whether the folder is the right checkout, and a read it could not
// make must not look like a plain folder.
func (s *Service) InspectFolder(ctx context.Context, path string) (FolderIdentity, error) {
	if s == nil || s.deps.Identity == nil {
		return FolderIdentity{}, fmt.Errorf("inspect folder: repository identity unavailable")
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return FolderIdentity{}, fmt.Errorf("inspect folder: path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return FolderIdentity{}, fmt.Errorf("inspect folder: resolve absolute path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return FolderIdentity{}, fmt.Errorf("inspect folder: stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return FolderIdentity{}, fmt.Errorf("inspect folder: %s is not a directory", abs)
	}
	identity, err := s.deps.Identity(ctx, abs)
	if err != nil {
		return FolderIdentity{}, fmt.Errorf("inspect folder %s: %w", abs, err)
	}
	if err := ctx.Err(); err != nil {
		return FolderIdentity{}, err
	}
	if identity.Repository && identity.RepositoryID == "" && identity.LookupError != "" && identity.IdentitySource != "" && s.deps.Store != nil {
		stored, err := s.deps.Store.GetProjectByPath(abs)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return FolderIdentity{}, err
		}
		if err == nil && !stored.Archived && identity.IdentitySource == stored.IdentitySource {
			identity.RepositoryID = stored.RepositoryID
		}
	}
	return FolderIdentity{Repository: identity.Repository, IdentitySource: identity.IdentitySource, RepositoryID: identity.RepositoryID, IdentityError: identity.LookupError}, nil
}
