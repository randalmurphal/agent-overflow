package gitapp

import (
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/store"
)

func newWatchTestService(t *testing.T) (*Service, string) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	workspace := t.TempDir()
	now := time.Now().UnixMilli()
	if _, err := database.CreateProject(store.Project{ID: "project", Path: workspace, Name: "project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := database.CreateThread(store.Thread{
		ID: "thread", ProjectID: "project", WorkspacePath: workspace,
		Provider: "claude", Model: "test", Title: "thread", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	manager := gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn: func(string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
		},
	})
	service := New(Deps{Store: database, Watch: manager})
	t.Cleanup(service.CloseStatus)
	return service, workspace
}

func TestSubscribeReplacesDeadPumpWithoutDroppingSuccessor(t *testing.T) {
	service, _ := newWatchTestService(t)
	first, err := service.Subscribe("thread")
	if err != nil {
		t.Fatalf("Subscribe first: %v", err)
	}

	service.status.mu.Lock()
	dying := service.status.pumps[first.Cwd]
	if dying == nil {
		service.status.mu.Unlock()
		t.Fatal("first subscription did not create a pump")
	}
	dying.dead = true
	service.status.mu.Unlock()

	second, err := service.Subscribe("thread")
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	service.status.mu.Lock()
	fresh := service.status.pumps[first.Cwd]
	held := service.status.handles[second.ID]
	service.status.mu.Unlock()
	if fresh == dying || held != fresh {
		t.Fatalf("second subscription did not take a fresh pump")
	}

	service.dropStatusPump(dying)
	service.status.mu.Lock()
	stillMapped := service.status.pumps[first.Cwd]
	stillHeld := service.status.handles[second.ID]
	_, staleHeld := service.status.handles[first.ID]
	service.status.mu.Unlock()
	if stillMapped != fresh || stillHeld != fresh || staleHeld {
		t.Fatalf("dead pump cleanup damaged successor: mapped=%p held=%p stale=%v", stillMapped, stillHeld, staleHeld)
	}
	service.Unsubscribe(second.ID)
}
