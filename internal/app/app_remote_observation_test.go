package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
)

// The source watcher is deliberately never started: a successful direct RPC
// must update remote_jobs and the background tray before that tool returns.
func TestRemoteCommandRepliesImmediatelyUpdateSourceWatch(t *testing.T) {
	for _, method := range []string{"status", "wait", "retry", "cancel", "tray-cancel"} {
		t.Run(method, func(t *testing.T) {
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.remoteJobs.Close()
			finish := make(chan struct{})
			manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, out io.Writer) (int, error) {
				select {
				case <-finish:
					_, _ = io.WriteString(out, "finished output")
					return 0, nil
				case <-ctx.Done():
					return -1, ctx.Err()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			receiver.remoteJobs = manager
			t.Cleanup(manager.Close)
			rec := &emitRecorder{}
			source.testEmitHook = rec.capture
			started, err := source.AgentRemoteStart(ctx, input)
			if err != nil || started.State != "running" {
				t.Fatalf("start: %+v %v", started, err)
			}
			close(finish)
			deadline := time.Now().Add(3 * time.Second)
			for {
				destination, err := receiver.store.GetRemoteJob(input.Request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if destination.State != "running" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("destination did not settle")
				}
				time.Sleep(time.Millisecond)
			}
			watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if err != nil || watch.Receipt.State != "running" {
				t.Fatalf("source already observed completion: %+v %v", watch, err)
			}
			var result RemoteCommand
			switch method {
			case "status":
				result, err = source.AgentRemoteStatus(ctx, input.ComputerID, input.Request.ID)
			case "wait":
				wait := 2.0
				var mcp remoteMCPResult
				mcp, err = source.waitRemoteResult(ctx, input.ComputerID, started, remoteResultOptions{WaitSeconds: &wait})
				result = mcp.RemoteCommand
			case "retry":
				result, err = source.AgentRemoteStart(ctx, input)
			case "cancel":
				result, err = source.AgentRemoteCancel(ctx, input.ComputerID, input.Request.ID)
			case "tray-cancel":
				result, err = source.CancelThreadRemoteCommand(ctx, started.SourceThreadID, input.ComputerID, input.Request.ID)
			}
			if err != nil || result.State != "succeeded" {
				t.Fatalf("terminal reply: %+v %v", result, err)
			}
			watch, err = source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
			if err != nil || watch.Receipt.State != result.State || watch.Receipt.Output != "" || watch.Notification != "pending" || watch.NextCheck != 0 {
				t.Fatalf("completion not immediately observed without log retention or delivery loss: %+v %v", watch, err)
			}
			items, err := source.remoteTrayItems(started.SourceThreadID, 0)
			if err != nil || len(items) != 2 || items[1].Kind != "tool_completion" || items[1].Status != "completed" {
				t.Fatalf("tray still running: %+v %v", items, err)
			}
			var changes int
			for _, call := range rec.snapshot() {
				if call.Channel == eventchan.ProviderBackgroundTasksChanged.String() {
					changes++
				}
			}
			if changes != 2 {
				t.Fatalf("want acceptance + completion invalidation, got %d", changes)
			}
			// Older in-flight status replies cannot move a completed source back
			// to running or emit a second refresh for unchanged state.
			if err := source.observeRemoteCommand(input.ComputerID, input.Request.ID, started.SourceThreadID, started); err != nil {
				t.Fatal(err)
			}
			if err := source.observeRemoteCommand(input.ComputerID, input.Request.ID, started.SourceThreadID, result); err != nil {
				t.Fatal(err)
			}
			if len(rec.snapshot()) != changes {
				t.Fatal("unchanged or stale receipt emitted another refresh")
			}
		})
	}
}

func TestRemoteCompletionUsesCanonicalReceiptWithoutDependingOnDestination(t *testing.T) {
	for _, lateReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "destination unavailable", true: "stale terminal reply"}[lateReply], func(t *testing.T) {
			a, rec := newAppForFlushQueueRPC(t)
			thread := remoteWatchThread(t, a, "claude")
			installCapturingClaudeSession(t, a, thread, filepath.Join(t.TempDir(), "provider.ndjson"))
			if err := a.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 0, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			w := completedRemoteWatch(t, a, thread)
			if lateReply {
				w.Receipt.State = "failed"
				w.Receipt.Output = "obsolete output"
				if err := a.deliverRemoteCompletion(context.Background(), w, false); err != nil {
					t.Fatal(err)
				}
			} else {
				// No peer manager exists. Reprobing instead of trusting the
				// durable receipt would panic and prevent notification delivery.
				a.checkRemoteWatch(w)
			}
			flushed := waitForAtLeastQueueFlushed(t, rec, 1)
			if len(flushed) != 1 || len(flushed[0].Items) != 1 {
				t.Fatalf("completion queue: %+v", flushed)
			}
			message := flushed[0].Items[0].Message
			if !strings.Contains(message, "Result: succeeded") || !strings.Contains(message, "Output was not retrieved") || strings.Contains(message, "obsolete output") {
				t.Fatalf("noncanonical or misleading notification: %s", message)
			}
		})
	}
}

// A thread action lock another operation holds costs a completion one poll,
// never the whole watcher tick: the wait is bounded by the check context and
// the watch stays pending for the next pass.
func TestRemoteCompletionDeliveryWaitIsBounded(t *testing.T) {
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, a, string(provider.Codex))
	w := completedRemoteWatch(t, a, thread)
	unlock := a.threadLocks().Lock(thread.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := a.deliverRemoteCompletion(ctx, w, false)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > 2*time.Second {
		t.Fatalf("held action lock did not bound delivery: %v after %s", err, time.Since(began))
	}
	if saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || saved.Notification != "pending" {
		t.Fatalf("deferred delivery changed the watch: %+v %v", saved, err)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 0 {
		t.Fatalf("queued without the lock: %+v", rows)
	}
	unlock()
	if err := a.deliverRemoteCompletion(context.Background(), w, false); err != nil {
		t.Fatal(err)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 {
		t.Fatalf("released lock did not deliver: %+v", rows)
	}
}

func TestRemoteObservationRejectsWrongRequestAndConversation(t *testing.T) {
	a, rec := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, "codex")
	w := registeredRemoteWatch(t, a, thread)
	for _, receipt := range []RemoteCommand{
		{ID: "different-request", SourceThreadID: thread.ID, State: "succeeded"},
		{ID: w.RequestID, SourceThreadID: "different-thread", State: "succeeded"},
	} {
		if err := a.observeRemoteCommand(w.ComputerID, w.RequestID, thread.ID, receipt); err == nil {
			t.Fatal("accepted mismatched receipt")
		}
	}
	got, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || got.Receipt != (store.RemoteJob{}) || len(rec.snapshot()) != 0 {
		t.Fatalf("mismatched receipt changed watch: %+v %v", got, err)
	}
}
