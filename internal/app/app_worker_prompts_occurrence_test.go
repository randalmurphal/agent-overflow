package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// askApproval opens an approval under requestID on the fixture's worker, in
// the session sessionToken names, and waits for its delivery.
func (f workerPromptFixture) askApproval(t *testing.T, requestID, sessionToken string) {
	t.Helper()
	meta, err := json.Marshal(provider.ApprovalRequest{RequestID: requestID, ThreadID: f.worker.ID, ToolName: "Bash", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: f.worker.ID, ItemID: requestID, Meta: meta,
		Timestamp: time.Now(), SessionToken: sessionToken,
	}); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
}

// resolveFromProvider resolves the worker's prompt under requestID as its
// provider does, without an answer from this app.
func (f workerPromptFixture) resolveFromProvider(t *testing.T, requestID, sessionToken string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"requestId": requestID, "decision": "declined"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalResolved, ThreadID: f.worker.ID, ItemID: requestID, Meta: meta,
		Timestamp: time.Now(), SessionToken: sessionToken,
	}); err != nil {
		t.Fatal(err)
	}
}

// A provider can ask twice under one request id in one live session, once
// the first prompt is resolved. The first prompt's token answers nothing of
// the second, even before the first one's settlement has run.
func TestAPromptTokenCannotAnswerALaterPromptOfTheSameSession(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "same-session", false)
	f.drain(t)
	first := strings.TrimPrefix(f.queuedPrompt(t, f.assistant.ID), workerPromptSendIDRoot)
	// The registry's queue is held, so the first prompt's settlement and the
	// second's registration have not run when the old token is answered.
	release := make(chan struct{})
	f.app.workerPrompts.queue.Go(func() { <-release })
	f.resolveFromProvider(t, f.request.RequestID, "")
	meta, err := json.Marshal(provider.ApprovalRequest{RequestID: f.request.RequestID, ThreadID: f.worker.ID, ToolName: "Bash", Description: "rm -rf build", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: f.worker.ID, ItemID: f.request.RequestID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.assistant.ID},
		threadtools.ReplyCall{Token: first, Decision: threadtools.DecisionAllow, Text: "go"})
	close(release)
	if !handled || !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the first prompt's token: handled %v, %v; want it closed", handled, err)
	}
	if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals; len(pending) != 1 || pending[0].Description != "rm -rf build" {
		t.Fatalf("the worker's prompts = %+v, want the second one open", pending)
	}
}

// workerPromptsOn routes the prompts of an app's workers to their spawners
// through the app's emit funnel, as the running app does, with no session
// to start for a spawner.
type workerPromptsOn struct{ app *App }

func newWorkerPromptsOn(t *testing.T, a *App) workerPromptsOn {
	t.Helper()
	a.triage = triage.NewRouter(a.store, a.emit)
	a.configureTriageQueueCallbacks()
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	return workerPromptsOn{app: a}
}

func (p workerPromptsOn) drain(t *testing.T) {
	t.Helper()
	workerPromptFixture{app: p.app}.drain(t)
}

// ask opens an approval and a question on workerID, as its provider does,
// and waits for their deliveries.
func (p workerPromptsOn) ask(t *testing.T, workerID, approvalID, questionID string) {
	t.Helper()
	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: approvalID, ThreadID: workerID, ToolName: "Bash", Kind: "command"})
	if err != nil {
		t.Fatal(err)
	}
	question, err := json.Marshal(provider.UserInputRequest{
		RequestID: questionID, ThreadID: workerID, ToolName: "user_input",
		Questions: []provider.UserInputQuestion{{ID: "q", Header: "Scope", Question: "Which module?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.ProviderEvent{
		{Kind: provider.EventApprovalRequest, ThreadID: workerID, ItemID: approvalID, Meta: approval, Timestamp: time.Now()},
		{Kind: provider.EventUserInputRequest, ThreadID: workerID, ItemID: questionID, Meta: question, Timestamp: time.Now()},
	} {
		if err := p.app.triage.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	p.drain(t)
}

// routes is where each of workerID's open prompts is routed, by request id;
// "" is the user.
func (p workerPromptsOn) routes(t *testing.T, workerID string) map[string]string {
	t.Helper()
	return workerPromptFixture{app: p.app, worker: store.Thread{ID: workerID}}.routedTo(t)
}

// An answer to a session's prompt that completes after a replacement
// session asked under the same request id touches nothing of the
// replacement's prompt: its success does not resolve it or settle its
// registration, and its failure does not release the claim another answer
// holds on it.
func TestALateAnswerToAnEndedSessionLeavesTheReplacementsPrompt(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "late-answer", false)
	f.app.triage.CleanupThread(f.worker.ID)
	f.app.triage.MarkThreadActive(f.worker.ID, "s1")
	f.askApproval(t, "req-7", "s1")
	first, _ := f.app.workerPrompts.get(strings.TrimPrefix(f.queuedPrompt(t, f.assistant.ID), workerPromptSendIDRoot))
	// The answer to s1's prompt has been admitted: it holds the claim.
	late, claimed := f.app.claimPrompt(f.worker.ID, "req-7", first.occurrence)
	if !claimed {
		t.Fatal("the first session's prompt could not be claimed")
	}
	f.app.triage.CleanupThread(f.worker.ID)
	f.app.triage.MarkThreadActive(f.worker.ID, "s2")
	f.askApproval(t, "req-7", "s2")
	tokens := workerPromptTokens(t, f.app, f.assistant.ID)
	if len(tokens) != 1 {
		t.Fatalf("the spawner holds prompt wakes %q, want the replacement's", tokens)
	}
	second := tokens[0]
	// Someone answers the replacement's prompt and holds its claim.
	if _, ok := f.app.claimPrompt(f.worker.ID, "req-7", 0); !ok {
		t.Fatal("the replacement's prompt could not be claimed")
	}

	// The late answer fails: its release leaves the replacement's claim.
	f.app.releasePrompt(late)
	if _, ok := f.app.claimPrompt(f.worker.ID, "req-7", 0); ok {
		t.Fatal("the late answer's release dropped the claim on the replacement's prompt")
	}
	// The late answer succeeds: its resolution leaves the replacement's prompt.
	f.app.emitApprovalResolution(f.worker.ID, late.Ref.Occurrence, "req-7", "approved", nil)
	f.drain(t)
	if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals; len(pending) != 1 {
		t.Fatalf("the replacement's prompt = %+v after a late answer to the ended session, want it open", pending)
	}
	if !promptRouted(&f.app.workerPrompts, second) {
		t.Fatal("the late answer settled the replacement's registration")
	}
}

// openPromptRef names the prompt open on threadID under requestID.
func openPromptRef(t *testing.T, router *triage.Router, threadID, requestID string) triage.PromptRef {
	t.Helper()
	pending := router.PendingInteractiveRequests(threadID)
	for _, request := range pending.Approvals {
		if request.RequestID == requestID {
			return triage.PromptRef{ThreadID: threadID, RequestID: requestID, Occurrence: request.Occurrence}
		}
	}
	for _, request := range pending.UserInputs {
		if request.RequestID == requestID {
			return triage.PromptRef{ThreadID: threadID, RequestID: requestID, Occurrence: request.Occurrence}
		}
	}
	t.Fatalf("no prompt open on %s under %s", threadID, requestID)
	return triage.PromptRef{}
}

// A user's answer goes only to the session that asked: a prompt a
// replacement session's start left open is that ended session's, and the
// answer is refused rather than written to the replacement.
func TestAUsersAnswerIsWrittenOnlyToTheSessionThatAsked(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "bound-answer", false)
	f.app.triage.CleanupThread(f.worker.ID)
	f.app.triage.MarkThreadActive(f.worker.ID, "s1")
	f.askApproval(t, "req-1", "s1")
	f.app.triage.MarkThreadActive(f.worker.ID, "s2")
	f.app.sessionManager().put(f.worker.ID, session{Provider: string(provider.Claude), Token: "s2"})
	if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals; len(pending) != 1 {
		t.Fatalf("the ended session's prompt = %+v, want it still listed", pending)
	}
	err := f.app.RespondToApproval(t.Context(), f.worker.ID, provider.ApprovalResponse{RequestID: "req-1", Decision: "accept"})
	if !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("an answer to the ended session's prompt: %v, want already handled", err)
	}
}
