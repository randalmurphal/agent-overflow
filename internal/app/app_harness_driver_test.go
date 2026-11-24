package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// HarnessStartSession runs the production start for the thread it names:
// the one the start seam stands in for, under the thread's action lock.
func TestHarnessStartSessionRunsTheProductionStart(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("harness-start")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	var started []string
	app.startSessionFn = func(threadID string) error {
		started = append(started, threadID)
		return nil
	}
	if err := NewHarness(app, HarnessPaths{}).HarnessStartSession(thread.ID); err != nil {
		t.Fatalf("HarnessStartSession: %v", err)
	}
	if len(started) != 1 || started[0] != thread.ID {
		t.Fatalf("starts = %v, want one of %s", started, thread.ID)
	}
}

// HarnessListItems answers every item of the thread in order, through the
// projection the paged loads apply.
func TestHarnessListItemsAnswersEveryItemOfTheThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("harness-items")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:1", 1, "second", `{}`)
	items, err := NewHarness(app, HarnessPaths{}).HarnessListItems(thread.ID, true)
	if err != nil {
		t.Fatalf("HarnessListItems: %v", err)
	}
	var got []string
	for _, item := range items {
		got = append(got, item.ID+"="+item.Summary)
	}
	if strings.Join(got, ",") != "user:0=first,user:1=second" {
		t.Fatalf("items = %v, want both user rows in order", got)
	}
}

// The soak's send goes through the page's entry with the page's options: a
// send to a thread whose turn is still open queues behind it, where the
// options of an older client are refused as "already working", and the
// row it leaves is found by its send id rather than a predicted user:<turn>.
func TestHarnessSendQueuesBehindAnOpenTurnAsThePageDoes(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("harness-send")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "receipt-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	installReceiptCaptureSession(t, app, thread)
	host := &harnessHost{app: app}
	if err := host.SendMessage(thread.ID, "while the turn runs"); err != nil {
		t.Fatalf("send to a working thread: %v", err)
	}
	app.flushDispatch.wg.Wait()
	if pending := app.triage.DeferredPendingFlushItemCount(thread.ID); pending != 1 {
		t.Fatalf("pending sends = %d, want the one send queued behind the turn", pending)
	}
}
