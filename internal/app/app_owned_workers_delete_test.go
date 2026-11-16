package app

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// Deleting a thread deletes the workers it owns, down the lineage
// (docs/specs/standing-agents.md, "Owned workers"). A spawned thread with a
// sidebar row is the person's and stays.

// workerChain is an owner with three workers in a line under it, the
// middle one archived, a worker it placed in the sidebar, and another
// thread's worker beside it.
type workerChain struct {
	owner, worker, deep, deeper, placed, other, elsewhere string
}

func seedWorkerChain(t *testing.T, f *organizeFixture, workerProject string) workerChain {
	return seedWorkerChainOn(t, f, workerProject, "")
}

// seedWorkerChainOn is seedWorkerChain with the deepest worker on a
// worktree.
func seedWorkerChainOn(t *testing.T, f *organizeFixture, workerProject, deeperWorktree string) workerChain {
	t.Helper()
	chain := workerChain{owner: "owner", worker: "worker", deep: "deep", deeper: "deeper", placed: "placed", other: "other", elsewhere: "elsewhere"}
	f.thread(t, chain.owner)
	for _, id := range []string{chain.worker, chain.deep, chain.deeper, chain.placed, chain.other, chain.elsewhere} {
		f.thread(t, id, func(row *store.Thread) {
			if workerProject != "" {
				row.ProjectID = workerProject
			}
			if id == chain.deeper {
				row.WorktreePath, row.Branch = deeperWorktree, deeperWorktree
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

// gone reports whether a thread's row is gone to every read.
func gone(t *testing.T, app *App, threadID string) bool {
	t.Helper()
	_, err := app.store.GetThread(threadID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	return err != nil
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
		"boot": func(t *testing.T, f *organizeFixture, chain workerChain) {
			unlock := f.app.threadLocks().Lock(chain.owner)
			defer unlock()
			if err := f.app.deleteThreadPacedLocked(chain.owner, func() {}); err != nil {
				t.Fatalf("deleteThreadPacedLocked: %v", err)
			}
		},
		"project removal": func(t *testing.T, f *organizeFixture, chain workerChain) {
			result, err := f.app.DeleteProject(f.project.ID)
			if err != nil {
				t.Fatalf("DeleteProject: %v", err)
			}
			if len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("workers not deleted = %+v", result.WorkersNotDeleted)
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
			if _, err := (threadToolsApp{app: f.app}).Loop(t.Context(), threadtools.Caller{ThreadID: chain.worker},
				threadtools.LoopCall{Label: "nightly", Note: "check", EverySeconds: 3600}); err != nil {
				t.Fatalf("Loop: %v", err)
			}
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

// A worker whose delete is refused is named beside the owner's delete,
// which goes on, and its siblings are deleted. The refused worker is
// standalone in the sidebar once its owner is gone.
func TestAWorkerThatCannotBeDeletedIsReported(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	owner := f.thread(t, "owner")
	stuck := f.thread(t, "stuck")
	sibling := f.thread(t, "sibling")
	withMessage(t, f.app, stuck.ID)
	recordSpawn(t, f.app, "tok-stuck", owner.ID, stuck.ID)
	recordSpawn(t, f.app, "tok-sibling", owner.ID, sibling.ID)
	if _, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: stuck.ID, PeerBackendID: entityid.New(), Kind: "copy", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	f.events.reset()
	result, err := f.app.DeleteThread(owner.ID)
	if err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if len(result.WorkersNotDeleted) != 1 {
		t.Fatalf("workers not deleted = %+v, want the stuck one", result.WorkersNotDeleted)
	}
	failure := result.WorkersNotDeleted[0]
	if failure.ThreadID != stuck.ID || failure.Title != stuck.Title || !strings.Contains(failure.Error, "transfer") {
		t.Errorf("failure = %+v, want the stuck worker titled with its transfer", failure)
	}
	if !gone(t, f.app, owner.ID) || !gone(t, f.app, sibling.ID) {
		t.Error("the owner or its other worker outlived the delete")
	}
	got := f.reload(t, stuck.ID)
	if got.Placement != store.PlacementStandalone || got.SpawnedByThreadID != "" {
		t.Errorf("the stuck worker = placement %s spawner %q, want standalone", got.Placement, got.SpawnedByThreadID)
	}
	placementFrames(t, "delete", f.threadFrames(t), stuck.ID, store.PlacementStandalone)
	if ids := catalogIDs(t, f.app); !slices.Contains(ids, stuck.ID) {
		t.Errorf("catalog = %v, want the stuck worker listed", ids)
	}
	host := &harnessHost{app: f.app}
	other := f.thread(t, "other")
	again := f.thread(t, "again")
	recordSpawn(t, f.app, "tok-again", other.ID, again.ID)
	if _, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: again.ID, PeerBackendID: entityid.New(), Kind: "copy", Direction: "outgoing",
		ActivationHash: strings.Repeat("b", 64), PrivateState: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.DeleteThread(other.ID); err == nil || !strings.Contains(err.Error(), again.ID) {
		t.Errorf("the harness delete = %v, want it to fail naming the worker left standing", err)
	}
}

// A worker given a sidebar row while its owner's delete runs is left
// alone: not deleted, and nothing of it stopped.
func TestAWorkerMovedDuringItsOwnersDeleteIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	owner := f.thread(t, "owner")
	first := f.thread(t, "w-a")
	moved := f.thread(t, "w-b")
	recordSpawn(t, f.app, "tok-a", owner.ID, first.ID)
	recordSpawn(t, f.app, "tok-b", owner.ID, moved.ID)
	runningSession(t, f.app, moved.ID)
	if _, err := (threadToolsApp{app: f.app}).Loop(t.Context(), threadtools.Caller{ThreadID: moved.ID},
		threadtools.LoopCall{Label: "nightly", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop: %v", err)
	}
	// The workers are deleted in id order: once the first is gone, and
	// before the second is reached, the person shows the second in the
	// sidebar.
	var once sync.Once
	var moveErr error
	f.app.testEmitHook = func(channel string, data any) {
		f.events.capture(channel, data)
		frame, ok := data.(triage.ThreadUpdateEvent)
		if channel != eventchan.ThreadUpdated.String() || !ok || frame.Action != triage.ThreadActionDeleted || frame.ID != first.ID {
			return
		}
		once.Do(func() {
			_, moveErr = f.app.SetThreadPlacement([]string{moved.ID}, store.PlacementStandalone)
		})
	}
	result, err := f.app.DeleteThread(owner.ID)
	if err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("DeleteThread = %+v, %v", result, err)
	}
	if moveErr != nil {
		t.Fatalf("move the worker: %v", moveErr)
	}
	if !gone(t, f.app, first.ID) {
		t.Error("the first worker outlived the delete")
	}
	if gone(t, f.app, moved.ID) {
		t.Fatal("the worker moved to the sidebar was deleted")
	}
	if _, live := f.app.sessionManager().get(moved.ID); !live {
		t.Error("the moved worker's session was stopped")
	}
	if wakes, err := f.app.store.ListAgentWakes(moved.ID); err != nil || len(wakes) != 1 || wakes[0].State != store.AgentWakeArmed {
		t.Errorf("the moved worker's loop = %+v, %v; want it armed", wakes, err)
	}
}

// A project removal refuses while a worker of one of its threads, in
// another project, is running: the thread's delete would delete it.
func TestAProjectRemovalRefusesWhileAWorkerOutsideRuns(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	other, err := f.app.store.CreateProject(store.Project{ID: entityid.New(), Path: t.TempDir(), Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	owner := f.thread(t, "owner")
	worker := f.thread(t, "worker", func(row *store.Thread) { row.ProjectID = other.ID })
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	runningSession(t, f.app, worker.ID)
	if _, err := f.app.DeleteProject(f.project.ID); err == nil || !strings.Contains(err.Error(), "worker "+worker.ID) {
		t.Fatalf("DeleteProject = %v, want it refused naming the running worker", err)
	}
	if gone(t, f.app, owner.ID) || gone(t, f.app, worker.ID) {
		t.Error("a refused project removal deleted a thread")
	}
}

// What a delete confirmation counts: the workers under the thread at any
// depth, archived ones included, and the worktrees they are on.
func TestGetDeleteDependentsCountsTheWorkers(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	chain := seedWorkerChainOn(t, f, "", "/wt/deeper")
	got, err := f.app.GetDeleteDependents(chain.owner)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workers != 3 || got.PastThreads != 0 || got.Helpers != 0 {
		t.Errorf("dependents = %+v, want three workers", got)
	}
	if len(got.WorkerWorktrees) != 1 || got.WorkerWorktrees[0].ProjectID != f.project.ID || got.WorkerWorktrees[0].WorktreePath != "/wt/deeper" || !slices.Equal(got.WorkerWorktrees[0].ThreadIDs, []string{chain.deeper}) {
		t.Errorf("worker worktrees = %+v", got.WorkerWorktrees)
	}
	if got, err := f.app.GetDeleteDependents(chain.placed); err != nil || got.Workers != 0 || len(got.WorkerWorktrees) != 0 {
		t.Errorf("a thread with no workers = %+v, %v", got, err)
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
// by the removal's own delete of it, not by waiting on the lock the
// removal holds.
func TestAProjectRemovalDeletesAWorkerInsideOnce(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	owner := f.thread(t, "a-owner")
	worker := f.thread(t, "b-worker")
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	done := make(chan error, 1)
	go func() {
		_, err := f.app.DeleteProject(f.project.ID)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the project removal waited on a lock it holds")
	}
	if !gone(t, f.app, owner.ID) || !gone(t, f.app, worker.ID) {
		t.Error("a thread of the removed project outlived it")
	}
}
