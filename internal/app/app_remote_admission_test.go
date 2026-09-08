package app

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

type remoteAdmissionReceiver struct {
	*App
	calls   atomic.Int32
	refusal error
}

func (r *remoteAdmissionReceiver) RemoteCommandStart(ctx context.Context, workspace gitapp.WorkspaceRef, request RemoteCommandRequest) (RemoteCommand, error) {
	r.calls.Add(1)
	if r.refusal != nil {
		return RemoteCommand{}, r.refusal
	}
	return r.App.RemoteCommandStart(ctx, workspace, request)
}

func remoteAdmissionFixture(t *testing.T) (*App, *remoteAdmissionReceiver, context.Context, AgentRemoteRequest) {
	t.Helper()
	receiver := &remoteAdmissionReceiver{}
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(receiver, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
			t.Fatal(err)
		}
		cfg.Dispatcher = dispatcher
	})
	receiver.App = backend.app
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "admission source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	invite, _ := backend.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err = backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	if err = source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	project, err := backend.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "target", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	backend.app.remoteJobs, err = remotejobs.New(ctx, backend.app.store, func(context.Context, string, []string, io.Writer) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.app.remoteJobs.Close)
	thread, _ := remoteMCPThread(t, source, "codex")
	caller := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	input := AgentRemoteRequest{ComputerID: peer.ID, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID}, Request: RemoteCommandRequest{ID: uuid.NewString(), Argv: []string{"test-helper"}, TimeoutSeconds: 60}}
	return source, receiver, caller, input
}

func TestRemoteAdmissionRepeatedRefusalDoesNotRetainPendingWatch(t *testing.T) {
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.refusal = errorsx.Public("remote_capacity", "Destination capacity is full.", nil)
	for attempt := range 2 {
		_, err := source.AgentRemoteStart(ctx, input)
		code, _, _ := errorsx.PublicDetails(err)
		if code != "remote_capacity" {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if watch.Notification != "dismissed" || watch.Receipt.ID != "" {
			t.Fatalf("refused attempt retained work: %#v", watch)
		}
		if pending, err := source.store.HasUnfinishedRemoteWatches(watch.ThreadID); err != nil || pending {
			t.Fatalf("refusal prevents thread deletion: %v %v", pending, err)
		}
	}
	if receiver.calls.Load() != 2 {
		t.Fatalf("retry did not reach destination: %d", receiver.calls.Load())
	}
	receiver.refusal = nil
	receipt, err := source.AgentRemoteStart(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watch.Notification != "pending" || watch.Receipt.ID != receipt.ID {
		t.Fatalf("successful retry did not restore completion tracking: %#v", watch)
	}
}

func TestRemoteAdmissionLateRefusalCannotHideAcceptedReceipt(t *testing.T) {
	source, _, ctx, input := remoteAdmissionFixture(t)
	receipt, err := source.AgentRemoteStart(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// A refusal produced before this accepted retry can arrive afterwards. Its
	// cleanup must atomically require an empty receipt, not rely on an earlier read.
	if err = source.store.RefuseRemoteWatch(input.ComputerID, input.Request.ID, "late capacity refusal"); err != nil {
		t.Fatal(err)
	}
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watch.Notification != "pending" || watch.Receipt.ID != receipt.ID || watch.Error != "" {
		t.Fatalf("late refusal hid accepted job: %#v", watch)
	}
	if err := source.RemoveBackend(input.ComputerID); err == nil || !strings.Contains(err.Error(), "awaiting completion") {
		t.Fatalf("forgot pending job's cancellation credentials: %v", err)
	}
	if _, err := source.AgentRemoteStatus(ctx, input.ComputerID, input.Request.ID); err != nil {
		t.Fatalf("refused removal damaged the connection: %v", err)
	}
	if err := source.store.QueueRemoteCompletion(input.ComputerID, input.Request.ID, store.FlushQueueItem{ID: uuid.NewString(), ThreadID: watch.ThreadID, Message: "finished", SendID: "remote-completion-test"}); err != nil {
		t.Fatal(err)
	}
	if err := source.RemoveBackend(input.ComputerID); err != nil {
		t.Fatalf("completion queue still prevented forgetting: %v", err)
	}
}

func TestRemoteAdmissionMissingSourceNeverDispatches(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "missing"
		if deleted {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			scope, _ := transport.CallerScopeFrom(ctx)
			if deleted {
				if err := source.store.DeleteThread(scope.ThreadID); err != nil {
					t.Fatal(err)
				}
			} else {
				scope.ThreadID = uuid.NewString()
				ctx = transport.WithCallerScope(ctx, scope)
			}
			if _, err := source.AgentRemoteStart(ctx, input); err == nil {
				t.Fatal("accepted work without a source conversation")
			}
			if receiver.calls.Load() != 0 {
				t.Fatal("orphaned command reached destination")
			}
		})
	}
}
