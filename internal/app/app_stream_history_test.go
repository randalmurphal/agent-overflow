package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestHistoryReadsCoverEmittedDeltas: every read that returns item rows
// writes the thread's stream buffers first. A live client places a read
// against the deltas it received by stream offset, so a delta the read
// did not hold must be one emitted after it.
func TestHistoryReadsCoverEmittedDeltas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reads := map[string]func(a *App, threadID, itemID string) error{
		"SyncThreadWindow": func(a *App, threadID, _ string) error {
			_, err := a.SyncThreadWindow(ctx, threadID, SyncThreadWindowRequest{})
			return err
		},
		"ListThreadSliceAround": func(a *App, threadID, _ string) error {
			_, err := a.ListThreadSliceAround(ctx, threadID, "", 0, TimelinePageOptions{})
			return err
		},
		"ListItemsBeforeCursor": func(a *App, threadID, _ string) error {
			_, err := a.ListItemsBeforeCursor(ctx, threadID, store.TimelineCursor{TurnIndex: 1 << 20}, 0, TimelinePageOptions{})
			return err
		},
		"ListItemsAfterCursor": func(a *App, threadID, _ string) error {
			_, err := a.ListItemsAfterCursor(ctx, threadID, store.TimelineCursor{TurnIndex: -1}, 0, TimelinePageOptions{})
			return err
		},
		"ListSubagentDescendants": func(a *App, threadID, itemID string) error {
			_, err := a.ListSubagentDescendants(threadID, itemID, false)
			return err
		},
		"GetThreadItem": func(a *App, threadID, itemID string) error {
			_, err := a.GetThreadItem(threadID, itemID)
			return err
		},
		"ListItems": func(a *App, threadID, _ string) error {
			_, err := a.ListItems(threadID, false)
			return err
		},
		"ListActivityRunMembers": func(a *App, threadID, itemID string) error {
			_, err := a.ListActivityRunMembers(ctx, threadID, ActivityRunMembersRequest{
				RunFirstItemID: itemID, Direction: "around", AroundItemID: itemID, Limit: 10,
			})
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			var deltas []triage.ItemStreamEvent
			app.triage = triage.NewRouter(app.store, func(channel eventchan.Channel, data any) {
				if evt, ok := data.(triage.ItemStreamEvent); ok && evt.Action == "delta" {
					deltas = append(deltas, evt)
				}
			})
			thread := testThread("thread-stream-read")
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			for _, chunk := range []string{"first", " sécond"} {
				if err := app.triage.Handle(provider.ProviderEvent{
					Kind: provider.EventThinking, ThreadID: thread.ID, Content: chunk, Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("thinking %q: %v", chunk, err)
				}
			}
			itemID := deltas[0].ItemID
			before, _, err := app.store.GetThreadItem(thread.ID, itemID)
			if err != nil {
				t.Fatalf("GetThreadItem: %v", err)
			}
			if before.Summary != "first" {
				t.Fatalf("stored summary before the read = %q, want the second delta still buffered", before.Summary)
			}

			if err := read(app, thread.ID, itemID); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			after, _, err := app.store.GetThreadItem(thread.ID, itemID)
			if err != nil {
				t.Fatalf("GetThreadItem: %v", err)
			}
			last := deltas[len(deltas)-1]
			end := *last.Offset + int64(len(last.Delta))
			if after.Summary != "first sécond" || after.StreamEnd == nil || *after.StreamEnd != end {
				t.Fatalf("stored row after %s = %q ending at %v, want %q ending at %d",
					name, after.Summary, after.StreamEnd, "first sécond", end)
			}
		})
	}
}

// TestSyncThreadWindowRowEndsWhereTheNextDeltaStarts: the window a client
// opens mid-stream holds the stream's text up to its stream end, and the
// next delta it receives starts there.
func TestSyncThreadWindowRowEndsWhereTheNextDeltaStarts(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	var deltas []triage.ItemStreamEvent
	app.triage = triage.NewRouter(app.store, func(channel eventchan.Channel, data any) {
		if evt, ok := data.(triage.ItemStreamEvent); ok && evt.Action == "delta" {
			deltas = append(deltas, evt)
		}
	})
	thread := testThread("thread-stream-window")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	send := func(chunk string) {
		t.Helper()
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventTextDelta, ThreadID: thread.ID, Content: chunk, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("text %q: %v", chunk, err)
		}
	}
	send("Hello")
	send(", wörld")
	resp, err := app.SyncThreadWindow(context.Background(), thread.ID, SyncThreadWindowRequest{})
	if err != nil || resp.Page == nil {
		t.Fatalf("SyncThreadWindow: page=%v err=%v", resp.Page, err)
	}
	var row *store.Item
	for i := range resp.Page.Items {
		if resp.Page.Items[i].ID == deltas[0].ItemID {
			row = &resp.Page.Items[i]
		}
	}
	if row == nil || row.Summary != "Hello, wörld" || row.StreamEnd == nil {
		t.Fatalf("window row = %+v, want the streamed text with its stream end", row)
	}
	send("!")
	if next := deltas[len(deltas)-1]; next.Offset == nil || *next.Offset != *row.StreamEnd {
		t.Fatalf("next delta offset = %v, want the window's stream end %d", next.Offset, *row.StreamEnd)
	}
}
