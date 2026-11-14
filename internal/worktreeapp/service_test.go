package worktreeapp

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
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

// goneWorktree registers a worktree of repo on a branch pushed to origin
// with one further local commit, then deletes its directory.
func goneWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), branch)
	testutil.RunGit(t, repo, "worktree", "add", "-b", branch, path)
	testutil.RunGit(t, path, "push", "-u", "origin", branch)
	testutil.RunGit(t, path, "commit", "--allow-empty", "-m", "local only")
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
	return path
}

// A registered worktree whose directory is gone has nothing uncommitted, and
// its branch's unpushed commits are still counted, from the project root.
func TestStatusOfAWorktreeWhoseDirectoryIsGone(t *testing.T) {
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	path := goneWorktree(t, repo, "gone")
	service, database := newTestService(t, nil, nil)
	seedProject(t, database, "project", repo)
	seedThread(t, database, "attached", "project", path, path)

	status, err := service.Status(repo, path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := WorktreeStatus{Path: status.Path, Branch: "gone", UnpushedCommits: 1, HasUpstream: true, AttachedThreads: 1, Missing: true}
	if status != want {
		t.Fatalf("Status = %+v, want %+v", status, want)
	}
}

func TestFindPresentRefusesAWorktreeWhoseDirectoryIsGone(t *testing.T) {
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	gone := goneWorktree(t, repo, "gone")
	live := filepath.Join(t.TempDir(), "live")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "live", live)
	service, _ := newTestService(t, nil, nil)

	if worktree, ok, err := service.FindPresent(repo, live); err != nil || !ok || worktree.Branch != "live" {
		t.Fatalf("FindPresent(live) = %+v, %v, %v; want the live worktree", worktree, ok, err)
	}
	if _, ok, err := service.Find(repo, gone); err != nil || !ok {
		t.Fatalf("Find(gone) = %v, %v; want it registered", ok, err)
	}
	if _, ok, err := service.FindPresent(repo, gone); err == nil || ok {
		t.Fatalf("FindPresent(gone) = %v, %v; want a refusal", ok, err)
	}
	if _, ok, err := service.FindPresent(repo, t.TempDir()); err != nil || ok {
		t.Fatalf("FindPresent(unregistered) = %v, %v; want not found without error", ok, err)
	}
}

// A worktree detached at a tag lists the tag and counts the commits made
// there that no ref reaches; one on a branch counts none, and a missing
// detached worktree still counts its orphans from the project root.
func TestDetachedWorktreeReportsItsTagAndOrphanCommits(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "tag", "v1")
	atTag := filepath.Join(t.TempDir(), "at-tag")
	testutil.RunGit(t, repo, "worktree", "add", "--detach", atTag, "refs/tags/v1")
	pastTag := filepath.Join(t.TempDir(), "past-tag")
	testutil.RunGit(t, repo, "worktree", "add", "--detach", pastTag, "refs/tags/v1")
	testutil.RunGit(t, pastTag, "commit", "--allow-empty", "-m", "one")
	testutil.RunGit(t, pastTag, "commit", "--allow-empty", "-m", "two")
	gone := filepath.Join(t.TempDir(), "gone")
	testutil.RunGit(t, repo, "worktree", "add", "--detach", gone, "refs/tags/v1")
	testutil.RunGit(t, gone, "commit", "--allow-empty", "-m", "lost")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	service, database := newTestService(t, nil, nil)
	seedProject(t, database, "project", repo)

	items, err := service.List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	tags := map[string]string{}
	for _, item := range items {
		tags[filepath.Base(item.Path)] = item.Tag
	}
	if tags["at-tag"] != "v1" || tags["past-tag"] != "" || tags[filepath.Base(repo)] != "" {
		t.Fatalf("list tags = %v; want v1 only for the worktree at the tag", tags)
	}
	for _, tc := range []struct {
		path string
		want int
	}{{atTag, 0}, {pastTag, 2}, {gone, 1}, {repo, 0}} {
		status, err := service.Status(repo, tc.path)
		if err != nil {
			t.Fatalf("Status(%s): %v", tc.path, err)
		}
		if status.OrphanCommits != tc.want {
			t.Fatalf("Status(%s).OrphanCommits = %d, want %d", filepath.Base(tc.path), status.OrphanCommits, tc.want)
		}
	}
}

func TestActivityRefusesBlankPath(t *testing.T) {
	service, _ := newTestService(t, nil, nil)
	if _, err := service.Activity("   "); err == nil {
		t.Fatal("Activity accepted a blank workspace path")
	}
}
