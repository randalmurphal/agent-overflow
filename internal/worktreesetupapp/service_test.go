package worktreesetupapp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/worktreesetup"
)

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
