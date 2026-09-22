package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

func TestQueuedAsyncAnswersRestoreWithoutReopeningOrResending(t *testing.T) {
	a, _ := setupE2EApp(t)
	thread, err := createTestThread(t, a, "codex", t.TempDir(), "gpt-5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.store.RecordAsyncQuestions(store.Item{ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: `{"delivery":"async","questions":[{"title":"Which?"},{"title":"When?"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	answers := []store.AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "First"}}
	body, _, err := a.store.AsyncAnswerMessage(thread.ID, "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.QueueAsyncAnswers(store.FlushQueueItem{ID: "queue", ThreadID: thread.ID, SendID: "send", Message: body}, answers); err != nil {
		t.Fatal(err)
	}
	a.restoreDurableFlushQueueAtBoot()
	draft, found, err := a.store.GetThreadDraft(thread.ID)
	if err != nil || !found || draft.Content != body {
		t.Fatalf("restored draft=%+v %v", draft, err)
	}
	rows, err := a.store.ListAsyncQuestions(thread.ID, item.ID)
	if err != nil || len(rows) != 2 || rows[0].State != "restored" || rows[1].State != "unanswered" {
		t.Fatalf("restored questions=%+v %v", rows, err)
	}
	pending, err := a.store.ListAsyncQuestions(thread.ID, "")
	if err != nil || len(pending) != 1 || pending[0].Index != 1 {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	a.restoreDurableFlushQueueAtBoot()
	draft, _, err = a.store.GetThreadDraft(thread.ID)
	if err != nil || draft.Content != body {
		t.Fatalf("repeated restore duplicated answers: %+v %v", draft, err)
	}
}

func TestAsyncAnswerSessionDeathRetainsRecoveryAfterStateWriteFailure(t *testing.T) {
	a, path := newTestAppWithStorePath(t)
	rec := &emitRecorder{}
	a.testEmitHook = rec.capture
	a.triage = triage.NewRouter(a.store, rec.captureChannel)
	a.configureTriageQueueCallbacks()
	thread := testThread("async-restore-failure")
	thread.Provider = "codex"
	thread.WorkspacePath = t.TempDir()
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	item, err := a.store.RecordAsyncQuestions(store.Item{ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: `{"delivery":"async","questions":[{"title":"Which?"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	answers := []store.AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "First"}}
	body, _, err := a.store.AsyncAnswerMessage(thread.ID, "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.registerQueueItem(thread.ID, body, SendMessageOptions{SendID: "send"}, injectedQueueOptions{
		preserveDraft: true,
		persist:       func(row store.FlushQueueItem) error { return a.store.QueueAsyncAnswers(row, answers) },
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_question_restore BEFORE UPDATE OF state ON async_questions WHEN NEW.state='restored' BEGIN SELECT RAISE(ABORT,'injected restoration failure'); END`); err != nil {
		t.Fatal(err)
	}
	a.restoreUnconfirmedQueueOnSessionDeath(thread.ID)
	if !rec.hasEvent("thread:error_notice") {
		t.Fatal("restoration failure was not surfaced to the user")
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 {
		t.Fatal("lost recovery record after question state write failed")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_question_restore`); err != nil {
		t.Fatal(err)
	}
	a.restoreDurableFlushQueueAtBoot()
	rows, err := a.store.ListAsyncQuestions(thread.ID, item.ID)
	if err != nil || len(rows) != 1 || rows[0].State != "restored" {
		t.Fatalf("recovery left answer stuck: %+v %v", rows, err)
	}
	draft, found, err := a.store.GetThreadDraft(thread.ID)
	if err != nil || !found || draft.Content != body {
		t.Fatalf("repeated restore duplicated draft: %+v %v", draft, err)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 0 {
		t.Fatalf("completed recovery retained queue: %+v", rows)
	}
}

func TestAsyncAnswersRejectAnUnrelatedAcceptedSendIdentity(t *testing.T) {
	a, _ := setupE2EApp(t)
	thread, err := createTestThread(t, a, "codex", t.TempDir(), "gpt-5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.store.RecordAsyncQuestions(store.Item{ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: `{"delivery":"async","questions":[{"title":"Which?"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.InsertFlushQueueItem(store.FlushQueueItem{ID: "queue", ThreadID: thread.ID, SendID: "collision", Message: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	err = a.SubmitAsyncAnswers(context.Background(), thread.ID, "collision", []store.AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}})
	if err == nil || !strings.Contains(err.Error(), "different message") {
		t.Fatalf("expected identity collision, got %v", err)
	}
	rows, err := a.store.ListAsyncQuestions(thread.ID, "")
	if err != nil || len(rows) != 1 || rows[0].State != "unanswered" {
		t.Fatalf("collision changed question: %+v %v", rows, err)
	}
}

func TestAsyncAnswerDispatchRetainsRecoveryUntilProviderEcho(t *testing.T) {
	a, _ := setupE2EApp(t)
	thread, err := createTestThread(t, a, "codex", t.TempDir(), "gpt-5", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.store.RecordAsyncQuestions(store.Item{ID: "question:ask", ThreadID: thread.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: `{"delivery":"async","questions":[{"title":"Which?"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	answers := []store.AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "First"}}
	body, _, err := a.store.AsyncAnswerMessage(thread.ID, "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.QueueAsyncAnswers(store.FlushQueueItem{ID: "queue", ThreadID: thread.ID, SendID: "send", Message: body}, answers); err != nil {
		t.Fatal(err)
	}
	// A successful steer only queues input inside Codex. The process can still
	// exit before the model consumes it and echoes its stable message identity.
	a.flushQueueSettlement(thread.ID, "queue", nil).Settle()
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 {
		t.Fatal("provider write discarded the unconfirmed answer's recovery record")
	}
	a.restoreDurableFlushQueueAtBoot()
	rows, err := a.store.ListAsyncQuestions(thread.ID, item.ID)
	if err != nil || rows[0].State != "restored" {
		t.Fatalf("crash before echo stranded submission: %+v %v", rows, err)
	}
	draft, found, err := a.store.GetThreadDraft(thread.ID)
	if err != nil || !found || draft.Content != body {
		t.Fatalf("unconfirmed answer not restored: %+v %v", draft, err)
	}
}
