package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestDispatchFlush_EndToEnd_TriggerThroughWireEcho_Codex pins the
// full G1–G5 backend pipeline against a fake Codex session, exercising
// the cross-package contract that earlier focused tests stub:
//
//  1. Two queue items registered via App.RegisterQueueItem land in
//     triage's per-thread queue and emit
//     `provider:queue_state_changed` snapshots after each register.
//  2. Driving EventTurnStart → EventToolStart (top-level) fires the
//     flush trigger exactly once for the round.
//  3. The dispatcher emits `provider:queue_flushed` carrying
//     (queueItemId, userItemId, message) for every batch entry, then
//     a follow-up `provider:queue_state_changed` with an empty list
//     so any client mirroring the queue observes Zone 1 collapsing.
//  4. Both `user:<turn>:flush:1` and `:flush:2` user_text rows land
//     in the store with the dispatched message and a pending-send
//     marker registered against each.
//  5. Driving the wire echo (EventUserText with provider_item_id meta)
//     for each row in order consumes the matching pending-send entry
//     and stamps `provider_item_id` onto the row's Meta — proving the
//     cross-cutting correlation chain (queue → optimistic persist →
//     wire confirmation) holds end-to-end.
//
// Per-piece coverage already pins the smaller invariants:
//   - internal/triage/flush_queue_test.go — trigger predicate + queue
//     state mutations.
//   - app_flush_queue_test.go — dispatcher persistence, fallback,
//     abort-on-error.
//   - internal/triage/handle_user_text_test.go — wire correlation +
//     pending-send pop.
// This test owns the cross-piece glue: a real wire-shaped event flow
// from register through wire echo with the dispatcher in place.
func TestDispatchFlush_EndToEnd_TriggerThroughWireEcho_Codex(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-e2e-codex")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Codex Steer needs a live session pointing at the fake codex
	// app-server binary. The "ok" outcome means turn/steer responds
	// with success, mirroring the happy-path race-free flush.
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-e2e-token",
		codex:    sess,
	}

	// 1. Register two queue items via the public RPC.
	first, err := app.RegisterQueueItem(thread.ID, "first queued", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem first: %v", err)
	}
	second, err := app.RegisterQueueItem(thread.ID, "second queued", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem second: %v", err)
	}

	// Sanity: pre-flush queue snapshot has both items.
	state := rec.lastQueueState(t)
	if len(state.Items) != 2 {
		t.Fatalf("pre-flush queue items: got %d, want 2", len(state.Items))
	}
	if state.Items[0].ID != first.ID || state.Items[1].ID != second.ID {
		t.Errorf("pre-flush order: got [%s, %s], want [%s, %s]",
			state.Items[0].ID, state.Items[1].ID, first.ID, second.ID)
	}

	// 2. Drive EventTurnStart so the round opens; the trigger needs
	// currentRoundID to be non-empty so it can record "fired this
	// round" against the round id.
	now := time.Now()
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: now,
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	// EventToolStart for a top-level tool (no ParentToolUseID) is the
	// trigger seam. handleToolStart calls maybeFireFlushTrigger BEFORE
	// any persistence so the dispatcher sees the queue intact.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemID:    "tool-1",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: now,
	}); err != nil {
		t.Fatalf("EventToolStart: %v", err)
	}

	// 3. provider:queue_flushed must have fired carrying both items
	// in original order with deterministic userItemIds.
	flushedEvts := emittedQueueFlushed(rec)
	if len(flushedEvts) != 1 {
		t.Fatalf("queue_flushed emissions: got %d, want exactly 1", len(flushedEvts))
	}
	flushed := flushedEvts[0]
	if flushed.ThreadID != thread.ID {
		t.Errorf("queue_flushed ThreadID: got %q, want %q", flushed.ThreadID, thread.ID)
	}
	if len(flushed.Items) != 2 {
		t.Fatalf("queue_flushed items: got %d, want 2", len(flushed.Items))
	}
	wantIDs := []string{"user:0:flush:1", "user:0:flush:2"}
	wantQueueIDs := []string{first.ID, second.ID}
	wantMessages := []string{"first queued", "second queued"}
	for i, item := range flushed.Items {
		if item.UserItemID != wantIDs[i] {
			t.Errorf("flushed[%d].UserItemID: got %q, want %q", i, item.UserItemID, wantIDs[i])
		}
		if item.QueueItemID != wantQueueIDs[i] {
			t.Errorf("flushed[%d].QueueItemID: got %q, want %q", i, item.QueueItemID, wantQueueIDs[i])
		}
		if item.Message != wantMessages[i] {
			t.Errorf("flushed[%d].Message: got %q, want %q", i, item.Message, wantMessages[i])
		}
	}

	// Post-flush queue snapshot is empty — fireFlushTriggerOnce
	// emptied the queue and dispatchFlush emits a follow-up
	// queue_state_changed so any mirror sees Zone 1 drain.
	postFlushState := rec.lastQueueState(t)
	if postFlushState.ThreadID != thread.ID {
		t.Errorf("post-flush state ThreadID: got %q, want %q", postFlushState.ThreadID, thread.ID)
	}
	if len(postFlushState.Items) != 0 {
		t.Errorf("post-flush queue items: got %d, want 0", len(postFlushState.Items))
	}

	// 4. Both user_text rows persisted at the resolved turn index
	// with the right ids, summaries, and pending-send markers
	// registered.
	items, err := app.store.ListItemsForTurn(thread.ID, 0)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	flushRowByID := make(map[string]store.Item, 2)
	for _, item := range items {
		if item.Kind == "user_text" && strings.HasPrefix(item.ID, "user:0:flush:") {
			flushRowByID[item.ID] = item
		}
	}
	for i, want := range wantIDs {
		row, ok := flushRowByID[want]
		if !ok {
			t.Errorf("missing flush row %q (got items %+v)", want, items)
			continue
		}
		if row.Summary != wantMessages[i] {
			t.Errorf("row %s Summary: got %q, want %q", want, row.Summary, wantMessages[i])
		}
		if row.Role != "user" || row.Status != "completed" {
			t.Errorf("row %s role/status: got role=%q status=%q, want user/completed",
				want, row.Role, row.Status)
		}
	}

	// Pending-send markers should be live for both rows pending wire
	// echo. HasPendingSendForThread is a cheap "any markers" check;
	// the order-preserving consume happens in step 5 below.
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send markers should be registered after dispatch")
	}

	// 5. Drive the wire echo for each row, in order. Each
	// EventUserText carries provider_item_id in Meta which both
	// stamps onto the row AND consumes the matching pending-send
	// FIFO entry.
	for i, row := range []string{"user:0:flush:1", "user:0:flush:2"} {
		echoMeta, _ := json.Marshal(map[string]any{
			"provider_item_id": "wire-echo-" + row,
		})
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventUserText,
			ThreadID:  thread.ID,
			TurnIndex: 0,
			ItemID:    row,
			Meta:      echoMeta,
			Content:   wantMessages[i],
			Timestamp: now,
		}); err != nil {
			t.Fatalf("EventUserText for %s: %v", row, err)
		}

		stored, found, err := app.store.GetThreadItem(thread.ID, row)
		if err != nil || !found {
			t.Fatalf("GetThreadItem(%s) found=%v err=%v", row, found, err)
		}
		var meta map[string]any
		if stored.Meta != "" && stored.Meta != "{}" {
			if err := json.Unmarshal([]byte(stored.Meta), &meta); err != nil {
				t.Fatalf("decode row %s meta %q: %v", row, stored.Meta, err)
			}
		}
		gotID, _ := meta["provider_item_id"].(string)
		if gotID != "wire-echo-"+row {
			t.Errorf("row %s provider_item_id: got %q, want %q",
				row, gotID, "wire-echo-"+row)
		}
	}

	// After both echoes consume their pending entries the marker
	// list should be empty — no leaked entries that could mis-route
	// a later wire user_text.
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send markers should be drained after both wire echoes")
	}
}

// emittedQueueFlushed returns every QueueFlushedEvent the recorder
// captured, in emission order. Used to assert "fired exactly once"
// when the trigger predicate gates the dispatch.
func emittedQueueFlushed(rec *emitRecorder) []QueueFlushedEvent {
	out := make([]QueueFlushedEvent, 0)
	for _, c := range rec.calls {
		if c.Channel != "provider:queue_flushed" {
			continue
		}
		evt, ok := c.Data.(QueueFlushedEvent)
		if !ok {
			continue
		}
		out = append(out, evt)
	}
	return out
}
