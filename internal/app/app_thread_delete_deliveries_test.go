package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A delete's claim fences a thread while the delete decides: what is owed
// to it meanwhile is deferred, not lost, and arrives once the delete is
// refused. A committed delete is the thread's end, and nothing owed to it
// is delivered.

// errRefusedForTest refuses a delete at its verify (deleteRequest.admit).
var errRefusedForTest = errors.New("refused for the test")

// refusedDelete runs a delete of threadID that is refused at its verify,
// with during run once its claim is written.
func refusedDelete(t *testing.T, a *App, threadID string, during func()) {
	t.Helper()
	a.deleteClaimedForTest = func([]string) { during() }
	_, err := a.deleteThreads(deleteRequest{seeds: []string{threadID}, admit: func(string) error { return errRefusedForTest }})
	a.deleteClaimedForTest = nil
	if !errors.Is(err, errRefusedForTest) {
		t.Fatalf("the delete = %v, want it refused", err)
	}
	untouched(t, a, threadID)
}

// committedDelete runs a delete of threadID, with during run once it
// committed, before it destroys anything.
func committedDelete(t *testing.T, a *App, threadID string, during func()) {
	t.Helper()
	a.deleteCommittedForTest = func([]string) { during() }
	result, err := a.DeleteThread(threadID)
	a.deleteCommittedForTest = nil
	if err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("the delete = %+v, %v", result, err)
	}
}

// An explicit reply to a sender that asked for no notification, written
// while a delete holds the sender's claim, arms its wake: the fence refuses
// the delivery, and once the delete is refused the wake's retry delivers
// it. Written once the delete committed, it arms nothing.
func TestAReplyDuringARefusedDeleteIsDelivered(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"refused", "committed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "starting on it")
			f.holdCallerTurn(t)
			ack := f.unattendedRunningRequest(t, "which items block the merge?")
			// A thread outside the sender's delete, which can still reply.
			if _, err := f.app.store.SetThreadPlacement([]string{ack.ThreadID}, store.PlacementStandalone); err != nil {
				t.Fatal(err)
			}
			reply := func() {
				if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
					Token: ack.Token, Text: "three items block it",
				}); err != nil {
					t.Errorf("Reply: %v", err)
				}
				f.app.threadRequestSettling.Wait()
			}
			if outcome == "committed" {
				committedDelete(t, f.app, f.caller.ID, func() {
					reply()
					if row := f.request(t, ack.Token); row.Notify {
						t.Errorf("a reply to a committed sender armed its wake: %+v", row)
					}
				})
				f.nothingOwed(t)
				return
			}
			refusedDelete(t, f.app, f.caller.ID, reply)
			if row := f.request(t, ack.Token); !row.Notify || row.State != store.ThreadRequestReplied {
				t.Fatalf("request = %+v, want the reply stored with its wake armed", row)
			}
			waitUntil(t, 10*time.Second, func() bool {
				f.app.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
				f.app.threadRequestSettling.Wait()
				return f.request(t, ack.Token).DeliveredAt != 0
			})
			wake := awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(ack.Token))
			if !strings.Contains(wake.Message, "three items block it") {
				t.Fatalf("wake = %q, want the reply", wake.Message)
			}
		})
	}
}

// A loop that fires while a delete holds its thread's claim is not fired
// again while the claim stands, and fires once the delete is refused. One
// that fires once the delete committed is not fired again.
func TestALoopFireDuringARefusedDeleteIsDelivered(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"refused", "committed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newDeleteFixture(t)
			thread := f.thread(t, "looping")
			// The wake stays queued: no session starts to take it.
			f.app.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
			armLoop(t, f.app, thread.ID)
			wakes, err := f.app.store.ListAgentWakes(thread.ID)
			if err != nil || len(wakes) != 1 {
				t.Fatalf("wakes = %+v, %v", wakes, err)
			}
			loop := wakes[0]
			fired := func() []store.FlushQueueItem {
				var out []store.FlushQueueItem
				for _, row := range durableQueueRows(t, f.app, thread.ID) {
					if strings.HasPrefix(row.SendID, agentWakeSendIDPrefix(loop.ID)) {
						out = append(out, row)
					}
				}
				return out
			}
			fire := func() {
				now := time.Now()
				if !f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}) {
					t.Error("the fire did not start")
					return
				}
				waitUntil(t, 10*time.Second, func() bool {
					row, found, err := f.app.store.GetAgentWake(loop.ID)
					return err == nil && found && row.State == store.AgentWakeArmed
				})
				if got := fired(); len(got) != 0 {
					t.Errorf("a fire reached the queue of a thread a delete holds: %+v", got)
				}
				due, err := f.app.store.DueAgentWakes(time.Now().Add(time.Hour).UnixMilli(), 32)
				if err != nil || len(due) != 0 {
					t.Errorf("due while the delete holds the thread = %+v, %v; want none", due, err)
				}
				// Nor does the sweep wake for it.
				if next, err := f.app.store.NextAgentWakeDue(); err != nil || next.Set {
					t.Errorf("next fire while the delete holds the thread = %+v, %v; want none", next, err)
				}
			}
			if outcome == "committed" {
				committedDelete(t, f.app, thread.ID, fire)
				return
			}
			refusedDelete(t, f.app, thread.ID, fire)
			f.app.fireDueAgentWakes(time.Now())
			waitUntil(t, 10*time.Second, func() bool { return len(fired()) == 1 })
		})
	}
}

// A long-running watch's output written while a delete holds its thread's
// claim is held, as a wake still queued holds it, and reaches the thread
// once the delete is refused.
func TestALongRunningWatchsOutputDuringARefusedDeleteIsDelivered(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	stubNoticeStarts(a)
	thread := noticeTestThread(t, a, "watching")
	testAssistant(t, a, thread.ID)
	release := filepath.Join(t.TempDir(), "release")
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "build",
		Command: "while [ ! -e '" + release + "' ]; do sleep 0.05; done; echo built; exec sleep 60", LongRunning: true})
	fired := func() int {
		count := 0
		for _, row := range durableQueueRows(t, a, thread.ID) {
			if strings.HasPrefix(row.SendID, agentWakeSendIDPrefix(watch.ID)) {
				count++
			}
		}
		return count
	}
	refusedDelete(t, a, thread.ID, func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// Long enough for the batch to be tried, and tried again.
		time.Sleep(3 * agentWakeBatchWindow)
		if n := fired(); n != 0 {
			t.Errorf("%d wakes reached the queue of a thread a delete holds", n)
		}
	})
	waitUntil(t, 10*time.Second, func() bool { return fired() == 1 })
	row, _, err := a.store.GetAgentWake(watch.ID)
	if err != nil || row.Undelivered != "" {
		t.Fatalf("watch = %+v, %v; want nothing recorded undelivered", row, err)
	}
}

// A worker's prompt routed to its spawner while a delete holds the
// spawner's claim stays routed there, and its wake reaches the spawner once
// the delete is refused. One routed there once the delete committed goes to
// the user.
func TestAWorkerPromptDuringARefusedDeleteIsDelivered(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"refused", "committed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppForFlushQueueRPC(t)
			a.triage = triage.NewRouter(a.store, a.emit)
			a.configureTriageQueueCallbacks()
			a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
			a.triage.SetPromptRouter(a.routeWorkerPrompt)
			spawner := noticeTestThread(t, a, "deleted-spawner")
			worker := noticeTestThread(t, a, "deleted-spawner-worker")
			if err := a.store.InsertThreadRequest(store.ThreadRequest{
				Token: "deleted-spawner-spawn", CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
				Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
			}); err != nil {
				t.Fatal(err)
			}
			drain := func() {
				for range 3 {
					if err := a.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
						t.Fatal(err)
					}
					a.waitAgentNoticeWork()
				}
			}
			ask := func() {
				request := provider.ApprovalRequest{RequestID: "approval-1", ThreadID: worker.ID, ToolName: "Bash", Description: "make deploy", Kind: "command"}
				meta, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if err := a.triage.Handle(provider.ProviderEvent{
					Kind: provider.EventApprovalRequest, ThreadID: worker.ID, ItemID: request.RequestID, Meta: meta, Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("approval request: %v", err)
				}
				drain()
				if tokens := workerPromptTokens(t, a, spawner.ID); len(tokens) != 0 {
					t.Errorf("a prompt reached the queue of a thread a delete holds: %q", tokens)
				}
			}
			routedTo := func() *provider.PromptRoute {
				pending := a.triage.PendingInteractiveRequests(worker.ID).Approvals
				if len(pending) != 1 {
					t.Fatalf("the worker's prompts = %+v, want one open", pending)
				}
				return pending[0].RoutedTo
			}
			if outcome == "committed" {
				committedDelete(t, a, spawner.ID, ask)
				drain()
				if route := routedTo(); route != nil {
					t.Errorf("a prompt to a deleted spawner is routed to %+v, want the user", route)
				}
				return
			}
			refusedDelete(t, a, spawner.ID, ask)
			drain()
			if route := routedTo(); route == nil || route.ThreadID != spawner.ID {
				t.Fatalf("the prompt is routed to %+v, want the spawner", route)
			}
			if tokens := workerPromptTokens(t, a, spawner.ID); len(tokens) != 1 {
				t.Fatalf("the spawner's prompt wakes = %q, want the prompt's", tokens)
			}
		})
	}
}
