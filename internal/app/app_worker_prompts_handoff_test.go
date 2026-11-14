package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A prompt registered again under a new token is settled by its current
// registration, whatever order the registry holds the two in: an earlier
// registration that was settled neither hides the later one from a
// resolution nor is settled again by a give-back of its own token.
func TestAWorkerPromptsCurrentRegistrationIsTheOneSettled(t *testing.T) {
	t.Parallel()
	open := func(string, string) bool { return true }
	for i := range 64 {
		var prompts workerPrompts
		old := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "a"}, open)
		if _, settled := prompts.settleToken(old.token); !settled {
			t.Fatal("the first registration was not settled")
		}
		next := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "b"}, open)
		if _, settled := prompts.settleToken(old.token); settled {
			t.Fatal("a settled registration was settled again")
		}
		if i%2 == 0 {
			prompt, settled := prompts.settle("worker", "approval-1")
			if !settled || prompt.token != next.token {
				t.Fatalf("resolution settled %+v (%v), want the current registration %s", prompt, settled, next.token)
			}
		} else if prompt, settled := prompts.settleToken(next.token); !settled || prompt.agentID != "b" {
			t.Fatalf("give-back settled %+v (%v), want the current registration", prompt, settled)
		}
	}
}

// workerPromptFixture is a thread, an assistant's or an ordinary one, that
// spawned a worker whose approval prompt triage routed to it.
type workerPromptFixture struct {
	app       *App
	assistant store.Thread
	worker    store.Thread
	request   provider.ApprovalRequest
}

func newWorkerPromptFixture(t *testing.T, id string, standing bool) workerPromptFixture {
	t.Helper()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	assistant := noticeTestThread(t, a, id+"-assistant")
	if standing {
		testAssistant(t, a, assistant.ID)
	}
	worker := noticeTestThread(t, a, id+"-worker")
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: id + "-spawn", CallerThreadID: assistant.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatalf("record the spawn: %v", err)
	}
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
	if pending := a.triage.PendingInteractiveRequests(worker.ID).Approvals; len(pending) != 1 || pending[0].RoutedTo == nil || pending[0].RoutedTo.ThreadID != assistant.ID {
		t.Fatalf("the worker's prompt = %+v, want it open and routed to its spawner", pending)
	}
	return workerPromptFixture{app: a, assistant: assistant, worker: worker, request: request}
}

// deliver runs what the emit tap queues for a prompt triage stamped with
// route, and waits for it.
func (f workerPromptFixture) deliver(t *testing.T, route string) {
	t.Helper()
	request := f.request
	f.app.workerPrompts.queue.Go(func() {
		f.app.deliverWorkerPrompt(provider.PromptRoute{ThreadID: route}, f.worker.ID, request.RequestID, &request, nil)
	})
	f.drain(t)
}

func (f workerPromptFixture) drain(t *testing.T) {
	t.Helper()
	for range 3 {
		if err := f.app.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		f.app.waitAgentNoticeWork()
	}
}

// queuedPrompt is the send id of the one worker prompt wake threadID holds.
func (f workerPromptFixture) queuedPrompt(t *testing.T, threadID string) string {
	t.Helper()
	durable, _ := queuedSendIDs(t, f.app, threadID)
	var prompts []string
	for _, id := range durable {
		if strings.HasPrefix(id, workerPromptSendIDRoot) {
			prompts = append(prompts, id)
		}
	}
	if len(prompts) != 1 {
		t.Fatalf("%s holds prompt wakes %q, want one", threadID, prompts)
	}
	return prompts[0]
}

// handOff replaces the assistant's current thread with a new one.
func (f workerPromptFixture) handOff(t *testing.T, from string) store.Thread {
	t.Helper()
	next, err := f.app.nextAssistantThread(context.Background(), from, false)
	if err != nil {
		t.Fatalf("New Thread, Same Assistant: %v", err)
	}
	f.drain(t)
	return next
}

// A prompt the handoff carried to the successor goes back to the user when
// the successor's wake is withdrawn, however the registry orders the
// prompt's two registrations.
func TestAHandedOnWorkerPromptGoesBackWhenItsWakeIsWithdrawn(t *testing.T) {
	t.Parallel()
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			f := newWorkerPromptFixture(t, fmt.Sprintf("handed-on-%d", i), true)
			f.deliver(t, f.assistant.ID)
			first := f.queuedPrompt(t, f.assistant.ID)
			next := f.handOff(t, f.assistant.ID)
			carried := f.queuedPrompt(t, next.ID)
			if carried == first {
				t.Fatal("the successor holds the old registration's wake")
			}
			stop, err := f.app.StopThreadAgentIncoming(t.Context(), next.ID, carried)
			if err != nil || stop.Outcome != threadAgentStopped {
				t.Fatalf("withdraw = %+v, %v", stop, err)
			}
			f.drain(t)
			if durable, _ := queuedSendIDs(t, f.app, next.ID); slices.Contains(durable, carried) {
				t.Fatalf("the withdrawn wake is still queued: %q", durable)
			}
			if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals; len(pending) != 1 || pending[0].RoutedTo != nil {
				t.Fatalf("the worker's prompt = %+v after its wake was withdrawn, want it open and back with the user", pending)
			}
		})
	}
}

// A prompt triage stamped with a thread New Thread, Same Assistant
// superseded before the prompt's delivery ran belongs to the thread its
// wake went to: the successor answers it, the old thread may not, and an
// answer in the worker withdraws the successor's wake.
func TestAWorkerPromptStampedBeforeAHandoffBelongsToTheSuccessor(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "stamped-before", true)
	next := f.handOff(t, f.assistant.ID)
	f.deliver(t, f.assistant.ID)
	sendID := f.queuedPrompt(t, next.ID)
	token := strings.TrimPrefix(sendID, workerPromptSendIDRoot)
	reply := threadtools.ReplyCall{Token: token, Decision: "allow", Text: "go ahead"}

	_, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.assistant.ID}, reply)
	if code, _, _ := errorsx.PublicDetails(err); !handled || code != threadtools.CodeRequestNotYours {
		t.Fatalf("the superseded thread's reply: handled %v, %v; want refused as not its own", handled, err)
	}
	_, handled, err = f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: next.ID}, reply)
	if code, _, _ := errorsx.PublicDetails(err); !handled || code == threadtools.CodeRequestNotYours {
		t.Fatalf("the successor's reply: handled %v, %v; want it taken as its own", handled, err)
	}
	// The answer did not reach the worker, which has no session, so the
	// prompt is still open; the user answers it in the worker.
	if !f.app.workerPrompts.isRouted(token) {
		t.Fatal("the prompt is no longer routed after a reply that did not reach the worker")
	}
	f.app.settleWorkerPrompt(f.worker.ID, f.request.RequestID)
	if durable, _ := queuedSendIDs(t, f.app, next.ID); slices.Contains(durable, sendID) {
		t.Fatalf("the successor still holds the wake of a prompt the user answered: %q", durable)
	}
}
