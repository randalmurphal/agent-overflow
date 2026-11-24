package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// Deleting a thread deletes the workers it owns, down the lineage, as one
// delete (docs/specs/standing-agents.md, "Owned workers"; familyDelete). A
// spawned thread with a sidebar row is the person's and stays.

// workerChain is an owner with three workers in a line under it, the
// middle one archived, a worker it placed in the sidebar, and another
// thread's worker beside it.
type workerChain struct {
	owner, worker, deep, deeper, placed, other, elsewhere string
}

func seedWorkerChain(t *testing.T, f *organizeFixture, workerProject string) workerChain {
	t.Helper()
	chain := workerChain{owner: "owner", worker: "worker", deep: "deep", deeper: "deeper", placed: "placed", other: "other", elsewhere: "elsewhere"}
	f.thread(t, chain.owner)
	for _, id := range []string{chain.worker, chain.deep, chain.deeper, chain.placed, chain.other, chain.elsewhere} {
		f.thread(t, id, func(row *store.Thread) {
			if workerProject != "" {
				row.ProjectID = workerProject
			}
		})
	}
	recordSpawn(t, f.app, "tok-worker", chain.owner, chain.worker)
	recordSpawn(t, f.app, "tok-deep", chain.worker, chain.deep)
	recordSpawn(t, f.app, "tok-deeper", chain.deep, chain.deeper)
	recordSpawn(t, f.app, "tok-placed", chain.owner, chain.placed)
	recordSpawn(t, f.app, "tok-elsewhere", chain.other, chain.elsewhere)
	if _, err := f.app.SetThreadPlacement([]string{chain.placed}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	withMessage(t, f.app, chain.placed)
	if _, _, err := f.app.threadApplication().Archive(chain.deep); err != nil {
		t.Fatalf("archive the middle worker: %v", err)
	}
	return chain
}

// newDeleteFixture is the organize fixture with a router, which a running
// worker's turn needs.
func newDeleteFixture(t *testing.T) *organizeFixture {
	t.Helper()
	f := newOrganizeFixture(t)
	f.app.triage = triage.NewRouter(f.app.store, f.events.captureChannel)
	return f
}

// gone reports whether a thread's row is dropped.
func gone(t *testing.T, app *App, threadID string) bool {
	t.Helper()
	_, err := app.store.GetThread(threadID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	return err != nil
}

// untouched fails unless every thread named is present, unclaimed and
// uncommitted.
func untouched(t *testing.T, app *App, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if gone(t, app, id) {
			t.Errorf("%s was deleted", id)
		} else if err := app.store.CheckThreadNotDeleting(id); err != nil {
			t.Errorf("%s is still a delete's: %v", id, err)
		}
	}
	if pending, err := app.store.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Errorf("claims left = %v, %v; want none", pending, err)
	}
}

func deletedFrames(frames []triage.ThreadUpdateEvent) []string {
	ids := []string{}
	for _, frame := range frames {
		if frame.Action == triage.ThreadActionDeleted {
			ids = append(ids, frame.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// armLoop gives threadID an armed loop, a wake its delete must take.
func armLoop(t *testing.T, app *App, threadID string) {
	t.Helper()
	if _, err := (threadToolsApp{app: app}).Loop(t.Context(), threadtools.Caller{ThreadID: threadID},
		threadtools.LoopCall{Label: "nightly", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop: %v", err)
	}
}

// startTransfer puts threadID in an outgoing transfer of kind.
func startTransfer(t *testing.T, app *App, threadID, kind string) {
	t.Helper()
	if _, err := app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: threadID, PeerBackendID: entityid.New(), Kind: kind, Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
}

// within fails the test unless done delivers before the timeout.
func within[T any](t *testing.T, what string, done <-chan T) T {
	t.Helper()
	select {
	case value := <-done:
		return value
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never finished", what)
		var zero T
		return zero
	}
}

// Every door that deletes a thread deletes its workers at any depth,
// archived ones included, each as an ordinary delete: a running worker's
// session stops, its loop and queued message go with it, and nothing of it
// starts again. Its sidebar-placed spawn and another thread's worker stay.
func TestDeletingAThreadDeletesItsWorkers(t *testing.T) {
	t.Parallel()
	doors := map[string]func(t *testing.T, f *organizeFixture, chain workerChain){
		"binding": func(t *testing.T, f *organizeFixture, chain workerChain) {
			result, err := f.app.DeleteThread(chain.owner)
			if err != nil {
				t.Fatalf("DeleteThread: %v", err)
			}
			if len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("workers not deleted = %+v", result.WorkersNotDeleted)
			}
		},
		"the sidebar": func(t *testing.T, f *organizeFixture, chain workerChain) {
			result, err := f.app.DeleteThreads([]string{chain.owner})
			if err != nil {
				t.Fatalf("DeleteThreads: %v", err)
			}
			if len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("workers not deleted = %+v", result.WorkersNotDeleted)
			}
		},
		"retention": func(t *testing.T, f *organizeFixture, chain workerChain) {
			if err := f.app.deleteThreadPaced(chain.owner, func() {}); err != nil {
				t.Fatalf("deleteThreadPaced: %v", err)
			}
		},
		"project removal": func(t *testing.T, f *organizeFixture, chain workerChain) {
			if _, err := f.app.DeleteProject(f.project.ID); err != nil {
				t.Fatalf("DeleteProject: %v", err)
			}
		},
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			// Project removal deletes the project's own threads anyway: its
			// workers live in another project, which only the cascade reaches.
			workerProject := ""
			if name == "project removal" {
				other, err := f.app.store.CreateProject(store.Project{ID: entityid.New(), Path: t.TempDir(), Name: "Elsewhere"})
				if err != nil {
					t.Fatal(err)
				}
				workerProject = other.ID
			}
			chain := seedWorkerChain(t, f, workerProject)
			if name != "project removal" {
				// A project removal refuses while a worker runs.
				runningSession(t, f.app, chain.worker)
			}
			armLoop(t, f.app, chain.worker)
			if _, err := f.app.registerQueueItem(chain.worker, "later", SendMessageOptions{SendID: "send-later"},
				injectedQueueOptions{preserveDraft: true}); err != nil {
				t.Fatalf("queue a message: %v", err)
			}

			f.events.reset()
			door(t, f, chain)

			for _, id := range []string{chain.owner, chain.worker, chain.deep, chain.deeper} {
				if !gone(t, f.app, id) {
					t.Errorf("%s outlived the delete", id)
				}
			}
			for _, id := range []string{chain.placed, chain.other, chain.elsewhere} {
				if gone(t, f.app, id) {
					t.Errorf("%s was deleted", id)
				}
			}
			if got := f.reload(t, chain.placed); got.Placement != store.PlacementStandalone || got.SpawnedByThreadID != "" {
				t.Errorf("placed spawn = placement %s spawner %q, want standalone and nobody's", got.Placement, got.SpawnedByThreadID)
			}
			if got := f.reload(t, chain.elsewhere); got.Placement != store.PlacementOwned || got.SpawnedByThreadID != chain.other {
				t.Errorf("another thread's worker = placement %s spawner %q", got.Placement, got.SpawnedByThreadID)
			}
			want := []string{chain.deep, chain.deeper, chain.owner, chain.worker}
			if got := deletedFrames(f.threadFrames(t)); !slices.Equal(got, want) {
				t.Errorf("deleted frames = %v, want %v", got, want)
			}
			if wakes, err := f.app.store.ListAgentWakes(chain.worker); err != nil || len(wakes) != 0 {
				t.Errorf("the worker's loops after its delete = %+v, %v", wakes, err)
			}
			if pending, err := f.app.store.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
				t.Errorf("claims left = %v, %v; want none", pending, err)
			}
			// Nothing the delete settled starts a session again.
			f.app.threadRequestSettling.Wait()
			f.app.agentNoticeWork.wg.Wait()
			for _, id := range []string{chain.owner, chain.worker, chain.deep, chain.deeper} {
				if _, live := f.app.sessionManager().get(id); live {
					t.Errorf("%s holds a session after its delete", id)
				}
			}
			if got := f.app.sessionManager().snapshot(); len(got) != 0 {
				t.Errorf("sessions after the delete = %v, want none", got)
			}
		})
	}
}

// P owns W owns G. W moved to the sidebar before the delete claims P's
// family is the person's: W, its session, its loop and G are untouched. A
// move once the family is claimed is refused, and W and G go with P.
func TestAWorkerIsTheDeletesOnlyFromItsClaim(t *testing.T) {
	t.Parallel()
	for _, move := range []string{"before the claim", "after the claim"} {
		t.Run("move "+move, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			p, w, g := f.thread(t, "p"), f.thread(t, "w"), f.thread(t, "g")
			recordSpawn(t, f.app, "tok-w", p.ID, w.ID)
			recordSpawn(t, f.app, "tok-g", w.ID, g.ID)
			runningSession(t, f.app, w.ID)
			armLoop(t, f.app, w.ID)
			var moveErr error
			if move == "before the claim" {
				_, moveErr = f.app.SetThreadPlacement([]string{w.ID}, store.PlacementStandalone)
			} else {
				f.app.deleteClaimedForTest = func([]string) {
					_, moveErr = f.app.SetThreadPlacement([]string{w.ID}, store.PlacementStandalone)
				}
			}
			result, err := f.app.DeleteThread(p.ID)
			if err != nil || len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("DeleteThread = %+v, %v; want a complete delete", result, err)
			}
			if !gone(t, f.app, p.ID) {
				t.Error("P outlived the delete")
			}
			if move == "after the claim" {
				if !errors.Is(moveErr, store.ErrThreadDeleting) {
					t.Errorf("a move after the claim = %v, want store.ErrThreadDeleting", moveErr)
				}
				if !gone(t, f.app, w.ID) || !gone(t, f.app, g.ID) {
					t.Error("W or G outlived the delete")
				}
				return
			}
			if moveErr != nil {
				t.Fatalf("a move before the claim: %v", moveErr)
			}
			untouched(t, f.app, w.ID, g.ID)
			if got := f.reload(t, w.ID); got.Placement != store.PlacementStandalone {
				t.Errorf("W = placement %s, want standalone", got.Placement)
			}
			if got := f.reload(t, g.ID); got.Placement != store.PlacementOwned || got.SpawnedByThreadID != w.ID {
				t.Errorf("G = placement %s spawner %q, want still W's", got.Placement, got.SpawnedByThreadID)
			}
			if _, live := f.app.sessionManager().get(w.ID); !live {
				t.Error("W's session was stopped")
			}
			if wakes, err := f.app.store.ListAgentWakes(w.ID); err != nil || len(wakes) != 1 || wakes[0].State != store.AgentWakeArmed {
				t.Errorf("W's loop = %+v, %v; want it armed", wakes, err)
			}
		})
	}
}

// A delete that something under the thread refuses deletes nothing: a
// worker in a transfer refuses it, and every claim is taken back. While the
// family is claimed a client's catalog read still lists its threads, and
// none is announced deleted. A transfer that would start once the family is
// claimed is refused instead, and the delete goes ahead.
func TestARefusedDeleteDeletesNothing(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the delete", "after the claim"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			owner, stuck, sibling := f.thread(t, "owner"), f.thread(t, "stuck"), f.thread(t, "sibling")
			withMessage(t, f.app, owner.ID)
			recordSpawn(t, f.app, "tok-stuck", owner.ID, stuck.ID)
			recordSpawn(t, f.app, "tok-sibling", owner.ID, sibling.ID)
			runningSession(t, f.app, owner.ID)
			var listed []string
			var transferErr error
			f.app.deleteClaimedForTest = func([]string) {
				listed = catalogIDs(t, f.app)
				if when == "after the claim" {
					_, transferErr = f.app.store.CreateThreadTransfer(store.ThreadTransfer{
						ID: entityid.New(), ThreadID: stuck.ID, PeerBackendID: entityid.New(), Kind: "copy", Direction: "outgoing",
						ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
					})
				}
			}
			if when == "before the delete" {
				startTransfer(t, f.app, stuck.ID, "copy")
			}
			f.events.reset()
			_, err := f.app.DeleteThread(owner.ID)
			if !slices.Contains(listed, owner.ID) {
				t.Errorf("catalog while the family is claimed = %v, want the owner listed", listed)
			}
			if when == "after the claim" {
				if !errors.Is(transferErr, store.ErrThreadDeleting) {
					t.Fatalf("a transfer of a claimed worker = %v, want store.ErrThreadDeleting", transferErr)
				}
				if err != nil || !gone(t, f.app, owner.ID) || !gone(t, f.app, stuck.ID) {
					t.Fatalf("DeleteThread = %v; want the family deleted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "thread "+stuck.ID+", which works under the thread being deleted, is being transferred") {
				t.Fatalf("DeleteThread = %v, want it refused naming the worker", err)
			}
			untouched(t, f.app, owner.ID, stuck.ID, sibling.ID)
			if !slices.Contains(catalogIDs(t, f.app), owner.ID) {
				t.Error("the owner left the catalog after a refused delete")
			}
			if _, live := f.app.sessionManager().get(owner.ID); !live {
				t.Error("a refused delete stopped the owner's session")
			}
			if got := deletedFrames(f.threadFrames(t)); len(got) != 0 {
				t.Errorf("deleted frames = %v, want none", got)
			}
		})
	}
}

// An assistant's delete is refused, with nothing deleted, while a thread
// under it is in a transfer: here a helper, beside a helper the assistant
// also spawned and project work it owns.
func TestARefusedAssistantDeleteKeepsItsFamily(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the delete"} {
		t.Run(when, func(t *testing.T) {
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
			helper := func(id string) {
				f.thread(t, id, func(thread *store.Thread) {
					thread.ProjectID, thread.WorkspacePath, thread.HelperOfAgentID = "", home, current.ID
				})
			}
			helper("helper-moving")
			helper("helper-spawned")
			if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
				Token: "tok-helper", CallerThreadID: current.ID, Kind: store.ThreadRequestSpawn,
				TargetThreadID: "helper-spawned", State: store.ThreadRequestFinished,
				Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: 2, CreatedAt: 1, UpdatedAt: 2,
			}); err != nil {
				t.Fatal(err)
			}
			f.thread(t, "work")
			recordSpawn(t, f.app, "tok-work", current.ID, "work")
			startTransfer(t, f.app, "helper-moving", "move")
			err := f.app.deleteThread(current.ID)
			if err == nil || !strings.Contains(err.Error(), "thread helper-moving, which lives under this assistant, is being transferred") {
				t.Fatalf("DeleteThread = %v, want the helper's transfer named", err)
			}
			untouched(t, f.app, current.ID, "helper-moving", "helper-spawned", "work")
			if _, err := os.Stat(home); err != nil {
				t.Errorf("the home after a refused delete: %v", err)
			}
		})
	}
}

// DeleteThread(Z) and the removal of Z's project cross: the removal starts
// while the delete holds Z's family, and is refused with nothing claimed;
// the delete finishes, and the removal then removes the project. No delete
// holds one lock while it takes another.
func TestADeleteAndAProjectRemovalCrossing(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	z, a := f.thread(t, "z-owner"), f.thread(t, "a-worker")
	recordSpawn(t, f.app, "tok-a", z.ID, a.ID)
	var removalErr error
	var crossing sync.Once
	f.app.deleteVerifiedForTest = func(string) {
		crossing.Do(func() { _, removalErr = f.app.DeleteProject(f.project.ID) })
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := f.app.DeleteThread(z.ID)
		deleted <- err
	}()
	if err := within(t, "DeleteThread(Z)", deleted); err != nil {
		t.Errorf("DeleteThread(Z): %v", err)
	}
	if !errors.Is(removalErr, store.ErrThreadDeleting) {
		t.Errorf("the crossing removal = %v, want store.ErrThreadDeleting", removalErr)
	}
	if !gone(t, f.app, z.ID) || !gone(t, f.app, a.ID) {
		t.Error("Z or A outlived the delete")
	}
	if _, err := f.app.DeleteProject(f.project.ID); err != nil {
		t.Errorf("DeleteProject once the delete ended: %v", err)
	}
	if pending, err := f.app.store.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Errorf("claims left = %v, %v; want none", pending, err)
	}
}

// A spawn admitted before its owner's delete, held after its thread is made
// and before it names it, finishes before the delete reads the owner's
// workers, so the new worker goes with the owner: no worker of a deleted
// thread survives it. Without the wait, the delete would read no worker,
// the spawn would name its thread while the owner's rows drain, and the
// worker would outlive the owner, standalone.
func TestASpawnUnderWayIsDeletedWithItsOwner(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working on it")
	// Enough items that the owner's drain pauses before its row drops.
	for i := range 600 {
		if err := f.app.store.InsertItem(store.Item{
			ID: fmt.Sprintf("owner-%d", i), ThreadID: f.caller.ID, TurnIndex: i / 10, ItemIndex: i % 10,
			Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "row",
		}); err != nil {
			t.Fatal(err)
		}
	}
	held, release := make(chan struct{}), make(chan struct{})
	var hold, free sync.Once
	f.app.spawnNamingForTest = func(id string) {
		if id == f.caller.ID {
			hold.Do(func() {
				close(held)
				<-release
			})
		}
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "work"})
		spawned <- err
	}()
	within(t, "the spawn's hold", held)
	var draining atomic.Bool
	spawnDone := make(chan struct{})
	deleted := make(chan error, 1)
	go func() {
		deleted <- f.app.deleteThreadPaced(f.caller.ID, func() {
			draining.Store(true)
			select {
			case <-spawnDone:
			case <-time.After(30 * time.Second):
			}
		})
	}()
	// The delete waits on the admission under way, or, were it not to,
	// reaches the owner's drain first.
	waitUntil(t, 10*time.Second, func() bool {
		f.app.resting.mu.Lock()
		defer f.app.resting.mu.Unlock()
		return f.app.resting.closed[f.caller.ID] > 0 || draining.Load()
	})
	free.Do(func() { close(release) })
	if err := within(t, "the spawn", spawned); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	close(spawnDone)
	if err := within(t, "the delete", deleted); err != nil {
		t.Fatalf("delete: %v", err)
	}
	f.app.threadRequestSettling.Wait()
	f.app.agentNoticeWork.wg.Wait()
	rows, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("threads after the owner's delete = %d (%s, placement %s), want none", len(rows), rows[0].ID, rows[0].Placement)
	}
	if got := f.app.sessionManager().snapshot(); len(got) != 0 {
		t.Errorf("sessions after the delete = %v, want none", got)
	}
}

// remaining lists the threads of ids still stored, claimed or not.
func remaining(t *testing.T, app *App, ids []string) []string {
	t.Helper()
	pending, err := app.store.ListPendingThreadDeletes()
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, id := range ids {
		if !gone(t, app, id) || slices.Contains(pending, id) {
			left = append(left, id)
		}
	}
	return left
}

// deleteWorktreeFixture is a git project with linked worktrees for threads
// to be on.
type deleteWorktreeFixture struct {
	*organizeFixture
}

func newDeleteWorktreeFixture(t *testing.T) deleteWorktreeFixture {
	t.Helper()
	f := newDeleteFixture(t)
	f.gitProject(t, "first")
	return deleteWorktreeFixture{f}
}

// worktree adds a linked worktree on branch.
func (f deleteWorktreeFixture) worktree(t *testing.T, branch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), branch)
	testutil.RunGit(t, f.project.Path, "worktree", "add", "-b", branch, path)
	return path
}

// on is a thread option putting the thread on worktree.
func on(worktree string) func(*store.Thread) {
	return func(row *store.Thread) {
		row.WorkspacePath, row.WorktreePath, row.Branch = worktree, worktree, filepath.Base(worktree)
	}
}

// dirty leaves unsaved work in tree.
func dirty(t *testing.T, tree string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tree, "unsaved.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// What a delete confirmation counts is what the delete takes: an
// assistant's past thread and the worker that past thread spawned. The
// delete closes the worker's terminals and keeps the worktree it was on.
func TestTheDeleteConfirmationCountsWhatTheDeleteTakes(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	f.app.terminals = terminal.NewManager(nil, nil)
	t.Cleanup(func() { _ = f.app.terminals.Shutdown() })
	tree := f.worktree(t, "past-work")
	home := filepath.Join(f.app.configDir, "agents", "agent")
	past := f.thread(t, "agent")
	if err := f.app.store.PlaceThreadInHome(past.ID, home); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.CreateStandingAgent(storeAgent(past.ID, home)); err != nil {
		t.Fatal(err)
	}
	current := f.thread(t, "agent-next")
	if err := f.app.store.PlaceThreadInHome(current.ID, home); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.HandOffAssistant(past.ID, current.ID, 1); err != nil {
		t.Fatal(err)
	}
	// A handoff moves the old thread's spawns to the new one; this one the
	// past thread made after it, finishing its last turn.
	worker := f.thread(t, "past-worker", on(tree))
	recordSpawn(t, f.app, "tok-past-worker", past.ID, worker.ID)
	for range 2 {
		if _, err := f.app.terminals.Open(worker.ID, terminal.SessionOptions{Reaper: procutiltest.Reaper(), Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: tree}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.app.GetDeleteDependents([]string{current.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := DeleteDependents{PastThreads: 1, Workers: 1}
	if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("GetDeleteDependents = %s, want %s", gotJSON, wantJSON)
	}
	if result, err := f.app.DeleteThreads([]string{current.ID}); err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("DeleteThreads = %+v, %v", result, err)
	}
	for _, id := range []string{current.ID, past.ID, worker.ID} {
		if !gone(t, f.app, id) {
			t.Errorf("%s outlived the delete", id)
		}
	}
	if !exists(t, tree) {
		t.Error("the delete removed the worktree the past thread's worker was on")
	}
	if open := f.app.terminals.Matching(func(terminal.SessionSummary) bool { return true }); len(open) != 0 {
		t.Errorf("terminals after the delete = %+v, want none", open)
	}
}

// What a delete confirmation counts: the workers under the thread at any
// depth, archived ones included.
func TestGetDeleteDependentsCountsTheWorkers(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	chain := seedWorkerChain(t, f, "")
	got, err := f.app.GetDeleteDependents([]string{chain.owner})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workers != 3 || got.PastThreads != 0 || got.Helpers != 0 {
		t.Errorf("dependents = %+v, want three workers", got)
	}
	if got, err := f.app.GetDeleteDependents([]string{chain.placed}); err != nil || got.Workers != 0 {
		t.Errorf("a thread with no workers = %+v, %v", got, err)
	}
}

// A project removal refuses while a worker of one of its threads, in
// another project, is running, and the worker keeps running: whether it ran
// before the removal or its turn, admitted before the claim, reached its
// start after it. A send to the worker once the removal claimed it is
// refused.
func TestAProjectRemovalRefusesWhileAWorkerOutsideRuns(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the removal", "after the claim"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			other, err := f.app.store.CreateProject(store.Project{ID: entityid.New(), Path: t.TempDir(), Name: "Elsewhere"})
			if err != nil {
				t.Fatal(err)
			}
			owner := f.thread(t, "owner")
			worker := f.thread(t, "worker", func(row *store.Thread) { row.ProjectID = other.ID })
			recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
			var sendErr error
			if when == "before the removal" {
				runningSession(t, f.app, worker.ID)
			} else {
				f.app.deleteClaimedForTest = func([]string) {
					_, sendErr = f.app.SendMessageWithOptions(context.Background(), worker.ID, "more", pageSendOptions(uuid.NewString()))
					runningSession(t, f.app, worker.ID)
				}
			}
			if _, err := f.app.DeleteProject(f.project.ID); err == nil || !strings.Contains(err.Error(), "cannot delete project while worker "+worker.ID+" of thread "+owner.ID+" is running") {
				t.Fatalf("DeleteProject = %v, want it refused naming the running worker", err)
			}
			if when == "after the claim" && !errors.Is(sendErr, store.ErrThreadDeleting) {
				t.Errorf("a send to the claimed worker = %v, want store.ErrThreadDeleting", sendErr)
			}
			untouched(t, f.app, owner.ID, worker.ID)
			if _, live := f.app.sessionManager().get(worker.ID); !live {
				t.Error("the refused removal stopped the worker")
			}
		})
	}
}

// A worker running a real turn for its owner's open spawn is stopped and
// deleted with the owner: its session ends, the settlement of the spawn it
// was answering wakes nobody, and no session starts again for either.
func TestDeletingAnOwnerStopsItsRunningWorker(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working on it")
	ack := f.runningRequest(t, "keep at it")
	if _, live := f.app.sessionManager().get(ack.ThreadID); !live {
		t.Fatal("the worker runs no session")
	}
	result, err := f.app.DeleteThread(f.caller.ID)
	if err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("DeleteThread = %+v, %v", result, err)
	}
	for _, id := range []string{f.caller.ID, ack.ThreadID} {
		if !gone(t, f.app, id) {
			t.Errorf("%s outlived the delete", id)
		}
	}
	f.app.threadRequestSettling.Wait()
	f.app.agentNoticeWork.wg.Wait()
	if got := f.app.sessionManager().snapshot(); len(got) != 0 {
		t.Errorf("sessions after the delete = %v, want none", got)
	}
	f.app.agentNoticeWork.mu.Lock()
	failing := len(f.app.agentNoticeWork.failing)
	f.app.agentNoticeWork.mu.Unlock()
	if failing != 0 {
		t.Errorf("%d threads were told a session would not start for them", failing)
	}
}

// A project removal deletes a thread's worker in the same project once,
// as one of the project's own threads.
func TestAProjectRemovalDeletesAWorkerInsideOnce(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	owner := f.thread(t, "a-owner")
	worker := f.thread(t, "b-worker")
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	f.events.reset()
	done := make(chan error, 1)
	go func() {
		_, err := f.app.DeleteProject(f.project.ID)
		done <- err
	}()
	if err := within(t, "the project removal", done); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if !gone(t, f.app, owner.ID) || !gone(t, f.app, worker.ID) {
		t.Error("a thread of the removed project outlived it")
	}
	if got := deletedFrames(f.threadFrames(t)); !slices.Equal(got, []string{owner.ID, worker.ID}) {
		t.Errorf("deleted frames = %v, want each thread once", got)
	}
}
