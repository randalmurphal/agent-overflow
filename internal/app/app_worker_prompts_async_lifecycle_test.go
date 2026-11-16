package app

// The lifecycle of a worker's async questions and of the spawner's answer
// to them: every thread frame reconciles them, and every exit the answer
// can take leaves its questions where its owner puts them.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
	"github.com/google/uuid"
)

// A conversation copied here with a question still unanswered waits on it
// from the moment it lands: the frame its installation sends reconciles its
// questions, with no restart and no edit of the question.
func TestATransferredConversationOpensItsUnansweredQuestion(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination := transferTestBackend(t), transferTestBackend(t)
	ctx := context.Background()
	sourcePath := testutil.InitGitRepo(t)
	sourceProject, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	destProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Asking", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), SessionRef: uuid.NewString(), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := source.claudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	nativeDir, err := sessionfork.WorkspaceProjectDir(projects, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := `{"type":"user","sessionId":"` + thread.SessionRef + `","uuid":"` + uuid.NewString() + `","parentUuid":null,"message":{"role":"user","content":"start"}}` + "\n"
	if err := os.WriteFile(filepath.Join(nativeDir, thread.SessionRef+".jsonl"), []byte(native), 0o600); err != nil {
		t.Fatal(err)
	}
	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, thread.ID, operation, destinationID, "copy", true)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, filepath.Join(t.TempDir(), "copied"), "copied-asking")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	awaitTransferComplete(t, source, destination, operation, false)
	if err := destination.drainWorkerPrompts(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if detail, routedTo, asking := destination.asyncQuestionBlocked(intent.TargetThreadID); !asking || detail != "Which scope?" || routedTo != "" {
		t.Fatalf("the copy asks %q of %q (%v), want its question open and the person's", detail, routedTo, asking)
	}
	state, err := (threadToolsApp{app: destination}).threadStateNow(ctx, intent.TargetThreadID)
	if err != nil || state.State != threadtools.StateAwaitingInput {
		t.Fatalf("thread_status of the copy = %+v, %v; want it awaiting input", state, err)
	}
}

// openWorkerRegistrations lists the worker's registrations still routed.
func openWorkerRegistrations(a *App, workerID string) []string {
	a.workerPrompts.mu.Lock()
	defer a.workerPrompts.mu.Unlock()
	var out []string
	for token, prompt := range a.workerPrompts.byToken {
		if prompt.workerID == workerID && prompt.state == workerPromptRouted {
			out = append(out, token)
		}
	}
	return out
}

// An archive that lands while the first reconcile of a sessionless
// worker's question is between its read and its registration is not lost:
// the archive's frame queues a reconcile behind it, which closes what it
// opened, so the drain leaves no registration and no wake with the spawner.
func TestAnArchiveDuringTheFirstReconcileClosesWhatItOpened(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-archive-race", store.PlacementOwned)
	// Recorded with nothing in memory, as a restart leaves it.
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: f.worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	read, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.app.asyncQuestionsReadForTest = func(threadID string) {
		if threadID == f.worker.ID {
			once.Do(func() {
				close(read)
				<-resume
			})
		}
	}
	f.app.seedAsyncQuestionPrompts()
	<-read
	if _, err := f.app.ArchiveThread(f.worker.ID); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	f.drain(t)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); len(tokens) != 0 {
		t.Fatalf("the spawner holds wakes %q for an archived worker", tokens)
	}
	if open := openWorkerRegistrations(f.app, f.worker.ID); len(open) != 0 {
		t.Fatalf("the archived worker's question is still registered under %q", open)
	}
	if _, _, asking := f.app.asyncQuestionBlocked(f.worker.ID); asking {
		t.Fatal("an archived worker's question is open")
	}
}

// echoAnswerWithItsRowFailing hands the spawner's queued answer to a live
// worker session and lands the session's echo of it with every timeline
// write of a user row refused: the provider has the answer, and only the
// row that records it failed.
func (f asyncQuestionFixture) echoAnswerWithItsRowFailing(t *testing.T) {
	t.Helper()
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want the answer", len(envelopes))
	}
	f.app.flushDispatch.wg.Wait()
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_echo_update BEFORE UPDATE OF meta ON items
		WHEN NEW.kind='user_text' AND json_extract(NEW.meta,'$.provider_item_id') IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'injected echo write failure'); END`)
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_echo_insert BEFORE INSERT ON items WHEN NEW.kind='user_text'
		BEGIN SELECT RAISE(ABORT, 'injected echo write failure'); END`)
	wireID, _ := envelopes[0]["uuid"].(string)
	text, _ := envelopeText(t, envelopes[0])
	meta, err := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.worker.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("the echo's row write was not refused")
	}
}

// requireTakenIn fails unless the answer under sendID stands as the
// worker took it in: its question answered and not reopened, its recovery
// row gone, and no other token putting the question to the spawner again.
func (f asyncQuestionFixture) requireTakenIn(t *testing.T, sendID, state string) {
	t.Helper()
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || len(questions) != 1 || questions[0].State != state || questions[0].Answer != "One" {
		t.Fatalf("the question = %+v, %v; want it %s with the spawner's answer", questions, err, state)
	}
	answered := strings.TrimPrefix(sendID, workerAnswerSendIDRoot)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); slices.ContainsFunc(tokens, func(token string) bool { return token != answered }) {
		t.Fatalf("the spawner was asked again: %q", tokens)
	}
	if open := openWorkerRegistrations(f.app, f.worker.ID); len(open) != 0 {
		t.Fatalf("the answered question is registered again under %q", open)
	}
	if _, _, asking := f.app.asyncQuestionBlocked(f.worker.ID); asking {
		t.Fatal("the answered question is open again")
	}
	if draft, _, err := f.app.store.GetThreadDraft(f.worker.ID); err != nil || strings.Contains(draft.Content, "Answer: One") {
		t.Fatalf("the worker's composer = %q, %v; want the answer kept out of it", draft.Content, err)
	}
}

// An answer the worker's provider echoed is taken in, whatever the echo's
// own row write did: its echo confirms the questions answered and retires
// the answer's recovery row, so an End Session after a failed row write
// neither reopens the question nor asks the spawner again.
func TestAnEchoedSpawnerAnswerWhoseRowFailedIsNotGivenBack(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-echo-row", store.PlacementOwned)
	sendID := f.answered(t)
	f.echoAnswerWithItsRowFailing(t)
	if err := f.app.StopSession(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireTakenIn(t, sendID, "delivered")
	if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); slices.Contains(durable, sendID) {
		t.Fatalf("the taken-in answer's recovery row is still queued: %q", durable)
	}
}

// An echo whose confirmation fails to write is kept, as an answer's or a
// completion's is: an End Session meanwhile reads the answer consumed and
// gives nothing back, and the confirmation lands at its next try.
func TestAnEchoedSpawnerAnswerWhoseConfirmationFailsIsKept(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-echo-confirm", store.PlacementOwned)
	sendID := f.answered(t)
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_answer_confirm BEFORE UPDATE OF state ON async_questions
		WHEN NEW.state='delivered' BEGIN SELECT RAISE(ABORT, 'injected confirm failure'); END`)
	f.echoAnswerWithItsRowFailing(t)
	if !f.app.echoed(sendID) {
		t.Fatal("the failed confirmation was not kept")
	}
	if err := f.app.StopSession(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireTakenIn(t, sendID, "submitted")
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_answer_confirm`)
	f.app.retryOwnedEchoes(f.worker.ID)
	if f.app.echoed(sendID) {
		t.Fatal("the confirmation did not land once the store took it")
	}
	f.requireTakenIn(t, sendID, "delivered")
	if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); slices.Contains(durable, sendID) {
		t.Fatalf("the taken-in answer's recovery row is still queued: %q", durable)
	}
}

// A person's withdrawal of the spawner's answer puts its questions to the
// person even when a reconcile that read the item before the return
// publishes after it: that older reading neither takes the person's
// override nor leaves the item's next occurrence with the spawner. One item
// asks two questions; the spawner answered the first while the second was
// dismissed, and the second is reopened, whose reconcile reads only it and
// waits while the person withdraws the answer.
func TestAReconcileOlderThanAWithdrawalLeavesTheQuestionsThePersons(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-withdraw-race", store.PlacementOwned)
	f.ask(t, "question:ask", twoAsyncQuestions)
	if err := f.app.SetAsyncQuestionDismissed(t.Context(), f.worker.ID, "question:ask", 1, true); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	answered, _ := f.wake(t, f.spawner.ID)
	if ack, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: answered, Text: "One"}); err != nil || !ack.Accepted {
		t.Fatalf("the spawner's reply = %+v, %v", ack, err)
	}
	f.drain(t)
	read, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.app.asyncQuestionsReadForTest = func(threadID string) {
		if threadID == f.worker.ID {
			once.Do(func() {
				close(read)
				<-resume
			})
		}
	}
	if err := f.app.SetAsyncQuestionDismissed(t.Context(), f.worker.ID, "question:ask", 1, false); err != nil {
		t.Fatal(err)
	}
	<-read
	stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.worker.ID, workerAnswerSendIDRoot+answered)
	close(resume)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	f.drain(t)
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || len(questions) != 2 || questions[0].State != "unanswered" || questions[1].State != "unanswered" {
		t.Fatalf("the questions = %+v, %v; want both unanswered", questions, err)
	}
	if detail, routedTo, asking := f.app.asyncQuestionBlocked(f.worker.ID); !asking || detail != "Which scope?" || routedTo != "" {
		t.Fatalf("the worker asks %q of %q (%v), want both questions the person's", detail, routedTo, asking)
	}
	if open := openWorkerRegistrations(f.app, f.worker.ID); len(open) != 0 {
		t.Fatalf("the withdrawn questions are registered with the spawner under %q", open)
	}
}
