package app

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/triage"
)

// A pre-init teardown has one owner: whoever holds the thread action lock.
// The event worker's reaper of a startup failure takes that lock before it
// unregisters the session, so a proof holding it tears the session down
// itself and returns only once the queue is restored and the process closed.

const preinitQueued = "queued before the start failed"

func preinitNotFound() string {
	return `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"errors":["No conversation found with session ID: ` + workflowResumeSession + `"]}`
}

func (f *workflowResumeFixture) queueForTheStart() {
	f.t.Helper()
	f.app.configureTriageQueueCallbacks()
	f.app.triage.RegisterQueueItem(f.thread.ID, triage.QueuedFlushItem{
		ID: "queue:preinit", Message: preinitQueued, Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
	})
}

// letTheReaperRunFirst holds the start, after it registered the session and
// before its proof, until the event worker's reaper has unregistered the
// session or a second has passed: the order in which a reaper that does not
// wait for the lock wins.
func (f *workflowResumeFixture) letTheReaperRunFirst() {
	var once sync.Once
	f.app.flushHandoffForTest = func(threadID string) {
		once.Do(func() {
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, ok := f.app.sessionManager().get(threadID); !ok {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// requireTornDown checks, without waiting, that the session is gone, its
// queued message is back in the composer and its process has exited.
func (f *workflowResumeFixture) requireTornDown() {
	f.t.Helper()
	if _, ok := f.app.sessionManager().get(f.thread.ID); ok {
		f.t.Fatal("the session is still registered")
	}
	draft, _, err := f.app.store.GetThreadDraft(f.thread.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if !strings.Contains(draft.Content, preinitQueued) {
		f.t.Fatalf("draft = %q, want the queued message restored before the proof returned", draft.Content)
	}
	if syscall.Kill(f.mockPID(), 0) == nil {
		f.t.Fatal("the CLI is still running after the proof returned")
	}
}

func TestEnsureProvenSessionOwnsTheTeardownTheReaperRaces(t *testing.T) {
	t.Parallel()
	t.Run("closed and restored before it returns", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		f.setMode("")
		f.useCLI(resumeCLI{startup: []string{preinitNotFound()}, loaded: true})
		f.queueForTheStart()
		f.letTheReaperRunFirst()

		_, err := f.app.ensureProvenSession(t.Context(), f.thread.ID, func() error { return nil })
		if !errors.Is(err, claude.ErrSessionNotFound) {
			t.Fatalf("proof = %v, want the conversation not found", err)
		}
		f.requireTornDown()
	})
	t.Run("a close failure is returned", func(t *testing.T) {
		t.Parallel()
		f := newWorkflowResumeFixture(t)
		f.setMode("")
		f.useCLI(resumeCLI{startup: []string{preinitNotFound()}, loaded: true})
		f.queueForTheStart()
		f.letTheReaperRunFirst()
		injected := errors.New("injected provider close failure")
		// The hook supplies the close's result after the real close ran, so
		// the teardown is as complete as without it.
		f.app.closeProviderSessionFn = func(string) error { return injected }

		_, err := f.app.ensureProvenSession(t.Context(), f.thread.ID, func() error { return nil })
		if !errors.Is(err, injected) || !strings.Contains(err.Error(), "No conversation found") {
			t.Fatalf("proof = %v, want the close failure with the start's outcome", err)
		}
		f.requireTornDown()
	})
}

// With nobody holding the lock, the reaper alone tears a startup failure
// down: the session leaves, its queue returns to the composer and the
// process ends.
func TestStartupFailureReaperTearsDownAlone(t *testing.T) {
	t.Parallel()
	f := newWorkflowResumeFixture(t)
	f.useCLI(resumeCLI{startup: []string{preinitNotFound()}, loaded: true})
	f.queueForTheStart()
	f.setMode("")
	if err := f.app.startSessionTakingLock(t.Context(), f.thread.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reaper to tear the session down", func() bool {
		_, ok := f.app.sessionManager().get(f.thread.ID)
		draft, _, _ := f.app.store.GetThreadDraft(f.thread.ID)
		return !ok && strings.Contains(draft.Content, preinitQueued) && syscall.Kill(f.mockPID(), 0) != nil
	})
}

// A reaper that fires for a session a user retry is replacing waits for the
// retry's start, which holds the lock, and then finds its token gone: the
// replacement and its queue are left alone.
func TestStartupFailureReaperLeavesARetrysReplacement(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("reaper-retry")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.triage.MarkThreadActive(thread.ID, "dead")
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "dead"})

	// The retry's start holds the lock while the reaper fires: it replaces
	// the dead session and a message is queued for the replacement.
	unlock := app.threadLocks().Lock(thread.ID)
	reaped := make(chan error, 1)
	go func() { reaped <- app.teardownDeadPreInitSession(thread.ID, "dead") }()
	waitFor(t, "the reaper to wait for the retry's lock", func() bool { return app.threadLocks().Refs(thread.ID) == 2 })
	if err := app.stopExistingSessionLocked(thread.ID); err != nil {
		t.Fatal(err)
	}
	app.triage.MarkThreadActive(thread.ID, "retry")
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "retry"})
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID: "queue:retry", Message: "for the replacement", Payload: json.RawMessage(`{}`), EnqueuedAt: 10, NoRow: true,
	})
	unlock()

	select {
	case err := <-reaped:
		if err != nil {
			t.Fatalf("reaper = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper did not return")
	}
	if current, ok := app.sessionManager().get(thread.ID); !ok || current.Token != "retry" {
		t.Fatal("the reaper disturbed the retry's replacement session")
	}
	if !app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("the reaper drained the replacement's queue")
	}
}
