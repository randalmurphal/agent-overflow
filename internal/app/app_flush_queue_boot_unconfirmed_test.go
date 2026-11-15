package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// A message queued behind a running Claude turn is written to the CLI with a
// quiet row that reserves its place, and shown only once the CLI confirms it.
// The app dies before that. The next boot finds the row still unconfirmed:
// the message goes back to the composer and its row leaves the timeline, so
// a message the model may never have seen does not read as sent.
func TestBootReturnsAnUnconfirmedQueuedMessageToTheComposer(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("boot-unconfirmed-quiet")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-2", ThreadID: thread.ID, TurnIndex: 2, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now())})

	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "written, never read", SendMessageOptions{SendID: "send-unread"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	var quiet store.Item
	waitUntil(t, 10*time.Second, func() bool {
		items, err := app.store.ListTurnItems(thread.ID, 2)
		if err != nil {
			t.Fatalf("ListTurnItems: %v", err)
		}
		for _, item := range items {
			if item.Kind == "user_text" && item.Summary == "written, never read" {
				quiet = item
				return len(durableQueueRows(t, app, thread.ID)) == 0
			}
		}
		return false
	})

	// The crash: the boot sweeps run over what the store holds.
	app.settlePriorInstance()
	app.restoreDurableFlushQueueAtBoot()

	if _, found, err := app.store.GetThreadItem(thread.ID, quiet.ID); err != nil || found {
		t.Fatalf("row %s after boot: found=%v err=%v, want it gone", quiet.ID, found, err)
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "written, never read" {
		t.Fatalf("draft = %q, want the unconfirmed message", draft.Content)
	}
}

// A message whose queue row outlives its provider write (an answer to an
// agent's question keeps the row until the echo) and whose quiet row is also
// unconfirmed is one message: the boot puts it in the composer once.
func TestBootRestoresAMessageWithBothHomesOnce(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("boot-unconfirmed-both")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
		ID: "queue:answer", ThreadID: thread.ID, SendID: "send-answer", Message: "the answer", EnqueuedAt: 1,
	}); err != nil {
		t.Fatalf("InsertFlushQueueItem: %v", err)
	}
	meta, err := json.Marshal(map[string]any{"sendId": "send-answer", "pendingFlush": true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "the answer", Meta: string(meta), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}

	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.Count(draft.Content, "the answer") != 1 {
		t.Fatalf("draft = %q, want the message once", draft.Content)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil || found {
		t.Fatalf("quiet row after boot: found=%v err=%v, want it gone", found, err)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows = %+v, want none", rows)
	}
}

// A confirmed row is history: the boot leaves it alone.
func TestBootLeavesAConfirmedQueuedMessageInTheTimeline(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("boot-confirmed")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "read by the model", Meta: `{"sendId":"send-read","provider_item_id":"uuid-1"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}

	app.restoreDurableFlushQueueAtBoot()

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil || !found {
		t.Fatalf("confirmed row after boot: found=%v err=%v, want it kept", found, err)
	}
	if draft, _, err := app.store.GetThreadDraft(thread.ID); err != nil || draft.Content != "" {
		t.Fatalf("draft = %q (%v), want it empty", draft.Content, err)
	}
}
