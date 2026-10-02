package worktreesetupapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/worktreesetup"
)

type admissionSetupEvents struct {
	once            sync.Once
	entered, resume chan struct{}
}

func (e *admissionSetupEvents) Setup(Event) {
	e.once.Do(func() { close(e.entered); <-e.resume })
}
func (*admissionSetupEvents) ThreadUpdated(store.Thread) {}

func TestSetupAdmissionOutlivesLaunchAndEndsAfterCleanup(t *testing.T) {
	root := t.TempDir()
	thread := store.Thread{ID: "thread", ProjectID: "project", ProjectPath: root, WorktreePath: root, WorkspacePath: root}
	storage := &testStore{threads: map[string]store.Thread{thread.ID: thread}, config: worktreesetup.Config{Run: [][]string{{"/bin/sh", "-c", "exit 0"}}}}
	events := &admissionSetupEvents{entered: make(chan struct{}), resume: make(chan struct{})}
	var active atomic.Int32
	service := New(Config{Store: storage, Events: events, Context: t.Context, BeginWork: func(context.Context) (func(), error) {
		active.Add(1)
		return func() { active.Add(-1) }, nil
	}})
	resume := sync.OnceFunc(func() { close(events.resume) })
	t.Cleanup(func() { resume(); service.Stop() })
	if err := service.LaunchThread(thread, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("setup did not start")
	}
	if active.Load() != 1 {
		t.Fatal("setup launch returned without retaining admission")
	}
	resume()
	service.Stop()
	if active.Load() != 0 {
		t.Fatalf("setup cleanup left %d leases", active.Load())
	}
	if err := service.LaunchThread(thread, true); err == nil {
		t.Fatal("stopped service accepted setup")
	}
	if active.Load() != 0 {
		t.Fatal("failed setup launch leaked admission")
	}
}

type testStore struct {
	mu      sync.Mutex
	threads map[string]store.Thread
	config  worktreesetup.Config
}

func (s *testStore) GetThread(threadID string) (store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.threads[threadID]
	if !ok {
		return store.Thread{}, os.ErrNotExist
	}
	return thread, nil
}

func (s *testStore) ProjectWorktreeSetup(string) (worktreesetup.Config, bool, error) {
	return s.config, true, nil
}

func (s *testStore) SetThreadWorktreeSetupState(threadID, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.threads[threadID]
	thread.WorktreeSetupState = state
	s.threads[threadID] = thread
	return nil
}

func (*testStore) SweepRunningThreadWorktreeSetups() (int64, error) { return 0, nil }

func TestStopRacingLaunchKeepsWaitGroupOwnershipStructural(t *testing.T) {
	projectRoot := t.TempDir()
	storage := &testStore{
		threads: make(map[string]store.Thread),
		config: worktreesetup.Config{Run: [][]string{
			{"/bin/sh", "-c", "sleep 0.05"},
		}},
	}
	const runCount = 24
	threads := make([]store.Thread, 0, runCount)
	for index := 0; index < runCount; index++ {
		worktreePath := filepath.Join(projectRoot, fmt.Sprintf("worktree-%d", index))
		if err := os.Mkdir(worktreePath, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		thread := store.Thread{
			ID: fmt.Sprintf("thread-%d", index), ProjectID: "project",
			ProjectPath: projectRoot, WorktreePath: worktreePath, WorkspacePath: worktreePath,
		}
		storage.threads[thread.ID] = thread
		threads = append(threads, thread)
	}

	shutdownErr := errors.New("shutting down")
	service := New(Config{Store: storage, ShutdownError: shutdownErr})
	start := make(chan struct{})
	var launches sync.WaitGroup
	for _, thread := range threads {
		thread := thread
		launches.Add(1)
		go func() {
			defer launches.Done()
			<-start
			err := service.LaunchThread(thread, true)
			if err != nil && !errors.Is(err, shutdownErr) {
				t.Errorf("LaunchThread(%s): %v", thread.ID, err)
			}
		}()
	}
	close(start)
	service.Stop()
	launches.Wait()

	if err := service.LaunchThread(threads[0], true); !errors.Is(err, shutdownErr) {
		t.Fatalf("LaunchThread after Stop = %v, want shutdown error", err)
	}
}

type recordingSetupEvents struct {
	mu     sync.Mutex
	frames []Event
}

func (e *recordingSetupEvents) Setup(event Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frames = append(e.frames, event)
}

func (*recordingSetupEvents) ThreadUpdated(store.Thread) {}

func (e *recordingSetupEvents) mark() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.frames)
}

// cancelledSince returns the cancelled terminal frames recorded after mark.
func (e *recordingSetupEvents) cancelledSince(mark int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var cancelled []Event
	for _, frame := range e.frames[mark:] {
		if frame.Phase == phaseFinished && frame.State == runCancelled {
			cancelled = append(cancelled, frame)
		}
	}
	return cancelled
}

func newCancelTestService(t *testing.T, recipe string) (*Service, *testStore, *recordingSetupEvents, store.Thread) {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	thread := store.Thread{ID: "thread", ProjectID: "project", ProjectPath: root, WorktreePath: worktree, WorkspacePath: worktree}
	storage := &testStore{
		threads: map[string]store.Thread{thread.ID: thread},
		config:  worktreesetup.Config{Run: [][]string{{"/bin/sh", "-c", recipe}}, Timeout: "60s"},
	}
	events := &recordingSetupEvents{}
	service := New(Config{Store: storage, Events: events, Context: t.Context})
	t.Cleanup(service.Stop)
	return service, storage, events, thread
}

// failSetup runs a failing recipe to completion and returns its run id.
func failSetup(t *testing.T, service *Service, storage *testStore, thread store.Thread) string {
	t.Helper()
	if err := service.LaunchThread(thread, true); err != nil {
		t.Fatalf("LaunchThread: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if !service.WaitThread(ctx, thread.ID) {
		t.Fatal("setup did not settle")
	}
	snapshot, err := service.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != runFailed || snapshot.RunID == "" {
		t.Fatalf("snapshot = %q run %q, want a retained failure", snapshot.State, snapshot.RunID)
	}
	if got, _ := storage.GetThread(thread.ID); got.WorktreeSetupState != store.WorktreeSetupStateFailed {
		t.Fatalf("durable state = %q, want failed", got.WorktreeSetupState)
	}
	return snapshot.RunID
}

func assertSetupCleared(t *testing.T, service *Service, storage *testStore, threadID string) {
	t.Helper()
	if got, _ := storage.GetThread(threadID); got.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("durable state = %q, want empty", got.WorktreeSetupState)
	}
	snapshot, err := service.GetThreadWorktreeSetup(threadID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != runIdle {
		t.Fatalf("snapshot state = %q, want idle", snapshot.State)
	}
}

func assertOneCancelled(t *testing.T, events *recordingSetupEvents, mark int, runID string) {
	t.Helper()
	cancelled := events.cancelledSince(mark)
	if len(cancelled) != 1 {
		t.Fatalf("cancelled frames = %d, want 1", len(cancelled))
	}
	if cancelled[0].ThreadID != "thread" || cancelled[0].RunID != runID {
		t.Fatalf("cancelled frame thread %q run %q, want thread run %q", cancelled[0].ThreadID, cancelled[0].RunID, runID)
	}
}

// A client showing a failed card drops it only on a setup frame; the
// thread-row update alone does not reach the card.
func TestCancelThreadRetiresARetainedFailure(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "exit 1")
	runID := failSetup(t, service, storage, thread)

	mark := events.mark()
	service.CancelThread(thread.ID)
	assertOneCancelled(t, events, mark, runID)
	assertSetupCleared(t, service, storage, thread.ID)

	mark = events.mark()
	service.CancelThread(thread.ID)
	if frames := events.cancelledSince(mark); len(frames) != 0 {
		t.Fatalf("repeat cancel emitted %d cancelled frames, want none", len(frames))
	}
}

func TestCancelPathRetiresARetainedFailure(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "exit 1")
	runID := failSetup(t, service, storage, thread)

	mark := events.mark()
	service.CancelPath(thread.WorktreePath)
	assertOneCancelled(t, events, mark, runID)
	assertSetupCleared(t, service, storage, thread.ID)

	// Worktree removal follows CancelPath with CancelThread for each occupant.
	// The failure is already retired, so no second frame follows.
	mark = events.mark()
	service.CancelThread(thread.ID)
	if frames := events.cancelledSince(mark); len(frames) != 0 {
		t.Fatalf("CancelThread after CancelPath emitted %d cancelled frames, want none", len(frames))
	}
}

// After a restart the failure survives only in the thread row, and the
// snapshot serves it with an empty run id.
func TestCancelThreadRetiresADurableOnlyFailure(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "exit 1")
	if err := storage.SetThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateFailed); err != nil {
		t.Fatal(err)
	}

	service.CancelThread(thread.ID)
	assertOneCancelled(t, events, 0, "")
	assertSetupCleared(t, service, storage, thread.ID)
}

// A live run reports its own cancellation when it settles; the cancel path
// must not add a second frame.
func TestCancelThreadOnALiveRunEmitsOneCancelledFrame(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "sleep 30")
	if err := service.LaunchThread(thread, true); err != nil {
		t.Fatalf("LaunchThread: %v", err)
	}

	service.CancelThread(thread.ID)
	cancelled := events.cancelledSince(0)
	if len(cancelled) != 1 || cancelled[0].RunID == "" {
		t.Fatalf("cancelled frames = %+v, want one for the live run", cancelled)
	}
	assertSetupCleared(t, service, storage, thread.ID)
}

// Dismissal retires a failure the same way cancellation does: one cancelled
// frame for every client and no durable state left to restore it.
func TestDismissThreadRetiresARetainedFailure(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "exit 1")
	runID := failSetup(t, service, storage, thread)

	mark := events.mark()
	if err := service.DismissThread(thread.ID); err != nil {
		t.Fatalf("DismissThread: %v", err)
	}
	assertOneCancelled(t, events, mark, runID)
	assertSetupCleared(t, service, storage, thread.ID)

	mark = events.mark()
	if err := service.DismissThread(thread.ID); err != nil {
		t.Fatalf("repeat DismissThread: %v", err)
	}
	if frames := events.cancelledSince(mark); len(frames) != 0 {
		t.Fatalf("repeat dismiss emitted %d cancelled frames, want none", len(frames))
	}
}

func TestDismissThreadRetiresADurableOnlyFailure(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "exit 1")
	if err := storage.SetThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateFailed); err != nil {
		t.Fatal(err)
	}

	if err := service.DismissThread(thread.ID); err != nil {
		t.Fatalf("DismissThread: %v", err)
	}
	assertOneCancelled(t, events, 0, "")
	assertSetupCleared(t, service, storage, thread.ID)
}

// Another client may have just retried. Dismissal must not kill that run or
// clear the state describing it.
func TestDismissThreadRefusesALiveRun(t *testing.T) {
	service, storage, events, thread := newCancelTestService(t, "sleep 30")
	if err := service.LaunchThread(thread, true); err != nil {
		t.Fatalf("LaunchThread: %v", err)
	}

	if err := service.DismissThread(thread.ID); err == nil {
		t.Fatal("DismissThread on a live run succeeded, want a refusal")
	}
	snapshot, err := service.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != runRunning {
		t.Fatalf("snapshot state = %q, want running", snapshot.State)
	}
	if got, _ := storage.GetThread(thread.ID); got.WorktreeSetupState != store.WorktreeSetupStateRunning {
		t.Fatalf("durable state = %q, want running", got.WorktreeSetupState)
	}
	if frames := events.cancelledSince(0); len(frames) != 0 {
		t.Fatalf("refused dismiss emitted %d cancelled frames, want none", len(frames))
	}
}

func TestDismissThreadRefusesABadThread(t *testing.T) {
	service, _, _, _ := newCancelTestService(t, "exit 1")
	if err := service.DismissThread("  "); err == nil {
		t.Fatal("DismissThread with a blank id succeeded, want an error")
	}
	if err := service.DismissThread("missing"); err == nil {
		t.Fatal("DismissThread for an unknown thread succeeded, want an error")
	}
}
