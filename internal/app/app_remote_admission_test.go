package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

type remoteAdmissionReceiver struct {
	*App
	calls, statusCalls atomic.Int32
	refusal            error
	// cancelRefusal, when set, refuses every RemoteCommandCancel.
	cancelRefusal atomic.Pointer[error]
	// cancelHold, when set, holds every RemoteCommandCancel until it is
	// closed or the call's context ends.
	cancelHold atomic.Pointer[chan struct{}]
	// lostReply, when set, starts the command and then fails the call, as a
	// reply lost on the way back does.
	lostReply atomic.Bool
	// wire is the destination's listener, which a test stops to take the
	// computer offline.
	wire *pairedBackend
}

func (r *remoteAdmissionReceiver) RemoteCommandCancel(ctx context.Context, id string) (RemoteCommand, error) {
	if hold := r.cancelHold.Load(); hold != nil {
		select {
		case <-*hold:
		case <-ctx.Done():
			return RemoteCommand{}, ctx.Err()
		}
	}
	if refusal := r.cancelRefusal.Load(); refusal != nil {
		return RemoteCommand{}, *refusal
	}
	return r.App.RemoteCommandCancel(ctx, id)
}

func (r *remoteAdmissionReceiver) RemoteCommandStart(ctx context.Context, workspace gitapp.WorkspaceRef, request RemoteCommandRequest) (RemoteCommand, error) {
	r.calls.Add(1)
	if r.refusal != nil {
		return RemoteCommand{}, r.refusal
	}
	if r.lostReply.Load() {
		if _, err := r.App.RemoteCommandStart(ctx, workspace, request); err != nil {
			return RemoteCommand{}, err
		}
		return RemoteCommand{}, errors.New("the reply was lost")
	}
	return r.App.RemoteCommandStart(ctx, workspace, request)
}

func (r *remoteAdmissionReceiver) RemoteCommandStatus(ctx context.Context, id string) (RemoteCommand, error) {
	r.statusCalls.Add(1)
	return r.App.RemoteCommandStatus(ctx, id)
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
	receiver.App, receiver.wire = backend.app, backend
	source := identityApp(t)
	manager, err := newTestBackends(t, t.TempDir(), "admission source", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitBudget(t))
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
	backend.app.remoteJobs, err = remotejobs.New(ctx, backend.app.store, func(context.Context, string, []string, io.Writer) (remotejobs.Outcome, error) {
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
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
	t.Parallel()
	remotetest.Require(t)
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

// Every refusal a destination answers before accepting releases the fresh
// watch, so a never-accepted request cannot block deletion, transfer or
// forgetting the computer forever. The same ID then admits an identical retry.
func TestRemoteAdmissionPreAcceptanceRefusalsReleaseTheWatch(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)
	for _, code := range []string{"remote_not_ready", "remote_shutting_down", "remote_not_accepted", transport.ErrCodeScopeRequired} {
		receiver.refusal = errorsx.Public(code, "refused before acceptance", nil)
		_, err := source.AgentRemoteStart(ctx, input)
		if got, _, _ := errorsx.PublicDetails(err); got != code {
			t.Fatalf("%s: %v", code, err)
		}
		watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
		if err != nil || watch.Notification != "dismissed" || watch.Receipt.ID != "" {
			t.Fatalf("%s retained work: %#v %v", code, watch, err)
		}
		if pending, err := source.store.HasUnfinishedRemoteWatches(scope.ThreadID); err != nil || pending {
			t.Fatalf("%s blocks the conversation: %v %v", code, pending, err)
		}
	}
	receiver.refusal = nil
	if _, err := source.AgentRemoteStart(ctx, input); err != nil {
		t.Fatal(err)
	}
	if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.Notification != "pending" || watch.Receipt.ID != input.Request.ID {
		t.Fatalf("retry after refusal lost completion tracking: %#v %v", watch, err)
	}
}

// A start the carrier never sent, because the computer cannot be reached,
// is refused at once: its fresh watch is released, so it neither blocks
// the conversation nor forgetting the computer, and the same ID admits an
// identical retry once the computer is back.
func TestRemoteAdmissionToAnUnreachableComputerReleasesTheWatch(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiver.wire.srv.Shutdown(stop); err != nil {
		t.Fatalf("stop the destination: %v", err)
	}
	_, err := source.AgentRemoteStart(ctx, input)
	if _, message, _ := errorsx.PublicDetails(err); !strings.Contains(message, "nothing was sent") {
		t.Fatalf("start on an unreachable computer = %v, want it said that nothing was sent", err)
	}
	if receiver.calls.Load() != 0 {
		t.Fatalf("the destination saw %d starts", receiver.calls.Load())
	}
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || watch.Notification != "dismissed" || watch.Receipt.ID != "" {
		t.Fatalf("a start never sent retained its watch: %#v %v", watch, err)
	}
	if pending, err := source.store.HasUnfinishedRemoteWatches(scope.ThreadID); err != nil || pending {
		t.Fatalf("a start never sent blocks the conversation: %v %v", pending, err)
	}
	if err := source.RemoveBackend(input.ComputerID, false); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
}

// A start whose reply was lost may have run. A retry under the same ID that
// cannot reach the computer says so beside its own failure, tells the
// caller to read the first attempt's status before retrying, and keeps the
// watch that stands for it.
func TestRemoteAdmissionRetryAfterALostReplySaysTheFirstAttemptMayHaveRun(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.lostReply.Store(true)
	if _, err := source.AgentRemoteStart(ctx, input); err == nil {
		t.Fatal("a start whose reply was lost succeeded")
	}
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiver.wire.srv.Shutdown(stop); err != nil {
		t.Fatalf("stop the destination: %v", err)
	}
	_, err := source.AgentRemoteStart(ctx, input)
	_, message, _ := errorsx.PublicDetails(err)
	for _, said := range []string{"nothing was sent", "earlier attempt with this request_id may already have run", "Check remote_status with these same IDs"} {
		if !strings.Contains(message, said) {
			t.Fatalf("retry on an unreachable computer = %q, want it to say %q", message, said)
		}
	}
	if receiver.calls.Load() != 1 {
		t.Fatalf("the destination saw %d starts, want the first alone", receiver.calls.Load())
	}
	if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.Notification != "pending" {
		t.Fatalf("the first attempt's watch = %#v %v, want it pending", watch, err)
	}
}

// A start whose reply was lost leaves a receipt-less pending watch. The poller
// settles it against the destination: no receipt there means the request was
// never accepted, and the watch is released. While a retry holds the attempt
// lock the poll defers instead, so it can never refuse a request that is
// about to be accepted.
func TestRemoteWatchProbeReleasesUnacceptedRequestUnlessStartIsInFlight(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)
	input.Request.SourceThreadID = scope.ThreadID
	if fresh, err := source.registerRemoteWatch(input); err != nil || !fresh {
		t.Fatalf("register: %v %v", fresh, err)
	}
	lost, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || lost.Notification != "pending" || lost.Receipt.ID != "" {
		t.Fatalf("lost reply watch: %#v %v", lost, err)
	}
	if pending, err := source.store.HasUnfinishedRemoteWatches(scope.ThreadID); err != nil || !pending {
		t.Fatalf("watch not pending: %v %v", pending, err)
	}
	unlock := source.remoteStartLocks().Lock(input.ComputerID + ":" + input.Request.ID)
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	receipt := lost.Receipt
	settled, err := source.probeUnacceptedRemoteWatch(bounded, lost, &receipt)
	cancel()
	if !settled || err != nil || receiver.statusCalls.Load() != 0 {
		t.Fatalf("in-flight start was probed: settled=%v err=%v status calls=%d", settled, err, receiver.statusCalls.Load())
	}
	if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.Notification != "pending" || watch.NextCheck == 0 {
		t.Fatalf("deferred poll changed the watch: %#v %v", watch, err)
	}
	unlock()
	source.checkRemoteWatch(lost)
	if receiver.statusCalls.Load() != 1 {
		t.Fatalf("destination not asked: %d", receiver.statusCalls.Load())
	}
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || watch.Notification != "dismissed" || watch.Receipt.ID != "" || !strings.Contains(watch.Error, "remote_job_not_found") {
		t.Fatalf("unaccepted request retained: %#v %v", watch, err)
	}
	if pending, err := source.store.HasUnfinishedRemoteWatches(scope.ThreadID); err != nil || pending {
		t.Fatalf("released watch still blocks the conversation: %v %v", pending, err)
	}
	if _, err := source.AgentRemoteStart(ctx, input); err != nil {
		t.Fatal(err)
	}
	if watch, err = source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.Notification != "pending" || watch.Receipt.ID != input.Request.ID {
		t.Fatalf("identical retry not tracked: %#v %v", watch, err)
	}
}

// Admission rederives execution ownership under the mutation fence: a
// conversation reserved for transfer neither registers a watch nor reaches
// the destination.
func TestRemoteAdmissionRefusesConversationReservedForTransfer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)
	digest := sha256.Sum256([]byte("activation"))
	if _, err := source.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: scope.ThreadID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing", ActivationHash: hex.EncodeToString(digest[:]), PrivateState: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var fenced *store.ThreadTransferError
	if _, err := source.AgentRemoteStart(ctx, input); !errors.As(err, &fenced) {
		t.Fatalf("reserved conversation started work: %v", err)
	}
	if receiver.calls.Load() != 0 {
		t.Fatal("fenced conversation reached destination")
	}
	if _, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fenced conversation registered a watch: %v", err)
	}
}

// A direct status reply settles the canonical receipt without its output. The
// completion then reads the saved log tail, so the notification carries the
// last lines instead of claiming nothing was retrieved.
func TestRemoteCompletionRecoversSavedLogTailAfterDirectStatus(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.remoteJobs.Close()
	manager, err := remotejobs.New(ctx, receiver.store, func(_ context.Context, _ string, _ []string, out io.Writer) (remotejobs.Outcome, error) {
		_, _ = io.WriteString(out, "first line\n"+strings.Repeat("x", 6000)+"\nfinal line\n")
		return remotejobs.Outcome{ExitCode: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver.remoteJobs = manager
	t.Cleanup(manager.Close)
	started, err := source.AgentRemoteStart(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		result, err := source.AgentRemoteStatus(ctx, input.ComputerID, input.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("destination did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || watch.Receipt.State != "succeeded" || watch.Receipt.Output != "" {
		t.Fatalf("direct status should settle the receipt without output: %+v %v", watch, err)
	}
	source.checkRemoteWatch(watch)
	rows := durableQueueRows(t, source, started.SourceThreadID)
	if len(rows) != 1 {
		t.Fatalf("completion queue: %+v", rows)
	}
	message := rows[0].Message
	if !strings.Contains(message, "first line") || !strings.Contains(message, "final line") || !strings.Contains(message, "Output omitted") || strings.Contains(message, "Output was not retrieved") || len(message) > 4<<10 {
		t.Fatalf("completion lost the saved head and tail: %s", message)
	}
}

// Deleting, archiving or moving a conversation stops the commands it still
// owns on the other computer and drops their pending notifications; the
// destination receipt records the cancellation.
func TestConversationLifecycleCancelsItsRemoteCommands(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, action := range []string{"delete", "archive", "move"} {
		t.Run(action, func(t *testing.T) {
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.remoteJobs.Close()
			manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, out io.Writer) (remotejobs.Outcome, error) {
				_, _ = io.WriteString(out, "still running")
				<-ctx.Done()
				return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			receiver.remoteJobs = manager
			t.Cleanup(manager.Close)
			started, err := source.AgentRemoteStart(ctx, input)
			if err != nil || started.State != "running" {
				t.Fatalf("start: %+v %v", started, err)
			}
			threadID := started.SourceThreadID
			switch action {
			case "delete":
				err = source.deleteThread(threadID)
			case "archive":
				err = archiveThread(source, threadID)
			case "move":
				prebuildTransferTemplate(t)
				source.configDir = t.TempDir()
				if err := source.startThreadTransfers(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(source.transfers.close)
				_, err = source.BeginThreadTransfer(ctx, threadID, uuid.NewString(), uuid.NewString(), "move", false)
			}
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			// Cancellation is acknowledged before the process group exits;
			// the receipt settles once it has.
			deadline := time.Now().Add(30 * time.Second)
			for {
				destination, err := receiver.store.GetRemoteJob(input.Request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if destination.State == "canceled" {
					break
				}
				if destination.State != "running" || time.Now().After(deadline) {
					t.Fatalf("destination job after %s: %+v", action, destination)
				}
				time.Sleep(time.Millisecond)
			}
			watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if action == "delete" {
				// The watch goes with its thread.
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("watch after delete: %+v %v", watch, err)
				}
			} else if err != nil || watch.Notification != "dismissed" {
				t.Fatalf("watch after %s: %+v %v", action, watch, err)
			}
			if pending, err := source.store.HasUnfinishedRemoteWatches(threadID); err != nil || pending {
				t.Fatalf("unfinished work remained after %s: %v %v", action, pending, err)
			}
		})
	}
}

func TestRemoteAdmissionLateRefusalCannotHideAcceptedReceipt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
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
	if err := source.RemoveBackend(input.ComputerID, false); err == nil || !strings.Contains(err.Error(), "awaiting completion") {
		t.Fatalf("forgot pending job's cancellation credentials: %v", err)
	}
	if _, err := source.AgentRemoteStatus(ctx, input.ComputerID, input.Request.ID); err != nil {
		t.Fatalf("refused removal damaged the connection: %v", err)
	}
	if err := source.store.QueueRemoteCompletion(input.ComputerID, input.Request.ID, store.FlushQueueItem{ID: uuid.NewString(), ThreadID: watch.ThreadID, Message: "finished", SendID: "remote-completion-test"}); err != nil {
		t.Fatal(err)
	}
	if err := source.RemoveBackend(input.ComputerID, false); err != nil {
		t.Fatalf("completion queue still prevented forgetting: %v", err)
	}
}

func TestRemoteAdmissionMissingSourceNeverDispatches(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
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
