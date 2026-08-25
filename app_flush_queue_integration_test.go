package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestDispatchFlush_EndToEnd_TriggerThroughWireEcho_Codex pins the
// full G1–G5 backend pipeline against a fake Codex session, exercising
// the cross-package contract that earlier focused tests stub:
//
//  1. EventTurnStart opens the active turn.
//  2. Two queue items registered via App.RegisterQueueItem land in
//     triage's per-thread queue, emit `provider:queue_state_changed`,
//     and flush immediately through the dispatch worker.
//  3. The dispatcher emits `provider:queue_flushed` for each successful
//     provider write, then emits `provider:queue_state_changed` with an
//     empty list so any client mirroring the queue observes Zone 1
//     collapse after it has a provider-sent marker to render.
//  4. No `user:<turn>:flush:*` rows land before provider confirmation;
//     pending-send markers carry deferred row data instead.
//  5. Driving the wire echo (EventUserText with provider_item_id meta)
//     for each row in order consumes the matching pending-send entry
//     and creates the row with `provider_item_id` already attached.
//
// Per-piece coverage already pins the smaller invariants:
//   - internal/triage/flush_queue_test.go — trigger predicate + queue
//     state mutations.
//   - app_flush_queue_test.go — dispatcher persistence, fallback,
//     abort-on-error.
//   - internal/triage/handle_user_text_test.go — wire correlation +
//     pending-send pop.
//
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

	now := time.Now()
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: now,
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	// Register two queue items via the public RPC. RegisterQueueItem now
	// drains as soon as a live provider session exists, so the items move
	// from queue snapshot to sent-but-unconfirmed markers immediately.
	first, err := app.RegisterQueueItem(thread.ID, "first queued", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem first: %v", err)
	}
	second, err := app.RegisterQueueItem(thread.ID, "second queued", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem second: %v", err)
	}

	// provider:queue_flushed must have fired once per accepted item
	// in original order with deterministic userItemIds.
	flushedEvts := waitForAtLeastQueueFlushed(t, rec, 2)
	if len(flushedEvts) != 2 {
		t.Fatalf("queue_flushed emissions: got %d, want 2", len(flushedEvts))
	}
	wantIDs := []string{"user:0:flush:1", "user:0:flush:2"}
	wantQueueIDs := []string{first.ID, second.ID}
	wantMessages := []string{"first queued", "second queued"}
	for i, flushed := range flushedEvts {
		if flushed.ThreadID != thread.ID {
			t.Errorf("queue_flushed[%d] ThreadID: got %q, want %q", i, flushed.ThreadID, thread.ID)
		}
		if len(flushed.Items) != 1 {
			t.Fatalf("queue_flushed[%d] items: got %d, want 1", i, len(flushed.Items))
		}
		item := flushed.Items[0]
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

	// Post-flush queue snapshot is empty — triage emptied the queue
	// and dispatchFlush emits a follow-up queue_state_changed so any
	// mirror sees Zone 1 drain.
	postFlushState := waitForEmptyQueueState(t, rec, thread.ID)
	if postFlushState.ThreadID != thread.ID {
		t.Errorf("post-flush state ThreadID: got %q, want %q", postFlushState.ThreadID, thread.ID)
	}
	if len(postFlushState.Items) != 0 {
		t.Errorf("post-flush queue items: got %d, want 0", len(postFlushState.Items))
	}

	// 4. Chat-history rows are still absent; they should not render
	// until the provider confirms the message is in context.
	items, err := app.store.ListItemsForTurn(thread.ID, 0)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, item := range items {
		if item.Kind == "user_text" && strings.HasPrefix(item.ID, "user:0:flush:") {
			t.Fatalf("flush row should wait for provider echo, got %+v", item)
		}
	}

	// Pending-send markers should be live for both rows pending wire
	// echo. HasPendingSendForThread is a cheap "any markers" check;
	// the order-preserving consume happens in step 5 below.
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send markers should be registered after dispatch")
	}

	// 5. Drive the wire echo for each row, in order. Each
	// EventUserText carries provider_item_id in Meta, which stamps onto
	// the row, plus the `clientId` the app-server echoes back from the
	// steer's `clientUserMessageId` — a Codex send registers its pending
	// entry BY that id, so an echo without one names nobody's row and
	// would persist as injected provider context.
	for i, row := range []string{"user:0:flush:1", "user:0:flush:2"} {
		echoMeta, _ := json.Marshal(map[string]any{
			"provider_item_id": "wire-echo-" + row,
			"client_id":        row,
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
		if stored.Summary != wantMessages[i] {
			t.Errorf("row %s Summary: got %q, want %q", row, stored.Summary, wantMessages[i])
		}
		if stored.Role != "user" || stored.Status != "completed" {
			t.Errorf("row %s role/status: got role=%q status=%q, want user/completed",
				row, stored.Role, stored.Status)
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

// TestParserToTriageSeam_QueuedCommandReplay_StampsRow pins the
// parser → triage seam for the Claude `queued_command` replay shape —
// the exact path that produced the Zone 2-stuck bug. The seam crosses
// two packages: the parser (provider/claude) decodes the NDJSON
// envelope into an EventUserText with `provider_item_id` in meta, and
// triage (handle_user_text.go) consumes the matching pending-send
// FIFO entry and stamps that id onto the AO-owned row. A regression
// at either side — parser drops the meta, triage stops popping the
// FIFO, the meta key gets renamed — would land here, where neither
// the parser-only tests (parse_user_replay_test.go) nor the
// triage-only tests (handle_user_text_test.go) can see it.
//
// This is the missing seam test that would have caught the original
// bug: before the fix, the queued_command shape produced empty
// `provider_item_id`, the triage merge no-op'd, and no upsert
// emission carried the stamp downstream.
func TestParserToTriageSeam_QueuedCommandReplay_StampsRow(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("parser-triage-seam")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	const aoItemID = "user:0:flush:1"
	const wireUUID = "queue-uuid-7777"

	// Stand the dispatcher's deferred pending-send state up by hand.
	// The end-to-end Codex test above seeds this via a real Steer; for
	// this test we want to focus on the parser → triage seam, not the
	// dispatcher.
	now := time.Now().UnixMilli()
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:test", store.Item{
		ID:        aoItemID,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued message",
		Meta:      `{"attachments":[]}`,
		CreatedAt: now,
		UpdatedAt: now,
	}, 0, triage.PendingSendExpectation{})

	// Reset captures so the assertions below only see emissions
	// driven by the parser → triage flow.
	rec.reset()

	// Parse the queued_command replay shape Claude actually emits.
	// `message` carries `{role, content}` only — no `id` field. The
	// stable id lives at the envelope's top-level `uuid`. See
	// claude-code-source-code/src/QueryEngine.ts:880-892.
	parser := claude.NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"` + wireUUID + `","session_id":"sess-x","parent_tool_use_id":null,"message":{"role":"user","content":"queued message"}}`)
	events, err := parser.ParseLine(thread.ID, line)
	if err != nil {
		t.Fatalf("parser.ParseLine: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventUserText {
		t.Fatalf("expected 1 EventUserText, got %d events: %+v", len(events), events)
	}

	if err := app.triage.Handle(events[0]); err != nil {
		t.Fatalf("triage.Handle: %v", err)
	}

	// Row's meta should now carry the wire uuid as provider_item_id,
	// preserving the existing attachments slot.
	persisted, found, err := app.store.GetThreadItem(thread.ID, aoItemID)
	if err != nil || !found {
		t.Fatalf("GetThreadItem: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(persisted.Meta), &meta); err != nil {
		t.Fatalf("decode persisted meta %q: %v", persisted.Meta, err)
	}
	if got, _ := meta["provider_item_id"].(string); got != wireUUID {
		t.Fatalf("meta.provider_item_id: got %q, want %q (full meta: %s)", got, wireUUID, persisted.Meta)
	}
	if _, ok := meta["attachments"]; !ok {
		t.Fatalf("attachments slot lost during meta merge: %s", persisted.Meta)
	}

	// And the upsert with the merged meta must reach the frontend so
	// the queue-confirm overlay can clear. Without this emission, the
	// Zone 2 marker would stay stuck — the original bug.
	var sawStampedUpsert bool
	for _, c := range rec.snapshot() {
		if c.Channel != "provider:item_event" {
			continue
		}
		ev, ok := c.Data.(triage.ItemStreamEvent)
		if !ok || ev.Action != "upsert" || ev.Item == nil {
			continue
		}
		if ev.Item.ID != aoItemID {
			continue
		}
		var emittedMeta map[string]any
		if err := json.Unmarshal([]byte(ev.Item.Meta), &emittedMeta); err != nil {
			t.Fatalf("decode emitted meta %q: %v", ev.Item.Meta, err)
		}
		if got, _ := emittedMeta["provider_item_id"].(string); got == wireUUID {
			sawStampedUpsert = true
			break
		}
	}
	if !sawStampedUpsert {
		t.Fatalf("expected provider:item_event upsert for %s with provider_item_id=%q", aoItemID, wireUUID)
	}
}

// TestDispatchFlush_MarksProposedPlanImplementedAfterDispatch pins the
// flush-path parity of the send/steer click-time mark: when a queued
// item carries SourceProposedPlan, the plan is marked implemented as
// soon as the dispatcher hands it off to the provider — not at the
// later wire-echo. The mark uses the freshly-allocated `user:N:flush:M`
// id even though that row is deferred until echo, so the implementing
// link is stable end-to-end.
func TestDispatchFlush_MarksProposedPlanImplementedAfterDispatch(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-implement-plan")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed an assistant-emitted proposed plan at turn 0; the queued
	// implement-click flushes at turn 0's active boundary (set up below)
	// and the user_text row lands at user:0:flush:1.
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"do","lineCount":1,"charCount":2}`,
		Data:      []byte("# Plan\n\nDo it."),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-implement-token",
		codex:    sess,
	}

	// Active turn at 0 so dispatchFlushItem attaches at turn 0 and the
	// flush row uses the user:0:flush:1 id format.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	if _, err := app.RegisterQueueItem(thread.ID, "Implement the plan.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	flushed := waitForAtLeastQueueFlushed(t, rec, 1)
	if len(flushed) != 1 || len(flushed[0].Items) != 1 {
		t.Fatalf("queue_flushed: got %+v, want one event with one item", flushed)
	}
	wantImplementingID := flushed[0].Items[0].UserItemID
	if wantImplementingID != "user:0:flush:1" {
		t.Fatalf("flush user item id = %q, want user:0:flush:1", wantImplementingID)
	}

	state, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState: %v", err)
	}
	if !found || state.ImplementedAt == 0 {
		t.Fatalf("plan state = %+v, want implemented after dispatch", state)
	}
	if state.ImplementedByItemID != wantImplementingID {
		t.Errorf("ImplementedByItemID = %q, want %q (the deferred user_text id, allocated at dispatch)", state.ImplementedByItemID, wantImplementingID)
	}
}

// emittedQueueFlushed returns every QueueFlushedEvent the recorder
// captured, in emission order. Used to assert "fired exactly once"
// when the trigger predicate gates the dispatch.
func emittedQueueFlushed(rec *emitRecorder) []QueueFlushedEvent {
	out := make([]QueueFlushedEvent, 0)
	for _, c := range rec.snapshot() {
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

func waitForAtLeastQueueFlushed(t *testing.T, rec *emitRecorder, want int) []QueueFlushedEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		flushed := emittedQueueFlushed(rec)
		if len(flushed) >= want {
			return flushed
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue_flushed emissions: got %d, want at least %d", len(flushed), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForEmptyQueueState(t *testing.T, rec *emitRecorder, threadID string) QueueStateChangedEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, state := range emittedQueueStates(rec) {
			if state.ThreadID == threadID && len(state.Items) == 0 {
				return state
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("empty queue_state_changed for %s not observed", threadID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
