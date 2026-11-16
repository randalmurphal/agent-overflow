package app

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
)

// A delete removes exactly the worktrees it verified and was allowed to
// remove (familyDelete.verifyWorktrees).

// sharedTree is a git project with one clean linked worktree that the
// threads a and b are on, in a store a test can reopen.
type sharedTree struct {
	app        *App
	path, repo string
	project    string
	tree       string
}

func newSharedTree(t *testing.T) sharedTree {
	t.Helper()
	app, path := newTestAppWithStorePath(t)
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-b", "main")
	testutil.RunGit(t, repo, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "initial commit")
	project, err := app.store.CreateProject(store.Project{ID: entityid.New(), Path: repo, Name: "repo", CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	f := sharedTree{app: app, path: path, repo: repo, project: project.ID, tree: filepath.Join(t.TempDir(), "shared")}
	testutil.RunGit(t, repo, "worktree", "add", "-b", "shared", f.tree)
	f.thread(t, "a", f.tree)
	f.thread(t, "b", f.tree)
	return f
}

// thread makes the thread id on worktree tree.
func (f sharedTree) thread(t *testing.T, id, tree string) {
	t.Helper()
	thread := store.Thread{
		ID: id, ProjectID: f.project, Title: id, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, RuntimeMode: string(provider.RuntimeApprovalRequired), CreatedAt: 1, UpdatedAt: 1,
	}
	on(tree)(&thread)
	if err := f.app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	withMessage(t, f.app, id)
}

// reopen closes the store and opens it again, as a boot does, and fails
// unless no thread of ids, and no delete's claim or commit, is left.
func (f sharedTree) reopen(t *testing.T, ids ...string) {
	t.Helper()
	f.app.appCancel()
	if err := f.app.store.Close(); err != nil {
		t.Fatal(err)
	}
	boot := newTestAppAtStorePath(t, f.path)
	if left := remaining(t, boot, ids); len(left) > 0 {
		t.Errorf("threads left after the reopen = %v", left)
	}
	if released, err := boot.store.ReleaseUndecidedDeleteClaims(); err != nil || released != 0 {
		t.Errorf("claims left after the reopen = %d, %v", released, err)
	}
	if pending, err := boot.store.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Errorf("commits left after the reopen = %v, %v", pending, err)
	}
}

// A delete removes only a worktree it found emptied at its verify and was
// allowed to remove. A client without git:operate deleting a thread whose
// worktree another thread shares is never asked for it, so when the other
// thread is deleted before its destroy reaches the worktree, the worktree
// stays; and the boot finishes such a delete as committed, never removing
// the worktree.
func TestADeleteRemovesOnlyTheWorktreesItWasAllowedTo(t *testing.T) {
	t.Parallel()
	t.Run("a client without git:operate", func(t *testing.T) {
		t.Parallel()
		f := newSharedTree(t)
		if err := f.app.initIdentity("backend-under-test"); err != nil {
			t.Fatal(err)
		}
		limited := callFrom(pairSessionWithScopes(t, f.app, "delete-limited", []identity.Scope{identity.ScopeThreadsOperate}).ID, false)
		var crossing error
		f.app.deleteCommittedForTest = func(ids []string) {
			if len(ids) == 1 && ids[0] == "a" {
				_, crossing = f.app.DeleteThread("b")
			}
		}
		result, err := f.app.DeleteThreadsAndWorktrees(limited, []string{"a"})
		if err != nil || len(result.WorkersNotDeleted) > 0 || len(result.RetainedWorktrees) > 0 {
			t.Fatalf("DeleteThreadsAndWorktrees = %+v, %v", result, err)
		}
		if crossing != nil {
			t.Fatalf("the crossing delete of b: %v", crossing)
		}
		if !exists(t, f.tree) {
			t.Error("a delete never allowed to remove the worktree removed it")
		}
		f.reopen(t, "a", "b")
	})
	t.Run("the boot", func(t *testing.T) {
		t.Parallel()
		f := newSharedTree(t)
		f.app.deleteCommittedForTest = func([]string) { runtime.Goexit() }
		stopped := make(chan bool, 1)
		go func() {
			returned := false
			defer func() { stopped <- returned }()
			_, _ = f.app.DeleteThreadsAndWorktrees(context.Background(), []string{"a"})
			returned = true
		}()
		if within(t, "the interrupted delete", stopped) {
			t.Fatal("the delete returned instead of stopping")
		}
		f.app.appCancel()
		if err := f.app.store.Close(); err != nil {
			t.Fatal(err)
		}
		boot := newTestAppAtStorePath(t, f.path)
		boot.maintenance.firstReadsFallback = time.Millisecond
		t.Cleanup(func() {
			boot.appCancel()
			boot.waitPendingThreadDeletes()
		})
		boot.releaseUndecidedDeletes()
		// b goes before the boot finishes a, so a is the worktree's last
		// thread when it does.
		if _, err := boot.DeleteThread("b"); err != nil {
			t.Fatal(err)
		}
		boot.startPendingThreadDeletes()
		waitUntil(t, 10*time.Second, func() bool { return len(remaining(t, boot, []string{"a", "b"})) == 0 })
		if !exists(t, f.tree) {
			t.Error("the boot removed a worktree its delete never verified")
		}
	})
}

// A thread outside the delete that reaches a worktree the delete verified
// keeps it, and the delete names it, its threads still deleted: one there
// by the commit leaves no thread recorded to remove it, and one that
// arrives after the commit is found when the destroy reaches it.
func TestAWorktreeAThreadReachesAfterTheVerifyIsKeptAndNamed(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the commit", "after the commit"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			f := newSharedTree(t)
			if _, err := f.app.DeleteThread("b"); err != nil {
				t.Fatal(err)
			}
			arrive := func() { f.thread(t, "c", f.tree) }
			req := deleteRequest{seeds: []string{"a"}, removeWorktrees: func() error { return nil }}
			// recorded is whether a's commit recorded that it removes the
			// worktree, which a boot finishing it would act on.
			var recorded bool
			f.app.deleteCommittedForTest = func([]string) {
				family, err := f.app.store.ListDeleteFamily([]string{"a"})
				if err != nil || len(family) != 1 {
					t.Errorf("a's family = %+v, %v", family, err)
					return
				}
				recorded = family[0].RemoveWorktrees
				if when == "after the commit" {
					arrive()
				}
			}
			if when == "before the commit" {
				req.verified = func([]store.DeleteMember) error {
					arrive()
					return nil
				}
			}
			outcome, err := f.app.deleteThreads(req)
			if err != nil || len(outcome.failed) > 0 {
				t.Fatalf("delete = %+v, %v", outcome, err)
			}
			if len(outcome.retained) != 1 || outcome.retained[0].Path != f.tree || outcome.retained[0].Reason != `thread "c", which this delete does not take, is on it` {
				t.Fatalf("retained = %+v, want the worktree named for c", outcome.retained)
			}
			if want := when == "after the commit"; recorded != want {
				t.Errorf("a's commit recorded the removal = %v, want %v", recorded, want)
			}
			if !gone(t, f.app, "a") || !exists(t, f.tree) {
				t.Errorf("a gone %v, worktree kept %v; want both", gone(t, f.app, "a"), exists(t, f.tree))
			}
			untouched(t, f.app, "c")
		})
	}
}
