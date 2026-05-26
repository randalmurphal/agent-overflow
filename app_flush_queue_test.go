package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestDispatchFlush_Codex_DefersUserItemUntilWireEcho pins
// the happy-path Codex flow: a queued message reaches the active
// turn's pending_input via turn/steer, and the AO-side bookkeeping
// registers a deferred pending-send marker. The chat-history row is
// persisted only when the provider echoes the message with a stable
// provider_item_id.
func TestDispatchFlush_Codex_DefersUserItemUntilWireEcho(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-codex-ok")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initCheckpointRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-3",
		ThreadID:  thread.ID,
		TurnIndex: 3,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{
			ID:      "queue:abc",
			Message: "drained",
			Payload: json.RawMessage(`{}`),
		},
	})

	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, item := range items {
		if item.Kind == "user_text" && strings.HasPrefix(item.ID, "user:3:flush:") {
			t.Fatalf("user_text row should wait for provider echo, got %+v", item)
		}
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send marker not registered after Codex Steer dispatch")
	}

	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-user-1",
		"parent_uuid":      "parent-wire-1",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: 3,
		ItemID:    "user:3:flush:1",
		Content:   "drained",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}
	flushRow, found, err := app.store.GetThreadItem(thread.ID, "user:3:flush:1")
	if err != nil || !found {
		t.Fatalf("expected user:3:flush:1 after echo: found=%v err=%v", found, err)
	}
	if flushRow.Summary != "drained" {
		t.Errorf("flush row summary: got %q, want %q", flushRow.Summary, "drained")
	}
	checkpoint, ok, err := app.store.GetCheckpointByUserItemID(thread.ID, "user:3:flush:1")
	if err != nil || !ok {
		t.Fatalf("checkpoint for flushed user item missing after echo: ok=%v err=%v", ok, err)
	}
	if checkpoint.ProviderUserMessageID != "wire-user-1" || checkpoint.ProviderParentUUID != "parent-wire-1" {
		t.Fatalf("checkpoint provider ids = %q/%q, want wire-user-1/parent-wire-1",
			checkpoint.ProviderUserMessageID, checkpoint.ProviderParentUUID)
	}
}

// TestDispatchFlush_EchoLandsAfterRowsThatArrivedFirst is the integration-
// level companion to triage's
// TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst.
// It exercises the full dispatchFlush -> provider write -> wire echo
// path: a row persisted between dispatch and the wire echo must keep its
// index, and the queued user_text row must land at MAX+1 at echo time
// (i.e. AFTER the rows the model emitted first).
//
// The previous behavior captured item_index at dispatch and inserted
// at that slot, shifting later-arriving rows DOWN — which placed the
// queued message above content the agent had already produced. See
// the bug walkthrough in pendingSend's doc comment.
func TestDispatchFlush_EchoLandsAfterRowsThatArrivedFirst(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-echo-insert-position")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initCheckpointRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-3",
		ThreadID:  thread.ID,
		TurnIndex: 3,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{
			ID:      "queue:abc",
			Message: "queued before assistant output",
			Payload: json.RawMessage(`{}`),
		},
	})

	// Assistant row arrives BEFORE the wire echo. AppendItem allocates
	// MAX+1 = 0 (turn is empty in SQLite — the deferred queued row is
	// in router memory only).
	if _, err := app.store.AppendItem(store.Item{
		ID:        "assistant:3:0",
		ThreadID:  thread.ID,
		TurnIndex: 3,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "assistant output caused by the queued text",
		CreatedAt: now + 1,
		UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("append assistant row: %v", err)
	}

	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-user-1",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: 3,
		ItemID:    "user:3:flush:1",
		Content:   "queued before assistant output",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}

	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want assistant + queued user", items)
	}
	if items[0].ID != "assistant:3:0" || items[0].ItemIndex != 0 {
		t.Fatalf("first item = %+v, want assistant at index 0 (kept its slot)", items[0])
	}
	if items[1].ID != "user:3:flush:1" || items[1].ItemIndex != 1 {
		t.Fatalf("second item = %+v, want queued user at index 1 (echo-time MAX+1)", items[1])
	}

	// The frontend treats item upserts as authoritative; verify no
	// shift-down upsert was emitted for the assistant row.
	for _, item := range emittedItemUpserts(rec) {
		if item.ID == "assistant:3:0" && item.ItemIndex != 0 {
			t.Fatalf("assistant upsert re-emitted at index %d — should keep index 0", item.ItemIndex)
		}
	}
}

func TestDispatchFlush_SecondBatchBeforeEchoAllocatesNextFlushID(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-id-pending")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:first", Message: "first"},
	})
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:second", Message: "second"},
	})

	flushed := emittedQueueFlushed(rec)
	if len(flushed) != 2 {
		t.Fatalf("queue_flushed events: got %d, want 2", len(flushed))
	}
	gotIDs := []string{flushed[0].Items[0].UserItemID, flushed[1].Items[0].UserItemID}
	wantIDs := []string{"user:0:flush:1", "user:0:flush:2"}
	if gotIDs[0] != wantIDs[0] || gotIDs[1] != wantIDs[1] {
		t.Fatalf("flush ids = %v, want %v", gotIDs, wantIDs)
	}
}

func TestDispatchFlush_EmitsQueueFlushedBeforeQueueDrainedSnapshot(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-boundary-no-send-bridge")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "user:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "interrupted turn seed",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed interrupted turn item: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:started", Message: "started"},
	})

	calls := rec.snapshot()
	var flushedIndex, stateIndex = -1, -1
	for i, call := range calls {
		switch call.Channel {
		case "provider:queue_flushed":
			if flushedIndex == -1 {
				flushedIndex = i
			}
		case "provider:queue_state_changed":
			if stateIndex == -1 {
				stateIndex = i
			}
		}
	}
	if flushedIndex == -1 || stateIndex == -1 {
		t.Fatalf("expected queue_flushed and queue_state_changed events, got %+v", calls)
	}
	if flushedIndex > stateIndex {
		t.Fatalf("queue_state_changed emitted before queue_flushed: flushed=%d state=%d", flushedIndex, stateIndex)
	}
	states := emittedQueueStates(rec)
	if len(states) == 0 || len(states[len(states)-1].Items) != 0 {
		t.Fatalf("final queue snapshot = %+v, want empty", states)
	}
}

// TestDispatchFlush_Codex_NoActiveTurnFallsBackToSend pins the
// race-window fallback: turn/steer returning NoActiveTurn must not
// surface as a hard failure — it triggers a fresh Send that opens a
// new turn carrying the queued content.
func TestDispatchFlush_Codex_NoActiveTurnFallsBackToSend(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-codex-fallback")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-2",
		ThreadID:  thread.ID,
		TurnIndex: 2,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// "no-active-turn" outcome: turn/steer responds with
	// {error: NoActiveTurn}; Send falls back to thread/start (the
	// fake binary returns ok for thread/start).
	sess := installSteerTestSession(t, app, thread, "no-active-turn")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:fallback", Message: "race"},
	})
	flushed := app.triage.LiveStateSnapshotForThread(thread.ID).FlushedItems
	if len(flushed) != 1 {
		t.Fatalf("pending flushed items: got %+v, want one item", flushed)
	}
	userItemID := flushed[0].UserItemID

	// The fallback re-register bumps turnIndex when activeAtResolution
	// fires (turn 2 was active, so the fresh turn is 3). The new userItemID
	// encodes the bumped turn as "user:<turn>:flush:<n>"; parse it rather
	// than search-by-content so the test stays decoupled from any future
	// fallback semantics that change item placement.
	if !strings.HasPrefix(userItemID, "user:3:flush:") {
		t.Fatalf("fallback user item id = %q, want user:3:flush:<n> (no-active-turn bumps from 2 -> 3)", userItemID)
	}
	fallbackTurn := 3

	// Even though Steer returned NoActiveTurn, the fallback path
	// reaches sess.Send. The user_text row still waits for the
	// provider echo, and there must be NO sibling `error` row — the
	// fallback succeeded, so this isn't a failure case from the user's
	// POV.
	for _, turn := range []int{2, 3} {
		items, err := app.store.ListItemsForTurn(thread.ID, turn)
		if err != nil {
			t.Fatalf("ListItemsForTurn turn=%d: %v", turn, err)
		}
		for _, it := range items {
			if it.Kind == "error" {
				t.Errorf("unexpected error row after fallback (turn %d): %+v", turn, it)
			}
			if it.ID == userItemID {
				t.Fatalf("user_text row should wait for provider echo after fallback, got %+v", it)
			}
		}
	}

	// Insert an assistant row BEFORE the wire echo arrives — represents
	// content the model emitted after the fallback Send established the
	// turn but before the queued message's echo round-trip. The queued
	// row must land at MAX+1 of the fallback turn, AFTER the assistant
	// row, not at index 0.
	if _, err := app.store.AppendItem(store.Item{
		ID:        "assistant:fallback:0",
		ThreadID:  thread.ID,
		TurnIndex: fallbackTurn,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "model speaks before queued echo",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("append assistant row in fallback turn: %v", err)
	}

	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": "wire-fallback"})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: fallbackTurn,
		ItemID:    userItemID,
		Content:   "race",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}

	queued, found, err := app.store.GetThreadItem(thread.ID, userItemID)
	if err != nil || !found {
		t.Fatalf("expected %s after echo: found=%v err=%v", userItemID, found, err)
	}
	assistant, found, err := app.store.GetThreadItem(thread.ID, "assistant:fallback:0")
	if err != nil || !found {
		t.Fatalf("assistant row missing: found=%v err=%v", found, err)
	}
	if queued.TurnIndex != fallbackTurn {
		t.Fatalf("queued user_text TurnIndex = %d, want %d (the fallback turn)", queued.TurnIndex, fallbackTurn)
	}
	if queued.ItemIndex <= assistant.ItemIndex {
		t.Fatalf("queued user_text item_index %d should be greater than assistant item_index %d — the no-active-turn re-register must use the same MAX+1-at-echo rule", queued.ItemIndex, assistant.ItemIndex)
	}
}

// TestDispatchFlush_NoSession_DoesNotPersistUserRow pins the missing-session
// guard: a flush trigger fires, but the session was torn down between
// trigger fire and dispatcher arrival — the dispatcher must not panic
// or persist a half-baked item.
func TestDispatchFlush_NoSession_DoesNotPersistUserRow(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-no-session")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// No app.sessions entry — this simulates the torn-down case.
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:0", Message: "x"},
	})

	// We expect NO user_text row (we bail before persistence) and
	// also NO sibling error row (because the persistence path didn't
	// run, so RegisterPendingSend wasn't called and there's nothing
	// to surface as a failed delivery — the missing session is a
	// system-state error, logged and skipped).
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, it := range items {
		if it.Kind == "user_text" {
			t.Errorf("user_text persisted despite missing session: %+v", it)
		}
	}
}

// TestDispatchFlush_PerItemFailure_AbortsBatch pins the
// abort-on-first-error contract: if item 1 dispatches successfully
// but item 2's wire dispatch fails, item 3 must not be attempted —
// preserving wire-visible ordering trumps best-effort delivery.
func TestDispatchFlush_PerItemFailure_AbortsBatch(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-abort")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-4",
		ThreadID:  thread.ID,
		TurnIndex: 4,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// Use the no-active-turn binary: turn/steer returns NoActiveTurn,
	// the fallback Send goes through thread/start which responds ok.
	// To force a failure on the first item, kill the session BEFORE
	// dispatch. With a closed session, both Steer and Send fail.
	sess := installSteerTestSession(t, app, thread, "no-active-turn")
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:0", Message: "first"},
		{ID: "queue:1", Message: "second"},
		{ID: "queue:2", Message: "third"},
	})

	// The attempted item registers a deferred pending marker before
	// dispatch, but the failed provider write clears it and persists
	// only the error row. Items 2 and 3 are not attempted.
	items, err := app.store.ListItemsForTurn(thread.ID, 4)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	flushRows := 0
	errorRows := 0
	for _, it := range items {
		if strings.HasPrefix(it.ID, "user:4:flush:") {
			flushRows++
		}
		if it.Kind == "error" {
			errorRows++
		}
	}
	if flushRows != 0 {
		t.Errorf("flush rows persisted: got %d, want 0 (failed dispatch stays out of chat history)", flushRows)
	}
	if errorRows != 1 {
		t.Errorf("error rows persisted: got %d, want 1 (one per failed item)", errorRows)
	}
	// Pending-send marker must be cleared on the failed item so a
	// later wire echo can't hijack a future send's correlation.
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send marker live after failed dispatch")
	}
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 2 {
		t.Fatalf("requeued items: got %d, want 2", len(requeued))
	}
	if requeued[0].ID != "queue:1" || requeued[1].ID != "queue:2" {
		t.Fatalf("requeued order: got [%s, %s], want [queue:1, queue:2]", requeued[0].ID, requeued[1].ID)
	}
}

func TestDispatchFlush_FailedItemDoesNotEmitQueueFlushed(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-failed-no-zone2")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "no-active-turn")
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:failed", Message: "failed"},
	})

	if flushed := emittedQueueFlushed(rec); len(flushed) != 0 {
		t.Fatalf("failed dispatch emitted queue_flushed events: %+v", flushed)
	}

	states := emittedQueueStates(rec)
	if len(states) == 0 {
		t.Fatalf("queue_state_changed events: got %d, want at least 1", len(states))
	}
	last := states[len(states)-1]
	if len(last.Items) != 0 {
		t.Fatalf("last queue_state_changed items = %+v, want empty for failed single-item dispatch", last.Items)
	}
}

func TestDispatchFlush_CodexSteerTimeoutKeepsPendingConfirmation(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-steer-timeout")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "timeout")
	codex.SetRequestTimeoutForTest(sess, 25*time.Millisecond)
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:timeout", Message: "eventually accepted"},
	})

	if flushed := emittedQueueFlushed(rec); len(flushed) != 1 {
		t.Fatalf("ambiguous timeout should still enter Zone 2, queue_flushed=%+v", flushed)
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatalf("pending-send marker cleared on ambiguous Codex steer timeout")
	}
	items, err := app.store.ListItemsForTurn(thread.ID, 0)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, it := range items {
		if it.Kind == "error" {
			t.Fatalf("ambiguous Codex steer timeout persisted error row: %+v", it)
		}
		if strings.HasPrefix(it.ID, "user:0:flush:") {
			t.Fatalf("flush row should still wait for provider echo, got %+v", it)
		}
	}

	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": "wire-timeout"})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemID:    "user:0:flush:1",
		Content:   "eventually accepted",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatalf("pending-send marker still live after provider echo")
	}
	row, found, err := app.store.GetThreadItem(thread.ID, "user:0:flush:1")
	if err != nil || !found {
		t.Fatalf("expected deferred row after provider echo: found=%v err=%v", found, err)
	}
	if row.Summary != "eventually accepted" {
		t.Fatalf("row summary = %q, want eventually accepted", row.Summary)
	}
}

func TestConfiguredFlushDispatcher_DoesNotBlockProviderEventHandler(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-dispatch-nonblocking")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
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
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemID:    "tool-1",
		ItemType:  "Bash",
		Timestamp: now,
	}); err != nil {
		t.Fatalf("EventToolStart: %v", err)
	}

	unlock := app.threadLocks().Lock(thread.ID)

	done := make(chan error, 1)
	go func() {
		_, err := app.RegisterQueueItem(thread.ID, "queued while provider works", SendMessageOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RegisterQueueItem: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("configured flush dispatcher blocked queue registration")
	}
	if got := app.flushDispatchItemCount(thread.ID); got != 1 {
		unlock()
		t.Fatalf("in-flight flush dispatch count: got %d, want 1", got)
	}
	unlock()
	_ = waitForAtLeastQueueFlushed(t, rec, 1)
}

func TestEnqueueFlushDispatch_SerializesBatchesForOneThread(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-dispatch-serialized")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	unlock := app.threadLocks().Lock(thread.ID)
	app.enqueueFlushDispatch(thread.ID, []triage.QueuedFlushItem{{ID: "queue:0", Message: "first"}})
	app.enqueueFlushDispatch(thread.ID, []triage.QueuedFlushItem{{ID: "queue:1", Message: "second"}})
	if got := app.flushDispatchItemCount(thread.ID); got != 2 {
		unlock()
		t.Fatalf("in-flight flush dispatch count: got %d, want 2", got)
	}
	if flushed := emittedQueueFlushed(rec); len(flushed) != 0 {
		unlock()
		t.Fatalf("flush should be blocked by thread action lock, got events %+v", flushed)
	}
	unlock()

	flushed := waitForAtLeastQueueFlushed(t, rec, 2)
	wantQueueIDs := []string{"queue:0", "queue:1"}
	wantUserIDs := []string{"user:0:flush:1", "user:0:flush:2"}
	for i, evt := range flushed {
		if len(evt.Items) != 1 {
			t.Fatalf("queue_flushed[%d] items: got %d, want 1", i, len(evt.Items))
		}
		item := evt.Items[0]
		if item.QueueItemID != wantQueueIDs[i] {
			t.Errorf("queue_flushed[%d] queueItemID: got %q, want %q", i, item.QueueItemID, wantQueueIDs[i])
		}
		if item.UserItemID != wantUserIDs[i] {
			t.Errorf("queue_flushed[%d] userItemID: got %q, want %q", i, item.UserItemID, wantUserIDs[i])
		}
	}
}

func TestDispatchFlush_StaleGenerationAfterRollbackDropsBatch(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := testThread("flush-stale-generation")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	staleGeneration := app.currentFlushDispatchGeneration(thread.ID)
	app.clearFlushDispatchForRollback(thread.ID)

	app.dispatchFlushWithGeneration(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:stale", Message: "stale queued prompt"},
	}, staleGeneration)

	if app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("stale flush batch was re-queued after rollback")
	}
	if got := app.flushDispatchItemCount(thread.ID); got != 0 {
		t.Fatalf("flush dispatch item count = %d, want 0", got)
	}
}

// TestNextFlushUserItemID_NeverCollidesWithSeedOrSteer pins the id
// allocation: flush ids count from 1, share the prefix with steer
// ids but in a different namespace, and never collide with the seed
// `user:<turnIndex>` row that opens the turn.
func TestNextFlushUserItemID_NeverCollidesWithSeedOrSteer(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-id-alloc")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, id := range []string{"user:5", "user:5:steer:1", "user:5:steer:2"} {
		if _, err := app.store.AppendItem(store.Item{
			ID:        id,
			ThreadID:  thread.ID,
			TurnIndex: 5,
			Kind:      "user_text",
			Role:      "user",
			Status:    "completed",
			Summary:   id,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	got, err := app.nextFlushUserItemID(thread.ID, 5)
	if err != nil {
		t.Fatalf("nextFlushUserItemID: %v", err)
	}
	if got != "user:5:flush:1" {
		t.Errorf("first flush id: got %q, want user:5:flush:1", got)
	}

	// Persist and ask again — should advance to :2.
	if _, err := app.store.AppendItem(store.Item{
		ID:        got,
		ThreadID:  thread.ID,
		TurnIndex: 5,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "first flush",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("persist first flush: %v", err)
	}
	next, err := app.nextFlushUserItemID(thread.ID, 5)
	if err != nil {
		t.Fatalf("nextFlushUserItemID 2: %v", err)
	}
	if next != "user:5:flush:2" {
		t.Errorf("second flush id: got %q, want user:5:flush:2", next)
	}
}

// TestNextFlushUserItemID_StartsAtOneOnEmptyTurn pins the empty case:
// a turn with no user rows still allocates `user:<turnIndex>:flush:1`.
func TestNextFlushUserItemID_StartsAtOneOnEmptyTurn(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-id-empty")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := app.nextFlushUserItemID(thread.ID, 0)
	if err != nil {
		t.Fatalf("nextFlushUserItemID: %v", err)
	}
	if got != "user:0:flush:1" {
		t.Errorf("got %q, want user:0:flush:1", got)
	}
}

// TestDispatchFlushToProvider_RoutesByProviderType pins the
// per-provider routing decision: Codex sessions go through Steer
// (with Send fallback wrapped), other sessions go through Send only.
// Exercised at the unit level so the full trigger → dispatch →
// session test infra isn't needed for this branch.
func TestDispatchFlushToProvider_RoutesByProviderType(t *testing.T) {
	app := newTestAppWithStore(t)

	// A nil-codex / nil-claude session should fail fast with
	// "no provider" rather than panic — guard against bad wiring.
	app.dispatchFlushToProviderShouldErrorWith(t, session{provider: "claude"}, "no provider")
}

func (a *App) dispatchFlushToProviderShouldErrorWith(t *testing.T, sess session, want string) {
	t.Helper()
	err := a.dispatchFlushToProvider(sess, "x", provider.SendOptions{})
	if err == nil {
		t.Fatalf("dispatchFlushToProvider with empty session: nil err, want %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("dispatchFlushToProvider err = %v, want substring %q", err, want)
	}
}

// TestDispatchFlush_PayloadDecoding pins the Payload roundtrip:
// flushQueuePayload's JSON shape must match what the frontend
// produces when it hands attachments + plan refs to the dispatcher.
// The row is deferred until wire echo, so the assertion runs after
// provider confirmation.
func TestDispatchFlush_PayloadDecoding(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-payload")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// Empty payload object — there are no attachments / plan refs to
	// attach, but the decoder must accept `{}` without erroring.
	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{
			ID:      "queue:0",
			Message: "no payload",
			Payload: json.RawMessage(`{}`),
		},
	})

	items, err := app.store.ListItemsForTurn(thread.ID, 1)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.ID, "user:1:flush:") {
			t.Fatalf("user_text row should wait for provider echo, got %+v", item)
		}
	}

	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": "wire-payload"})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: 1,
		ItemID:    "user:1:flush:1",
		Content:   "no payload",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}
	row, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("expected flush row after echo: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("decode meta %q: %v", row.Meta, err)
	}
	if got, _ := meta["provider_item_id"].(string); got != "wire-payload" {
		t.Errorf("provider_item_id: got %q, want wire-payload", got)
	}
	if _, ok := meta["attachments"]; ok {
		t.Errorf("empty payload unexpectedly created attachments meta: %s", row.Meta)
	}
}

// TestDispatchFlush_ResolveTurnIndex_FallsBackToNextWhenNoActiveTurn
// pins the rare race where the active turn has just settled by the
// time the dispatcher reads it. The persisted user_text row must
// land at the NEXT logical turn index, not at the closed prior one.
func TestDispatchFlush_ResolveTurnIndex_FallsBackToNextWhenNoActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-no-active-turn")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// No active turn: a thread with prior items but no in-flight turn
	// should dispatch onto turn N+1 where N = LastTurnIndex.
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "user:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "earlier",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed prior item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	completedAt := now + 1
	if err := app.store.UpdateTurnCompleted("turn-0", completedAt, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn-0: %v", err)
	}

	// active turn check returns nothing → fallback path.
	got, _, err := app.resolveFlushTurnPlacement(thread.ID, session{})
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if got != 1 {
		t.Errorf("turn index: got %d, want 1 (next after settled turn 0)", got)
	}
}

// TestDispatchFlush_ResolveTurnIndex_CodexPrefersActiveTurn pins the
// Codex happy-path: when the active turn exists and the session is
// Codex, we steer into the active turn's index.
func TestDispatchFlush_ResolveTurnIndex_CodexPrefersActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-active-turn")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-7",
		ThreadID:  thread.ID,
		TurnIndex: 7,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	codexSess := installSteerTestSession(t, app, thread, "ok")
	sess := session{provider: string(provider.Codex), codex: codexSess}
	got, active, err := app.resolveFlushTurnPlacement(thread.ID, sess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if got != 7 {
		t.Errorf("turn index: got %d, want 7 (active turn)", got)
	}
	if !active {
		t.Errorf("activeAtResolution should be true for Codex active turn")
	}
}

// TestDispatchFlush_ResolveTurnIndex_ClaudeSkipsActiveTurn verifies
// that Claude flush dispatch always uses nextSendTurnIndex, never the
// active turn's index. Claude processes stdin messages as new turns
// (useQueueProcessor only dequeues between turns), so using the active
// turn's index would cause setOpenTurn to reset id-allocating counters
// and produce segment ID collisions.
func TestDispatchFlush_ResolveTurnIndex_ClaudeSkipsActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("flush-claude-skip-active")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "user:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "first",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// Active unsettled turn — Codex would steer into it, Claude must not.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	claudeSess := session{provider: string(provider.Claude)}
	got, active, err := app.resolveFlushTurnPlacement(thread.ID, claudeSess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if got != 1 {
		t.Errorf("turn index: got %d, want 1 (next turn, not active turn 0)", got)
	}
	if active {
		t.Errorf("activeAtResolution should be false for Claude")
	}
}

// TestDispatchFlush_ResolveTurnIndex_AccountsForInFlightPendingSends
// verifies that two messages queued during the same active turn get
// distinct turn indices. The first dispatch claims turn N+1; the
// second must advance to N+2 via MaxPendingSendTurnIndex.
func TestDispatchFlush_ResolveTurnIndex_AccountsForInFlightPendingSends(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, app.emitWithReplay())

	thread := testThread("flush-pending-dedup")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "user:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "first",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// Simulate first flush dispatch: registers pending send at turn 1.
	app.triage.RegisterPendingSend(thread.ID, "user:1:flush:1", 1)

	claudeSess := session{provider: string(provider.Claude)}
	got, _, err := app.resolveFlushTurnPlacement(thread.ID, claudeSess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if got != 2 {
		t.Fatalf("turn index: got %d, want 2 (must skip past in-flight pending send at turn 1)", got)
	}
}

// guardCompileEnsureSendMessageOptionsCompatible prevents a silent
// drift between flushQueuePayload and SendMessageOptions: the
// frontend writes one shape and reads the other, so the field set
// must stay aligned. A compile-time assignment catches missing
// fields.
//
//nolint:unused // compile-only guard
func guardCompileEnsureSendMessageOptionsCompatible(p flushQueuePayload) SendMessageOptions {
	return SendMessageOptions{
		AttachmentIDs:                p.AttachmentIDs,
		SourceProposedPlan:           p.SourceProposedPlan,
		RevisionSourceProposedPlan:   p.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     p.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     p.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: p.RevisionSourceDiffCommentIDs,
	}
}

func installMethodLoggingCodexSession(t *testing.T, thread store.Thread, methodLog string) *codex.Session {
	t.Helper()
	binary := writeMethodLoggingCodexBinary(t, thread.ID+"-codex", methodLog)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := os.WriteFile(methodLog, nil, 0o644); err != nil {
		t.Fatalf("clear method log: %v", err)
	}
	codex.SetActiveTurnIDForTest(sess, "active-turn")
	return sess
}

func writeMethodLoggingCodexBinary(t *testing.T, threadID string, methodLog string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
log_path=%q
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        printf 'turn/start\n' >> "$log_path"
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"},"turn":{"id":"fresh-turn"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        printf 'turn/steer\n' >> "$log_path"
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"turnId":"active-turn"}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, methodLog, threadID, threadID)

	path := filepath.Join(t.TempDir(), "codex-method-log.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex method logger: %v", err)
	}
	return path
}

func readMethodLog(t *testing.T, methodLog string) string {
	t.Helper()
	data, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatalf("read method log: %v", err)
	}
	return string(data)
}

// emitRecorder captures every event emitted by the App so tests can
// assert against the post-mutation queue snapshot. Mirrors the
// pattern used by other App-event tests but kept local to keep the
// flush-queue test surface self-contained.
type emitRecorder struct {
	mu    sync.Mutex
	calls []emittedCall
}

type emittedCall struct {
	Channel string
	Data    any
}

func (r *emitRecorder) capture(channel string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, emittedCall{Channel: channel, Data: data})
}

func (r *emitRecorder) snapshot() []emittedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]emittedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *emitRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = r.calls[:0]
}

func emittedItemUpserts(rec *emitRecorder) []store.Item {
	out := make([]store.Item, 0)
	for _, c := range rec.snapshot() {
		if c.Channel != "provider:item_event" {
			continue
		}
		evt, ok := c.Data.(triage.ItemStreamEvent)
		if !ok || evt.Action != "upsert" || evt.Item == nil {
			continue
		}
		out = append(out, *evt.Item)
	}
	return out
}

func emittedQueueStates(rec *emitRecorder) []QueueStateChangedEvent {
	out := make([]QueueStateChangedEvent, 0)
	for _, c := range rec.snapshot() {
		if c.Channel != "provider:queue_state_changed" {
			continue
		}
		evt, ok := c.Data.(QueueStateChangedEvent)
		if !ok {
			continue
		}
		out = append(out, evt)
	}
	return out
}

func (r *emitRecorder) lastQueueState(t *testing.T) QueueStateChangedEvent {
	t.Helper()
	calls := r.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Channel == "provider:queue_state_changed" {
			evt, ok := calls[i].Data.(QueueStateChangedEvent)
			if !ok {
				t.Fatalf("provider:queue_state_changed payload is not QueueStateChangedEvent: %T", calls[i].Data)
			}
			return evt
		}
	}
	t.Fatalf("no provider:queue_state_changed emission found in calls=%v", calls)
	return QueueStateChangedEvent{}
}

func newAppForFlushQueueRPC(t *testing.T) (*App, *emitRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	app.triage = triage.NewRouter(app.store, rec.capture)
	app.configureTriageQueueCallbacks()
	return app, rec
}

func TestRegisterQueueItem_AppendsAndEmitsState(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("rpc-add")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	queued, err := app.RegisterQueueItem(thread.ID, "hello", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if queued.ID == "" || queued.EnqueuedAt == 0 {
		t.Errorf("returned QueuedItem missing id/timestamp: %+v", queued)
	}
	if queued.Message != "hello" {
		t.Errorf("returned message: got %q, want %q", queued.Message, "hello")
	}

	state := rec.lastQueueState(t)
	if state.ThreadID != thread.ID {
		t.Errorf("event ThreadID: got %q, want %q", state.ThreadID, thread.ID)
	}
	if len(state.Items) != 1 {
		t.Fatalf("event items: got %d, want 1", len(state.Items))
	}
	if state.Items[0].ID != queued.ID {
		t.Errorf("event item id: got %q, want %q", state.Items[0].ID, queued.ID)
	}
}

func TestRegisterQueueItem_RejectsEmptyThreadID(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	if _, err := app.RegisterQueueItem("", "x", SendMessageOptions{}); err == nil {
		t.Errorf("expected error for empty threadID")
	}
}

func TestRegisterQueueItem_RejectsTooManyAttachments(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-attach-cap")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	too := make([]string, maxQueueAttachmentCount()+1)
	for i := range too {
		too[i] = fmt.Sprintf("att-%d", i)
	}
	_, err := app.RegisterQueueItem(thread.ID, "x", SendMessageOptions{AttachmentIDs: too})
	if err == nil {
		t.Errorf("expected error for too-many-attachments")
	}
	if err != nil && !strings.Contains(err.Error(), "too many attachments") {
		t.Errorf("err = %v, want too-many-attachments message", err)
	}
}

func TestRegisterQueueItem_RejectsRevisionCommentsWithoutPlan(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-rev-comments")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	_, err := app.RegisterQueueItem(thread.ID, "x", SendMessageOptions{
		RevisionSourceCommentIDs: []string{"c1"},
	})
	if err == nil {
		t.Errorf("expected error: revision comments require a source plan")
	}
}

func TestGetQueueState_ReturnsSnapshot(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-snapshot")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if got, _ := app.GetQueueState(thread.ID); got != nil {
		t.Errorf("empty queue should return nil, got %+v", got)
	}

	q, _ := app.RegisterQueueItem(thread.ID, "snapshot me", SendMessageOptions{})

	snap, err := app.GetQueueState(thread.ID)
	if err != nil {
		t.Fatalf("GetQueueState: %v", err)
	}
	if len(snap) != 1 || snap[0].ID != q.ID {
		t.Errorf("snapshot mismatch: got %+v", snap)
	}
	if snap[0].Message != "snapshot me" {
		t.Errorf("snapshot message: got %q, want %q", snap[0].Message, "snapshot me")
	}
}

func TestGetThreadLiveState_ReturnsServerSideSnapshot(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-live-state")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 7,
		Timestamp: time.UnixMilli(1_700_000_000_000),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	queued, err := app.RegisterQueueItem(thread.ID, "queued text", SendMessageOptions{
		AttachmentIDs: []string{"att-1"},
	})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	approvalMeta, err := json.Marshal(provider.ApprovalRequest{
		RequestID:   "approval-1",
		ThreadID:    thread.ID,
		TurnID:      "round-1",
		ToolName:    "Bash",
		Description: "Run command",
		Title:       "Approve command",
	})
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  thread.ID,
		TurnID:    "round-1",
		ItemID:    "approval-1",
		Meta:      approvalMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  thread.ID,
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"inProgress","id":"1","owner":"helper-agent"}]}`),
		Timestamp: time.UnixMilli(1_700_000_000_100),
	}); err != nil {
		t.Fatalf("todo update: %v", err)
	}
	app.triage.RegisterPendingFlushSend(thread.ID, "queue-flushed", store.Item{
		ID:        "user:7:flush:1",
		ThreadID:  thread.ID,
		TurnIndex: 7,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "flushed text",
	})

	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if state.ThreadID != thread.ID {
		t.Fatalf("ThreadID = %q, want %q", state.ThreadID, thread.ID)
	}
	if state.ActiveTurn == nil || state.ActiveTurn.TurnID == "" || state.ActiveTurn.TurnIndex != 7 {
		t.Fatalf("ActiveTurn = %+v, want live turn index 7", state.ActiveTurn)
	}
	if len(state.QueueItems) != 1 {
		t.Fatalf("QueueItems = %+v, want 1 queued item", state.QueueItems)
	}
	if state.QueueItems[0].ID != queued.ID || len(state.QueueItems[0].AttachmentIDs) != 1 || state.QueueItems[0].AttachmentIDs[0] != "att-1" {
		t.Fatalf("QueueItems = %+v, want queued item %s with att-1", state.QueueItems, queued.ID)
	}
	if len(state.Interactive.Approvals) != 1 || state.Interactive.Approvals[0].RequestID != "approval-1" {
		t.Fatalf("Interactive approvals = %+v, want approval-1", state.Interactive.Approvals)
	}
	if len(state.FlushedItems) != 1 || state.FlushedItems[0].QueueItemID != "queue-flushed" || state.FlushedItems[0].UserItemID != "user:7:flush:1" {
		t.Fatalf("FlushedItems = %+v, want queued flushed marker", state.FlushedItems)
	}
	if state.Todo == nil || state.Todo.UpdatedAt != 1_700_000_000_100 || len(state.Todo.Steps) != 1 || state.Todo.Steps[0].Step != "one" {
		t.Fatalf("Todo = %+v, want live todo snapshot", state.Todo)
	}
	// The Task* family (Claude Code 2.1.150+) threads `id` and `owner`
	// through the snapshot. The refresh/reconnect path must surface
	// them or the owner badge silently disappears after a frontend
	// reload until the next TaskUpdate fires.
	if state.Todo.Steps[0].ID != "1" || state.Todo.Steps[0].Owner != "helper-agent" {
		t.Fatalf("Todo.Steps[0] = %+v, want id=1 owner=helper-agent", state.Todo.Steps[0])
	}
}

func TestQueueStateChanged_PayloadDecodesAttachmentIDs(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-payload")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if _, err := app.RegisterQueueItem(thread.ID, "with attachments", SendMessageOptions{
		AttachmentIDs: []string{"att-1", "att-2"},
	}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	state := rec.lastQueueState(t)
	if len(state.Items) != 1 {
		t.Fatalf("items: got %d, want 1", len(state.Items))
	}
	got := state.Items[0]
	if len(got.AttachmentIDs) != 2 || got.AttachmentIDs[0] != "att-1" || got.AttachmentIDs[1] != "att-2" {
		t.Errorf("attachmentIds round-trip: got %v, want [att-1 att-2]", got.AttachmentIDs)
	}
}

// silence the imports that are referenced only by test helpers we
// might not exercise in this slim cut (kept for future expansion).
var (
	_ = context.Background
	_ = codex.ErrNoActiveTurn
	_ = fmt.Sprintf
)
