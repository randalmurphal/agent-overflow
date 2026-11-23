package gitapp

import (
	"context"
	"errors"
	"sync"
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
	manager := gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn: func(context.Context, string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
		},
	})
	service := New(Deps{Store: database, Watch: manager})
	t.Cleanup(service.CloseStatus)
	return service, workspace
}

// untiedStatus ties a test's handles to nothing.
var untiedStatus StatusTie = func(string) (func(), error) { return nil, nil }

// TestSubscribeTiesTheHandleAsItPublishesIt: a refused tie publishes no
// handle and starts no stream, and the untie the tie answered runs once the
// handle is gone, whether its Unsubscribe or its pump's end removes it. The
// handle is tied before anything can remove it, so there is no moment a
// published handle has no untie.
func TestSubscribeTiesTheHandleAsItPublishesIt(t *testing.T) {
	service, _ := newWatchTestService(t)
	refused := errors.New("the owner is gone")
	if _, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"}, func(string) (func(), error) {
		return nil, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("subscribe with a refused tie = %v, want %v", err, refused)
	}
	service.status.mu.Lock()
	handles, pumps := len(service.status.handles), len(service.status.pumps)
	service.status.mu.Unlock()
	if handles != 0 || pumps != 0 {
		t.Fatalf("a refused tie left %d handles and %d streams", handles, pumps)
	}

	untied := map[string]int{}
	var mu sync.Mutex
	tie := func(id string) (func(), error) {
		return func() {
			mu.Lock()
			untied[id]++
			mu.Unlock()
		}, nil
	}
	first, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"}, tie)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"}, tie)
	if err != nil {
		t.Fatal(err)
	}
	service.Unsubscribe(first.ID)
	service.Unsubscribe(first.ID)
	service.status.mu.Lock()
	pump := service.status.handles[second.ID].pump
	service.status.mu.Unlock()
	service.dropStatusPump(pump)
	mu.Lock()
	defer mu.Unlock()
	if untied[first.ID] != 1 || untied[second.ID] != 1 {
		t.Fatalf("unties ran %v, want each handle's once", untied)
	}
}

func TestSubscribeReplacesDeadPumpWithoutDroppingSuccessor(t *testing.T) {
	service, _ := newWatchTestService(t)
	first, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"}, untiedStatus)
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

	second, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"}, untiedStatus)
	if err != nil {
		t.Fatalf("Subscribe second: %v", err)
	}
	service.status.mu.Lock()
	fresh := service.status.pumps[first.Cwd]
	held := service.status.handles[second.ID].pump
	service.status.mu.Unlock()
	if fresh == dying || held != fresh {
		t.Fatalf("second subscription did not take a fresh pump")
	}

	service.dropStatusPump(dying)
	service.status.mu.Lock()
	stillMapped := service.status.pumps[first.Cwd]
	stillHeld := service.status.handles[second.ID].pump
	_, staleHeld := service.status.handles[first.ID]
	service.status.mu.Unlock()
	if stillMapped != fresh || stillHeld != fresh || staleHeld {
		t.Fatalf("dead pump cleanup damaged successor: mapped=%p held=%p stale=%v", stillMapped, stillHeld, staleHeld)
	}
	service.Unsubscribe(second.ID)
}
