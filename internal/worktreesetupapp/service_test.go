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
