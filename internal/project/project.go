package project

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/gitroot"
	"agent-overflow/internal/store"
)

// EnsureForWorkspace finds or creates a project row for the given
// workspace path. Used by flows (CreateThreadFromPR, session import,
// worktree/thread creation) that need a project implicitly before a
// Thread can be inserted.
//
// Lookup precedence:
//  1. Project whose path exactly matches the MAIN repository root of the
//     workspace.
//  2. Project whose path matches the workspace path verbatim.
//  3. Create a new project at whichever path is a repository root, or fall
//     back to the workspace path.
//
// "Main repository root" is `gitroot.MainRoot` — git's `--git-common-dir`
// semantics, not `--show-toplevel`'s. A workspace that is a LINKED WORKTREE
// resolves to the repository it was cut from, so a thread running in a
// worktree lands in the real project instead of minting one named after the
// branch (root AGENTS.md, core principle 7: a project is the repository, a
// workspace is where the provider operates). The resolution is pure
// filesystem reads, so it costs no subprocess and is safe to run per row.
//
// Only the PROJECT is resolved this way. The caller's own workspace path is
// what belongs on the thread — a worktree thread keeps working in its
// worktree.
//
// Returns an error when the store is nil or the workspace path is empty.
func EnsureForWorkspace(s *store.Store, workspacePath string) (store.Project, error) {
	if s == nil {
		return store.Project{}, fmt.Errorf("resolve project: store unavailable")
	}
	trimmed := strings.TrimSpace(workspacePath)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("resolve project: workspace path is required")
	}

	// Prefer the repository root when detectable — two threads in sibling
	// worktrees of one repository should share the same project row.
	candidatePath := trimmed
	if root, ok := gitroot.MainRoot(trimmed); ok {
		candidatePath = root
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
