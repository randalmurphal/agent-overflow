package app

import (
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
)

// A worktree deleted with `rm -rf` stays in git's registry. The app lists it
// as missing, removes the registration on request, and puts no thread in it.

// goneWorktree is a registered worktree of f on branch whose directory has
// been deleted.
func goneWorktree(t *testing.T, f watchFixture, branch string) string {
	t.Helper()
	path := f.worktree(t, branch)
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
	return path
}

func TestGitListWorktreesMarksARegisteredWorktreeWhoseDirectoryIsGone(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	live := f.worktree(t, "list-live")
	deleted := goneWorktree(t, f, "list-deleted")

	items, err := f.app.GitListWorktrees(WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo})
	if err != nil {
		t.Fatalf("GitListWorktrees: %v", err)
	}
	missing := map[string]bool{}
	for _, item := range items {
		missing[gitops.CanonicalPath(item.Path)] = item.Missing
	}
	for path, want := range map[string]bool{f.repo: false, live: false, deleted: true} {
		got, ok := missing[gitops.CanonicalPath(path)]
		if !ok || got != want {
			t.Errorf("%s: listed=%v missing=%v, want listed with missing=%v (items %+v)", path, ok, got, want, items)
		}
	}
}

// The confirmation's status read and the unforced removal both work with no
// directory to run git in: the registration goes, and a thread still on the
// path moves to the project root.
func TestRemoveOtherWorktreeDropsARegistrationWhoseDirectoryIsGone(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	worktree := goneWorktree(t, f, "removal-gone")
	occupant := f.thread(t, "thread-removal-gone", worktree, "removal-gone")
	root := WorkspaceRef{ProjectID: f.project.ID, WorkspacePath: f.repo}

	status, err := f.app.GitWorktreeStatus(root, worktree)
	if err != nil {
		t.Fatalf("GitWorktreeStatus: %v", err)
	}
	if !status.Missing || status.Dirty || status.AttachedThreads != 1 {
		t.Fatalf("status = %+v, want missing, clean, one attached thread", status)
	}

	removal, err := f.app.RemoveOtherWorktree(root, worktree, false)
	if err != nil {
		t.Fatalf("RemoveOtherWorktree: %v", err)
	}
	if len(removal.Reattached) != 1 || removal.Reattached[0].ID != occupant.ID {
		t.Errorf("reattached = %+v, want the occupant", removal.Reattached)
	}
	f.assertAtRoot(t, occupant.ID)
	if _, registered, err := f.app.findWorktree(f.repo, worktree); err != nil || registered {
		t.Fatalf("after removal: registered=%v err=%v, want the registration gone", registered, err)
	}
}

// Unpushed commits on the missing worktree's branch still need the caller's
// force, as they do for a worktree that is present.
func TestRemoveOtherWorktreeGuardsUnpushedCommitsOfAMissingWorktree(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo, _ := initGitRepoWithOrigin(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	f := watchFixture{app: app, repo: project.Path, project: project}
	worktree := f.worktree(t, "removal-gone-unpushed")
	testutil.RunGit(t, worktree, "push", "-u", "origin", "removal-gone-unpushed")
	testutil.RunGit(t, worktree, "commit", "--allow-empty", "-m", "local only")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
	root := WorkspaceRef{ProjectID: project.ID, WorkspacePath: project.Path}

	status, err := app.GitWorktreeStatus(root, worktree)
	if err != nil {
		t.Fatalf("GitWorktreeStatus: %v", err)
	}
	if !status.Missing || status.UnpushedCommits != 1 || !status.HasUpstream {
		t.Fatalf("status = %+v, want missing with one unpushed commit", status)
	}
	if _, err := app.RemoveOtherWorktree(root, worktree, false); err == nil || !strings.Contains(err.Error(), "unpushed=1") {
		t.Fatalf("unforced removal error = %v, want the unpushed-commit refusal", err)
	}
	if _, err := app.RemoveOtherWorktree(root, worktree, true); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if _, registered, err := app.findWorktree(project.Path, worktree); err != nil || registered {
		t.Fatalf("after removal: registered=%v err=%v, want the registration gone", registered, err)
	}
}

func TestUpdateThreadWorkspaceRefusesAWorktreeWhoseDirectoryIsGone(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	worktree := goneWorktree(t, f, "switch-gone")
	thread := f.thread(t, "thread-switch-gone", f.repo, "main")

	if _, err := f.app.UpdateThreadWorkspace(thread.ID, worktree); err == nil || !strings.Contains(err.Error(), "directory is missing") {
		t.Fatalf("UpdateThreadWorkspace error = %v, want the missing-directory refusal", err)
	}
	f.assertAtRoot(t, thread.ID)
}

// thread_spawn names the worktree in prose the model reads, however the call
// arrived at it: by branch, by path, or by inheriting the caller's.
func TestThreadSpawnNamesAWorktreeWhoseDirectoryIsGone(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	worktree := goneWorktree(t, f, "spawn-gone")
	inside := store.Thread{ProjectID: f.project.ID, WorkspacePath: worktree, WorktreePath: worktree, Branch: "spawn-gone"}
	for name, tc := range map[string]struct {
		caller store.Thread
		call   threadtools.SpawnCall
	}{
		"by branch": {call: threadtools.SpawnCall{WorktreeBranch: "spawn-gone"}},
		"by path":   {call: threadtools.SpawnCall{WorkspacePath: worktree}},
		"inherited": {caller: inside},
	} {
		opts := spawnOptions{CreateThreadOptions: CreateThreadOptions{ProjectID: f.project.ID}}
		err := threadToolsApp{app: f.app}.applySpawnWorkspace(&opts, tc.caller, tc.call)
		_, message, public := errorsx.PublicDetails(err)
		if !public || !strings.Contains(message, "its folder is gone") || !strings.Contains(message, opts.WorktreePath) {
			t.Errorf("%s: error = %v, want a public refusal naming the worktree", name, err)
		}
	}
}

func TestCreateThreadRefusesAWorktreeWhoseDirectoryIsGone(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	worktree := goneWorktree(t, f, "create-gone")

	_, err := f.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID:    f.project.ID,
		Provider:     string(provider.Codex),
		Model:        "gpt-5.4",
		Mode:         "chat",
		WorktreePath: worktree,
	})
	if err == nil || !strings.Contains(err.Error(), "directory is missing") {
		t.Fatalf("CreateThread error = %v, want the missing-directory refusal", err)
	}
}
