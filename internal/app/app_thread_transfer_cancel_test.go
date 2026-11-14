package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"

	"github.com/google/uuid"
)

// A move stops the thread's remote commands before it fences the thread.
// A caller that stops waiting after that, while the move waits for the
// thread's lock, still gets the fence: the commands are not stopped for a
// move that never happened.
func TestThreadMoveCancelledAfterStoppingItsCommandsStillRecordsTheFence(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source := transferTestBackend(t)
	workspace := testutil.InitGitRepo(t)
	project, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: workspace})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: project.ID, Title: "Moving", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), WorkspacePath: workspace, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	computerID, requestID := uuid.NewString(), uuid.NewString()
	if _, err := source.store.RegisterRemoteWatch(store.RemoteWatch{
		ComputerID: computerID, RequestID: requestID, ThreadID: thread.ID, Fingerprint: strings.Repeat("ab", 32), Label: "build",
		Command: "make", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	unlock := source.threadLocks().Lock(thread.ID)
	ctx, cancel := context.WithCancel(context.Background())
	operation := uuid.NewString()
	done := make(chan error, 1)
	go func() {
		_, err := source.BeginThreadTransfer(ctx, thread.ID, operation, uuid.NewString(), "move", false)
		done <- err
	}()
	// The commands are stopped (their watch dismissed) before the lock wait.
	deadline := time.Now().Add(10 * time.Second)
	for {
		watch, err := source.store.GetRemoteWatch(computerID, requestID)
		if err != nil {
			t.Fatal(err)
		}
		if watch.Notification == "dismissed" {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("the move never stopped the thread's commands")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	unlock()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BeginThreadTransfer did not return")
	}
	if err != nil {
		t.Fatalf("move cancelled after stopping its commands = %v, want the fence recorded", err)
	}
	if row, err := source.store.GetThreadTransfer(operation); err != nil || row.ThreadID != thread.ID || row.Kind != "move" {
		t.Fatalf("transfer row = %+v, %v; want the move's fence", row, err)
	}
}
