package store

import (
	"bytes"
	"context"
	"testing"
)

func TestAsyncQuestionHistoryTransferCloneAndCut(t *testing.T) {
	s, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, s, "q")
	mustCreateThread(t, s, "fork")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: body}, answers); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(Item{ID: "answer", ThreadID: "q", TurnIndex: 1, Kind: "user_text", Role: "user", Meta: `{"sendId":"send","provider_item_id":"native"}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	if count, err := s.PrepareThreadHistory(context.Background(), "q"); err != nil || count != 1 {
		t.Fatalf("question history preparation: %d %v", count, err)
	}
	var exported bytes.Buffer
	if err := s.ExportThreadHistory(context.Background(), "q", &exported); err != nil {
		t.Fatal(err)
	}
	thread, err := s.GetThread("q")
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.ImportThreadHistory(context.Background(), thread, &exported); err != nil {
		t.Fatal(err)
	}
	rows, err := destination.ListAsyncQuestions("q", item.ID)
	if err != nil || len(rows) != 2 || rows[0].State != "delivered" || rows[0].UserItemID != "answer" || rows[1].State != "dismissed" {
		t.Fatalf("transfer=%+v %v", rows, err)
	}
	cut := 0
	ids, err := s.CloneThreadItems("q", "fork", &cut)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("fork", ids[item.ID])
	if err != nil || len(rows) != 2 || rows[0].State != "unanswered" || rows[0].SendID != "" || rows[1].State != "dismissed" {
		t.Fatalf("fork=%+v %v", rows, err)
	}
	if _, _, err := s.DeleteConversationFromItem("q", "answer"); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", item.ID)
	if err != nil || rows[0].State != "unanswered" || rows[0].Answer != "" {
		t.Fatalf("answer cut=%+v %v", rows, err)
	}
	if _, _, err := s.DeleteConversationFromTurn("q", 0); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAsyncQuestions("q", item.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("question cut=%+v %v", rows, err)
	}
}

func TestAsyncQuestionRetriesCannotChangeQuestionSetOrContent(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}, {ItemID: item.ID, Index: 1, Answer: "Tomorrow"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	queue := FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: "unrelated message"}
	if err := s.QueueAsyncAnswers(queue, answers); err == nil {
		t.Fatal("claimed answers for unrelated message")
	}
	queue.Message = body
	if err := s.QueueAsyncAnswers(queue, answers); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AsyncAnswerMessage("q", "send", answers[:1]); err == nil {
		t.Fatal("retried only part of accepted snapshot")
	}
	item.Meta = `{"delivery":"async","questions":[{"title":"Replaced?"}]}`
	if _, err := s.RecordAsyncQuestions(item); err == nil {
		t.Fatal("mutated an immutable question")
	}
}
