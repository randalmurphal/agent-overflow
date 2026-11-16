package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The phases of a family delete (app_thread_delete.go): a claimed thread is
// fenced at every door, deletes over one family do not overlap, a crash at
// any phase is settled by the boot, and a failure after the commit keeps
// what it must for the next attempt.

// Every door that runs, moves or changes a thread refuses one a delete
// claimed, with the claim kept, and works again once the claim is released.
// Transfers are fenced in the store (TestADeleteClaimsItsFamilyInOneTransaction).
func TestEveryDoorRefusesAClaimedThread(t *testing.T) {
	t.Parallel()
	type door struct {
		// seed makes the thread the door acts on, and prepares what it needs
		// before the claim.
		seed func(t *testing.T, f *requestFixture) string
		// open walks through the door; nth counts the walks.
		open func(t *testing.T, f *requestFixture, threadID string, nth int) error
	}
	plain := func(t *testing.T, f *requestFixture) string { return f.plainThread(t).ID }
	doors := map[string]door{
		"send": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			return f.app.SendMessage(id, "hello", nil)
		}},
		"steer": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.app.SteerMessageWithOptions(t.Context(), id, "steer", SendMessageOptions{})
			return err
		}},
		"session start": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			return f.app.startSession(t.Context(), id)
		}},
		"thread_send to it": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: id, Message: "look"})
			return err
		}},
		"thread_spawn from it": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, id), threadtools.SpawnCall{Prompt: "work"})
			return err
		}},
		"thread_remind from it": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.adapter().Remind(t.Context(), f.targetIdentity(t, id), threadtools.RemindCall{
				DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "later",
			})
			return err
		}},
		"wake delivery": {func(t *testing.T, f *requestFixture) string {
			id := f.plainThread(t).ID
			armLoop(t, f.app, id)
			return id
		}, func(t *testing.T, f *requestFixture, id string, nth int) error {
			wakes, err := f.app.store.ListAgentWakes(id)
			if err != nil || len(wakes) != 1 {
				t.Fatalf("wakes = %+v, %v", wakes, err)
			}
			return f.app.deliverAgentWake(t.Context(), wakes[0], int64(nth+1), testAgentWake(t, wakes[0], "fired"))
		}},
		"fork": {func(t *testing.T, f *requestFixture) string {
			return f.forkableThread(t, "sess-door-fork").ID
		}, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.app.ForkThread(t.Context(), id, nil)
			return err
		}},
		"handoff": {func(t *testing.T, f *requestFixture) string {
			f.assistantCaller(t)
			return f.caller.ID
		}, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.app.StartNextAssistantThread(t.Context(), id)
			return err
		}},
		"archive": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.app.ArchiveThread(id)
			return err
		}},
		"placement": {func(t *testing.T, f *requestFixture) string {
			id := f.plainThread(t).ID
			recordSpawn(t, f.app, "tok-door-placement", f.caller.ID, id)
			return id
		}, func(t *testing.T, f *requestFixture, id string, _ int) error {
			_, err := f.app.SetThreadPlacement([]string{id}, store.PlacementStandalone)
			return err
		}},
		"group": {plain, func(t *testing.T, f *requestFixture, id string, _ int) error {
			row, err := f.app.store.GetThread(id)
			if err != nil {
				t.Fatal(err)
			}
			group, err := f.app.store.CreateThreadGroup(row.ProjectID, "door "+entityid.New())
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.app.SetThreadGroup([]string{id}, group.ID)
			return err
		}},
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaude(t, "one", "two", "three")
			id := door.seed(t, f)
			if _, err := f.app.store.ClaimDeleteFamily("door-op", []string{id}); err != nil {
				t.Fatal(err)
			}
			if err := door.open(t, f, id, 0); !errors.Is(err, store.ErrThreadDeleting) {
				t.Fatalf("%s on a claimed thread = %v, want store.ErrThreadDeleting", name, err)
			}
			if err := f.app.store.CheckThreadNotDeleting(id); !errors.Is(err, store.ErrThreadDeleting) {
				t.Fatalf("the claim after a refused %s = %v, want it kept", name, err)
			}
			if err := f.app.store.ReleaseDeleteClaim("door-op"); err != nil {
				t.Fatal(err)
			}
			err := door.open(t, f, id, 1)
			// Claude takes no steer: past the fence, the provider refuses it.
			if name == "steer" && err != nil && strings.Contains(err.Error(), `not supported for provider "claude"`) {
				err = nil
			}
			if err != nil {
				t.Fatalf("%s once the claim was released: %v", name, err)
			}
		})
	}
}

// Two deletes over one family never overlap: a delete of a thread another
// delete holds is refused with nothing claimed, and the first finishes. A
// first delete refused at its verify releases every claim, so the second,
// retried, deletes the whole family: here a Delete with worktrees from a
// client without git:operate, crossed by a plain delete of the same thread.
func TestDeletesOverOneFamilyDoNotOverlap(t *testing.T) {
	t.Parallel()
	t.Run("the second is refused while the first holds the family", func(t *testing.T) {
		t.Parallel()
		f := newDeleteFixture(t)
		owner, worker := f.thread(t, "owner"), f.thread(t, "worker")
		recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
		var second error
		var once sync.Once
		f.app.deleteVerifiedForTest = func(string) {
			once.Do(func() { _, second = f.app.DeleteThread(worker.ID) })
		}
		if result, err := f.app.DeleteThread(owner.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
			t.Fatalf("the first delete = %+v, %v", result, err)
		}
		if !errors.Is(second, store.ErrThreadDeleting) || !strings.Contains(second.Error(), "is already being deleted; nothing was deleted, try again once that delete ends") {
			t.Fatalf("the second delete = %v, want it refused", second)
		}
		if !gone(t, f.app, owner.ID) || !gone(t, f.app, worker.ID) {
			t.Error("the family outlived the first delete")
		}
	})
	t.Run("the first is refused at its verify and the second retried", func(t *testing.T) {
		t.Parallel()
		f := newDeleteWorktreeFixture(t)
		if err := f.app.initIdentity("backend-under-test"); err != nil {
			t.Fatal(err)
		}
		limited := callFrom(pairSessionWithScopes(t, f.app, "delete-limited", []identity.Scope{identity.ScopeThreadsOperate}).ID, false)
		tree := f.worktree(t, "worker-tree")
		owner := f.thread(t, "owner")
		worker := f.thread(t, "worker", on(tree))
		recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
		var second error
		var once sync.Once
		f.app.deleteVerifiedForTest = func(string) {
			once.Do(func() { _, second = f.app.DeleteThread(owner.ID) })
		}
		_, err := f.app.DeleteThreadsAndWorktrees(limited, []string{owner.ID})
		wantScopeRefusal(t, err, transport.ScopeGitOperate)
		if !errors.Is(second, store.ErrThreadDeleting) {
			t.Fatalf("the crossing delete = %v, want it refused", second)
		}
		untouched(t, f.app, owner.ID, worker.ID)
		f.app.deleteVerifiedForTest = nil
		if result, err := f.app.DeleteThread(owner.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
			t.Fatalf("the retried delete = %+v, %v", result, err)
		}
		if !gone(t, f.app, owner.ID) || !gone(t, f.app, worker.ID) {
			t.Error("the retried delete left the family")
		}
		if !exists(t, tree) {
			t.Error("a plain delete removed the worktree")
		}
	})
}

// A delete reads its family once to claim it and once per growth pass of its
// verify, however many threads it has: an unchanged family of 200 is read
// twice, and a spawn its owner admitted before the delete, which names its
// worker while the delete drains that admission, adds one pass.
func TestADeleteReadsItsFamilyOncePerGrowthPass(t *testing.T) {
	t.Parallel()
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "one late spawn"}[late], func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			owner := f.thread(t, "owner")
			ids := []string{owner.ID}
			for i := range 199 {
				worker := f.thread(t, fmt.Sprintf("worker-%03d", i))
				recordSpawn(t, f.app, "tok-"+worker.ID, owner.ID, worker.ID)
				ids = append(ids, worker.ID)
			}
			if late {
				done, err := f.app.admitWork(owner.ID, "")
				if err != nil {
					t.Fatal(err)
				}
				spawned := f.thread(t, "late-worker")
				ids = append(ids, spawned.ID)
				if _, err := f.app.store.AcceptLocalThreadRequest(store.ThreadRequest{
					Token: "tok-late", CallerThreadID: owner.ID, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
				}, store.ThreadRequestReceipt{Token: "tok-late", OwnerDeviceID: "device", Kind: store.ThreadRequestSpawn, SourceThreadID: owner.ID}); err != nil {
					t.Fatal(err)
				}
				named := make(chan error, 1)
				go func() {
					defer done()
					// Name the spawn's thread once the delete waits on it.
					for {
						f.app.resting.mu.Lock()
						closed := f.app.resting.closed[owner.ID] > 0
						f.app.resting.mu.Unlock()
						if closed {
							break
						}
						time.Sleep(time.Millisecond)
					}
					named <- f.app.store.NameLocalThreadRequestTarget("tok-late", spawned.ID, true)
				}()
				t.Cleanup(func() {
					if err := within(t, "the late spawn", named); err != nil {
						t.Errorf("name the late spawn's thread: %v", err)
					}
				})
			}
			var reads atomic.Int32
			f.app.deleteFamilyReadForTest = func() { reads.Add(1) }
			if result, err := f.app.DeleteThread(owner.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("DeleteThread = %+v, %v", result, err)
			}
			if left := remaining(t, f.app, ids); len(left) > 0 {
				t.Fatalf("threads left = %v", left)
			}
			want := int32(2)
			if late {
				want = 3
			}
			if got := reads.Load(); got != want {
				t.Errorf("family reads = %d, want %d", got, want)
			}
		})
	}
}

// A worker running a real turn on a worktree with unsaved work refuses its
// owner's Delete with worktrees: nothing stops, and the turn completes.
func TestARunningWorkerOnADirtyWorktreeRefusesTheDeleteAndFinishes(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	release := f.mockClaudeAfterTheAck(t, "working")
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-b", "main")
	testutil.RunGit(t, repo, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "initial commit")
	tree := filepath.Join(t.TempDir(), "feature")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature", tree)
	project, err := f.app.CreateProject(repo)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7", WorktreePath: tree,
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, owner.ID), threadtools.SpawnCall{Prompt: "work"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitReceiptRunning(t, ack.Token)
	if worker := f.reload(t, ack.ThreadID); worker.WorktreePath != tree {
		t.Fatalf("the worker's worktree = %q, want the owner's %q", worker.WorktreePath, tree)
	}
	dirty(t, tree)

	_, err = f.app.DeleteThreadsAndWorktrees(t.Context(), []string{owner.ID})
	if err == nil || !strings.Contains(err.Error(), "unsaved work") {
		t.Fatalf("DeleteThreadsAndWorktrees = %v, want it refused for the unsaved work", err)
	}
	untouched(t, f.app, owner.ID, ack.ThreadID)
	if _, live := f.app.sessionManager().get(ack.ThreadID); !live {
		t.Fatal("the refused delete stopped the worker")
	}
	release(t, ack)
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptFinished
	})
	if !exists(t, tree) {
		t.Error("the refused delete removed the worktree")
	}
}

// crashFamily is an assistant whose current thread sorts before its helper,
// each with a worker on its own clean worktree, deleted with its worktrees.
type crashFamily struct {
	app        *App
	path, home string
	ids, trees []string
}

func newCrashFamily(t *testing.T) crashFamily {
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
	home := filepath.Join(app.configDir, "agents", "a-agent")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	family := crashFamily{app: app, path: path, home: home, ids: []string{"a-agent", "b-helper", "c-worker", "d-worker"}}
	row := func(id string, apply func(*store.Thread)) {
		thread := store.Thread{
			ID: id, ProjectID: project.ID, Title: id, Provider: string(provider.Claude), Model: "claude-opus-4-7",
			Mode: threadmode.ModeChat, WorkspacePath: repo, RuntimeMode: string(provider.RuntimeApprovalRequired), CreatedAt: 1, UpdatedAt: 1,
		}
		apply(&thread)
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		withMessage(t, app, id)
	}
	worktree := func(branch string) func(*store.Thread) {
		tree := filepath.Join(t.TempDir(), branch)
		testutil.RunGit(t, repo, "worktree", "add", "-b", branch, tree)
		family.trees = append(family.trees, tree)
		return on(tree)
	}
	row("a-agent", func(*store.Thread) {})
	if err := app.store.PlaceThreadInHome("a-agent", home); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateStandingAgent(storeAgent("a-agent", home)); err != nil {
		t.Fatal(err)
	}
	row("b-helper", func(thread *store.Thread) {
		thread.ProjectID, thread.WorkspacePath, thread.HelperOfAgentID = "", home, "a-agent"
	})
	row("c-worker", worktree("c-tree"))
	row("d-worker", worktree("d-tree"))
	recordSpawn(t, app, "tok-c", "a-agent", "c-worker")
	recordSpawn(t, app, "tok-d", "b-helper", "d-worker")
	return family
}

// A delete stopped by a crash at any phase is settled by the next boot. One
// stopped after its claim, before its commit, deleted nothing: the boot
// takes the claims back and every thread, worktree and the assistant's home
// are as they were. One stopped after its commit, before its first destroy
// or between two, is finished by one boot with the policy it committed:
// every thread, both worktrees and the home are gone.
func TestABootSettlesAnInterruptedFamilyDelete(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"after the claim", "after the commit", "between two destroys"} {
		t.Run(crash, func(t *testing.T) {
			t.Parallel()
			family := newCrashFamily(t)
			app := family.app
			switch crash {
			case "after the claim":
				app.deleteClaimedForTest = func([]string) { runtime.Goexit() }
			case "after the commit":
				app.deleteCommittedForTest = func([]string) { runtime.Goexit() }
			default:
				app.deleteDestroyedForTest = func(string) { runtime.Goexit() }
			}
			stopped := make(chan bool, 1)
			go func() {
				returned := false
				defer func() { stopped <- returned }()
				_, _ = app.DeleteThreadsAndWorktrees(context.Background(), []string{"a-agent"})
				returned = true
			}()
			if within(t, "the interrupted delete", stopped) {
				t.Fatal("the delete returned instead of stopping")
			}
			left := remaining(t, app, family.ids)
			if crash == "between two destroys" && len(left) != len(family.ids)-1 {
				t.Fatalf("threads left after the interruption = %v, want one destroyed", left)
			}
			app.appCancel()
			if err := app.store.Close(); err != nil {
				t.Fatal(err)
			}

			boot := newTestAppAtStorePath(t, family.path)
			boot.configDir = app.configDir
			boot.maintenance.firstReadsFallback = time.Millisecond
			t.Cleanup(func() {
				boot.appCancel()
				boot.waitPendingThreadDeletes()
			})
			boot.releaseUndecidedDeletes()
			boot.startPendingThreadDeletes()
			if crash == "after the claim" {
				untouched(t, boot, family.ids...)
				for _, tree := range family.trees {
					if !exists(t, tree) {
						t.Errorf("worktree %s was removed by a delete that never committed", tree)
					}
				}
				if !exists(t, family.home) {
					t.Error("the assistant's home was removed by a delete that never committed")
				}
				if items, err := boot.store.ListItems("a-agent"); err != nil || len(items) != 1 {
					t.Errorf("the assistant's history = %d items, %v", len(items), err)
				}
				return
			}
			waitUntil(t, 10*time.Second, func() bool {
				if len(remaining(t, boot, family.ids)) > 0 || exists(t, family.home) {
					return false
				}
				return !slices.ContainsFunc(family.trees, func(tree string) bool { return exists(t, tree) })
			})
			if _, found, err := boot.store.GetStandingAgent("a-agent"); err != nil || found {
				t.Errorf("the assistant's record after the boot = %v, %v", found, err)
			}
		})
	}
}

// An assistant whose helper's teardown fails keeps its home, its record and
// both threads' history, with both committed and hidden; deleting it again
// finishes them.
func TestAnAssistantWhoseHelperFailsIsFinishedByDeletingAgain(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	home := filepath.Join(f.app.configDir, "agents", "agent")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	current := f.thread(t, "agent")
	if err := f.app.store.PlaceThreadInHome(current.ID, home); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.CreateStandingAgent(storeAgent(current.ID, home)); err != nil {
		t.Fatal(err)
	}
	helper := f.thread(t, "helper", func(thread *store.Thread) {
		thread.ProjectID, thread.WorkspacePath, thread.HelperOfAgentID = "", home, current.ID
	})
	withMessage(t, f.app, current.ID)
	withMessage(t, f.app, helper.ID)
	var failing atomic.Bool
	failing.Store(true)
	stopErr := errors.New("the remote computer did not answer")
	f.app.deletePortsForTest = func(ports *threadapp.DeletePorts) {
		inner := ports.StopRemoteWork
		ports.StopRemoteWork = func(id string) error {
			if id == helper.ID && failing.Load() {
				return stopErr
			}
			if inner != nil {
				return inner(id)
			}
			return nil
		}
	}
	result, err := f.app.DeleteThread(current.ID)
	if err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if len(result.WorkersNotDeleted) != 1 || result.WorkersNotDeleted[0].ThreadID != helper.ID || !strings.Contains(result.WorkersNotDeleted[0].Error, stopErr.Error()) {
		t.Fatalf("not deleted = %+v, want the helper with its reason", result.WorkersNotDeleted)
	}
	if !exists(t, home) {
		t.Error("the home went before the helper")
	}
	if _, found, err := f.app.store.GetStandingAgent(current.ID); err != nil || !found {
		t.Errorf("the assistant's record = %v, %v; want it kept", found, err)
	}
	for _, id := range []string{current.ID, helper.ID} {
		if items, err := f.app.store.ListItems(id); err != nil || len(items) != 1 {
			t.Errorf("%s's history = %d items, %v; want it kept", id, len(items), err)
		}
	}
	pending, err := f.app.store.ListPendingThreadDeletes()
	slices.Sort(pending)
	if err != nil || !slices.Equal(pending, []string{current.ID, helper.ID}) {
		t.Fatalf("committed = %v, %v; want both", pending, err)
	}
	if ids := catalogIDs(t, f.app); slices.Contains(ids, current.ID) || slices.Contains(ids, helper.ID) {
		t.Errorf("catalog = %v, want both hidden", ids)
	}

	failing.Store(false)
	if result, err := f.app.DeleteThread(current.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("deleting again = %+v, %v", result, err)
	}
	if !gone(t, f.app, current.ID) || !gone(t, f.app, helper.ID) || exists(t, home) {
		t.Error("deleting again left the assistant, its helper or its home")
	}
	if _, found, err := f.app.store.GetStandingAgent(current.ID); err != nil || found {
		t.Errorf("the assistant's record after deleting again = %v, %v", found, err)
	}
}

// Two threads selected together on one worktree with a terminal open: the
// confirmation counts the worktree and its terminal once, and one delete
// removes it once. Either thread alone leaves it to the other.
func TestTwoSelectedThreadsOnOneWorktreeAreOneDelete(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	f.app.terminals = terminal.NewManager(nil, nil)
	t.Cleanup(func() { _ = f.app.terminals.Shutdown() })
	tree := f.worktree(t, "shared")
	a := f.thread(t, "a", on(tree))
	b := f.thread(t, "b", on(tree))
	if _, err := f.app.terminals.Open(a.ID, terminal.SessionOptions{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: tree}); err != nil {
		t.Fatal(err)
	}
	alone, err := f.app.GetDeleteDependents([]string{a.ID})
	if err != nil || len(alone.Worktrees) != 0 {
		t.Fatalf("one thread's dependents = %+v, %v; want the worktree left to the other", alone, err)
	}
	got, err := f.app.GetDeleteDependents([]string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := DeleteDependents{Worktrees: []DeleteWorktree{{ProjectID: f.project.ID, WorktreePath: tree, Terminals: 1}}}
	if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("GetDeleteDependents = %s, want %s", gotJSON, wantJSON)
	}
	result, err := f.app.DeleteThreadsAndWorktrees(t.Context(), []string{a.ID, b.ID})
	if err != nil || len(result.WorkersNotDeleted) > 0 || len(result.RetainedWorktrees) > 0 {
		t.Fatalf("DeleteThreadsAndWorktrees = %+v, %v", result, err)
	}
	if !gone(t, f.app, a.ID) || !gone(t, f.app, b.ID) || exists(t, tree) {
		t.Error("the delete left a thread or the worktree")
	}
	if open := f.app.terminals.Matching(func(terminal.SessionSummary) bool { return true }); len(open) != 0 {
		t.Errorf("terminals after the delete = %+v, want none", open)
	}
}
