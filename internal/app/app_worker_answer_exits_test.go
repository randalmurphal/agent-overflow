package app

// Every exit a spawner's answer to its worker's async questions can take,
// beside those the async question tests cover: where its questions are
// left, and whether the spawner is asked again.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
	"github.com/google/uuid"
)

// requireNotAskedAgain fails if the spawner holds a wake for the worker's
// question under any token but the one it answered.
func (f asyncQuestionFixture) requireNotAskedAgain(t *testing.T, sendID string) {
	t.Helper()
	answered := strings.TrimPrefix(sendID, workerAnswerSendIDRoot)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); slices.ContainsFunc(tokens, func(token string) bool { return token != answered }) {
		t.Fatalf("the spawner was asked again: %q", tokens)
	}
	if open := openWorkerRegistrations(f.app, f.worker.ID); len(open) != 0 {
		t.Fatalf("the question is registered with the spawner under %q", open)
	}
}

// dispatchAnswer hands the spawner's queued answer to a live worker session
// that never echoes it, so it is written and unconfirmed.
func (f asyncQuestionFixture) dispatchAnswer(t *testing.T) {
	t.Helper()
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	if envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1); len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want the answer", len(envelopes))
	}
	f.app.flushDispatch.wg.Wait()
}

// An answer the worker's provider echoed, its row written, is the
// worker's: an End Session after it neither reopens the question nor asks
// the spawner again.
func TestAnEchoedSpawnerAnswerStaysAnswered(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-echoed", store.PlacementOwned)
	sendID := f.answered(t)
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want the answer", len(envelopes))
	}
	f.app.flushDispatch.wg.Wait()
	wireID, _ := envelopes[0]["uuid"].(string)
	text, _ := envelopeText(t, envelopes[0])
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.worker.ID, Content: text,
		Meta: []byte(`{"provider_item_id":"` + wireID + `"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if err := f.app.StopSession(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireTakenIn(t, sendID, "delivered")
}

// An archive of a worker whose session holds the spawner's answer written
// and unconfirmed ends the session, which gives the answer back to its
// questions: the archived worker asks nobody, and its unarchive puts the
// question to the spawner again under a new token.
func TestArchivingAWorkerWithASessionReturnsItsUnconfirmedAnswer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-archived-live", store.PlacementOwned)
	sendID := f.answered(t)
	f.dispatchAnswer(t)
	if result, err := f.app.ArchiveThread(f.worker.ID); err != nil || result.CleanupError != "" {
		t.Fatalf("archive = %+v, %v", result, err)
	}
	f.drain(t)
	f.requireReturned(t, sendID)
	f.requireNotAskedAgain(t, sendID)
	if _, err := f.app.UnarchiveThread(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireRoutedAgain(t, sendID)
}

// A Stop of the worker's spawn brings it to rest with the spawner's answer
// still queued: the answer leaves the queue and the question it answered is
// dismissed with the worker's other questions, put to nobody.
func TestAStopOfAWorkerWithAQueuedAnswerDismissesItsQuestion(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-rest", store.PlacementOwned)
	sendID := f.answered(t)
	if _, err := f.app.StopThreadAgentRow(t.Context(), f.spawner.ID, "async-answer-rest-spawn"); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if durable, memory := queuedSendIDs(t, f.app, f.worker.ID); slices.Contains(durable, sendID) || slices.Contains(memory, sendID) {
		t.Fatalf("the rested worker's queue %q (in memory %q) still holds the answer", durable, memory)
	}
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || len(questions) != 1 || questions[0].State != "dismissed" {
		t.Fatalf("the question = %+v, %v; want it dismissed", questions, err)
	}
	f.requireNotAskedAgain(t, sendID)
}

// A delete of the worker takes its queued answer and its questions with it:
// nothing is put to the spawner again.
func TestDeletingAWorkerWithAQueuedAnswerAsksNobody(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-deleted", store.PlacementOwned)
	sendID := f.answered(t)
	if _, err := f.app.DeleteThread(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireNotAskedAgain(t, sendID)
	if _, _, asking := f.app.asyncQuestionBlocked(f.worker.ID); asking {
		t.Fatal("the deleted worker's question is open")
	}
}

// A revert's queue clear (the Codex rollback's) gives the spawner's answer
// the session was handed and never confirmed back to its question, which
// goes to the spawner again. One still in triage's queue stays queued, as
// the person's messages do, for the session the cut keeps.
func TestARevertsQueueClearReturnsASpawnerAnswer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-reverted", store.PlacementOwned)
	sendID := f.answered(t)
	f.dispatchAnswer(t)
	f.app.dropFlushQueue(f.worker.ID)
	f.app.emit(eventchan.UserMessageReverted, UserMessageRevertedEvent{ThreadID: f.worker.ID})
	f.drain(t)
	f.requireReturned(t, sendID)
	// The session the cut keeps still holds the write it never confirmed,
	// which its row reads before the question; the question is the
	// spawner's again.
	if token, _ := f.wake(t, f.spawner.ID); workerAnswerSendIDRoot+token == sendID {
		t.Fatal("the returned question went back to the spawner under its old token")
	}
	if _, route, asking := f.app.asyncQuestionBlocked(f.worker.ID); !asking || promptRouteTitle(route) != f.spawner.Title {
		t.Fatalf("the returned question is routed to %q (%v), want the spawner", promptRouteTitle(route), asking)
	}
}

// A Codex resend that fails after the spawner's answer was written gives it
// back to its question rather than to the worker's composer.
func TestAFailedResendReturnsASpawnerAnswer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-resend", store.PlacementOwned)
	sendID := f.answered(t)
	rows := durableQueueRows(t, f.app, f.worker.ID)
	f.app.triage.CleanupThread(f.worker.ID)
	var persisted []triage.EagerPersistedFlush
	for _, row := range rows {
		// The dispatch took it: the provider write keeps an answer's row
		// until its echo.
		if _, err := f.app.store.DeleteDispatchedFlushQueueItems([]string{row.ID}); err != nil {
			t.Fatal(err)
		}
		meta, err := usermessage.Marshal(usermessage.Input{SendID: row.SendID})
		if err != nil {
			t.Fatal(err)
		}
		persisted = append(persisted, triage.EagerPersistedFlush{
			UserItemID: "user:eager:" + row.ID, Content: row.Message, Meta: meta, QueueItemID: row.ID, EnqueuedAt: row.EnqueuedAt,
		})
	}
	f.app.restoreEagerPersistedFlushesToDraft(f.worker.ID, persisted)
	f.drain(t)
	f.requireReturned(t, sendID)
	f.requireRoutedAgain(t, sendID)
}

// An answer the app quit with written to the worker's session and never
// confirmed is the question's again at the boot, as a queued one is, and
// the person's.
func TestASpawnerAnswerUnconfirmedAtQuitIsReturnedAtBoot(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-unconfirmed-boot", store.PlacementOwned)
	sendID := f.answered(t)
	f.dispatchAnswer(t)
	f.app.sessionManager().take(f.worker.ID)
	forgetPromptsAtRestart(f.app)
	f.app.triage = triage.NewRouter(f.app.store, f.app.emit)
	f.app.configureTriageQueueCallbacks()
	f.app.restoreDurableFlushQueueAtBoot()
	f.app.giveBackUnconfirmedAtBoot()
	f.drain(t)
	f.requireReturned(t, sendID)
	f.app.syncAsyncQuestionPrompts(f.worker.ID)
	f.drain(t)
	f.requireThePersons(t)
}

// A worker holding the spawner's answer queued does not move to another
// computer: the transfer refuses queued work, and the answer stays queued.
func TestAWorkerWithAQueuedAnswerDoesNotTransfer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-transfer", store.PlacementOwned)
	sendID := f.answered(t)
	worker, err := f.app.store.GetThread(f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.SessionRef = "async-answer-transfer-session"
	if err := f.app.store.UpdateThread(worker); err != nil {
		t.Fatal(err)
	}
	if err := f.app.checkTransferIdle(worker); err == nil || !strings.Contains(err.Error(), "queued work") {
		t.Fatalf("the transfer check = %v, want queued work refused", err)
	}
	if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); !slices.Contains(durable, sendID) {
		t.Fatalf("the worker's queue %q lost the answer", durable)
	}
}

// A worker moved to another computer with its question open asks nobody
// here any more: the move's transfer frame reconciles its questions, which
// this computer no longer holds, so the wake that put it to the spawner is
// withdrawn.
func TestAMovedWorkersQuestionLeavesItsSpawner(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination := transferTestBackend(t), transferTestBackend(t)
	source.startSessionFn = func(string) error { return nil }
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
	spawner := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Lead", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	worker := store.Thread{ID: uuid.NewString(), ProjectID: sourceProject.ID, Title: "Asking", Provider: "claude", RuntimeMode: string(provider.RuntimeApprovalRequired), SessionRef: uuid.NewString(), WorkspacePath: sourcePath, CreatedAt: 1, UpdatedAt: 1}
	for _, thread := range []store.Thread{spawner, worker} {
		if err := source.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.store.InsertThreadRequest(store.ThreadRequest{
		Token: uuid.NewString(), CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestFinished,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
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
	native := `{"type":"user","sessionId":"` + worker.SessionRef + `","uuid":"` + uuid.NewString() + `","parentUuid":null,"message":{"role":"user","content":"start"}}` + "\n"
	if err := os.WriteFile(filepath.Join(nativeDir, worker.SessionRef+".jsonl"), []byte(native), 0o600); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		t.Helper()
		for range 3 {
			if err := source.drainWorkerPrompts(ctx, 10*time.Second); err != nil {
				t.Fatal(err)
			}
			source.waitAgentNoticeWork()
		}
	}
	source.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": worker.ID})
	drain()
	if tokens := workerPromptTokens(t, source, spawner.ID); len(tokens) != 1 {
		t.Fatalf("before the move the spawner holds %q, want the question's wake", tokens)
	}

	destinationID, _ := destination.backendIdentity()
	operation := uuid.NewString()
	intent, err := source.BeginThreadTransfer(ctx, worker.ID, operation, destinationID, "move", true)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, destProject.ID, filepath.Join(t.TempDir(), "moved"), "moved-asking")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, worker.ID, offer); err != nil {
		t.Fatal(err)
	}
	awaitTransferComplete(t, source, destination, operation, false)
	drain()
	if tokens := workerPromptTokens(t, source, spawner.ID); len(tokens) != 0 {
		t.Fatalf("after the move the spawner holds %q for a worker no longer here", tokens)
	}
	if _, _, asking := source.asyncQuestionBlocked(worker.ID); asking {
		t.Fatal("the moved worker's question is open here")
	}
}
