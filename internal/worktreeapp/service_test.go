package worktreeapp

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func newTestService(t *testing.T, transient []string, background map[string]int) (*Service, *store.Store) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service := New(Deps{
		Store: database,
		TransientBusyThreadIDs: func() []string {
			return slices.Clone(transient)
		},
		CountBackgroundTasks: func(threadID string) (int, error) {
			return background[threadID], nil
		},
	})
	return service, database
}

func seedThread(t *testing.T, database *store.Store, id, projectID, workspace, worktree string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := database.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     projectID,
		WorkspacePath: workspace,
		WorktreePath:  worktree,
		Provider:      "claude",
		Model:         "test",
		Title:         id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreateThread(%s): %v", id, err)
	}
}

func seedProject(t *testing.T, database *store.Store, id, path string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := database.CreateProject(store.Project{ID: id, Path: path, Name: id, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
}

func TestActivityAggregatesTransientWorkAcrossCanonicalWorkspaceRefs(t *testing.T) {
	workspace := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	service, database := newTestService(t, []string{"busy-a", "busy-b"}, map[string]int{"busy-a": 1, "busy-b": 2})
	seedProject(t, database, "project", workspace)
	seedThread(t, database, "busy-a", "project", workspace, "")
	seedThread(t, database, "busy-b", "project", t.TempDir(), alias)
	seedThread(t, database, "idle", "project", workspace, "")

	activity, err := service.Activity(alias)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if activity.ActiveTurnThreads != 0 || activity.RunningBackgroundTasks != 3 {
		t.Fatalf("Activity = %+v, want three tasks and no open turns", activity)
	}
	want := []BusyThread{
		{ThreadID: "busy-a", RunningBackgroundTasks: 1},
		{ThreadID: "busy-b", RunningBackgroundTasks: 2},
	}
	if !slices.Equal(activity.BusyThreads, want) {
		t.Fatalf("BusyThreads = %+v, want %+v", activity.BusyThreads, want)
	}
}

func TestThreadsReferencingWorkspaceMatchesEitherPathColumn(t *testing.T) {
	workspace := t.TempDir()
	service, database := newTestService(t, nil, nil)
	seedProject(t, database, "project", workspace)
	seedThread(t, database, "workspace-ref", "project", workspace, "")
	seedThread(t, database, "worktree-ref", "project", t.TempDir(), workspace)

	ids, err := service.ThreadsReferencingWorkspace(workspace)
	if err != nil {
		t.Fatalf("ThreadsReferencingWorkspace: %v", err)
	}
	slices.Sort(ids)
	if want := []string{"workspace-ref", "worktree-ref"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestActivityRefusesBlankPath(t *testing.T) {
	service, _ := newTestService(t, nil, nil)
	if _, err := service.Activity("   "); err == nil {
		t.Fatal("Activity accepted a blank workspace path")
	}
}
