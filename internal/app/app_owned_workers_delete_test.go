package app

import (
	"database/sql"
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
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
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

// gone reports whether a thread's row is gone to every read: dropped, or
// claimed by a delete.
func gone(t *testing.T, app *App, threadID string) bool {
	t.Helper()
	_, err := app.store.GetThread(threadID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	return err != nil
}

// untouched fails unless every thread named is present and unclaimed.
func untouched(t *testing.T, app *App, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if gone(t, app, id) {
			t.Errorf("%s was deleted or claimed", id)
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
		"with worktrees": func(t *testing.T, f *organizeFixture, chain workerChain) {
			result, err := f.app.DeleteThreadAndWorktrees(t.Context(), chain.owner)
			if err != nil {
				t.Fatalf("DeleteThreadAndWorktrees: %v", err)
			}
			if len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("workers not deleted = %+v", result.WorkersNotDeleted)
			}
		},
		"boot": func(t *testing.T, f *organizeFixture, chain workerChain) {
			if err := f.app.deleteThreadPaced(chain.owner, func() {}); err != nil {
				t.Fatalf("deleteThreadPaced: %v", err)
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

// P owns W owns G. W moved to the sidebar after P's claim and before W's,
// whether before the delete read P's workers or after, is the person's: W, its session, its loop and G are untouched, and the
// delete reports nothing. A move after W's claim is refused, and W and G
// go with P.
func TestAWorkerIsTheDeletesOnlyFromItsClaim(t *testing.T) {
	t.Parallel()
	for _, moveAfter := range []string{"P", "a sibling", "W"} {
		t.Run("move after "+moveAfter+"'s claim", func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			p, w, g := f.thread(t, "p"), f.thread(t, "w"), f.thread(t, "g")
			// The sibling sorts before W: both are read in one pass, and the
			// sibling's claim comes first, so a move after it is fenced by
			// W's claim alone.
			sibling := f.thread(t, "a-sibling")
			recordSpawn(t, f.app, "tok-sibling", p.ID, sibling.ID)
			recordSpawn(t, f.app, "tok-w", p.ID, w.ID)
			recordSpawn(t, f.app, "tok-g", w.ID, g.ID)
			runningSession(t, f.app, w.ID)
			armLoop(t, f.app, w.ID)
			at := map[string]string{"P": p.ID, "a sibling": sibling.ID, "W": w.ID}[moveAfter]
			var moveErr error
			var once sync.Once
			f.app.deleteClaimedForTest = func(id string) {
				if id == at {
					once.Do(func() { _, moveErr = f.app.SetThreadPlacement([]string{w.ID}, store.PlacementStandalone) })
				}
			}
			result, err := f.app.DeleteThread(p.ID)
			if err != nil || len(result.WorkersNotDeleted) > 0 {
				t.Fatalf("DeleteThread = %+v, %v; want a complete delete", result, err)
			}
			if !gone(t, f.app, p.ID) || !gone(t, f.app, sibling.ID) {
				t.Error("P or its other worker outlived the delete")
			}
			if moveAfter == "W" {
				if !errors.Is(moveErr, store.ErrThreadDeleting) {
					t.Errorf("a move after W's claim = %v, want store.ErrThreadDeleting", moveErr)
				}
				if !gone(t, f.app, w.ID) || !gone(t, f.app, g.ID) {
					t.Error("W or G outlived the delete")
				}
				return
			}
			if moveErr != nil {
				t.Fatalf("a move before W's claim: %v", moveErr)
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
// worker in a transfer refuses it whether the transfer began before the
// delete or between the owner's claim and the worker's, and the owner's
// claim is taken back.
func TestARefusedDeleteDeletesNothing(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the delete", "after the owner's claim"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			owner, stuck, sibling := f.thread(t, "owner"), f.thread(t, "stuck"), f.thread(t, "sibling")
			recordSpawn(t, f.app, "tok-stuck", owner.ID, stuck.ID)
			recordSpawn(t, f.app, "tok-sibling", owner.ID, sibling.ID)
			runningSession(t, f.app, owner.ID)
			if when == "before the delete" {
				startTransfer(t, f.app, stuck.ID, "copy")
			} else {
				f.app.deleteClaimedForTest = func(id string) {
					if id == owner.ID {
						startTransfer(t, f.app, stuck.ID, "copy")
					}
				}
			}
			f.events.reset()
			_, err := f.app.DeleteThread(owner.ID)
			if err == nil || !strings.Contains(err.Error(), "thread "+stuck.ID+", which works under the thread being deleted, is being transferred") {
				t.Fatalf("DeleteThread = %v, want it refused naming the worker", err)
			}
			untouched(t, f.app, owner.ID, stuck.ID, sibling.ID)
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
	for _, when := range []string{"before the delete", "after the assistant's claim"} {
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
			if when == "before the delete" {
				startTransfer(t, f.app, "helper-moving", "move")
			} else {
				f.app.deleteClaimedForTest = func(id string) {
					if id == current.ID {
						startTransfer(t, f.app, "helper-moving", "move")
					}
				}
			}
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

// DeleteThread(Z) and the removal of Z's project cross where the old
// cascade deadlocked: Z owns A, A sorts first, the delete has claimed Z and
// is about to claim A, and the removal holds A's lock and wants Z's. Both
// finish, because no delete holds one lock while it takes another.
func TestADeleteAndAProjectRemovalCrossing(t *testing.T) {
	t.Parallel()
	f := newDeleteFixture(t)
	z, a := f.thread(t, "z-owner"), f.thread(t, "a-worker")
	recordSpawn(t, f.app, "tok-a", z.ID, a.ID)
	removalHoldsA := make(chan struct{})
	var holds sync.Once
	f.app.projectLockedForTest = func(id string) {
		if id == a.ID {
			holds.Do(func() { close(removalHoldsA) })
		}
	}
	removed := make(chan error, 1)
	var crossing sync.Once
	f.app.deleteClaimedForTest = func(id string) {
		if id != z.ID {
			return
		}
		crossing.Do(func() {
			go func() {
				_, err := f.app.DeleteProject(f.project.ID)
				removed <- err
			}()
			within(t, "the removal's lock on A", removalHoldsA)
		})
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := f.app.DeleteThread(z.ID)
		deleted <- err
	}()
	if err := within(t, "DeleteThread(Z)", deleted); err != nil {
		t.Errorf("DeleteThread(Z): %v", err)
	}
	if err := within(t, "DeleteProject", removed); err != nil {
		t.Errorf("DeleteProject: %v", err)
	}
	if !gone(t, f.app, z.ID) || !gone(t, f.app, a.ID) {
		t.Error("Z or A outlived both deletes")
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

// A delete interrupted, as a crash interrupts it, with the database closed
// and opened again, is finished by the boot with everything under the
// thread: stopped after the owner's claim, before any worker was claimed,
// and stopped between two workers.
func TestABootFinishesAnInterruptedFamilyDelete(t *testing.T) {
	t.Parallel()
	for _, crash := range []string{"after the owner's claim", "between two workers"} {
		t.Run(crash, func(t *testing.T) {
			t.Parallel()
			app, path := newTestAppWithStorePath(t)
			ids := []string{"p", "w1", "w2", "g"}
			for _, id := range ids {
				if err := app.store.CreateThread(testThread(id)); err != nil {
					t.Fatal(err)
				}
			}
			recordSpawn(t, app, "tok-w1", "p", "w1")
			recordSpawn(t, app, "tok-w2", "p", "w2")
			recordSpawn(t, app, "tok-g", "w1", "g")
			if crash == "after the owner's claim" {
				app.deleteClaimedForTest = func(id string) {
					if id == "p" {
						runtime.Goexit()
					}
				}
			} else {
				app.testEmitHook = func(_ string, data any) {
					if frame, ok := data.(triage.ThreadUpdateEvent); ok && frame.Action == triage.ThreadActionDeleted {
						runtime.Goexit()
					}
				}
			}
			stopped := make(chan bool, 1)
			go func() {
				returned := false
				defer func() { stopped <- returned }()
				_, _ = app.DeleteThread("p")
				returned = true
			}()
			if within(t, "the interrupted delete", stopped) {
				t.Fatal("the delete returned instead of stopping")
			}
			if left := remaining(t, app, ids); len(left) < 3 {
				t.Fatalf("threads left after the interruption = %v, want the delete stopped early", left)
			}
			app.appCancel()
			if err := app.store.Close(); err != nil {
				t.Fatal(err)
			}

			boot := newTestAppAtStorePath(t, path)
			boot.maintenance.firstReadsFallback = time.Millisecond
			t.Cleanup(func() {
				boot.appCancel()
				boot.waitPendingThreadDeletes()
			})
			boot.startPendingThreadDeletes()
			waitUntil(t, 10*time.Second, func() bool { return len(remaining(t, boot, ids)) == 0 })
		})
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

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A person's delete removes the worktrees it empties, the thread's and its
// workers', and keeps one a thread it does not take is on.
func TestADeleteWithWorktreesRemovesTheOnesItEmpties(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	ownerTree, workerTree, sharedTree := f.worktree(t, "owner-tree"), f.worktree(t, "worker-tree"), f.worktree(t, "shared-tree")
	owner := f.thread(t, "owner", on(ownerTree))
	worker := f.thread(t, "worker", on(workerTree))
	beside := f.thread(t, "beside", on(workerTree))
	shared := f.thread(t, "shared", on(sharedTree))
	stranger := f.thread(t, "stranger", on(sharedTree))
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	recordSpawn(t, f.app, "tok-beside", worker.ID, beside.ID)
	recordSpawn(t, f.app, "tok-shared", owner.ID, shared.ID)
	result, err := f.app.DeleteThreadAndWorktrees(t.Context(), owner.ID)
	if err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("DeleteThreadAndWorktrees = %+v, %v", result, err)
	}
	for _, id := range []string{owner.ID, worker.ID, beside.ID, shared.ID} {
		if !gone(t, f.app, id) {
			t.Errorf("%s outlived the delete", id)
		}
	}
	untouched(t, f.app, stranger.ID)
	if exists(t, ownerTree) || exists(t, workerTree) {
		t.Error("a worktree the delete emptied is still there")
	}
	if !exists(t, sharedTree) {
		t.Error("the worktree another thread is on was removed")
	}
	if got := f.reload(t, stranger.ID); got.WorktreePath != sharedTree {
		t.Errorf("the other thread on the shared worktree = %q, want it kept there", got.WorktreePath)
	}
}

// A caller without git:operate deletes a thread with no worktree to
// remove, and is refused, with nothing deleted, when there is one.
func TestADeleteWithWorktreesAsksForGitOperateOnlyForAWorktree(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	if err := f.app.initIdentity("backend-under-test"); err != nil {
		t.Fatal(err)
	}
	limited := callFrom(pairSessionWithScopes(t, f.app, "delete-limited", []identity.Scope{identity.ScopeThreadsOperate}).ID, false)
	tree := f.worktree(t, "worker-tree")
	plain := f.thread(t, "plain")
	owner := f.thread(t, "owner")
	worker := f.thread(t, "worker", on(tree))
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	if _, err := f.app.DeleteThreadAndWorktrees(limited, plain.ID); err != nil || !gone(t, f.app, plain.ID) {
		t.Fatalf("a delete with no worktree to remove = %v, gone %v", err, gone(t, f.app, plain.ID))
	}
	_, err := f.app.DeleteThreadAndWorktrees(limited, owner.ID)
	wantScopeRefusal(t, err, transport.ScopeGitOperate)
	untouched(t, f.app, owner.ID, worker.ID)
	if !exists(t, tree) {
		t.Error("a refused delete removed the worktree")
	}
}

// A delete without a person's worktree removal, as from a tool or the boot,
// keeps every worktree.
func TestADeleteKeepsWorktreesUnlessAPersonRemovesThem(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	tree := f.worktree(t, "worker-tree")
	owner := f.thread(t, "owner")
	worker := f.thread(t, "worker", on(tree))
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	if _, err := f.app.DeleteThread(owner.ID); err != nil {
		t.Fatal(err)
	}
	if !gone(t, f.app, worker.ID) || !exists(t, tree) {
		t.Errorf("worker gone %v, worktree kept %v; want both", gone(t, f.app, worker.ID), exists(t, tree))
	}
}

// A worker given a sidebar row by another client after the confirmation
// read what the delete takes is the person's: the delete leaves its
// session and worktree alone.
func TestADeleteLeavesAWorkerPlacedAfterItsConfirmation(t *testing.T) {
	t.Parallel()
	f := newDeleteWorktreeFixture(t)
	tree := f.worktree(t, "worker-tree")
	owner := f.thread(t, "owner")
	worker := f.thread(t, "worker", on(tree))
	recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
	runningSession(t, f.app, worker.ID)
	preview, err := f.app.GetDeleteDependents(owner.ID)
	if err != nil || preview.Workers != 1 || len(preview.Worktrees) != 1 {
		t.Fatalf("GetDeleteDependents = %+v, %v; want the worker and its worktree", preview, err)
	}
	if _, err := f.app.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DeleteThreadAndWorktrees(t.Context(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if !gone(t, f.app, owner.ID) {
		t.Error("the owner outlived its delete")
	}
	untouched(t, f.app, worker.ID)
	if _, live := f.app.sessionManager().get(worker.ID); !live {
		t.Error("the placed worker's session was stopped")
	}
	if !exists(t, tree) {
		t.Error("the placed worker's worktree was removed")
	}
}

// A worktree that cannot be removed fails the delete of the threads on it.
// For the named thread nothing is deleted. A worker is named in the result
// and stays with the threads under it, standalone once its owner is gone,
// and the rest of the delete goes on.
func TestAWorktreeThatCannotBeRemovedFailsTheDeleteOfItsThreads(t *testing.T) {
	t.Parallel()
	dirty := func(t *testing.T, tree string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, "unsaved.txt"), []byte("work"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a worker's", func(t *testing.T) {
		t.Parallel()
		f := newDeleteWorktreeFixture(t)
		stuckTree, cleanTree := f.worktree(t, "stuck-tree"), f.worktree(t, "clean-tree")
		owner := f.thread(t, "owner")
		stuck := f.thread(t, "stuck", on(stuckTree))
		under := f.thread(t, "under")
		sibling := f.thread(t, "sibling", on(cleanTree))
		recordSpawn(t, f.app, "tok-stuck", owner.ID, stuck.ID)
		recordSpawn(t, f.app, "tok-under", stuck.ID, under.ID)
		recordSpawn(t, f.app, "tok-sibling", owner.ID, sibling.ID)
		withMessage(t, f.app, stuck.ID)
		dirty(t, stuckTree)
		result, err := f.app.DeleteThreadAndWorktrees(t.Context(), owner.ID)
		if err != nil {
			t.Fatalf("DeleteThreadAndWorktrees: %v", err)
		}
		if len(result.WorkersNotDeleted) != 1 {
			t.Fatalf("workers not deleted = %+v, want the stuck one", result.WorkersNotDeleted)
		}
		failure := result.WorkersNotDeleted[0]
		if failure.ThreadID != stuck.ID || failure.Title != stuck.Title || !strings.Contains(failure.Error, "unsaved work") {
			t.Errorf("failure = %+v, want the stuck worker titled with its worktree's reason", failure)
		}
		if !gone(t, f.app, owner.ID) || !gone(t, f.app, sibling.ID) || exists(t, cleanTree) {
			t.Error("the owner or its other worker outlived the delete, or kept its worktree")
		}
		untouched(t, f.app, stuck.ID, under.ID)
		if !exists(t, stuckTree) {
			t.Error("the worktree that could not be removed is gone")
		}
		if got := f.reload(t, stuck.ID); got.Placement != store.PlacementStandalone || got.SpawnedByThreadID != "" {
			t.Errorf("the stuck worker = placement %s spawner %q, want standalone", got.Placement, got.SpawnedByThreadID)
		}
		if got := f.reload(t, under.ID); got.Placement != store.PlacementOwned || got.SpawnedByThreadID != stuck.ID {
			t.Errorf("the thread under it = placement %s spawner %q, want still the stuck worker's", got.Placement, got.SpawnedByThreadID)
		}
		if ids := catalogIDs(t, f.app); !slices.Contains(ids, stuck.ID) {
			t.Errorf("catalog = %v, want the stuck worker listed", ids)
		}
	})
	t.Run("the named thread's", func(t *testing.T) {
		t.Parallel()
		f := newDeleteWorktreeFixture(t)
		ownerTree, workerTree := f.worktree(t, "owner-tree"), f.worktree(t, "worker-tree")
		owner := f.thread(t, "owner", on(ownerTree))
		worker := f.thread(t, "worker", on(workerTree))
		recordSpawn(t, f.app, "tok-worker", owner.ID, worker.ID)
		dirty(t, ownerTree)
		if _, err := f.app.DeleteThreadAndWorktrees(t.Context(), owner.ID); err == nil || !strings.Contains(err.Error(), "unsaved work") {
			t.Fatalf("DeleteThreadAndWorktrees = %v, want the worktree's reason", err)
		}
		untouched(t, f.app, owner.ID, worker.ID)
		if !exists(t, ownerTree) || !exists(t, workerTree) {
			t.Error("a refused delete removed a worktree")
		}
	})
}

// What a delete confirmation counts is what the delete takes: an
// assistant's past thread, the worker that past thread spawned, and the
// worktree that worker is on with the terminals open in it; the delete
// removes that worktree and closes those terminals.
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
		if _, err := f.app.terminals.Open(worker.ID, terminal.SessionOptions{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Cwd: tree}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.app.GetDeleteDependents(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := DeleteDependents{PastThreads: 1, Workers: 1, Worktrees: []DeleteWorktree{{ProjectID: f.project.ID, WorktreePath: tree, Terminals: 2}}}
	if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("GetDeleteDependents = %s, want %s", gotJSON, wantJSON)
	}
	if result, err := f.app.DeleteThreadAndWorktrees(t.Context(), current.ID); err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("DeleteThreadAndWorktrees = %+v, %v", result, err)
	}
	for _, id := range []string{current.ID, past.ID, worker.ID} {
		if !gone(t, f.app, id) {
			t.Errorf("%s outlived the delete", id)
		}
	}
	if exists(t, tree) {
		t.Error("the past thread's worker's worktree is still there")
	}
	if open := f.app.terminals.Matching(func(terminal.SessionSummary) bool { return true }); len(open) != 0 {
		t.Errorf("terminals after the delete = %+v, want none", open)
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
	if len(got.Worktrees) != 1 || got.Worktrees[0].ProjectID != f.project.ID || got.Worktrees[0].WorktreePath != "/wt/deeper" {
		t.Errorf("worktrees = %+v", got.Worktrees)
	}
	if got, err := f.app.GetDeleteDependents(chain.placed); err != nil || got.Workers != 0 || len(got.Worktrees) != 0 {
		t.Errorf("a thread with no workers = %+v, %v", got, err)
	}
}

// A project removal refuses while a worker of one of its threads, in
// another project, is running, and the worker keeps running: whether it
// ran before the removal or started a turn after the removal checked it.
func TestAProjectRemovalRefusesWhileAWorkerOutsideRuns(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the removal", "after its preflight"} {
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
			if when == "before the removal" {
				runningSession(t, f.app, worker.ID)
			} else {
				var start sync.Once
				f.app.projectLockedForTest = func(string) {
					start.Do(func() { runningSession(t, f.app, worker.ID) })
				}
			}
			if _, err := f.app.DeleteProject(f.project.ID); err == nil || !strings.Contains(err.Error(), "worker "+worker.ID) {
				t.Fatalf("DeleteProject = %v, want it refused naming the running worker", err)
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
