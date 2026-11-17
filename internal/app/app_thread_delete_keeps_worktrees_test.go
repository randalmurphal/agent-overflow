package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agent-overflow/internal/transport"
)

// Deleting a thread never removes a worktree, never checks one for unsaved
// work and is never refused because of one (docs/decisions.md): every door
// stops and deletes the threads it takes, and the worktrees they were on
// stay with their files.

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

// requireWorktreeKept fails unless tree is still a worktree of the
// fixture's project, with its unsaved file when unsaved.
func requireWorktreeKept(t *testing.T, f deleteWorktreeFixture, tree string, unsaved bool) {
	t.Helper()
	if _, found, err := f.app.findWorktree(f.project.Path, tree); err != nil || !found {
		t.Errorf("worktree %s after the delete: found %v, %v; want it still a worktree of the project", tree, found, err)
	}
	if info, err := os.Stat(tree); err != nil || !info.IsDir() {
		t.Errorf("worktree %s after the delete: %v; want it on disk", tree, err)
	}
	if !unsaved {
		return
	}
	if body, err := os.ReadFile(filepath.Join(tree, "unsaved.txt")); err != nil || string(body) != "work" {
		t.Errorf("the unsaved file in %s after the delete = %q, %v; want it kept", tree, body, err)
	}
}

// A delete by each door of a thread on a worktree, whose worker is on
// another one and running, goes through, whether the worktrees hold unsaved
// work or none: both threads are gone, the worker's session stopped, and
// both worktrees stay with their files.
func TestDeletingAThreadKeepsItsWorktrees(t *testing.T) {
	t.Parallel()
	doors := map[string]func(f deleteWorktreeFixture, ids ...string) error{
		"the sidebar": func(f deleteWorktreeFixture, ids ...string) error {
			_, err := f.app.DeleteThreads(ids[:1])
			return err
		},
		"a selection": func(f deleteWorktreeFixture, ids ...string) error {
			_, err := f.app.DeleteThreads(ids)
			return err
		},
		"the archived list": func(f deleteWorktreeFixture, ids ...string) error {
			if _, _, err := f.app.threadApplication().Archive(ids[0]); err != nil {
				return err
			}
			_, err := f.app.DeleteThreads(ids[:1])
			return err
		},
		"a thread's own route": func(f deleteWorktreeFixture, ids ...string) error {
			_, err := f.app.DeleteThread(ids[0])
			return err
		},
	}
	for name, door := range doors {
		for _, unsaved := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, unsaved work %v", name, unsaved), func(t *testing.T) {
				t.Parallel()
				deleteKeepingWorktrees(t, name, door, unsaved)
			})
		}
	}
}

// deleteKeepingWorktrees is one door's case of
// TestDeletingAThreadKeepsItsWorktrees.
func deleteKeepingWorktrees(t *testing.T, name string, door func(f deleteWorktreeFixture, ids ...string) error, unsaved bool) {
	f := newDeleteWorktreeFixture(t)
	ownerTree, workerTree, otherTree := f.worktree(t, "owner-tree"), f.worktree(t, "worker-tree"), f.worktree(t, "other-tree")
	owner := f.thread(t, "owner", on(ownerTree))
	worker := f.thread(t, "worker", on(workerTree))
	other := f.thread(t, "other", on(otherTree))
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	runningSession(t, f.app, worker.ID)
	if unsaved {
		for _, tree := range []string{ownerTree, workerTree, otherTree} {
			dirty(t, tree)
		}
	}
	if err := door(f, owner.ID, other.ID); err != nil {
		t.Fatalf("the delete = %v, want it to go through", err)
	}
	deleted := []string{owner.ID, worker.ID}
	if name == "a selection" {
		deleted = append(deleted, other.ID)
	} else {
		untouched(t, f.app, other.ID)
	}
	for _, id := range deleted {
		if !gone(t, f.app, id) {
			t.Errorf("%s outlived the delete", id)
		}
		if _, live := f.app.sessionManager().get(id); live {
			t.Errorf("%s holds a session after its delete", id)
		}
	}
	for _, tree := range []string{ownerTree, workerTree, otherTree} {
		requireWorktreeKept(t, f, tree, unsaved)
	}
}

// A caller allowed to operate threads and not git deletes a thread on a
// worktree with unsaved work: the delete doors ask for nothing a worktree
// would need, at the transport or in the delete.
func TestACallerWithoutGitOperateDeletesAThreadOnAWorktree(t *testing.T) {
	t.Parallel()
	granted := []string{string(transport.ScopeThreadsOperate)}
	for _, method := range []string{"DeleteThreads", "DeleteThread"} {
		if refusal := transport.AuthorizeSessionMethod(granted, method, transport.CallerProof{}); refusal != nil {
			t.Errorf("%s with threads:operate alone = %+v, want it admitted", method, refusal)
		}
	}
	f := newDeleteWorktreeFixture(t)
	tree := f.worktree(t, "worker-tree")
	owner := f.thread(t, "owner", on(tree))
	dirty(t, tree)
	if _, err := f.app.DeleteThreads([]string{owner.ID}); err != nil {
		t.Fatalf("the delete = %v, want it to go through", err)
	}
	if !gone(t, f.app, owner.ID) {
		t.Error("the thread outlived its delete")
	}
	requireWorktreeKept(t, f, tree, true)
}

// Two deletes under way whose threads share a worktree both complete, in
// either order of their phases, and the worktree stays.
func TestTwoDeletesSharingAWorktreeBothComplete(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{"the first verified", "the first committed"} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			f := newDeleteWorktreeFixture(t)
			tree := f.worktree(t, "shared-tree")
			a := f.thread(t, "a", on(tree))
			b := f.thread(t, "b", on(tree))
			dirty(t, tree)
			stopped := newHold()
			if hold == "the first verified" {
				f.app.deleteVerifiedForTest = func(id string) {
					if id == a.ID {
						stopped.stop()
					}
				}
			} else {
				f.app.deleteCommittedForTest = func(ids []string) {
					if len(ids) == 1 && ids[0] == a.ID {
						stopped.stop()
					}
				}
			}
			first := make(chan error, 1)
			go func() {
				_, err := f.app.DeleteThreads([]string{a.ID})
				first <- err
			}()
			within(t, "the first delete's hold", stopped.reached)
			if _, err := f.app.DeleteThreads([]string{b.ID}); err != nil {
				t.Fatalf("the second delete = %v, want it to go through", err)
			}
			close(stopped.release)
			if err := within(t, "the first delete", first); err != nil {
				t.Fatalf("the first delete = %v, want it to go through", err)
			}
			if left := remaining(t, f.app, []string{a.ID, b.ID}); len(left) > 0 {
				t.Errorf("threads left = %v", left)
			}
			requireWorktreeKept(t, f, tree, true)
		})
	}
}
