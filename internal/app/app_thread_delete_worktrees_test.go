package app

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
)

// A delete removes exactly the worktrees it verified and was allowed to
// remove (familyDelete.verifyWorktrees), and of two deletes that share a
// worktree, one removes it or names it as kept.

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

// hold stops a delete at a hook until the test lets it go.
type hold struct {
	reached, release chan struct{}
	once             sync.Once
}

func newHold() *hold { return &hold{reached: make(chan struct{}), release: make(chan struct{})} }

// stop is the hook's body: it signals, once, and waits.
func (h *hold) stop() {
	h.once.Do(func() {
		close(h.reached)
		<-h.release
	})
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

// crossingDelete is one of two deletes of the threads on one worktree: the
// thread it names, whether it removes worktrees, and where it stops for the
// other: once its thread is verified ("verify") and once it committed
// ("commit"), in that order.
type crossingDelete struct {
	thread string
	remove bool
	holds  []string
}

// Two deletes of the threads on one worktree, each named alone, crossing
// while both hold their claims. A remover that finds the other thread
// another delete's is refused, naming it, when that delete's claim is the
// older, and its retry, once that delete ended, removes the worktree; when
// its own claim is the older it goes ahead and keeps the worktree, naming
// it. A plain delete is never refused. Whatever the order, after a reopen
// no thread, claim or commit is left, and the worktree is removed or was
// named as kept.
func TestTwoDeletesSharingAWorktreeLeaveItRemovedOrNamed(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		first, second crossingDelete
		// refused is whether the second is refused.
		refused bool
	}{
		"both remove, the second runs whole while the first verifies": {
			first: crossingDelete{"a", true, []string{"verify"}}, second: crossingDelete{"b", true, nil}, refused: true,
		},
		"both remove, each verified before the other commits": {
			first:   crossingDelete{"a", true, []string{"verify", "commit"}},
			second:  crossingDelete{"b", true, []string{"verify"}},
			refused: true,
		},
		"both remove, the first's checks with the second claimed": {
			first: crossingDelete{"a", true, []string{"verify"}}, second: crossingDelete{"b", true, []string{"verify"}},
		},
		"the remover first, a plain delete committed beside it": {
			first: crossingDelete{"a", true, []string{"verify"}}, second: crossingDelete{"b", false, []string{"commit"}},
		},
		"the remover first, a plain delete runs whole while it verifies": {
			first: crossingDelete{"a", true, []string{"verify"}}, second: crossingDelete{"b", false, nil},
		},
		"a plain delete first, the remover while it verifies": {
			first: crossingDelete{"a", false, []string{"verify"}}, second: crossingDelete{"b", true, nil}, refused: true,
		},
		"a plain delete first, the remover after it committed": {
			first: crossingDelete{"a", false, []string{"commit"}}, second: crossingDelete{"b", true, nil}, refused: true,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSharedTree(t)
			holds := map[string]*hold{}
			for _, d := range []crossingDelete{c.first, c.second} {
				for _, at := range d.holds {
					holds[d.thread+"@"+at] = newHold()
				}
			}
			stop := func(key string) {
				if h := holds[key]; h != nil {
					h.stop()
				}
			}
			f.app.deleteVerifiedForTest = func(id string) { stop(id + "@verify") }
			f.app.deleteCommittedForTest = func(ids []string) {
				if len(ids) == 1 {
					stop(ids[0] + "@commit")
				}
			}
			type answer struct {
				result DeleteThreadResult
				err    error
			}
			start := func(d crossingDelete) chan answer {
				done := make(chan answer, 1)
				go func() {
					var a answer
					if d.remove {
						a.result, a.err = f.app.DeleteThreadsAndWorktrees(context.Background(), []string{d.thread})
					} else {
						a.result, a.err = f.app.DeleteThread(d.thread)
					}
					done <- a
				}()
				return done
			}
			at := func(d crossingDelete, i int) *hold { return holds[d.thread+"@"+d.holds[i]] }

			firstDone := start(c.first)
			within(t, "the first delete's hold", at(c.first, 0).reached)
			secondDone := start(c.second)
			var second answer
			if len(c.second.holds) > 0 {
				within(t, "the second delete's hold", at(c.second, 0).reached)
			} else {
				second = within(t, "the second delete", secondDone)
			}
			close(at(c.first, 0).release)
			if len(c.first.holds) > 1 {
				within(t, "the first delete's commit", at(c.first, 1).reached)
			}
			if len(c.second.holds) > 0 && len(c.first.holds) > 1 {
				close(at(c.second, 0).release)
				second = within(t, "the second delete", secondDone)
			}
			if len(c.first.holds) > 1 {
				close(at(c.first, 1).release)
			}
			first := within(t, "the first delete", firstDone)
			if len(c.second.holds) > 0 && len(c.first.holds) == 1 {
				close(at(c.second, 0).release)
				second = within(t, "the second delete", secondDone)
			}

			if first.err != nil || len(first.result.WorkersNotDeleted) > 0 {
				t.Fatalf("the first delete = %+v, %v", first.result, first.err)
			}
			if !c.refused {
				if second.err != nil || len(second.result.WorkersNotDeleted) > 0 {
					t.Fatalf("the second delete = %+v, %v", second.result, second.err)
				}
			} else {
				err := second.err
				if code, _, ok := errorsx.PublicDetails(err); !ok || code != "delete_worktree_being_deleted" || !errors.Is(err, store.ErrThreadDeleting) ||
					!strings.Contains(err.Error(), `Nothing was deleted: thread "`+c.first.thread+`", which is also on the worktree `+f.tree+`, is being deleted. Delete again when that ends.`) {
					t.Fatalf("the second delete = %v, want it refused naming %s", err, c.first.thread)
				}
				// Its retry, once the older ended, finds the worktree its own.
				retry, err := f.app.DeleteThreadsAndWorktrees(context.Background(), []string{c.second.thread})
				if err != nil || len(retry.RetainedWorktrees) > 0 {
					t.Fatalf("the retry = %+v, %v", retry, err)
				}
				if exists(t, f.tree) {
					t.Error("the retry left the worktree")
				}
			}
			named := false
			for _, result := range []DeleteThreadResult{first.result, second.result} {
				for _, kept := range result.RetainedWorktrees {
					if kept.Path == f.tree && strings.Contains(kept.Reason, "is being deleted by another delete") {
						named = true
					}
				}
			}
			if exists(t, f.tree) && !named {
				t.Errorf("the worktree is on disk and no delete named it: first %+v, second %+v", first.result, second.result)
			}
			f.reopen(t, "a", "b")
		})
	}
}
