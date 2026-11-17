package app

// A spawned Codex thread's async questions go to its spawner as its other
// prompts do (app_worker_prompts_async.go): the same wake, thread_reply and
// hand-back, answered through the async-answer path, with every lifecycle
// the routing of its approvals has.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

type asyncQuestionFixture struct {
	app      *App
	dbPath   string
	spawner  store.Thread
	worker   store.Thread
	notified *recordingNotificationSender
}

// newAsyncQuestionFixture is a spawner and the Codex worker it spawned,
// owned or standalone, with no provider: every session start fails, so
// what is queued stays queued.
func newAsyncQuestionFixture(t *testing.T, id string, placement string) asyncQuestionFixture {
	t.Helper()
	a, dbPath := newTestAppWithStorePath(t)
	a.testEmitHook = (&emitRecorder{}).capture
	a.triage = triage.NewRouter(a.store, a.emit)
	a.configureTriageQueueCallbacks()
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	notified := &recordingNotificationSender{}
	a.osNotifications = notified
	a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	spawner := noticeTestThread(t, a, id+"-spawner")
	worker := testThread(id + "-worker")
	worker.WorkspacePath, worker.Title = t.TempDir(), "Release worker"
	if err := a.store.CreateThread(worker); err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: id + "-spawn", CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatalf("record the spawn: %v", err)
	}
	if _, err := a.store.SetThreadPlacement([]string{worker.ID}, placement); err != nil {
		t.Fatal(err)
	}
	return asyncQuestionFixture{app: a, dbPath: dbPath, spawner: spawner, worker: worker, notified: notified}
}

// drain waits out the prompts, notice starts and settlements begun; a start
// waiting for a delete's decision counts as done (quietAtGates).
func (f asyncQuestionFixture) drain(t *testing.T) {
	t.Helper()
	for range 3 {
		quietAtGates(t, f.app)
		f.app.threadRequestsWG.Wait()
		f.app.notifications.queue.Wait()
	}
}

// ask records the worker's async questions under itemID, as triage does
// for its Codex session, and announces them.
func (f asyncQuestionFixture) ask(t *testing.T, itemID string, meta string) {
	t.Helper()
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{
		ID: itemID, ThreadID: f.worker.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
	f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": f.worker.ID})
	f.drain(t)
}

// wake is the one worker prompt wake the spawner's queue holds: its token
// and its text.
func (f asyncQuestionFixture) wake(t *testing.T, threadID string) (string, string) {
	t.Helper()
	var found []store.FlushQueueItem
	for _, row := range durableQueueRows(t, f.app, threadID) {
		if strings.HasPrefix(row.SendID, workerPromptSendIDRoot) {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d prompt wakes, want one", threadID, len(found))
	}
	return strings.TrimPrefix(found[0].SendID, workerPromptSendIDRoot), found[0].Message
}

// row is the spawner's Agents row of its worker and the spawner's summary.
func (f asyncQuestionFixture) row(t *testing.T) (ThreadAgentRow, ThreadWorkerSummary) {
	t.Helper()
	listing, err := f.app.ListThreadAgentRows(t.Context(), f.spawner.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := f.app.GetThreadWorkerSummary(t.Context(), f.spawner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range listing.Rows {
		if row.ThreadID == f.worker.ID {
			return row, summary
		}
	}
	return ThreadAgentRow{}, summary
}

// questionSends is what was sent and retracted for the worker's question
// notifications.
func (f asyncQuestionFixture) questionSends(itemID string) []notify.Send {
	var out []notify.Send
	for _, send := range f.notified.snapshot() {
		if send.ID == "question:"+f.worker.ID+":"+itemID {
			out = append(out, send)
		}
	}
	return out
}

const twoAsyncQuestions = `{"delivery":"async","questions":[{"title":"Which scope?","options":["One","Two"]},{"title":"When should it run?"}]}`

// The worker's async questions wake its spawner as one prompt, the worker
// reads blocked on a question routed to the spawner, no one is notified,
// and the spawner's thread_reply answers them in the worker through the
// async-answer path, attributed to the spawner, once.
func TestAWorkersAsyncQuestionGoesToItsSpawnerAndItsReplyAnswersIt(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answered", store.PlacementOwned)
	f.ask(t, "question:ask", twoAsyncQuestions)
	token, body := f.wake(t, f.spawner.ID)
	for _, want := range []string{`"1. Which scope?"`, `"   option: One"`, `"2. When should it run?"`, "thread_reply, token " + token + ", once: one answer per part in answers"} {
		if !strings.Contains(body, want) {
			t.Errorf("the wake does not say %q:\n%s", want, body)
		}
	}
	row, summary := f.row(t)
	if row.Worker != workerBlocked || row.BlockedOn != "question" || row.BlockedDetail != "Which scope?" || row.BlockedRoutedTo != f.spawner.Title {
		t.Fatalf("the worker's row = %+v, want blocked on its question, routed to the spawner", row)
	}
	if summary.Open != 1 || summary.NeedYou != 0 {
		t.Fatalf("the spawner's summary = %+v, want one open worker that does not need the user", summary)
	}
	if sends := f.questionSends("question:ask"); len(sends) != 1 || !sends[0].Retract {
		t.Fatalf("question notifications = %+v, want none raised", sends)
	}

	caller := threadtools.Caller{ThreadID: f.spawner.ID}
	for call, says := range map[*threadtools.ReplyCall]string{
		{Token: token, Text: "One"}:                         "That question has 2 parts.",
		{Token: token, Answers: []string{"One", " "}}:       "Give an answer for every part of that question.",
		{Token: token, Decision: threadtools.DecisionAllow}: "That is a question, not an approval",
	} {
		_, handled, err := f.app.answerWorkerPrompt(t.Context(), caller, *call)
		if _, message, _ := errorsx.PublicDetails(err); !handled || !strings.HasPrefix(message, says) {
			t.Fatalf("reply %+v: handled %v, %v; want it refused with %q", *call, handled, err, says)
		}
	}
	if _, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.worker.ID}, threadtools.ReplyCall{Token: token, Answers: []string{"One", "tonight"}}); !handled {
		t.Fatal("another thread's reply was not taken as a prompt's")
	} else if code, _, _ := errorsx.PublicDetails(err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("another thread's reply: %v, want refused as not its own", err)
	}
	reply := threadtools.ReplyCall{Token: token, Answers: []string{"One", "tonight"}}
	ack, handled, err := f.app.answerWorkerPrompt(t.Context(), caller, reply)
	if err != nil || !handled || !ack.Accepted || ack.SourceThreadID != f.worker.ID {
		t.Fatalf("the spawner's reply = %+v handled %v, %v", ack, handled, err)
	}
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || len(questions) != 2 || questions[0].State != "submitted" || questions[0].Answer != "One" || questions[1].Answer != "tonight" {
		t.Fatalf("the worker's questions = %+v, %v; want both submitted with the spawner's answers", questions, err)
	}
	var answer *store.FlushQueueItem
	for _, row := range durableQueueRows(t, f.app, f.worker.ID) {
		if row.SendID == workerAnswerSendIDRoot+token {
			answer = &row
		}
	}
	if answer == nil || answer.Message != "Question: Which scope?\nAnswer: One\n\nQuestion: When should it run?\nAnswer: tonight" {
		t.Fatalf("the worker's queued answer = %+v", answer)
	}
	var payload flushQueuePayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Origin != agentmeta.OriginAgentThread || payload.OriginThread == nil || payload.OriginThread.ThreadID != f.spawner.ID {
		t.Fatalf("the answer is attributed %q %+v, want the spawner", payload.Origin, payload.OriginThread)
	}
	// The worker's row reads the answer as the spawner's message, all of it.
	if read := agentmeta.Describe(answer.Message, agentmeta.Meta{Origin: payload.Origin, OriginThread: payload.OriginThread}); len(read) != 1 ||
		read[0].Facts.Form != threadtools.FormMessage || read[0].Body() != answer.Message || read[0].Facts.Headline != "Question: Which scope?" {
		t.Fatalf("the worker's row reads the answer as %+v", read)
	}

	f.drain(t)
	if ack, handled, err := f.app.answerWorkerPrompt(t.Context(), caller, reply); err != nil || !handled {
		t.Fatalf("the same reply again = %+v, %v; want it taken as already given", ack, err)
	}
	if _, _, err := f.app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: token, Answers: []string{"Two", "now"}}); !errors.Is(err, errWorkerPromptAnswered) {
		t.Fatalf("a different reply: %v, want refused as answered", err)
	}
	if _, _, asking := f.app.asyncQuestionBlocked(f.worker.ID); asking {
		t.Fatal("the answered questions are still open")
	}
}

// The person answering one part in the worker closes the spawner's prompt
// and withdraws its wake; the part left goes to the spawner again under a
// new token, and the old token answers nothing. Answering that one too
// withdraws the second wake.
func TestAWorkersAsyncQuestionAnsweredInTheWorkerRefusesTheSpawnersReply(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-person", store.PlacementOwned)
	f.ask(t, "question:ask", twoAsyncQuestions)
	first, _ := f.wake(t, f.spawner.ID)
	if err := f.app.SubmitAsyncAnswers(t.Context(), f.worker.ID, "person-1", []store.AsyncQuestionAnswer{{ItemID: "question:ask", Index: 0, Answer: "Two"}}); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	second, body := f.wake(t, f.spawner.ID)
	if second == first || strings.Contains(body, "Which scope?") || !strings.Contains(body, "When should it run?") || !strings.Contains(body, "once: your answer in text") {
		t.Fatalf("the second wake (%s after %s) is:\n%s\nwant only the part left", second, first, body)
	}
	caller := threadtools.Caller{ThreadID: f.spawner.ID}
	if _, _, err := f.app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: first, Answers: []string{"One", "now"}}); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the first token's reply: %v, want the prompt closed", err)
	}
	if err := f.app.SubmitAsyncAnswers(t.Context(), f.worker.ID, "person-2", []store.AsyncQuestionAnswer{{ItemID: "question:ask", Index: 1, Answer: "now"}}); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); len(tokens) != 0 {
		t.Fatalf("the spawner still holds wakes %q for questions the person answered", tokens)
	}
	if _, _, err := f.app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: second, Text: "later"}); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the second token's reply: %v, want the prompt closed", err)
	}
	if row, _ := f.row(t); row.Worker == workerBlocked {
		t.Fatalf("the worker's row = %+v after the person answered", row)
	}
}

// A question the spawner cannot take goes to the person: when its wake is
// withdrawn from the spawner's queue, the worker's row needs the user and,
// as the owned worker's turn end raised nothing, the question notifies,
// until the person answers it.
func TestAnOwnedWorkersAsyncQuestionTheSpawnerCannotTakeNotifiesThePerson(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-handed-back", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.spawner.ID, workerPromptSendIDRoot+token)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	f.drain(t)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); len(tokens) != 0 {
		t.Fatalf("the spawner was given the question again: %q", tokens)
	}
	row, summary := f.row(t)
	if row.Worker != workerBlocked || row.BlockedOn != "question" || row.BlockedRoutedTo != "" || summary.NeedYou != 1 {
		t.Fatalf("the worker's row = %+v, summary %+v; want it blocked on the user", row, summary)
	}
	sends := f.questionSends("question:ask")
	if len(sends) == 0 || sends[len(sends)-1].Retract || sends[len(sends)-1].Body != "Waiting for your answer" ||
		sends[len(sends)-1].Target.ThreadID != f.worker.ID {
		t.Fatalf("question notifications = %+v, want the last one raised for the worker", sends)
	}
	if _, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: token, Text: "One"}); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the spawner's late reply: %v, want the prompt closed", err)
	}
	if err := f.app.SubmitAsyncAnswers(t.Context(), f.worker.ID, "person", []store.AsyncQuestionAnswer{{ItemID: "question:ask", Index: 0, Answer: "One"}}); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if sends := f.questionSends("question:ask"); !sends[len(sends)-1].Retract {
		t.Fatalf("question notifications = %+v, want the last one retracted", sends)
	}
}

// A sidebar worker whose spawner is archived or deleted keeps its question
// open and the person's: the spawner's wake is withdrawn and its token
// answers nothing. Its own turn's end notified, so the question does not.
func TestAWorkersAsyncQuestionGoesToThePersonWhenItsSpawnerLeaves(t *testing.T) {
	t.Parallel()
	for _, leave := range []string{"archive", "delete"} {
		t.Run(leave, func(t *testing.T) {
			t.Parallel()
			f := newAsyncQuestionFixture(t, "async-leave-"+leave, store.PlacementStandalone)
			f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
			token, _ := f.wake(t, f.spawner.ID)
			var err error
			if leave == "archive" {
				_, err = f.app.ArchiveThread(f.spawner.ID)
			} else {
				_, err = f.app.DeleteThread(f.spawner.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			ref := triage.PromptRef{ThreadID: f.worker.ID, RequestID: "question:ask"}
			if prompt, _ := f.app.workerPrompts.get(token); prompt.state != workerPromptSettled {
				t.Fatalf("the spawner's registration is %v, want it closed", prompt.state)
			} else {
				ref.Occurrence = prompt.occurrence
			}
			if route, open := f.app.asyncQuestionRoute(ref); !open || route != nil {
				t.Fatalf("the question is routed %+v (open %v), want it open and the person's", route, open)
			}
			if detail, routedTo, asking := f.app.asyncQuestionBlocked(f.worker.ID); !asking || detail != "Which scope?" || routedTo != "" {
				t.Fatalf("the worker asks %q of %q (%v), want the person", detail, routedTo, asking)
			}
			for _, send := range f.questionSends("question:ask") {
				if !send.Retract {
					t.Fatalf("a sidebar worker's question notified: %+v", send)
				}
			}
		})
	}
}

// A worker's session ending leaves its async questions open and with its
// spawner, and an archive of the worker closes them, withdrawing the wake;
// unarchived, they go to the spawner again.
func TestAWorkersAsyncQuestionOutlivesItsSessionAndFollowsItsArchive(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-archive", store.PlacementStandalone)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	f.app.settleEndedWorkerPrompts(f.worker.ID)
	f.drain(t)
	if !promptRouted(&f.app.workerPrompts, token) || !slices.Contains(workerPromptTokens(t, f.app, f.spawner.ID), token) {
		t.Fatal("the worker's session end closed its async question")
	}
	if _, err := f.app.ArchiveThread(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); len(tokens) != 0 {
		t.Fatalf("the spawner holds wakes %q for an archived worker", tokens)
	}
	if _, _, asking := f.app.asyncQuestionBlocked(f.worker.ID); asking {
		t.Fatal("an archived worker's question is open")
	}
	if _, err := f.app.UnarchiveThread(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if again, _ := f.wake(t, f.spawner.ID); again == token {
		t.Fatal("the unarchived worker's question came back under its old token")
	}
	// A message waiting for the worker is what it waits on first: the
	// question is not what holds it while it has work.
	sendWithin(t, f.app, f.worker.ID, "keep going", 5*time.Second)
	f.drain(t)
	if row, _ := f.row(t); row.Worker != workerQueued || row.BlockedOn != "" {
		t.Fatalf("the worker's row with a message queued = %+v, want queued and not blocked", row)
	}
}

// A Stop of the worker's spawn brings it to rest: its async questions are
// dismissed and the spawner's wake withdrawn.
func TestAStopOfAWorkerDismissesItsAsyncQuestions(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-stop", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	if _, err := f.app.StopThreadAgentRow(t.Context(), f.spawner.ID, "async-stop-spawn"); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || len(questions) != 1 || questions[0].State != "dismissed" {
		t.Fatalf("the worker's questions = %+v, %v; want them dismissed", questions, err)
	}
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); slices.Contains(tokens, token) {
		t.Fatalf("the spawner still holds the stopped worker's question: %q", tokens)
	}
	if row, _ := f.row(t); row.Worker != "" {
		t.Fatalf("the stopped worker's row = %+v, want it at rest", row)
	}
}

// After a restart the app has forgotten every registration and discarded
// the wakes it quit with; the boot routes the open question again under a
// new token, and the old one answers nothing.
func TestAWorkersOpenAsyncQuestionIsRoutedAgainAtBoot(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-boot", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	old, _ := f.wake(t, f.spawner.ID)
	// The restart: memory goes, the queue is restored as the boot does.
	f.app.workerPrompts.mu.Lock()
	f.app.workerPrompts.byToken, f.app.workerPrompts.current = nil, nil
	f.app.workerPrompts.mu.Unlock()
	f.app.workerPrompts.async.mu.Lock()
	f.app.workerPrompts.async.open = nil
	f.app.workerPrompts.async.mu.Unlock()
	f.app.restoreDurableFlushQueueAtBoot()
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); len(tokens) != 0 {
		t.Fatalf("the boot kept prompt wakes %q", tokens)
	}
	f.app.seedAsyncQuestionPrompts()
	f.drain(t)
	token, _ := f.wake(t, f.spawner.ID)
	if token == old {
		t.Fatal("the boot reused the old token")
	}
	if _, handled, _ := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: old, Text: "One"}); handled {
		t.Fatal("the token from before the restart is still a prompt's")
	}
	if row, _ := f.row(t); row.Worker != workerBlocked || row.BlockedRoutedTo != f.spawner.Title {
		t.Fatalf("the worker's row after the boot = %+v", row)
	}
}

// answered has the spawner answer the worker's one question with "One" and
// returns the answer's send id, still queued in the worker.
func (f asyncQuestionFixture) answered(t *testing.T) string {
	t.Helper()
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	if ack, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: token, Text: "One"}); err != nil || !ack.Accepted {
		t.Fatalf("the spawner's reply = %+v, %v", ack, err)
	}
	f.drain(t)
	sendID := workerAnswerSendIDRoot + token
	if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); !slices.Contains(durable, sendID) {
		t.Fatalf("the worker's queue %q does not hold the answer", durable)
	}
	return sendID
}

// requireReturned fails unless the answer under sendID is gone from the
// worker's queue and its composer, and its question is unanswered again.
func (f asyncQuestionFixture) requireReturned(t *testing.T, sendID string) {
	t.Helper()
	if durable, memory := queuedSendIDs(t, f.app, f.worker.ID); slices.Contains(durable, sendID) || slices.Contains(memory, sendID) {
		t.Fatalf("the worker's queue %q (in memory %q) still holds the answer", durable, memory)
	}
	if draft, _, err := f.app.store.GetThreadDraft(f.worker.ID); err != nil || strings.Contains(draft.Content, "Answer: One") {
		t.Fatalf("the worker's composer = %q, %v; want the spawner's answer kept out of it", draft.Content, err)
	}
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || questions[0].State != "unanswered" || questions[0].SendID != "" {
		t.Fatalf("the question = %+v, %v; want it unanswered", questions, err)
	}
}

// requireRoutedAgain fails unless the returned question is put to the
// spawner again under a token other than old's.
func (f asyncQuestionFixture) requireRoutedAgain(t *testing.T, sendID string) {
	t.Helper()
	token, _ := f.wake(t, f.spawner.ID)
	if workerAnswerSendIDRoot+token == sendID {
		t.Fatal("the returned question went back to the spawner under its old token")
	}
	if row, _ := f.row(t); row.Worker != workerBlocked || row.BlockedRoutedTo != f.spawner.Title {
		t.Fatalf("the worker's row = %+v, want blocked on its question, routed to the spawner", row)
	}
}

// A person withdrawing the spawner's queued answer from the worker's
// Incoming rows overrules the agent: the question is unanswered again and
// the person's, as a hand-back leaves it, and the person can answer it.
func TestAWithdrawnSpawnerAnswerPutsTheQuestionToThePerson(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-withdrawn", store.PlacementOwned)
	sendID := f.answered(t)
	stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.worker.ID, sendID)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	f.drain(t)
	f.requireReturned(t, sendID)
	// The fixture's spawner answered without its wake being delivered, so
	// that wake may still be queued; it is inert (workerPromptLive). Only a
	// wake under another token would put the question to it again.
	answeredToken := strings.TrimPrefix(sendID, workerAnswerSendIDRoot)
	if tokens := workerPromptTokens(t, f.app, f.spawner.ID); slices.ContainsFunc(tokens, func(token string) bool { return token != answeredToken }) {
		t.Fatalf("the spawner was given the question again: %q", tokens)
	}
	row, summary := f.row(t)
	if row.Worker != workerBlocked || row.BlockedRoutedTo != "" || summary.NeedYou != 1 {
		t.Fatalf("the worker's row = %+v, summary %+v; want it blocked on the person", row, summary)
	}
	if sends := f.questionSends("question:ask"); len(sends) == 0 || sends[len(sends)-1].Retract {
		t.Fatalf("question notifications = %+v, want the last one raised for the person", sends)
	}
	if err := f.app.SubmitAsyncAnswers(t.Context(), f.worker.ID, "person", []store.AsyncQuestionAnswer{{ItemID: "question:ask", Index: 0, Answer: "Two"}}); err != nil {
		t.Fatalf("the person's answer: %v", err)
	}
}

// An answer still queued when the app quits is the question's again at the
// boot, never the worker's composer text, and goes to the spawner again
// under a new token.
func TestASpawnerAnswerQueuedAtQuitIsReturnedAtBoot(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-boot", store.PlacementOwned)
	sendID := f.answered(t)
	f.app.workerPrompts.mu.Lock()
	f.app.workerPrompts.byToken, f.app.workerPrompts.current = nil, nil
	f.app.workerPrompts.mu.Unlock()
	f.app.workerPrompts.async.mu.Lock()
	f.app.workerPrompts.async.open = nil
	f.app.workerPrompts.async.mu.Unlock()
	f.app.triage = triage.NewRouter(f.app.store, f.app.emit)
	f.app.configureTriageQueueCallbacks()
	f.app.restoreDurableFlushQueueAtBoot()
	f.drain(t)
	f.requireReturned(t, sendID)
	f.app.seedAsyncQuestionPrompts()
	f.drain(t)
	f.requireRoutedAgain(t, sendID)
}

// A session that ends with the spawner's answer still queued gives it back
// to its question, which goes to the spawner again.
func TestASessionDeathReturnsAQueuedSpawnerAnswer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-died", store.PlacementOwned)
	sendID := f.answered(t)
	f.app.restoreUnconfirmedQueueOnSessionDeath(f.worker.ID)
	f.drain(t)
	f.requireReturned(t, sendID)
	f.requireRoutedAgain(t, sendID)
}

// A wholesale drop of the worker's queue (here End Session's) keeps the
// spawner's answer out of the drop and gives it back to its question.
func TestAQueueDropReturnsASpawnerAnswer(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-dropped", store.PlacementOwned)
	sendID := f.answered(t)
	if err := f.app.StopSession(f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireReturned(t, sendID)
	f.requireRoutedAgain(t, sendID)
}

// workerRecordingSession gives the worker a live session whose stdin is
// recorded and which never echoes, joining what is queued into one message
// as Claude does, so what is written to it stays unconfirmed.
func (f asyncQuestionFixture) workerRecordingSession(t *testing.T) string {
	t.Helper()
	stdinLog := filepath.Join(t.TempDir(), "stdin.jsonl")
	sess, err := claude.NewSession(context.Background(), f.worker.ID,
		claude.Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0), Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: f.worker.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.app.sessionManager().put(f.worker.ID, session{Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now())})
	return stdinLog
}

// The spawner's answer joined with the person's text and never confirmed:
// the session's end splits them, the person's text alone goes to the
// composer, and the answer goes back to its question, which goes to the
// spawner again.
func TestASessionDeathSplitsASpawnerAnswerFromThePersonsText(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-joined", store.PlacementOwned)
	sendID := f.answered(t)
	// The session below stands in for the one the dispatcher would start for
	// the person's message (ensureSessionForFlushLocked), so the dispatcher
	// is held until it is up.
	holdFlushQueue(f.app)
	if _, err := f.app.RegisterQueueItem(t.Context(), f.worker.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatal(err)
	}
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	if envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1); len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want one joined", len(envelopes))
	}
	f.app.flushDispatch.wg.Wait()
	if sends := f.app.triage.UnconfirmedFlushSends(f.worker.ID); len(sends) != 1 || !unconfirmedMembersOf(sends[0]).joined {
		t.Fatalf("unconfirmed sends = %+v, want one joined message", sends)
	}
	f.app.restoreUnconfirmedQueueOnSessionDeath(f.worker.ID)
	f.drain(t)
	requireDraft(t, f.app, f.worker.ID, "my own words")
	f.requireReturned(t, sendID)
	f.requireRoutedAgain(t, sendID)
}

// An answer written to the worker and never confirmed keeps its queue row
// until its echo. A session death that cannot retire its quiet row requeues
// it; withdrawn before the teardown registers that requeue again, it stays
// withdrawn: the re-registration does not take its missing row for one the
// write retired, nothing sends it again, and its quiet row is retired.
func TestAWrittenAnswerWithdrawnAfterAFailedRestoreStaysWithdrawn(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-answer-requeued", store.PlacementOwned)
	sendID := f.answered(t)
	// A Claude turn running, so the dispatch persists the answer's row
	// quietly into it.
	execOnStore(t, f.dbPath, fmt.Sprintf(`UPDATE threads SET provider = 'claude' WHERE id = '%s'`, f.worker.ID))
	if err := f.app.store.InsertTurn(store.Turn{TurnID: "turn-0", ThreadID: f.worker.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	waitForRecordedUserEnvelopes(t, stdinLog, 1)
	f.app.flushDispatch.wg.Wait()
	sends := f.app.triage.UnconfirmedFlushSends(f.worker.ID)
	if len(sends) != 1 || sends[0].QuietItem == nil {
		t.Fatalf("unconfirmed sends = %+v, want the answer with its quiet row", sends)
	}
	if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); !slices.Contains(durable, sendID) {
		t.Fatalf("the worker's queue %q dropped the written answer's row before its echo", durable)
	}
	execOnStore(t, f.dbPath, fmt.Sprintf(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = '%s' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`, sends[0].QuietItem.ID))
	requeued := f.app.restoreUnconfirmedQueueOnSessionDeath(f.worker.ID)
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_row_delete`)
	if len(requeued) != 1 {
		t.Fatalf("requeued = %+v, want the answer whose row cleanup failed", requeued)
	}
	if stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.worker.ID, sendID); err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	f.app.triage.CleanupThread(f.worker.ID)
	f.app.requeueUnconfirmedFlushItems(f.worker.ID, requeued, queueRestoredReasonSessionDied)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	f.app.flushDispatch.wg.Wait()
	f.drain(t)
	if n := len(recordedUserEnvelopes(t, stdinLog)); n != 1 {
		t.Fatalf("the worker was written %d messages, want the answer once", n)
	}
	f.requireReturned(t, sendID)
	// Nothing of the message is left, so its quiet row leaves the timeline.
	f.requireRowGone(t, *sends[0].QuietItem)
}

// answeredAgain answers a second question of the worker, itemID, through its
// own prompt to the spawner, and returns the answer's send id.
func (f asyncQuestionFixture) answeredAgain(t *testing.T, itemID string) string {
	t.Helper()
	before := workerPromptTokens(t, f.app, f.spawner.ID)
	f.ask(t, itemID, `{"delivery":"async","questions":[{"title":"Which branch?"}]}`)
	var token string
	for _, candidate := range workerPromptTokens(t, f.app, f.spawner.ID) {
		if !slices.Contains(before, candidate) {
			token = candidate
		}
	}
	if token == "" {
		t.Fatalf("no prompt for %s reached the spawner", itemID)
	}
	if ack, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: token, Text: "Main"}); err != nil || !ack.Accepted {
		t.Fatalf("the spawner's reply = %+v, %v", ack, err)
	}
	f.drain(t)
	return workerAnswerSendIDRoot + token
}

// requireUnanswered fails unless the worker's question itemID is unanswered.
func (f asyncQuestionFixture) requireUnanswered(t *testing.T, itemID string) {
	t.Helper()
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, itemID)
	if err != nil || len(questions) == 0 || questions[0].State != "unanswered" || questions[0].SendID != "" {
		t.Fatalf("question %s = %+v, %v; want it unanswered", itemID, questions, err)
	}
}

// joinedWithFailedRestore writes what the worker's queue holds to a live
// session as one joined message, with a Claude turn running so its row is
// persisted quietly (the questions were a Codex worker's), then ends the session with the quiet row's cleanup
// failing, so the restore requeues the message. It returns the requeued
// items, the quiet row and the stdin log.
func (f asyncQuestionFixture) joinedWithFailedRestore(t *testing.T) ([]triage.UnconfirmedFlushItem, store.Item, string) {
	t.Helper()
	f.claudeTurnRunning(t)
	stdinLog := f.workerRecordingSession(t)
	f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)
	f.app.triage.FlushQueuedItems(f.worker.ID)
	if envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1); len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want one joined", len(envelopes))
	}
	f.app.flushDispatch.wg.Wait()
	sends := f.app.triage.UnconfirmedFlushSends(f.worker.ID)
	if len(sends) != 1 || sends[0].QuietItem == nil || !unconfirmedMembersOf(sends[0]).joined {
		t.Fatalf("unconfirmed sends = %+v, want one joined message with its quiet row", sends)
	}
	quiet := *sends[0].QuietItem
	execOnStore(t, f.dbPath, fmt.Sprintf(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = '%s' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`, quiet.ID))
	requeued := f.app.restoreUnconfirmedQueueOnSessionDeath(f.worker.ID)
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_row_delete`)
	if len(requeued) == 0 {
		t.Fatal("the failed restore requeued nothing")
	}
	return requeued, quiet, stdinLog
}

// claudeTurnRunning makes the worker a Claude thread with a turn running, so
// a dispatch persists its row quietly into it.
func (f asyncQuestionFixture) claudeTurnRunning(t *testing.T) {
	t.Helper()
	execOnStore(t, f.dbPath, fmt.Sprintf(`UPDATE threads SET provider = 'claude' WHERE id = '%s'`, f.worker.ID))
	if err := f.app.store.InsertTurn(store.Turn{TurnID: "turn-0", ThreadID: f.worker.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
}

// requireRowGone fails unless the worker's timeline no longer has the joined
// message's row. A message sent again alone may land under the same id, its
// send id's, with its own text.
func (f asyncQuestionFixture) requireRowGone(t *testing.T, joined store.Item) {
	t.Helper()
	items, err := f.app.store.ListItems(f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(items, func(item store.Item) bool { return item.ID == joined.ID && item.Summary == joined.Summary }) {
		t.Fatalf("the joined message's row %s is still in the timeline", joined.ID)
	}
}

// resendAfterWithdrawal withdraws withdrawn from the worker's Incoming rows
// and sends what is queued. With teardown, the queue is torn down and
// requeued registered again in between, as a session end's teardown does;
// without, the requeue stays queued as it was. It returns what the session
// was written after the joined message.
func (f asyncQuestionFixture) resendAfterWithdrawal(t *testing.T, requeued []triage.UnconfirmedFlushItem, withdrawn, stdinLog string, teardown bool) []string {
	t.Helper()
	if stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.worker.ID, withdrawn); err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	if teardown {
		f.app.triage.CleanupThread(f.worker.ID)
		f.app.requeueUnconfirmedFlushItems(f.worker.ID, requeued, queueRestoredReasonSessionDied)
	}
	f.app.triage.FlushQueuedItems(f.worker.ID)
	f.app.flushDispatch.wg.Wait()
	f.drain(t)
	// The session's stdin reaches its log after the write returns.
	var later []string
	for _, envelope := range waitForRecordedUserEnvelopes(t, stdinLog, 2)[1:] {
		text, _ := envelopeText(t, envelope)
		later = append(later, text)
	}
	return later
}

// joinedCases are the cases a joined send's withdrawal tests run: each
// member order, with and without a teardown registering the requeue again
// after the withdrawal.
func joinedCases(first string) []struct {
	name            string
	first, teardown bool
} {
	var cases []struct {
		name            string
		first, teardown bool
	}
	for _, isFirst := range []bool{true, false} {
		for _, teardown := range []bool{true, false} {
			name := "the other first"
			if isFirst {
				name = first
			}
			if teardown {
				name += ", then a teardown"
			}
			cases = append(cases, struct {
				name            string
				first, teardown bool
			}{name, isFirst, teardown})
		}
	}
	return cases
}

// S r10: a joined message holds each queued message it carries as its own.
// The spawner's answer joined with the person's text, in either order, whose
// session ends with the quiet row's cleanup failing, is requeued; the answer
// withdrawn while it is queued again, or before a teardown registers the
// requeue again, takes the answer alone. The person's text is sent again
// exactly once, the answer never again, and neither the joined row nor a
// queue row is left behind.
func TestAWithdrawnAnswerOfAJoinedSendLeavesThePersonsText(t *testing.T) {
	t.Parallel()
	for _, c := range joinedCases("the answer first") {
		answerFirst := c.first
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newAsyncQuestionFixture(t, "async-joined-withdrawn", store.PlacementOwned)
			// The session below stands in for the one the dispatcher would
			// start for the person's message, so the dispatcher is held until
			// it is up.
			holdFlushQueue(f.app)
			person := func() {
				if _, err := f.app.RegisterQueueItem(t.Context(), f.worker.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
					t.Fatal(err)
				}
			}
			if !answerFirst {
				person()
			}
			sendID := f.answered(t)
			if answerFirst {
				person()
			}
			requeued, quiet, stdinLog := f.joinedWithFailedRestore(t)
			later := f.resendAfterWithdrawal(t, requeued, sendID, stdinLog, c.teardown)

			if len(later) != 1 || later[0] != "my own words" {
				t.Fatalf("written after the joined message: %q, want the person's text alone, once", later)
			}
			if draft, _, err := f.app.store.GetThreadDraft(f.worker.ID); err != nil || draft.Content != "" {
				t.Fatalf("the worker's composer = %q, %v; want it empty, the text sent", draft.Content, err)
			}
			f.requireReturned(t, sendID)
			f.requireRowGone(t, quiet)
			if rows := durableQueueRows(t, f.app, f.worker.ID); len(rows) != 0 {
				t.Fatalf("queue rows left: %+v", rows)
			}
		})
	}
}

// Two of the spawner's answers joined, one withdrawn after a failed restore
// requeued them, with or without a teardown after: the other is sent again alone, the withdrawn one never, and
// the joined row leaves the timeline whichever of them held it.
func TestAJoinedSendOfTwoAnswersKeepsTheOneNotWithdrawn(t *testing.T) {
	t.Parallel()
	for _, c := range joinedCases("the first withdrawn") {
		withdrawFirst := c.first
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newAsyncQuestionFixture(t, "async-joined-answers", store.PlacementOwned)
			holdFlushQueue(f.app)
			first := f.answered(t)
			second := f.answeredAgain(t, "question:ask2")
			texts := make(map[string]string)
			for _, row := range durableQueueRows(t, f.app, f.worker.ID) {
				texts[row.SendID] = row.Message
			}
			withdrawn, kept, withdrawnItem := second, first, "question:ask2"
			if withdrawFirst {
				withdrawn, kept, withdrawnItem = first, second, "question:ask"
			}
			requeued, quiet, stdinLog := f.joinedWithFailedRestore(t)
			later := f.resendAfterWithdrawal(t, requeued, withdrawn, stdinLog, c.teardown)
			if len(later) != 1 || !strings.Contains(later[0], texts[kept]) || strings.Contains(later[0], texts[withdrawn]) {
				t.Fatalf("written after the joined message: %q, want the answer kept (%q) alone, once", later, texts[kept])
			}
			f.requireUnanswered(t, withdrawnItem)
			f.requireRowGone(t, quiet)
			// The answer sent again keeps its row until its echo.
			if durable, _ := queuedSendIDs(t, f.app, f.worker.ID); !slices.Equal(durable, []string{kept}) {
				t.Fatalf("queue rows = %q, want the kept answer's alone", durable)
			}
		})
	}
}

// A hand-back announced behind the reconcile that closed its question
// raises nothing: the person answered first, and the question's last
// notification stays the retraction.
func TestAHandBackAnnouncedAfterItsQuestionClosedDoesNotNotify(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-late-hand-back", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	release := f.holdPrompts(t)
	// The person's answer queues the reconcile that closes the question.
	if err := f.app.SubmitAsyncAnswers(t.Context(), f.worker.ID, "person", []store.AsyncQuestionAnswer{{ItemID: "question:ask", Index: 0, Answer: "One"}}); err != nil {
		t.Fatal(err)
	}
	// Another screen withdraws the spawner's wake before it runs: the hand-back
	// still reads the question open and queues its announcement after it.
	if stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.spawner.ID, workerPromptSendIDRoot+token); err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	release()
	f.drain(t)
	sends := f.questionSends("question:ask")
	if len(sends) == 0 || !sends[len(sends)-1].Retract {
		t.Fatalf("question notifications = %+v, want the last one a retraction", sends)
	}
	for _, send := range sends {
		if !send.Retract {
			t.Fatalf("a closed question notified: %+v", sends)
		}
	}
}

// Every thread frame but a patch reconciles its thread, and that costs one
// store read whatever the thread: one asking nothing, a Claude thread
// included, or one whose open question is unchanged. A patch reads nothing.
func TestAThreadFrameReconcilesItsThreadInOneRead(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-frames", store.PlacementOwned)
	frame := func(action, id string) (int, map[string]int) {
		t.Helper()
		reads := readsUnder(f.app.store, "app.(*App).reconcileAsyncQuestions")
		f.app.tapWorkerPrompt(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: action, ID: id})
		f.drain(t)
		return reads()
	}
	for _, id := range []string{f.spawner.ID, f.worker.ID} {
		if total, by := frame(triage.ThreadActionFull, id); total != 1 {
			t.Fatalf("a frame of %s, asking nothing, read %d times: %v", id, total, by)
		}
	}
	if total, by := frame(triage.ThreadActionPatch, f.worker.ID); total != 0 {
		t.Fatalf("a patch read %d times: %v", total, by)
	}
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	if total, by := frame(triage.ThreadActionFull, f.worker.ID); total != 1 {
		t.Fatalf("a frame of the thread with an unchanged question read %d times: %v", total, by)
	}
	if again, _ := f.wake(t, f.spawner.ID); again != token {
		t.Fatalf("a frame put the unchanged question again under %s, was %s", again, token)
	}
}

// A fork carries its source's open questions, which no frame of a thread
// holding none reconciles: the fork says they changed, so they are open in
// the fork from the start.
func TestAForkOpensTheAsyncQuestionsItCarries(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-fork", store.PlacementStandalone)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	if _, err := f.app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkAtBinary(t, codexForkMock{resumedThreadID: "async-fork-provider", forkedThreadID: "async-fork-child"}),
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := f.app.store.GetThread(f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.SessionRef = "async-fork-provider"
	if err := f.app.store.UpdateThread(worker); err != nil {
		t.Fatal(err)
	}
	fork, err := f.app.ForkThread(t.Context(), f.worker.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}
	f.drain(t)
	if detail, routedTo, asking := f.app.asyncQuestionBlocked(fork.ID); !asking || detail != "Which scope?" || routedTo != "" {
		t.Fatalf("the fork asks %q of %q (%v), want its question open and the person's", detail, routedTo, asking)
	}
}

// A fork from a message opens the async questions it carries the same way.
func TestAForkFromAMessageOpensTheAsyncQuestionsItCarries(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	workspace := t.TempDir()
	writeClaudeProjectSession(t, testProviderHome(t, app), workspace, "slice-ids-source", sliceIDsTestSessionJSONL)
	source := createAppTestThread(t, app, "async-fork-message", "claude", workspace)
	source.SessionRef = "slice-ids-source"
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:0", 0, "first", `{"provider_item_id":"u0"}`)
	insertUserItemWithMeta(t, app.store, source.ID, "user:1", 1, "second", `{"provider_item_id":"u1"}`)
	if _, err := app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: source.ID, TurnIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed",
		Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, source.ID, "user:2", 2, "third", `{"provider_item_id":"u2"}`)
	seedMessageAnchor(t, app.store, source.ID, "user:1", 1, "u1", "a0")
	seedMessageAnchor(t, app.store, source.ID, "user:2", 2, "u2", "a1")
	fork, err := app.ForkThreadFromMessage(t.Context(), source.ID, "user:2")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}
	if err := app.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if detail, routedTo, asking := app.asyncQuestionBlocked(fork.ID); !asking || detail != "Which scope?" || routedTo != "" {
		t.Fatalf("the fork asks %q of %q (%v), want its question open and the person's", detail, routedTo, asking)
	}
}

// A spawn's fork carries its source's async questions, and they go to the
// spawner once the request names it, even when its prompt never reaches it.
func TestASpawnedForkPutsTheAsyncQuestionsItCarriesToItsSpawner(t *testing.T) {
	t.Parallel()
	for _, placement := range []string{threadtools.PlacementOwned, threadtools.PlacementStandalone} {
		t.Run(placement, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			source := f.forkableThread(t, "async-spawn-source-"+placement)
			if _, err := f.app.store.RecordAsyncQuestions(store.Item{
				ID: "question:ask", ThreadID: source.ID, Kind: "assistant_text", Role: "assistant", Status: "completed",
				Meta: `{"delivery":"async","questions":[{"title":"Which scope?"}]}`,
			}); err != nil {
				t.Fatal(err)
			}
			vanishingClaudeAfterTheSpawnIsChecked(t, f.app)
			_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "start", FromThread: source.ID, Placement: placement})
			made := regexp.MustCompile(`Thread (\S+) was created for it`).FindStringSubmatch(fmt.Sprint(err))
			if made == nil {
				t.Fatalf("Spawn: %v, want a refusal naming the thread it made", err)
			}
			for range 3 {
				if err := f.app.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
					t.Fatal(err)
				}
				f.app.threadRequestsWG.Wait()
			}
			if detail, routedTo, asking := f.app.asyncQuestionBlocked(made[1]); !asking || detail != "Which scope?" || routedTo != f.caller.Title {
				t.Fatalf("the spawned fork asks %q of %q (%v), want its question open and its spawner's", detail, routedTo, asking)
			}
		})
	}
}

// Shutdown joins the prompts' queue before the store closes, and nothing
// queues a reconcile once it has begun: a change emitted after it reads
// nothing.
func TestNoAsyncQuestionReconcileRunsAfterShutdownBegins(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-shutdown", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	reads := readsUnder(f.app.store, "app.(*App).reconcileAsyncQuestions")
	var during error
	f.app.shutdownStepFn = func(step string, _ error) {
		if step == "drain triage" {
			// Shutdown has begun; the prompts' queue is not yet joined.
			f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": f.worker.ID})
			if err := f.app.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
				during = err
			}
		}
	}
	if err := f.app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if during != nil {
		t.Fatal(during)
	}
	f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": f.worker.ID})
	f.app.workerPrompts.run(func() { f.app.reconcileAsyncQuestions(f.worker.ID, false) })
	f.app.workerPrompts.queue.Wait()
	if total, by := reads(); total != 0 {
		t.Fatalf("reconciles read %d times once shutdown began: %v", total, by)
	}
}

// holdPrompts holds the prompts' serial queue until the returned release
// runs, so what is queued behind it waits.
func (f asyncQuestionFixture) holdPrompts(t *testing.T) (release func()) {
	t.Helper()
	held, gate := make(chan struct{}), make(chan struct{})
	f.app.workerPrompts.queue.Go(func() {
		close(held)
		<-gate
	})
	<-held
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// A part dismissed and reopened before any reconcile reads the change is
// a new occurrence of the question: the token it was put to the spawner
// under answers nothing, and it goes to the spawner again under a new one.
func TestAnAsyncQuestionDismissedAndReopenedRetiresItsToken(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-reopen", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	old, _ := f.wake(t, f.spawner.ID)
	release := f.holdPrompts(t)
	for _, dismissed := range []bool{true, false} {
		if err := f.app.SetAsyncQuestionDismissed(t.Context(), f.worker.ID, "question:ask", 0, dismissed); err != nil {
			t.Fatal(err)
		}
	}
	release()
	f.drain(t)
	if _, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: old, Text: "One"}); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the old token's reply: %v, want the prompt closed", err)
	}
	if questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask"); err != nil || questions[0].State != "unanswered" {
		t.Fatalf("the reopened question = %+v, %v; want it unanswered", questions, err)
	}
	if token, _ := f.wake(t, f.spawner.ID); token == old {
		t.Fatal("the reopened question is still put to the spawner under its old token")
	}
}

// A reply that passed the registry's check and waits to be written while
// the question is dismissed and reopened is refused where its answer is
// claimed: it was written for an occurrence that is gone.
func TestAnAsyncReplyPausedWhileItsQuestionIsReplacedIsRefused(t *testing.T) {
	t.Parallel()
	f := newAsyncQuestionFixture(t, "async-paused", store.PlacementOwned)
	f.ask(t, "question:ask", `{"delivery":"async","questions":[{"title":"Which scope?"}]}`)
	token, _ := f.wake(t, f.spawner.ID)
	key := sendAdmissionKey(f.worker.ID, workerAnswerSendIDRoot+token)
	unlock := f.app.sendAdmissionLocks().Lock(key)
	replied := make(chan error, 1)
	go func() {
		_, _, err := f.app.answerWorkerPrompt(context.Background(), threadtools.Caller{ThreadID: f.spawner.ID}, threadtools.ReplyCall{Token: token, Text: "One"})
		replied <- err
	}()
	// The reply waits on the answer's admission, past the registry's check.
	waitUntil(t, 10*time.Second, func() bool { return f.app.sendAdmissionLocks().Refs(key) == 2 })
	for _, dismissed := range []bool{true, false} {
		if err := f.app.SetAsyncQuestionDismissed(t.Context(), f.worker.ID, "question:ask", 0, dismissed); err != nil {
			unlock()
			t.Fatal(err)
		}
	}
	f.drain(t)
	unlock()
	if err := <-replied; !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the paused reply: %v, want it refused as closed", err)
	}
	questions, err := f.app.store.ListAsyncQuestions(f.worker.ID, "question:ask")
	if err != nil || questions[0].State != "unanswered" || questions[0].Answer != "" {
		t.Fatalf("the question = %+v, %v; want it unanswered", questions, err)
	}
	for _, row := range durableQueueRows(t, f.app, f.worker.ID) {
		if strings.HasPrefix(row.SendID, workerAnswerSendIDRoot) {
			t.Fatalf("the refused reply was queued: %+v", row)
		}
	}
}
