package app

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func TestEmptySendRefusedBeforeDraftHistoryQueueOrRuntimeEffects(t *testing.T) {
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		for _, entry := range []string{"send", "legacy", "queue", "steer", "workflow"} {
			t.Run(providerName+"/"+entry, func(t *testing.T) {
				app, _ := newAppForFlushQueueRPC(t)
				thread := testThread("empty-send")
				thread.Provider = providerName
				if entry == "workflow" {
					thread.Mode = "workflow"
				}
				if err := app.store.CreateThread(thread); err != nil {
					t.Fatal(err)
				}
				if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{ThreadID: thread.ID, Content: "keep my draft", Attachments: "[]", TerminalChips: "[]"}); err != nil {
					t.Fatal(err)
				}
				opts := SendMessageOptions{RuntimeMode: "plan", SendID: "empty-attempt", AttachmentIDs: []string{"", " "}, ReconcileBySendID: true}
				var err error
				switch entry {
				case "send", "workflow":
					_, err = app.SendMessageWithOptions(context.Background(), thread.ID, " \n\t", opts)
				case "legacy":
					err = app.SendMessage(thread.ID, "", nil)
				case "queue":
					_, err = app.RegisterQueueItem(context.Background(), thread.ID, " \n\t", opts)
				case "steer":
					_, err = app.SteerMessageWithOptions(context.Background(), thread.ID, " \n\t", opts)
				}
				if err == nil || !strings.Contains(err.Error(), "enter a message or attach a file") {
					t.Fatalf("empty admission: %v", err)
				}
				draft, found, err := app.store.GetThreadDraft(thread.ID)
				if err != nil || !found || draft.Content != "keep my draft" {
					t.Fatalf("draft changed: %+v, found=%v, %v", draft, found, err)
				}
				items, err := app.store.ListItems(thread.ID)
				if err != nil || len(items) != 0 || len(durableQueueRows(t, app, thread.ID)) != 0 || app.triage.QueuedFlushItemCount(thread.ID) != 0 {
					t.Fatalf("empty send created history or queue: %+v, %v", items, err)
				}
				after, err := app.store.GetThread(thread.ID)
				if err != nil || after.Mode != thread.Mode {
					t.Fatalf("runtime changed: %s, %v", after.Mode, err)
				}
				if _, started := app.sessionManager().get(thread.ID); started {
					t.Fatal("empty send started a provider")
				}
			})
		}
	}
}

func TestEmptyRetryReturnsAcceptedQueueReceiptAcrossSendMethods(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("empty-retry")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	opts := SendMessageOptions{SendID: "accepted"}
	first, err := app.RegisterQueueItem(context.Background(), thread.ID, "accepted once", opts)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := app.RegisterQueueItem(context.Background(), thread.ID, "", opts)
	if err != nil || queued.ID != first.ID {
		t.Fatalf("queue retry lost receipt: %+v, %v", queued, err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "", opts); err != nil {
		t.Fatalf("send retry lost receipt: %v", err)
	}
	if _, err := app.SteerMessageWithOptions(context.Background(), thread.ID, "", opts); err != nil {
		t.Fatalf("steer retry lost receipt: %v", err)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 1 || rows[0].Message != "accepted once" {
		t.Fatalf("retry changed accepted input: %+v", rows)
	}
}

func TestQueueAdmitsAttachmentAndRevisionCommentOnlyInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts SendMessageOptions
	}{
		{"attachment", SendMessageOptions{AttachmentIDs: []string{"attachment"}}},
		{"plan comments", SendMessageOptions{RevisionSourceProposedPlan: &SourceProposedPlan{ItemID: "plan"}, RevisionSourceCommentIDs: []string{"comment"}}},
		{"diff comments", SendMessageOptions{RevisionSourceDiffReview: &SourceDiffReview{}, RevisionSourceDiffCommentIDs: []string{"comment"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := newAppForFlushQueueRPC(t)
			thread := testThread("nontext-input")
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			// Queue admission retains references; dispatch resolves their content.
			if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "", tc.opts); err != nil {
				t.Fatal(err)
			}
			if len(durableQueueRows(t, app, thread.ID)) != 1 {
				t.Fatal("non-text input was not admitted")
			}
		})
	}
}

func TestLegacyEmptyQueueSettlesWithoutBlockingMeaningfulInput(t *testing.T) {
	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(providerName, func(t *testing.T) {
			app, rec := newAppForFlushQueueRPC(t)
			thread := testThread("legacy-empty-queue")
			thread.Provider = providerName
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			var batch []triage.QueuedFlushItem
			for _, row := range []store.FlushQueueItem{
				{ID: "empty-1", ThreadID: thread.ID, Payload: []byte(`{}`)},
				{ID: "empty-2", ThreadID: thread.ID, Message: " \n", Payload: []byte(`{}`)},
				{ID: "meaningful", ThreadID: thread.ID, Message: "keep this message", Payload: []byte(`{}`)},
			} {
				if err := app.store.InsertFlushQueueItem(row); err != nil {
					t.Fatal(err)
				}
				batch = append(batch, triage.QueuedFlushItem{ID: row.ID, Message: row.Message, Payload: row.Payload, Settlement: app.flushQueueSettlement(thread.ID, row.ID, nil)})
			}
			batch[0].StaleUserItemID = "stale-empty-user"
			if err := app.store.InsertItem(store.Item{ID: batch[0].StaleUserItemID, ThreadID: thread.ID, Kind: "user_text", Role: "user"}); err != nil {
				t.Fatal(err)
			}
			// No session: the meaningful message must retain ordinary retry
			// behavior, while the preceding empty rows settle without dispatch.
			app.dispatchFlush(thread.ID, batch)
			rows := durableQueueRows(t, app, thread.ID)
			if len(rows) != 1 || rows[0].ID != "meaningful" {
				t.Fatalf("durable queue after empty cleanup: %+v", rows)
			}
			queued, err := app.GetQueueState(thread.ID)
			if err != nil || len(queued) != 1 || queued[0].ID != "meaningful" {
				t.Fatalf("empty rows blocked meaningful input: %+v, %v", queued, err)
			}
			items, err := app.store.ListItems(thread.ID)
			if err != nil || len(items) != 0 || len(emittedQueueFlushed(rec)) != 0 {
				t.Fatalf("empty cleanup fabricated history or delivery: %+v, %v", items, err)
			}
		})
	}
}

func TestAttachmentOnlySendStillReachesProvider(t *testing.T) {
	app := newMixedTurnApp(t)
	thread, capture := newMixedTurnThread(t, app, "attachment-only-send")
	ids, fileLine := mixedTurnFixture(t, app, thread.ID)
	if err := app.SendMessage(thread.ID, "", ids); err != nil {
		t.Fatal(err)
	}
	blocks := waitForCapturedUserEnvelopes(t, capture, 1)[0]
	images, foundFile := 0, false
	for _, block := range blocks {
		if block[0] == "image" {
			images++
		}
		if block[0] == "text" && strings.Contains(block[1], fileLine) {
			foundFile = true
		}
	}
	if images != 2 || !foundFile {
		t.Fatalf("attachment-only provider input: %+v", blocks)
	}
}
