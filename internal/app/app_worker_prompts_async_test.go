package app

// A spawned Codex thread's async questions go to its spawner as its other
// prompts do (app_worker_prompts_async.go): the same wake, thread_reply and
// hand-back, answered through the async-answer path, with every lifecycle
// the routing of its approvals has.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

type asyncQuestionFixture struct {
	app      *App
	spawner  store.Thread
	worker   store.Thread
	notified *recordingNotificationSender
}

// newAsyncQuestionFixture is a spawner and the Codex worker it spawned,
// owned or standalone, with no provider: every session start fails, so
// what is queued stays queued.
func newAsyncQuestionFixture(t *testing.T, id string, placement string) asyncQuestionFixture {
	t.Helper()
	a, _ := newAppForFlushQueueRPC(t)
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
	return asyncQuestionFixture{app: a, spawner: spawner, worker: worker, notified: notified}
}

func (f asyncQuestionFixture) drain(t *testing.T) {
	t.Helper()
	for range 3 {
		if err := f.app.drainWorkerPrompts(context.Background(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		f.app.waitAgentNoticeWork()
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
	if payload.Origin != usermessage.OriginAgentThread || payload.OriginThread == nil || payload.OriginThread.ThreadID != f.spawner.ID {
		t.Fatalf("the answer is attributed %q %+v, want the spawner", payload.Origin, payload.OriginThread)
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
				err = f.app.DeleteThread(f.spawner.ID)
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
