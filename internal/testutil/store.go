package testutil

import (
	"testing"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// EnsureProject inserts a project row with the given path, returning the
// created row. Threads require a project_id FK; tests that used
// to manually construct store.Thread values need a project to hang them
// off of first. Using a stable id-per-path lookup means the helper is
// idempotent: calling it twice with the same path returns the existing
// project instead of failing on the UNIQUE constraint.
func EnsureProject(t *testing.T, st *store.Store, path string) store.Project {
	t.Helper()
	if existing, err := st.GetProjectByPath(path); err == nil {
		return existing
	}
	now := time.Now().UnixMilli()
	p := store.Project{
		ID:        uuid.NewString(),
		Path:      path,
		Name:      path,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateProject(p); err != nil {
		t.Fatalf("EnsureProject(%q): %v", path, err)
	}
	return p
}
