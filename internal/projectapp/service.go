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
	// Identity derives a checkout's repository identity — the `origin`
	// remote as git reports it, and the smallest root commit of HEAD.
	// `internal/app` supplies the git-backed implementation; nil means the
	// service answers "not known" for every path, which is what keeps
	// project policy testable without a git subprocess.
	Identity func(path string) (remoteURL, rootCommit string)
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

// repoIdentity asks the injected deriver what repository a path is a checkout
// of. Nil-safe: with no deriver wired the answer is "not known", the same
// value a non-git directory produces, so no caller needs a second shape for
// "identity is unavailable here".
func (s *Service) repoIdentity(path string) (remoteURL, rootCommit string) {
	if s == nil || s.deps.Identity == nil {
		return "", ""
	}
	return s.deps.Identity(path)
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
	remoteURL, rootCommit := s.repoIdentity(abs)
	project := store.Project{
		// Globally unique by construction: a client attached to more than
		// one backend keys projects by this string (internal/entityid).
		ID:   entityid.New(),
		Path: abs,
		Name: filepath.Base(abs),
		// Derived at the one moment the row is written, so a project is
		// identified from its first appearance in a client's sidebar
		// rather than only after the next boot's backfill.
		RemoteURL:  remoteURL,
		RootCommit: rootCommit,
		CreatedAt:  now,
		UpdatedAt:  now,
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
