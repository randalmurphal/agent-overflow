package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func questionFixture(t *testing.T, s *Store, threadID, itemID string) Item {
	t.Helper()
	item, err := s.RecordAsyncQuestions(Item{ID: itemID, ThreadID: threadID, TurnIndex: 0, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "Pick a scope", Meta: `{"delivery":"async","questions":[{"title":"Pick a scope","options":["One","Two"]},{"title":"Deadline?"}]}`, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestAsyncQuestionsSubmissionLifecycle(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "Two"}}
	body, already, err := s.AsyncAnswerMessage("q", "send-1", answers)
	if err != nil || already || body != "Question: Pick a scope\nAnswer: Two" {
		t.Fatalf("message=%q already=%v err=%v", body, already, err)
	}
	queue := FlushQueueItem{ID: "queue-1", ThreadID: "q", SendID: "send-1", Message: body, Payload: json.RawMessage(`{}`), EnqueuedAt: 3}
	if err := s.QueueAsyncAnswers(queue, answers, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAsyncQuestions(item); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 2 || rows[0].State != "submitted" || rows[1].State != "unanswered" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if _, already, err := s.AsyncAnswerMessage("q", "send-1", answers); err != nil || !already {
		t.Fatalf("retry=%v %v", already, err)
	}
	other := queue
	other.ID = "queue-2"
	other.SendID = "send-2"
	if err := s.QueueAsyncAnswers(other, answers, nil); err == nil {
		t.Fatal("second client duplicated answer")
	}
	if err := s.InsertItem(Item{ID: "user-answer", ThreadID: "q", ItemIndex: 1, Kind: "user_text", Role: "user", Meta: `{"sendId":"send-1","provider_item_id":"native-answer"}`, CreatedAt: 4, UpdatedAt: 4}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", item.ID)
	if err != nil || rows[0].State != "delivered" || rows[0].UserItemID != "user-answer" {
		t.Fatalf("delivery=%+v %v", rows, err)
	}
	if queued, err := s.ListFlushQueueItems("q"); err != nil || len(queued) != 0 {
		t.Fatalf("confirmed echo retained recovery: %+v %v", queued, err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 0, false); err == nil {
		t.Fatal("reopened a delivered answer")
	}
}

func TestAsyncQuestionsQueueFailureIsAtomic(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	for _, answers := range [][]AsyncQuestionAnswer{
		{{ItemID: item.ID, Index: 0, Answer: "One"}, {ItemID: item.ID, Index: 9, Answer: "bad"}},
		{{ItemID: item.ID, Index: 0, Answer: "One"}, {ItemID: item.ID, Index: 0, Answer: "Two"}},
	} {
		if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send"}, answers, nil); err == nil {
			t.Fatal("invalid batch accepted")
		}
		rows, err := s.ListAsyncQuestions("q", "")
		if err != nil || len(rows) != 2 || rows[0].State != "unanswered" {
			t.Fatalf("partial write: %+v %v", rows, err)
		}
	}
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "occupied", ThreadID: "q", Message: "existing message"}); err != nil {
		t.Fatal(err)
	}
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "occupied", ThreadID: "q", SendID: "send", Message: body}, answers, nil); err == nil {
		t.Fatal("queue insert should conflict")
	}
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || rows[0].State != "unanswered" {
		t.Fatalf("queue failure claimed question: %+v %v", rows, err)
	}
}

func TestAsyncQuestionSurvivesCacheRemovalAndRejectsChangedReplay(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id=? AND id=?`, "q", item.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("cache deletion lost question: %+v %v", rows, err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	item.Meta = `{"delivery":"async","questions":[{"title":"Changed"}]}`
	if _, err := s.RecordAsyncQuestions(item); err == nil {
		t.Fatal("changed replay accepted after cache eviction")
	}
}

func TestAsyncQuestionsImportedHistoryRequiresExplicitOpen(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := Item{ID: "historic", ThreadID: "q", Kind: "assistant_text", Role: "assistant", Meta: `{"delivery":"async","questions":[{"title":"Still needed?"}]}`, CreatedAt: 1, UpdatedAt: 1}
	if err := s.InsertItem(item); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("historic popup: %+v %v", rows, err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 1 || rows[0].State != "unanswered" {
		t.Fatalf("open=%+v %v", rows, err)
	}
}

func TestAsyncQuestionsSurviveReopenAndQueueDrop(t *testing.T) {
	path := newTestStorePath(t)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: body}, answers, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	rows, err := s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 2 || rows[0].State != "submitted" || rows[1].State != "unanswered" {
		t.Fatalf("restart=%+v %v", rows, err)
	}
	if err := s.DeleteFlushQueueItemsForThread("q", ""); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", "")
	if err != nil || len(rows) != 2 || rows[0].State != "unanswered" || rows[0].SendID != "" {
		t.Fatalf("dropped answers=%+v %v", rows, err)
	}
}

func TestRestoredAsyncAnswerSettlesAtomicallyWithQueueRemoval(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: body}, answers, nil); err != nil {
		t.Fatal(err)
	}
	expected, _, err := s.GetThreadDraft("q")
	if err != nil {
		t.Fatal(err)
	}
	merged := expected
	merged.Content = body
	if _, err := s.db.Exec(`CREATE TRIGGER reject_queue_removal BEFORE DELETE ON flush_queue_items BEGIN SELECT RAISE(ABORT,'test deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFlushQueueToDraft(expected, merged, []string{"queue"}); err == nil {
		t.Fatal("expected deletion failure")
	}
	if draft, _, err := s.GetThreadDraft("q"); err != nil || draft != expected {
		t.Fatalf("failed restoration wrote its draft: %+v %v", draft, err)
	}
	rows, err := s.ListAsyncQuestions("q", item.ID)
	if err != nil || rows[0].State != "submitted" {
		t.Fatalf("partial restoration: %+v %v", rows, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_queue_removal`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFlushQueueToDraft(expected, merged, []string{"queue"}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", item.ID)
	if err != nil || rows[0].State != "restored" {
		t.Fatalf("restoration: %+v %v", rows, err)
	}
	queued, err := s.ListFlushQueueItems("q")
	if err != nil || len(queued) != 0 {
		t.Fatalf("retained restored queue: %+v %v", queued, err)
	}
}

// The boot lists the threads with a question still unanswered, on the
// pending index, and a rest dismisses only those questions.
func TestUnansweredAsyncQuestionsAreListedAndDismissedByThread(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"open", "answered", "other"} {
		mustCreateThread(t, s, id)
		questionFixture(t, s, id, "question:call")
	}
	answers := []AsyncQuestionAnswer{{ItemID: "question:call", Index: 0, Answer: "Two"}, {ItemID: "question:call", Index: 1, Answer: "Friday"}}
	body, _, err := s.AsyncAnswerMessage("answered", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "answered", SendID: "send", Message: body, Payload: json.RawMessage(`{}`), EnqueuedAt: 3}, answers, nil); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ThreadsWithUnansweredAsyncQuestions(); err != nil || !slices.Equal(slices.Sorted(slices.Values(ids)), []string{"open", "other"}) {
		t.Fatalf("threads with unanswered questions = %q, %v", ids, err)
	}
	assertPlanUses(t, s.db, "idx_async_questions_pending", "EXPLAIN QUERY PLAN "+threadsWithUnansweredAsyncQuestionsSQL)
	if n, err := s.DismissUnansweredAsyncQuestions("open"); err != nil || n != 2 {
		t.Fatalf("dismissed %d, %v; want both of its questions", n, err)
	}
	if n, err := s.DismissUnansweredAsyncQuestions("answered"); err != nil || n != 0 {
		t.Fatalf("dismissed %d answered questions, %v", n, err)
	}
	if ids, err := s.ThreadsWithUnansweredAsyncQuestions(); err != nil || !slices.Equal(ids, []string{"other"}) {
		t.Fatalf("threads with unanswered questions after the dismissal = %q, %v", ids, err)
	}
}

// A thread's unanswered questions are read only while this computer holds
// the thread unarchived: an archived thread, one being deleted and one
// moved away read none, and an unarchived one reads them again.
func TestUnansweredAsyncQuestionsAreReadOnlyForAThreadHeldHere(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"archived", "deleting", "moved"} {
		mustCreateThread(t, s, id)
		questionFixture(t, s, id, "question:call")
	}
	if _, err := s.db.Exec(`UPDATE threads SET archived=1 WHERE id='archived'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE threads SET deleting=1 WHERE id='deleting'`); err != nil {
		t.Fatal(err)
	}
	seedCompletedOutgoingMove(t, s, "moved")
	for _, id := range []string{"archived", "deleting", "moved"} {
		if groups, err := s.UnansweredAsyncQuestions(id); err != nil || len(groups) != 0 {
			t.Fatalf("the questions of %s = %+v, %v; want none", id, groups, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE threads SET archived=0 WHERE id='archived'`); err != nil {
		t.Fatal(err)
	}
	if groups, err := s.UnansweredAsyncQuestions("archived"); err != nil || len(groups) != 1 || len(groups[0].Questions) != 2 {
		t.Fatalf("the unarchived thread's questions = %+v, %v; want both", groups, err)
	}
}

// An app-written answer the provider did not take in goes back whole: its
// queue row goes and its questions are unanswered, in one transaction. One
// the provider took in, or returned before, reopens nothing.
func TestAReturnedAsyncAnswerReopensItsQuestions(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: "question:call", Index: 0, Answer: "Two"}, {ItemID: "question:call", Index: 1, Answer: "Friday"}}
	body, _, err := s.AsyncAnswerMessage("q", "agent-answer:t", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "agent-answer:t", Message: body}, answers, nil); err != nil {
		t.Fatal(err)
	}
	if items, err := s.AsyncAnswerItems("q", "agent-answer:t"); err != nil || !slices.Equal(items, []string{"question:call"}) {
		t.Fatalf("the answer's items = %q, %v", items, err)
	}
	if reopened, err := s.ReturnAsyncAnswer("q", "agent-answer:t"); err != nil || !reopened {
		t.Fatalf("return = %v, %v; want its questions reopened", reopened, err)
	}
	if groups, err := s.UnansweredAsyncQuestions("q"); err != nil || len(groups) != 1 || len(groups[0].Questions) != 2 {
		t.Fatalf("after the return = %+v, %v; want both questions unanswered", groups, err)
	}
	if queued, err := s.ListFlushQueueItems("q"); err != nil || len(queued) != 0 {
		t.Fatalf("the returned answer is still queued: %+v, %v", queued, err)
	}
	if reopened, err := s.ReturnAsyncAnswer("q", "agent-answer:t"); err != nil || reopened {
		t.Fatalf("a second return = %v, %v; want nothing reopened", reopened, err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue-2", ThreadID: "q", SendID: "agent-answer:u", Message: body}, answers, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(Item{ID: "echo", ThreadID: "q", ItemIndex: 1, Kind: "user_text", Role: "user", Meta: `{"sendId":"agent-answer:u","provider_item_id":"native"}`, CreatedAt: 4, UpdatedAt: 4}); err != nil {
		t.Fatal(err)
	}
	if reopened, err := s.ReturnAsyncAnswer("q", "agent-answer:u"); err != nil || reopened {
		t.Fatalf("returning a delivered answer = %v, %v; want nothing reopened", reopened, err)
	}
}

// An item's unanswered questions are read together with its revision, on
// the pending index, and an answer claimed against a revision the item
// left is refused whole, its questions and its queue row untouched.
func TestAsyncAnswersAreClaimedAgainstTheirItemsRevision(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	questionFixture(t, s, "q", "question:call")
	groups, err := s.UnansweredAsyncQuestions("q")
	if err != nil || len(groups) != 1 || groups[0].ItemID != "question:call" || len(groups[0].Questions) != 2 || groups[0].Questions[1].Title != "Deadline?" {
		t.Fatalf("unanswered = %+v, %v", groups, err)
	}
	assertPlanUses(t, s.db, "idx_async_questions_pending", "EXPLAIN QUERY PLAN "+unansweredAsyncQuestionsSQL, "q")
	asked := groups[0].Revision
	if err := s.SetAsyncQuestionDismissed("q", "question:call", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsyncQuestionDismissed("q", "question:call", 1, false); err != nil {
		t.Fatal(err)
	}
	groups, err = s.UnansweredAsyncQuestions("q")
	if err != nil || len(groups) != 1 || len(groups[0].Questions) != 2 || groups[0].Revision <= asked {
		t.Fatalf("after a dismissal and a reopen = %+v, %v; want both questions at a revision past %d", groups, err, asked)
	}
	answers := []AsyncQuestionAnswer{{ItemID: "question:call", Index: 0, Answer: "Two"}}
	body, _, err := s.AsyncAnswerMessage("q", "agent-answer:t", answers)
	if err != nil {
		t.Fatal(err)
	}
	queue := FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "agent-answer:t", Message: body}
	if err := s.QueueAsyncAnswers(queue, answers, map[string]int64{"question:call": asked}); !errors.Is(err, ErrAsyncQuestionHandled) {
		t.Fatalf("an answer for the old revision: %v, want ErrAsyncQuestionHandled", err)
	}
	if rows, err := s.ListAsyncQuestions("q", "question:call"); err != nil || rows[0].State != "unanswered" || rows[0].SendID != "" {
		t.Fatalf("after the refusal = %+v, %v", rows, err)
	}
	if queued, err := s.ListFlushQueueItems("q"); err != nil || len(queued) != 0 {
		t.Fatalf("the refused answer was queued: %+v, %v", queued, err)
	}
	if err := s.QueueAsyncAnswers(queue, answers, map[string]int64{"question:call": groups[0].Revision}); err != nil {
		t.Fatalf("an answer for the current revision: %v", err)
	}
}
