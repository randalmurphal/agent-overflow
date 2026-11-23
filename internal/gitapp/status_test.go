package gitapp

import (
	"context"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func newStatusTestService(t *testing.T, watch *gitwatch.Manager, emit func(StatusEvent)) *Service {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	project := testutil.InitGitRepo(t)
	now := time.Now().UnixMilli()
	if _, err := database.CreateProject(store.Project{ID: "project", Path: project, Name: "project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	service := New(Deps{Store: database, Core: gitops.NewCore(), Watch: watch, EmitStatus: emit})
	t.Cleanup(service.CloseStatus)
	return service
}

// Status and CheckoutStatus answer with what the workspace's stream
// publishes: a read sequenced after the stream's snapshot, broadcast to
// the stream's subscribers with the same seq.
func TestStatusAnswersArePublishedThroughTheWorkspaceStream(t *testing.T) {
	t.Parallel()
	reads := map[string]func(*Service, WorkspaceRef) (gitops.GitStatus, error){
		"Status": func(service *Service, ref WorkspaceRef) (gitops.GitStatus, error) {
			return service.Status(t.Context(), ref)
		},
		"CheckoutStatus": func(service *Service, ref WorkspaceRef) (gitops.GitStatus, error) {
			_, workspace, err := service.ResolveWorkspace(ref)
			if err != nil {
				return gitops.GitStatus{}, err
			}
			return service.CheckoutStatus(workspace)
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			core := gitops.NewCore()
			// The snapshot is not the repository's status, so publishing
			// the real one changes it and is broadcast.
			watch := gitwatch.NewManager(gitwatch.ManagerConfig{
				StatusFn: core.Status,
				FastStatusFn: func(context.Context, string) (gitops.GitStatus, error) {
					return gitops.GitStatus{IsRepo: true, Branch: "elsewhere"}, nil
				},
			})
			events := make(chan StatusEvent, 16)
			service := newStatusTestService(t, watch, func(event StatusEvent) { events <- event })
			ref := WorkspaceRef{ProjectID: "project"}
			sub, err := service.Subscribe(t.Context(), ref, untiedStatus)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer service.Unsubscribe(sub.ID)

			got, err := read(service, ref)
			if err != nil {
				t.Fatal(err)
			}
			if got.Branch != "main" || got.Seq <= sub.Status.Seq {
				t.Fatalf("answered %+v, want branch main at a seq above the snapshot's %d", got, sub.Status.Seq)
			}
			deadline := time.After(5 * time.Second)
			for found := false; !found; {
				select {
				case event := <-events:
					found = event.Cwd == sub.Cwd && event.Status == got
				case <-deadline:
					t.Fatalf("no git:status broadcast of the answer %+v", got)
				}
			}
		})
	}
}

// Without a status manager there is no stream to order against: the
// answer is the read itself, unsequenced.
func TestStatusAnswersWithoutAWatchManagerAreUnsequenced(t *testing.T) {
	t.Parallel()
	service := newStatusTestService(t, nil, nil)
	ref := WorkspaceRef{ProjectID: "project"}
	status, err := service.Status(t.Context(), ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Branch != "main" || status.Seq != 0 {
		t.Fatalf("Status = %+v, want branch main with no seq", status)
	}
	_, workspace, err := service.ResolveWorkspace(ref)
	if err != nil {
		t.Fatalf("ResolveWorkspace: %v", err)
	}
	checkout, err := service.CheckoutStatus(workspace)
	if err != nil {
		t.Fatalf("CheckoutStatus: %v", err)
	}
	if checkout.Branch != "main" || checkout.Seq != 0 {
		t.Fatalf("CheckoutStatus = %+v, want branch main with no seq", checkout)
	}
}
