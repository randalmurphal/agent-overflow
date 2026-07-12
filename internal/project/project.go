package project

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// EnsureForWorkspace finds or creates a project row for the given
// workspace path. Used by flows (CreateThreadFromPR, auto-import) that
// need a project implicitly before a Thread can be inserted.
//
// Lookup precedence:
//  1. Project whose path exactly matches the resolved git repository root.
//  2. Project whose path matches the workspace path verbatim.
//  3. Create a new project at whichever path is a git root, or fall back
//     to the workspace path.
//
// A nil core is treated as "no git probe available" — the function
// degrades to a verbatim-path lookup/create rather than failing, so the
// caller can run before git wiring has landed (a few test fixtures rely
// on this).
//
// Returns an error when the store is nil or the workspace path is empty.
func EnsureForWorkspace(s *store.Store, core *gitops.Core, workspacePath string) (store.Project, error) {
	if s == nil {
		return store.Project{}, fmt.Errorf("resolve project: store unavailable")
	}
	trimmed := strings.TrimSpace(workspacePath)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("resolve project: workspace path is required")
	}

	// Prefer the git repo root when detectable — two threads in sibling
	// checkouts should share the same project row.
	candidatePath := trimmed
	if core != nil {
		if root, err := core.RepositoryRoot(trimmed); err == nil {
			if r := strings.TrimSpace(root); r != "" {
				candidatePath = r
			}
		}
	}

	if existing, err := s.GetProjectByPath(candidatePath); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.Project{}, err
	}
	if candidatePath != trimmed {
		if existing, err := s.GetProjectByPath(trimmed); err == nil {
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
	if err := s.CreateProject(p); err != nil {
		return store.Project{}, err
	}
	return s.GetProject(p.ID)
}
