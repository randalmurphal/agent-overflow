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
	"agent-overflow/internal/triage"
)

// promptRouted reports whether the registration under token is routed.
func promptRouted(w *workerPrompts, token string) bool {
	prompt, found := w.get(token)
	return found && prompt.state == workerPromptRouted
}

// A prompt registered again under a new token is settled by its current
// registration, whatever order the registry holds the two in: an earlier
// registration that was settled neither hides the later one from a
// resolution nor is settled again by a give-back of its own token.
func TestAWorkerPromptsCurrentRegistrationIsTheOneSettled(t *testing.T) {
	t.Parallel()
	open := func(triage.PromptRef) bool { return true }
	for i := range 64 {
		var prompts workerPrompts
		old, _ := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "a"}, open)
		if _, settled := prompts.settleToken(old.token); !settled {
			t.Fatal("the first registration was not settled")
		}
		next, replaced := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "b"}, open)
		if replaced == nil || replaced.token != old.token || replaced.state != workerPromptSettled {
			t.Fatalf("replaced = %+v, want the settled first registration", replaced)
		}
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

// A later registration of a prompt retires the earlier one: a routed one
// is closed and handed back for its wake to be withdrawn, and one being
// answered does not come back routed when its answer fails.
func TestALaterRegistrationRetiresTheEarlierOne(t *testing.T) {
	t.Parallel()
	open := func(triage.PromptRef) bool { return true }
	var prompts workerPrompts
	first, _ := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "a", sessionToken: "s1"}, open)
	second, replaced := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "b", sessionToken: "s2"}, open)
	if replaced == nil || replaced.token != first.token || replaced.agentID != "a" {
		t.Fatalf("replaced = %+v, want the first registration", replaced)
	}
	if promptRouted(&prompts, first.token) || !promptRouted(&prompts, second.token) {
		t.Fatal("the earlier registration is still routed, or the later one is not")
	}
	if err := prompts.beginAnswer(second.token, "allow"); err != nil {
		t.Fatal(err)
	}
	third, replaced := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "c", sessionToken: "s2"}, open)
	if replaced == nil || replaced.token != second.token || replaced.state != workerPromptAnswering {
		t.Fatalf("replaced = %+v, want the registration being answered, left to its answer", replaced)
	}
	prompts.endAnswer(second.token, workerPromptRouted)
	if promptRouted(&prompts, second.token) {
		t.Fatal("a replaced registration came back routed after its answer failed")
	}
	if !promptRouted(&prompts, third.token) {
		t.Fatal("the current registration is not routed")
	}
}

// The registry closes a routed registration whose prompt is no longer open
// in the session that asked it.
func TestAWorkerPromptOfAnEndedSessionIsClosed(t *testing.T) {
	t.Parallel()
	var prompts workerPrompts
	ended := map[string]bool{}
	open := func(ref triage.PromptRef) bool { return !ended[ref.SessionToken] }
	first, _ := prompts.add(workerPrompt{workerID: "worker", requestID: "approval-1", agentID: "a", sessionToken: "s1"}, open)
	ended["s1"] = true
	prompts.add(workerPrompt{workerID: "other", requestID: "approval-9", agentID: "a", sessionToken: "s9"}, open)
	if promptRouted(&prompts, first.token) {
		t.Fatal("a prompt of an ended session is still routed")
	}
}

// workerPromptFixture is a thread, an assistant's or an ordinary one, that
// spawned a worker whose approval prompt triage routed to it. Triage emits
// through the app's funnel, so the emit tap delivers each announcement.
type workerPromptFixture struct {
	app       *App
	assistant store.Thread
	worker    store.Thread
	request   provider.ApprovalRequest
}

func newWorkerPromptFixture(t *testing.T, id string, standing bool) workerPromptFixture {
	t.Helper()
	a, _ := newAppForFlushQueueRPC(t)
	a.triage = triage.NewRouter(a.store, a.emit)
	a.configureTriageQueueCallbacks()
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
			f.drain(t)
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

// A prompt delivered to a thread New Thread, Same Assistant superseded
// after triage routed it there moves on with its route rather than being
// queued under the old thread's name: the successor holds one wake and
// answers it, the old thread may not, and an answer in the worker withdraws
// the successor's wake.
func TestAWorkerPromptStampedBeforeAHandoffBelongsToTheSuccessor(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "stamped-before", true)
	f.drain(t)
	// Taken in, the prompt has no wake for the handoff to give back, so
	// triage still routes it to the old thread when the delivery runs: once
	// the handoff committed and before it moved anything on.
	f.takeIn(t, f.assistant.ID)
	f.app.handOffCommittedForTest = func(old, successor string) {
		request := f.request
		f.app.deliverWorkerPrompt(provider.PromptRoute{ThreadID: old}, workerPrompt{
			workerID: f.worker.ID, requestID: request.RequestID, approval: &request,
		})
		// Nothing is queued under the old thread's name: the successor
		// could not answer it.
		if tokens := workerPromptTokens(t, f.app, successor); len(tokens) != 0 {
			t.Errorf("the successor holds prompt wakes %q before the prompt moved to it", tokens)
		}
	}
	next := f.handOff(t, f.assistant.ID)
	if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) != 0 {
		t.Fatalf("the superseded thread holds prompt wakes %q", tokens)
	}
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
	if !promptRouted(&f.app.workerPrompts, token) {
		t.Fatal("the prompt is no longer routed after a reply that did not reach the worker")
	}
	f.app.settleWorkerPrompt(f.worker.ID, f.request.RequestID)
	if durable, _ := queuedSendIDs(t, f.app, next.ID); slices.Contains(durable, sendID) {
		t.Fatalf("the successor still holds the wake of a prompt the user answered: %q", durable)
	}
}

// takeIn removes the one worker prompt wake threadID holds from its queue,
// as its agent taking it in does, and returns its token.
func (f workerPromptFixture) takeIn(t *testing.T, threadID string) string {
	t.Helper()
	sendID := f.queuedPrompt(t, threadID)
	if removal, err := f.app.removeQueuedItem(t.Context(), threadID, sendID); err != nil || removal != queueRemovalRemoved {
		t.Fatalf("take in %s: %v, %v", sendID, removal, err)
	}
	return strings.TrimPrefix(sendID, workerPromptSendIDRoot)
}

// routedTo is where each of the worker's open prompts is routed, by request
// id; "" is the user.
func (f workerPromptFixture) routedTo(t *testing.T) map[string]string {
	t.Helper()
	pending := f.app.triage.PendingInteractiveRequests(f.worker.ID)
	out := map[string]string{}
	for _, request := range pending.Approvals {
		out[request.RequestID] = ""
		if request.RoutedTo != nil {
			out[request.RequestID] = request.RoutedTo.ThreadID
		}
	}
	for _, request := range pending.UserInputs {
		out[request.RequestID] = ""
		if request.RoutedTo != nil {
			out[request.RequestID] = request.RoutedTo.ThreadID
		}
	}
	return out
}

// ask puts a question to the worker's user, as its provider does.
func (f workerPromptFixture) ask(t *testing.T, requestID string) {
	t.Helper()
	meta, err := json.Marshal(provider.UserInputRequest{
		RequestID: requestID, ThreadID: f.worker.ID, ToolName: "user_input",
		Questions: []provider.UserInputQuestion{{ID: "q", Header: "Scope", Question: "Which module?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserInputRequest, ThreadID: f.worker.ID, ItemID: requestID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
}

// A prompt the assistant's thread already took in and left unanswered goes
// on to its next thread with the handoff: triage routes it there, the
// successor holds its wake under a new token and may answer it, and the old
// thread's token answers nothing.
func TestATakenInWorkerPromptFollowsTheHandoff(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "taken-in-handoff", true)
	f.drain(t)
	old := f.takeIn(t, f.assistant.ID)
	next := f.handOff(t, f.assistant.ID)
	if routes := f.routedTo(t); routes[f.request.RequestID] != next.ID {
		t.Fatalf("the worker's prompt is routed %q, want the successor %s", routes, next.ID)
	}
	carried := strings.TrimPrefix(f.queuedPrompt(t, next.ID), workerPromptSendIDRoot)
	if carried == old {
		t.Fatal("the successor holds the old thread's token")
	}
	reply := threadtools.ReplyCall{Token: old, Decision: threadtools.DecisionAllow, Text: "go ahead"}
	if _, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.assistant.ID}, reply); !handled || !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the old thread's reply: handled %v, %v; want the token closed", handled, err)
	}
	reply.Token = carried
	_, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: next.ID}, reply)
	if code, _, _ := errorsx.PublicDetails(err); !handled || code == threadtools.CodeRequestNotYours || errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the successor's reply: handled %v, %v; want it taken as its own", handled, err)
	}
}

// An ordinary spawner archived or deleted after taking in its worker's
// prompts leaves a worker that keeps running, whose open prompts, an
// approval and a question, go back to the user.
func TestATakenInWorkerPromptGoesBackWhenItsSpawnerLeaves(t *testing.T) {
	t.Parallel()
	for _, leave := range []string{"archive", "delete"} {
		t.Run(leave, func(t *testing.T) {
			t.Parallel()
			f := newWorkerPromptFixture(t, "taken-in-"+leave, false)
			if _, err := f.app.store.SetThreadPlacement([]string{f.worker.ID}, store.PlacementStandalone); err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			f.takeIn(t, f.assistant.ID)
			f.ask(t, "question-1")
			question := f.takeIn(t, f.assistant.ID)
			var err error
			if leave == "archive" {
				err = f.app.ArchiveThread(f.assistant.ID)
			} else {
				err = f.app.DeleteThread(f.assistant.ID)
			}
			if err != nil {
				t.Fatalf("%s the spawner: %v", leave, err)
			}
			f.drain(t)
			if routes := f.routedTo(t); len(routes) != 2 || routes[f.request.RequestID] != "" || routes["question-1"] != "" {
				t.Fatalf("the worker's prompts are routed %q after the spawner's %s, want both open and the user's", routes, leave)
			}
			if prompt, _ := f.app.workerPrompts.get(question); prompt.state != workerPromptSettled {
				t.Fatalf("the question's registration is %v after the spawner's %s, want it closed", prompt.state, leave)
			}
		})
	}
}
