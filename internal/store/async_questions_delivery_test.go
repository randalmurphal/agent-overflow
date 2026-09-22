package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestAsyncAnswerEchoRequiresIdentityAndSettlesOnMetaUpdate(t *testing.T) {
	for _, identity := range []any{nil, "", "  ", 4, false} {
		t.Run(stringMustJSON(t, identity), func(t *testing.T) {
			s := newTestStore(t)
			mustCreateThread(t, s, "q")
			mustCreateThread(t, s, "other")
			item := questionFixture(t, s, "q", "question:call")
			answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
			body, _, err := s.AsyncAnswerMessage("q", "send", answers)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: body}, answers); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "other-queue", ThreadID: "other", SendID: "send", Message: "unrelated"}); err != nil {
				t.Fatal(err)
			}
			meta := stringMustJSON(t, map[string]any{"sendId": "send", "provider_item_id": identity})
			if err := s.InsertItem(Item{ID: "answer", ThreadID: "q", ItemIndex: 1, Kind: "user_text", Role: "user", Meta: meta}); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ListAsyncQuestions("q", item.ID)
			if err != nil || rows[0].State != "submitted" {
				t.Fatalf("unconfirmed echo marked delivered: %+v %v", rows, err)
			}
			if err := s.DeleteDispatchedFlushQueueItem("queue"); err != nil {
				t.Fatal(err)
			}
			if queued, err := s.ListFlushQueueItems("q"); err != nil || len(queued) != 1 {
				t.Fatalf("lost unconfirmed answer: %+v %v", queued, err)
			}
			if _, err := s.db.Exec(`UPDATE items SET meta='{"sendId":"send","provider_item_id":"native"}' WHERE thread_id='q' AND id='answer'`); err != nil {
				t.Fatal(err)
			}
			rows, err = s.ListAsyncQuestions("q", item.ID)
			if err != nil || rows[0].State != "delivered" || rows[0].UserItemID != "answer" {
				t.Fatalf("echo update failed: %+v %v", rows, err)
			}
			if queued, err := s.ListFlushQueueItems("q"); err != nil || len(queued) != 0 {
				t.Fatalf("confirmed answer still queued: %+v %v", queued, err)
			}
			if queued, err := s.ListFlushQueueItems("other"); err != nil || len(queued) != 1 {
				t.Fatalf("echo crossed threads: %+v %v", queued, err)
			}
		})
	}
}

func TestConcurrentAsyncAnswersHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}, {ItemID: item.ID, Index: 1, Answer: "Tomorrow"}}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		sendID := fmt.Sprintf("send-%d", i)
		body, _, err := s.AsyncAnswerMessage("q", sendID, answers)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			results <- s.QueueAsyncAnswers(FlushQueueItem{ID: sendID, ThreadID: "q", SendID: sendID, Message: body}, answers)
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrAsyncQuestionHandled) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
	queued, err := s.ListFlushQueueItems("q")
	if err != nil || len(queued) != 1 {
		t.Fatalf("duplicate admission: %+v %v", queued, err)
	}
	rows, err := s.ListAsyncQuestions("q", item.ID)
	if err != nil || len(rows) != 2 || rows[0].SendID != queued[0].SendID || rows[1].SendID != queued[0].SendID {
		t.Fatalf("split ownership: %+v %v", rows, err)
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRestoreFlushQueueCannotOverwriteNewerDraftOrCrossThreads(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	mustCreateThread(t, s, "other")
	expected, _, err := s.GetThreadDraft("q")
	if err != nil {
		t.Fatal(err)
	}
	newer := expected
	newer.Content = "newer edit"
	if _, err := s.UpsertThreadDraft(newer); err != nil {
		t.Fatal(err)
	}
	merged := expected
	merged.Content = "restored answer"
	if _, err := s.RestoreFlushQueueToDraft(expected, merged, nil); err == nil {
		t.Fatal("overwrote racing composer edit")
	}
	if err := s.InsertFlushQueueItem(FlushQueueItem{ID: "other-queue", ThreadID: "other", Message: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFlushQueueToDraft(newer, merged, []string{"other-queue"}); err == nil {
		t.Fatal("retired another thread's queue")
	}
	if current, _, err := s.GetThreadDraft("q"); err != nil || current.Content != newer.Content {
		t.Fatalf("invalid restoration changed draft: %+v %v", current, err)
	}
}
