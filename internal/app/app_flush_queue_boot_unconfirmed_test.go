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
	"agent-overflow/internal/triage"
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
		claude.Config{Retention: claude.UserHomeRetention(0), Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
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

// The boot's composer write, the removal of the unconfirmed row and the
// return of the review comments the row claimed are one write: a boot that
// fails part way leaves all of them for the next boot, which puts the
// message in the composer once and the comments back in drafts.
func TestBootThatFailsPartWayRestoresTheUnconfirmedMessageOnce(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	app.triage = triage.NewRouter(app.store, rec.captureChannel)
	app.configureTriageQueueCallbacks()
	thread := testThread("boot-unconfirmed-part-way")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	const rowID = "user:1:flush:1"
	owner := store.ThreadReviewOwner(thread.ID)
	comment, err := app.store.CreateDiffReviewComment(store.DiffReviewComment{
		ID: "comment-1", ThreadID: thread.ID, Scope: string(store.DiffReviewScopeWorkspace), SourceKey: "diff-1",
		FilePath: "a.go", NewLine: 1, Side: "new", Body: "Rename it.", CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}
	if _, err := app.store.ClaimDiffReviewCommentsSent(owner, comment.Scope, comment.SourceKey,
		[]store.DiffReviewCommentSnapshot{{ID: comment.ID, Body: comment.Body}},
		store.DiffReviewDelivery{SentAt: 2, TurnID: thread.ID + ":1", ThreadID: thread.ID, ItemID: rowID, Queued: true}); err != nil {
		t.Fatalf("ClaimDiffReviewCommentsSent: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: rowID, ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "written, never read", Meta: `{"sendId":"send-unread","pendingFlush":true}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}

	execOnStore(t, dbPath, `CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = '`+rowID+`' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`)
	app.restoreDurableFlushQueueAtBoot()
	execOnStore(t, dbPath, `DROP TRIGGER refuse_row_delete`)
	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.Count(draft.Content, "written, never read") != 1 {
		t.Fatalf("draft = %q, want the message once", draft.Content)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, rowID); err != nil || found {
		t.Fatalf("row after the second boot: found=%v err=%v, want it gone", found, err)
	}
	if got, err := app.store.GetDiffReviewComment(owner, comment.ID); err != nil || got.Status != "draft" {
		t.Fatalf("comment after the second boot = %+v, %v; want a draft", got, err)
	}
}

// The boot takes back only residue: a marked row in its thread's last turn.
// A marked row a later turn follows is history the next session ran with,
// and stays where it is with the composer untouched, whether it is alone in
// its thread or beside residue; the residue goes to the composer once,
// however many boots run.
func TestBootTakesBackOnlyResidueOfTheLastTurn(t *testing.T) {
	t.Parallel()
	const history, residue = "shown as sent, a later turn followed", "written as the app died"
	for _, tc := range []struct {
		name             string
		history, residue bool
		wantDraft        string
	}{
		{name: "history", history: true},
		{name: "residue", residue: true, wantDraft: residue},
		{name: "both", history: true, residue: true, wantDraft: residue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _ := newAppForFlushQueueRPC(t)
			thread := testThread("boot-residue-" + tc.name)
			thread.Provider = string(provider.Claude)
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			now := time.Now().UnixMilli()
			rows := []store.Item{
				{ID: "user:9", TurnIndex: 9, Kind: "user_text", Role: "user", Summary: "turn nine", Meta: `{"sendId":"send-9"}`},
			}
			if tc.history {
				rows = append(rows, store.Item{ID: "user:9:flush:1", TurnIndex: 9, Kind: "user_text", Role: "user", Summary: history, Meta: `{"sendId":"send-old","pendingFlush":true}`})
			}
			rows = append(rows,
				store.Item{ID: "assistant:9", TurnIndex: 9, Kind: "assistant_text", Role: "assistant", Summary: "the interrupted answer", Meta: `{}`},
				store.Item{ID: "user:10", TurnIndex: 10, Kind: "user_text", Role: "user", Summary: "turn ten", Meta: `{"sendId":"send-10"}`},
				store.Item{ID: "assistant:10", TurnIndex: 10, Kind: "assistant_text", Role: "assistant", Summary: "the next answer", Meta: `{}`},
			)
			if tc.residue {
				rows = append(rows, store.Item{ID: "user:10:flush:1", TurnIndex: 10, Kind: "user_text", Role: "user", Summary: residue, Meta: `{"sendId":"send-new","pendingFlush":true}`})
			}
			for _, row := range rows {
				row.ThreadID, row.Status, row.CreatedAt, row.UpdatedAt = thread.ID, "completed", now, now
				if _, err := app.store.AppendItem(row); err != nil {
					t.Fatalf("AppendItem(%s): %v", row.ID, err)
				}
			}

			app.restoreDurableFlushQueueAtBoot()
			app.restoreDurableFlushQueueAtBoot()

			draft, _, err := app.store.GetThreadDraft(thread.ID)
			if err != nil {
				t.Fatalf("GetThreadDraft: %v", err)
			}
			if draft.Content != tc.wantDraft {
				t.Fatalf("draft = %q, want %q", draft.Content, tc.wantDraft)
			}
			if tc.history {
				item, found, err := app.store.GetThreadItem(thread.ID, "user:9:flush:1")
				if err != nil || !found || item.Summary != history {
					t.Fatalf("history row after boot: %+v found=%v err=%v, want it kept", item, found, err)
				}
			}
			if tc.residue {
				if _, found, err := app.store.GetThreadItem(thread.ID, "user:10:flush:1"); err != nil || found {
					t.Fatalf("residue row after boot: found=%v err=%v, want it gone", found, err)
				}
			}
		})
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
