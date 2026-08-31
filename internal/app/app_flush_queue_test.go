package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()

	thread := testThread("flush-codex-ok")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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

	// The `clientId` is the other half of the echo: a Codex send registers
	// its pending entry BY the `clientUserMessageId` the steer stamped, so an
	// echo that names nothing is somebody else's message.
	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-user-1",
		"parent_uuid":      "parent-wire-1",
		"client_id":        "user:3:flush:1",
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
	anchor, ok, err := app.store.GetMessageAnchor(thread.ID, "user:3:flush:1")
	if err != nil || !ok {
		t.Fatalf("anchor for flushed user item missing after echo: ok=%v err=%v", ok, err)
	}
	if anchor.ProviderUserMessageID != "wire-user-1" || anchor.ProviderParentUUID != "parent-wire-1" {
		t.Fatalf("anchor provider ids = %q/%q, want wire-user-1/parent-wire-1",
			anchor.ProviderUserMessageID, anchor.ProviderParentUUID)
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
	thread.WorkspacePath = initGitRepo(t)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
		"client_id":        "user:3:flush:1",
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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

	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-fallback",
		"client_id":        userItemID,
	})
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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	// The FAILING item requeues ahead of the unattempted tail (round-13,
	// CT13-1): dropping it would leave the message in no state at all.
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 3 {
		t.Fatalf("requeued items: got %d, want 3 (failing item + unattempted tail)", len(requeued))
	}
	if requeued[0].ID != "queue:0" || requeued[1].ID != "queue:1" || requeued[2].ID != "queue:2" {
		t.Fatalf("requeued order: got [%s, %s, %s], want [queue:0, queue:1, queue:2]",
			requeued[0].ID, requeued[1].ID, requeued[2].ID)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	// The failed item requeues (round-13, CT13-1) — the final
	// queue_state_changed must show it back in Zone 1, not vanished.
	last := states[len(states)-1]
	if len(last.Items) != 1 || last.Items[0].ID != "queue:failed" {
		t.Fatalf("last queue_state_changed items = %+v, want the failed item requeued", last.Items)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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

	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-timeout",
		"client_id":        "user:0:flush:1",
	})
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

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
// per-provider routing decision: Codex sessions go through Steer (the
// caller owns the no-active-turn fallback), other sessions go through
// Send only. Exercised at the unit level so the full trigger →
// dispatch → session test infra isn't needed for this branch.
func TestDispatchFlushToProvider_RoutesByProviderType(t *testing.T) {
	app := newTestAppWithStore(t)

	// A nil-codex / nil-claude session should fail fast with
	// "no provider" rather than panic — guard against bad wiring.
	app.dispatchFlushToProviderShouldErrorWith(t, session{Provider: "claude"}, "no provider")
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
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})
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

	echoMeta, _ := json.Marshal(map[string]any{
		"provider_item_id": "wire-payload",
		"client_id":        "user:1:flush:1",
	})
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
	sess := session{Provider: string(provider.Codex), Codex: codexSess}
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

	claudeSess := session{Provider: string(provider.Claude)}
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
	app.triage.RegisterPendingFlushResendWithExpectation(thread.ID, "user:1:flush:1", 1, triage.PendingSendExpectation{})

	claudeSess := session{Provider: string(provider.Claude)}
	got, _, err := app.resolveFlushTurnPlacement(thread.ID, claudeSess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if got != 2 {
		t.Fatalf("turn index: got %d, want 2 (must skip past in-flight pending send at turn 1)", got)
	}
}

// TestDispatchFlush_Claude_EagerPersistAtActiveTurn verifies that when
// a Claude session has an active turn, the flush-dispatched user_text
// row is persisted quietly at the active turn's index (reserving its
// timeline position in SQLite) while the frontend keeps the Zone 2
// queued marker visible. No provider:item_event fires until the
// provider echo stamps provider_item_id — at which point the upsert
// clears Zone 2 and the item appears in the timeline at the reserved
// position. The pending send is registered at the next turn so
// resolveTurnIndexOnStart opens a fresh turn for the response.
func TestDispatchFlush_Claude_EagerPersistAtActiveTurn(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-eager")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:3", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "original prompt", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	// Simulate a tool_call row at turn 3 so there's existing content.
	if _, err := app.store.AppendItem(store.Item{
		ID: "toolu_sleep", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "sleep 10", CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:msg1", Message: "queued follow-up", Payload: json.RawMessage(`{}`)},
	})

	// The user_text row must be persisted immediately at turn 3 (the active turn).
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var flushRow *store.Item
	for i, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			flushRow = &items[i]
			break
		}
	}
	if flushRow == nil {
		t.Fatalf("eagerly-persisted flush row not found at turn 3; items: %+v", items)
	}
	if flushRow.Summary != "queued follow-up" {
		t.Errorf("flush row summary: got %q, want %q", flushRow.Summary, "queued follow-up")
	}
	if flushRow.ItemIndex <= 1 {
		t.Errorf("flush row item_index=%d, want > 1 (after user:3 and tool_call)", flushRow.ItemIndex)
	}

	// The pending send must be registered at the NEXT turn (4), not the active turn (3).
	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok {
		t.Fatalf("no pending send registered after eager-persist dispatch")
	}
	if head.TurnIndex != 4 {
		t.Errorf("pending send TurnIndex: got %d, want 4 (response turn)", head.TurnIndex)
	}
	// Claude flush dispatch mints the uuid the CLI will echo back; the
	// fabricated echo below must carry it or identity matching rejects it.
	if head.ExpectedProviderItemID == "" {
		t.Fatalf("pending send has no ExpectedProviderItemID — Claude flush dispatch should mint one")
	}
	echoID := head.ExpectedProviderItemID

	// provider:queue_flushed must have been emitted (Zone 1 → Zone 2 transition).
	if !rec.hasEvent("provider:queue_flushed") {
		t.Error("provider:queue_flushed not emitted")
	}

	// provider:item_event must NOT have been emitted during dispatch — the
	// quiet persist reserves the timeline position in SQLite but the
	// frontend keeps showing the Zone 2 queued marker until the echo.
	if rec.hasEvent("provider:item_event") {
		t.Error("provider:item_event should not fire during quiet persist (Zone 2 should stay)")
	}

	// Simulate the provider echo — attachProviderItemIDToUserRow stamps
	// provider_item_id and emits the upsert that clears Zone 2.
	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": echoID})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: thread.ID,
		TurnIndex: 3, ItemID: flushRow.ID, Content: "queued follow-up",
		Meta: echoMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText echo: %v", err)
	}

	// After echo, provider:item_event must have been emitted (Zone 2 clears).
	if !rec.hasEvent("provider:item_event") {
		t.Error("provider:item_event not emitted after echo — Zone 2 would stay stuck")
	}

	// The row should carry provider_item_id in meta.
	stamped, found, err := app.store.GetThreadItem(thread.ID, flushRow.ID)
	if err != nil || !found {
		t.Fatalf("row after echo: found=%v err=%v", found, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(stamped.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != echoID {
		t.Errorf("provider_item_id after echo: got %v, want %s", meta["provider_item_id"], echoID)
	}
	// On echo the eager flush row is repositioned to the turn tail
	// (BumpItemToTurnEnd) so it lands AFTER any rows the model emitted
	// between dispatch and echo. No intervening content arrived in this
	// test, so the move is degenerate — but the row must still end up last
	// at its turn. The behavioral guard with intervening content lives in
	// TestDispatchFlush_Claude_EagerPersist_RepositionsAfterContentBeforeEcho.
	afterEcho, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn after echo: %v", err)
	}
	for _, it := range afterEcho {
		if it.ID != stamped.ID && it.ItemIndex >= stamped.ItemIndex {
			t.Errorf("flush row not at turn tail after echo: %s at index %d >= flush index %d", it.ID, it.ItemIndex, stamped.ItemIndex)
		}
	}
	// The echo confirms the CLI consumed the message, so the flush row gets
	// its own message anchor — that row is now an un-send/fork target like
	// any other user message. Rolling back to it is safe at a shared turn
	// because Claude truncation is item-granular
	// (DeleteConversationFromItem): the original user:3 prompt and the
	// agent work before the queued send survive.
	anchor, ok, err := app.store.GetMessageAnchor(thread.ID, flushRow.ID)
	if err != nil || !ok {
		t.Fatalf("eagerly-persisted flush row has no anchor after echo (ok=%v err=%v)", ok, err)
	}
	if anchor.TurnIndex != 3 {
		t.Errorf("flush anchor turn_index: got %d, want 3 (the shared turn)", anchor.TurnIndex)
	}
	if anchor.ProviderUserMessageID != echoID {
		t.Errorf("flush anchor provider_user_message_id: got %q, want %q", anchor.ProviderUserMessageID, echoID)
	}
}

// TestDispatchFlush_Claude_EagerPersist_RepositionsAfterContentBeforeEcho
// reproduces the queued-message ordering bug. A message queued while
// Claude is mid-turn is eagerly persisted at the active turn, then the
// model emits more rows (response text, a file read, a tail command)
// BEFORE it consumes the queued message. The wire echo must reposition
// the queued row to the turn tail so it lands AFTER that content — not
// stranded at its dispatch-time slot above it.
func TestDispatchFlush_Claude_EagerPersist_RepositionsAfterContentBeforeEcho(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-reposition")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:3", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "original prompt", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	// A tool_call already running when the user queues the message.
	if _, err := app.store.AppendItem(store.Item{
		ID: "toolu_inflight", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "in-flight work", CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed in-flight tool_call: %v", err)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:msg1", Message: "feel free to update", Payload: json.RawMessage(`{}`)},
	})

	const flushID = "user:3:flush:1"
	dispatched, found, err := app.store.GetThreadItem(thread.ID, flushID)
	if err != nil || !found {
		t.Fatalf("flush row after dispatch: found=%v err=%v", found, err)
	}

	// The model keeps working in the active turn AFTER the queue dispatch
	// but BEFORE it consumes the queued message: response text, a file
	// read, then a tail command. Each lands at MAX+1, above the
	// dispatch-time flush slot.
	intervening := []store.Item{
		{ID: "assistant:3:0", Kind: "assistant_text", Summary: "let me check the bindings"},
		{ID: "toolu_read", Kind: "tool_call", Summary: "read manifest_bindings.py"},
		{ID: "toolu_tail", Kind: "tool_call", Summary: "tail -25"},
	}
	for i, it := range intervening {
		it.ThreadID = thread.ID
		it.TurnIndex = 3
		it.Role = "assistant"
		it.Status = "completed"
		it.CreatedAt = now + int64(2+i)
		it.UpdatedAt = it.CreatedAt
		if _, err := app.store.AppendItem(it); err != nil {
			t.Fatalf("append intervening %s: %v", it.ID, err)
		}
	}

	// Sanity: before the echo the flush row is stranded ABOVE the
	// intervening content — the bug, if left uncorrected.
	tail, found, err := app.store.GetThreadItem(thread.ID, "toolu_tail")
	if err != nil || !found {
		t.Fatalf("tail row: found=%v err=%v", found, err)
	}
	if dispatched.ItemIndex >= tail.ItemIndex {
		t.Fatalf("test setup wrong: dispatch-time flush index %d should be < tail index %d", dispatched.ItemIndex, tail.ItemIndex)
	}

	// The provider echo: Claude consumed the queued message after the tail.
	// It must carry the uuid minted at dispatch or identity matching rejects it.
	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok || head.ExpectedProviderItemID == "" {
		t.Fatalf("no pending send with a minted uuid after dispatch (ok=%v)", ok)
	}
	echoID := head.ExpectedProviderItemID
	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": echoID})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: thread.ID,
		TurnIndex: 3, ItemID: flushID, Content: "feel free to update",
		Meta: echoMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText echo: %v", err)
	}

	// After echo, the queued row must sort AFTER every row that arrived
	// before the echo — the whole point of the fix.
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn after echo: %v", err)
	}
	indexByID := map[string]int{}
	var flushRow store.Item
	for _, it := range items {
		indexByID[it.ID] = it.ItemIndex
		if it.ID == flushID {
			flushRow = it
		}
	}
	if flushRow.ID == "" {
		t.Fatalf("flush row missing after echo; items: %+v", items)
	}
	for _, id := range []string{"assistant:3:0", "toolu_read", "toolu_tail"} {
		if indexByID[id] >= flushRow.ItemIndex {
			t.Errorf("flush row index %d must exceed %s index %d (queued message stranded above content that arrived first)", flushRow.ItemIndex, id, indexByID[id])
		}
	}

	var meta map[string]any
	if err := json.Unmarshal([]byte(flushRow.Meta), &meta); err != nil {
		t.Fatalf("unmarshal flush meta: %v", err)
	}
	if meta["provider_item_id"] != echoID {
		t.Errorf("provider_item_id after echo: got %v, want %s", meta["provider_item_id"], echoID)
	}
}

// TestDispatchFlush_Claude_InterruptPromotesAfterStoppedByUser verifies
// that on interrupt, quietly-persisted flush messages are bumped to
// MAX(item_index)+1 so they sort AFTER the "Stopped by user" marker.
func TestDispatchFlush_Claude_InterruptPromotesAfterStoppedByUser(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-interrupt")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:3", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "run sleep", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := app.store.AppendItem(store.Item{
		ID: "toolu_sleep", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "sleep 10", CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	// Dispatch two flush messages while agent is working.
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:m1", Message: "test message 1", Payload: json.RawMessage(`{}`)},
		{ID: "queue:m2", Message: "test message 2", Payload: json.RawMessage(`{}`)},
	})

	// Both flush rows are quietly persisted at turn 3.
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var flushIndices []int
	for _, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			flushIndices = append(flushIndices, it.ItemIndex)
		}
	}
	if len(flushIndices) != 2 {
		t.Fatalf("expected 2 flush rows at turn 3, got %d", len(flushIndices))
	}

	// Simulate the interrupt sequence — pre-ack sample + mark, then the
	// post-ack bookkeeping persists "Stopped by user" after the flush
	// messages. We need an open turn in triage for this to work.
	app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID,
		TurnIndex: 3, Timestamp: time.Now(),
	})
	interruptedTurn := app.triage.OpenTurnIndex(thread.ID)
	stampToken := app.triage.MarkFlushSendsInterrupted(thread.ID, interruptedTurn)
	stoppedID, err := app.triage.MarkUserInterrupt(thread.ID, interruptedTurn, stampToken)
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	if stoppedID == "" {
		t.Fatal("MarkUserInterrupt returned empty ID")
	}

	// "Stopped by user" should be at a higher item_index than the flush rows.
	stoppedItem, found, err := app.store.GetThreadItem(thread.ID, stoppedID)
	if err != nil || !found {
		t.Fatalf("Stopped by user row: found=%v err=%v", found, err)
	}
	for _, fi := range flushIndices {
		if fi >= stoppedItem.ItemIndex {
			t.Errorf("flush item_index %d >= stopped item_index %d before promote", fi, stoppedItem.ItemIndex)
		}
	}

	rec.reset()

	// Promote — should bump flush messages after "Stopped by user".
	promoted := app.triage.PromoteQuietFlushSends(thread.ID, stampToken)
	if len(promoted) != 2 {
		t.Errorf("promoted: got %d, want 2", len(promoted))
	}

	// provider:item_event must have been emitted for promoted items.
	if !rec.hasEvent("provider:item_event") {
		t.Error("provider:item_event not emitted after promote")
	}

	// Re-read and verify ordering: both flush rows should now be after
	// "Stopped by user".
	items, err = app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn after promote: %v", err)
	}
	for _, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			if it.ItemIndex <= stoppedItem.ItemIndex {
				t.Errorf("flush row %s item_index=%d should be > stopped item_index=%d",
					it.ID, it.ItemIndex, stoppedItem.ItemIndex)
			}
		}
	}
}

// TestDispatchFlush_Claude_NoActiveTurn_DefersLikeCodex verifies that
// when no active turn exists for a Claude session, flush dispatch uses
// the deferred persistence path (same as Codex) — the item appears in
// the timeline only after the provider echoes it.
func TestDispatchFlush_Claude_NoActiveTurn_DefersLikeCodex(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-no-active")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// Seed a completed turn — no active turn.
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:0", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "done", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-0", ThreadID: thread.ID, TurnIndex: 0,
		StartedAt: now, CompletedAt: &now,
	}); err != nil {
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:def", Message: "follow-up after turn", Payload: json.RawMessage(`{}`)},
	})

	// No user_text row should exist yet — deferred until echo.
	items, err := app.store.ListItemsForTurn(thread.ID, 1)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			t.Fatalf("flush row should not be persisted before echo (deferred path); got %+v", it)
		}
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("pending-send marker not registered")
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

// captureChannel is capture as the triage.Router emit callback takes it:
// same recorder, typed channel.
func (r *emitRecorder) captureChannel(channel eventchan.Channel, data any) {
	r.capture(channel.String(), data)
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

func (r *emitRecorder) hasEvent(channel string) bool {
	for _, c := range r.snapshot() {
		if c.Channel == channel {
			return true
		}
	}
	return false
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

func emittedQueueRestored(rec *emitRecorder) []QueueRestoredEvent {
	out := make([]QueueRestoredEvent, 0)
	for _, c := range rec.snapshot() {
		if c.Channel != "provider:queue_restored" {
			continue
		}
		evt, ok := c.Data.(QueueRestoredEvent)
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
	app.triage = triage.NewRouter(app.store, rec.captureChannel)
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

func TestSessionDeathRestoresQueuedFlushItemsToDraft(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("restore-queued-on-death")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	settled := 0
	settlement := triage.NewFlushSettlement(func() { settled++ })
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID:         "queue:first",
		Message:    "first queued",
		Payload:    json.RawMessage(`{}`),
		EnqueuedAt: 10,
		Settlement: settlement,
	})
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID:         "queue:second",
		Message:    "second queued",
		Payload:    json.RawMessage(`{}`),
		EnqueuedAt: 20,
	})

	app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	if app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("queued flush items still live after session-death restore")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "first queued\n\nsecond queued" {
		t.Fatalf("draft content = %q, want joined queued messages", draft.Content)
	}
	state := rec.lastQueueState(t)
	if len(state.Items) != 0 {
		t.Fatalf("queue state items = %+v, want empty after restore", state.Items)
	}
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 {
		t.Fatalf("queue restored events = %+v, want one", restored)
	}
	if strings.Join(restored[0].QueueItemIDs, ",") != "queue:first,queue:second" {
		t.Fatalf("restored queue ids = %+v", restored[0].QueueItemIDs)
	}
	if settled != 1 {
		t.Fatalf("durable composer recovery settled the injected message %d times, want 1", settled)
	}
	settlement.Settle()
	if settled != 1 {
		t.Fatalf("settlement fired again after recovery: %d", settled)
	}
}

func TestSessionDeathDraftFailureRequeuesWithoutSettling(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("restore-draft-failure")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	settled := 0
	settlement := triage.NewFlushSettlement(func() { settled++ })
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID: "queue:retry", Message: "still pending", Payload: json.RawMessage(`{}`),
		EnqueuedAt: 10, Settlement: settlement,
	})
	if err := app.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	requeued := app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	if settled != 0 {
		t.Fatalf("failed draft recovery settled the message %d times, want 0", settled)
	}
	if len(requeued) != 1 || requeued[0].Settlement != settlement {
		t.Fatalf("requeued recovery = %+v, want the original pending settlement", requeued)
	}
	queued := app.triage.QueuedFlushItems(thread.ID)
	if len(queued) != 1 || queued[0].Settlement != settlement {
		t.Fatalf("live requeue = %+v, want the original pending settlement", queued)
	}
}

// TestPreInitTeardownRestoresQueuedSendsToDraft: the dead-on-arrival
// teardown must hand queued sends back to the composer draft BEFORE its
// triage cleanup sweeps the queue — the queue lives only in router
// memory, so skipping the restore (as routing through the generic
// teardownAndCloseSession would) silently discards the user's text.
func TestPreInitTeardownRestoresQueuedSendsToDraft(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("doa-restore")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID:         "queue:doa",
		Message:    "queued while the dead session was starting",
		Payload:    json.RawMessage(`{}`),
		EnqueuedAt: 10,
	})
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "doa-token"})

	app.teardownDeadPreInitSession(thread.ID, "doa-token")

	if _, ok := app.sessionManager().get(thread.ID); ok {
		t.Fatal("dead pre-init session still registered after teardown")
	}
	if app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("queued flush items still live after DOA teardown")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "queued while the dead session was starting" {
		t.Fatalf("draft content = %q, want the restored queued message", draft.Content)
	}
}

func TestSessionStatusErrorRestoresQueuedFlushItemsToDraft(t *testing.T) {
	for _, providerType := range []provider.ProviderKind{provider.Claude, provider.Codex} {
		t.Run(string(providerType), func(t *testing.T) {
			app, rec := newAppForFlushQueueRPC(t)

			thread := testThread("restore-handler-" + string(providerType))
			thread.Provider = string(providerType)
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			if err := app.triage.Handle(provider.ProviderEvent{
				Kind:      provider.EventTurnStart,
				ThreadID:  thread.ID,
				TurnIndex: 0,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn start: %v", err)
			}
			app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
				ID:         "queue:handler",
				Message:    "restore through handler",
				Payload:    json.RawMessage(`{}`),
				EnqueuedAt: 10,
			})

			handler := app.sessionEventHandler(thread.ID, "session-token", string(providerType))
			handler(provider.ProviderEvent{
				Kind:      provider.EventSessionStatus,
				ThreadID:  thread.ID,
				Content:   "error",
				Timestamp: time.Now(),
			})

			var draft store.ThreadDraft
			waitForCondition(t, "session error restores queued message", func() bool {
				var err error
				draft, _, err = app.store.GetThreadDraft(thread.ID)
				return err == nil && draft.Content == "restore through handler"
			})
			if app.triage.HasQueuedFlushItems(thread.ID) {
				t.Fatal("queued flush items still live after handler restore")
			}
			if restored := emittedQueueRestored(rec); len(restored) != 1 {
				t.Fatalf("queue restored events = %+v, want one", restored)
			}
		})
	}
}

func TestSessionDeathRestoresDeferredAndQuietFlushesToDraft(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("restore-flushed-on-death")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	quiet := store.Item{
		ID:        "user:0:flush:1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "quiet pending",
		Meta:      `{"attachments":[{"id":"att-1","threadId":"restore-flushed-on-death","filename":"a.png","mimeType":"image/png","size":1}]}`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.triage.PersistItemQuiet(quiet, nil); err != nil {
		t.Fatalf("PersistItemQuiet: %v", err)
	}
	app.triage.RegisterPendingQuietFlushSendWithExpectation(thread.ID, "queue:quiet", quiet, 0, 10, triage.PendingSendExpectation{ProviderItemID: ""})
	deferred := store.Item{
		ID:        "user:1:flush:1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "deferred pending",
		CreatedAt: now + 1,
		UpdatedAt: now + 1,
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:deferred", deferred, 20, triage.PendingSendExpectation{ProviderItemID: ""})

	app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("pending queued sends still live after session-death restore")
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, quiet.ID); err != nil {
		t.Fatalf("GetThreadItem quiet: %v", err)
	} else if found {
		t.Fatal("quiet pending flush row still exists after restore")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "quiet pending\n\ndeferred pending" {
		t.Fatalf("draft content = %q, want quiet then deferred", draft.Content)
	}
	if draft.Attachments != `["att-1"]` {
		t.Fatalf("draft attachments = %s, want att-1", draft.Attachments)
	}
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 {
		t.Fatalf("queue restored events = %+v, want one", restored)
	}
	if strings.Join(restored[0].UserItemIDs, ",") != "user:0:flush:1,user:1:flush:1" {
		t.Fatalf("restored user ids = %+v", restored[0].UserItemIDs)
	}
}

// TestDispatchFlush_StaleRowCleanupBeforePersist pins R11-1 (round
// 11): a requeued item carrying StaleUserItemID — a quiet row from a
// previous dispatch whose session-death cleanup failed — must have
// that row removed before the redispatch persists a fresh one.
// Without the retry, the timeline shows the message twice: the stale
// unconsumed row plus the new dispatch's row.
func TestDispatchFlush_StaleRowCleanupBeforePersist(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-stale-cleanup")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	// The stale row a dead session's failed cleanup left behind, on an
	// earlier turn than the fresh dispatch will target — the redispatch
	// mints its row on the ACTIVE turn (3), so the stale id stays
	// distinguishable from the fresh one.
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:0:flush:1", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "retry me", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
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
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID: "queue:retry", Message: "retry me", Payload: json.RawMessage(`{}`),
		StaleUserItemID: "user:0:flush:1",
	}})

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:0:flush:1"); err != nil {
		t.Fatalf("GetThreadItem stale: %v", err)
	} else if found {
		t.Error("stale flush row survived the redispatch cleanup")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	count := 0
	for _, it := range items {
		if it.Kind == "user_text" && it.Summary == "retry me" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("rows with the message = %d, want exactly 1 (stale row cleaned, fresh row persisted)", count)
	}
}

// TestDispatchFlush_FailedDispatchKeepsStaleRow pins R12-1 (round 12):
// the stale-row cleanup retry must run AFTER every failure-prone
// resolution step (envelope, thread load, session lookup, turn
// placement). When dispatch aborts before reaching the persist — here,
// no live session — the stale row is the message's only durable copy
// and must survive for the next retry; a cleanup at the top of the
// dispatch would delete it and then fail, losing the message entirely.
func TestDispatchFlush_FailedDispatchKeepsStaleRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-stale-keep")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:0:flush:1", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "keep me", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	// No session registered: dispatch fails at session resolution.
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID: "queue:keep", Message: "keep me", Payload: json.RawMessage(`{}`),
		StaleUserItemID: "user:0:flush:1",
	}})

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:0:flush:1"); err != nil {
		t.Fatalf("GetThreadItem stale: %v", err)
	} else if !found {
		t.Error("failed dispatch deleted the stale flush row — the message's only durable copy is gone")
	}
	// The failing item requeues with its stale marker intact (round-13,
	// CT13-1): the session-lookup failure happened BEFORE the cleanup
	// ran, so the retry obligation must survive for the next dispatch.
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("requeued items: got %d, want 1 (the failing item)", len(requeued))
	}
	if requeued[0].ID != "queue:keep" || requeued[0].StaleUserItemID != "user:0:flush:1" {
		t.Fatalf("requeued item = %+v, want queue:keep with StaleUserItemID user:0:flush:1", requeued[0])
	}
}

// TestDispatchFlush_PostPersistFailureRequeuesWithFreshRowMarker pins
// the marker-transition half of R13-1 (round 13): when the stale-row
// cleanup succeeds and the eager quiet persist lands but the provider
// send then fails, the requeued item must carry StaleUserItemID = the
// FRESH row id — the redispatch's cleanup then removes that quiet row
// before persisting again, so the timeline never shows the message
// twice. Requeuing with the old marker (row already deleted) or no
// marker (fresh row left behind) both duplicate.
func TestDispatchFlush_PostPersistFailureRequeuesWithFreshRowMarker(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-postpersist-marker")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:0:flush:1", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "retry me", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// A registered session whose subprocess is already closed: session
	// resolution and the quiet persist succeed, the stdin write fails.
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID: "queue:retry", Message: "retry me", Payload: json.RawMessage(`{}`),
		StaleUserItemID: "user:0:flush:1",
	}})

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:0:flush:1"); err != nil {
		t.Fatalf("GetThreadItem stale: %v", err)
	} else if found {
		t.Error("stale flush row survived a dispatch that reached the persist")
	}
	fresh, found, err := app.store.GetThreadItem(thread.ID, "user:3:flush:1")
	if err != nil || !found {
		t.Fatalf("fresh quiet row user:3:flush:1: found=%v err=%v", found, err)
	}
	if fresh.Summary != "retry me" {
		t.Fatalf("fresh row summary = %q, want the message", fresh.Summary)
	}
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("requeued items: got %d, want 1", len(requeued))
	}
	if requeued[0].StaleUserItemID != "user:3:flush:1" {
		t.Fatalf("requeued StaleUserItemID = %q, want the fresh row id user:3:flush:1", requeued[0].StaleUserItemID)
	}
}

// TestCodexResendAfterInterrupt_FailedSendRequeuesWithStaleMarkers
// pins R13-2 (round 13): the Codex resend-after-interrupt clears the
// pending entries (their echoes never come — Codex discards steered
// pending_input at turn/interrupt) and re-sends; when that send FAILS,
// the eagerly-persisted rows would otherwise sit in the timeline
// looking delivered while no recovery path knows about them. The
// failure must requeue each message with StaleUserItemID = its row id
// so the redispatch cleans the eager row before re-persisting.
// TestCodexResendAfterInterrupt_FailedSendRestoresDraft pins the
// Codex-TUI-parity recovery for a definite resend failure: input the
// model never consumed goes back to the composer (eager row + its
// anchor deleted, content restored to the draft, queue_restored
// emitted), never stays in the timeline looking sent and never
// requeues (the TUI restores unconsumed input to the composer; the
// queue path was the pre-parity behavior, round-13 CT13-2).
func TestCodexResendAfterInterrupt_FailedSendRestoresDraft(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("codex-resend-requeue")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	appSess := session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Codex:    sess,
		Liveness: newSessionLiveness(time.Now()),
	}
	app.sessionManager().put(thread.ID, appSess)

	deferred := store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "resend me",
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:q1", deferred, 10, triage.PendingSendExpectation{ProviderItemID: ""})
	tok := app.triage.MarkFlushSendsInterrupted(thread.ID, -1)

	app.eagerPersistFlushSendsOnInterrupt(thread.ID, appSess, -1, tok)

	// The eager row is deleted — the timeline must not show a message
	// the provider never received...
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Error("eager row survived the failed resend — the timeline shows a message the provider never received")
	}
	// ...its message anchor went with it (FK cascade)...
	if _, ok, err := app.store.GetMessageAnchor(thread.ID, "user:1:flush:1"); err != nil {
		t.Fatalf("GetMessageAnchor: %v", err)
	} else if ok {
		t.Error("anchor for the deleted eager row survived")
	}
	// ...the dead-send pending entry is gone...
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("pending-send entry live after failed resend — its echo can never arrive")
	}
	// ...nothing is requeued...
	if requeued := app.triage.QueuedFlushItems(thread.ID); len(requeued) != 0 {
		t.Fatalf("requeued items = %+v, want none — recovery is the composer draft", requeued)
	}
	// ...the content is back in the composer draft...
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "resend me" {
		t.Fatalf("draft content = %q, want %q", draft.Content, "resend me")
	}
	// ...and the frontend was told which rows to drop.
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 {
		t.Fatalf("queue restored events = %+v, want one", restored)
	}
	if restored[0].Reason != "resend_failed" {
		t.Errorf("restore reason = %q, want resend_failed", restored[0].Reason)
	}
	if strings.Join(restored[0].UserItemIDs, ",") != "user:1:flush:1" {
		t.Errorf("restored user ids = %+v, want [user:1:flush:1]", restored[0].UserItemIDs)
	}
}

// TestCodexResendAfterInterrupt_DraftMergeFailureRequeues pins the
// fallback inside restoreEagerPersistedFlushesToDraft: when the draft
// write fails after the eager rows are already deleted, the messages
// must keep a delivery vehicle — requeued with their queue identity —
// and the row removal must still reach the frontend so no ghost
// timeline row outlives its deleted store row.
func TestCodexResendAfterInterrupt_DraftMergeFailureRequeues(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("codex-resend-draft-fail")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// A draft whose attachments blob cannot decode makes MergeParts —
	// and therefore the restore's draft write — fail deterministically
	// while every store read/write still works.
	if err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:    thread.ID,
		Content:     "half-typed draft",
		Attachments: "corrupt",
		UpdatedAt:   time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	appSess := session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Codex:    sess,
		Liveness: newSessionLiveness(time.Now()),
	}
	app.sessionManager().put(thread.ID, appSess)

	deferred := store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "resend me",
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:q1", deferred, 10, triage.PendingSendExpectation{ProviderItemID: ""})
	tok := app.triage.MarkFlushSendsInterrupted(thread.ID, -1)

	app.eagerPersistFlushSendsOnInterrupt(thread.ID, appSess, -1, tok)

	// The row deletion happened before the draft write failed...
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Error("eager row survived — cleanup runs before the draft write")
	}
	// ...so the message falls back to the queue with its identity intact...
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("requeued items: got %d, want 1", len(requeued))
	}
	got := requeued[0]
	if got.ID != "queue:q1" || got.Message != "resend me" || got.EnqueuedAt != 10 {
		t.Errorf("requeued identity = %+v, want queue:q1/resend me/enqueued 10", got)
	}
	if got.StaleUserItemID != "user:1:flush:1" {
		// The marker resolves as already-cleaned at redispatch (the row
		// is gone); carrying it is what makes a partial delete retry-safe.
		t.Errorf("requeued StaleUserItemID = %q, want user:1:flush:1", got.StaleUserItemID)
	}
	// ...and the frontend still learns the row is gone.
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 {
		t.Fatalf("queue restored events = %+v, want one", restored)
	}
	if restored[0].Reason != "resend_failed" {
		t.Errorf("restore reason = %q, want resend_failed", restored[0].Reason)
	}
	if strings.Join(restored[0].UserItemIDs, ",") != "user:1:flush:1" {
		t.Errorf("restored user ids = %+v, want [user:1:flush:1]", restored[0].UserItemIDs)
	}
}

// TestStartSession_FlushesRequeuedItems pins R12-2 (round 12): a
// session-death restore can REQUEUE a message instead of restoring it
// to the draft (failed stale-row cleanup, R11-1). Every other drain
// trigger needs a live session (RegisterQueueItem) or wire traffic
// (the boundary drains) — an idle replacement session fires none of
// them, so without the start-funnel flush the requeued message would
// sit in the queue indefinitely. StartSession must drain the queue
// once the new session can accept sends.
func TestStartSession_FlushesRequeuedItems(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-on-start")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeClaudePassthroughBinary(t)}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	// Requeued by a prior session's death drain; no session is live.
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID: "queue:idle", Message: "flush me", Payload: json.RawMessage(`{}`),
		EnqueuedAt: 10,
	})

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })

	// The start-funnel flush hands the batch to the dispatch worker,
	// which serializes behind the thread action lock StartSession holds.
	// With no active turn the dispatch takes the DEFERRED path — the
	// user row persists only at the provider echo, which a passthrough
	// binary never sends — so the observable success is the drained
	// queue plus the provider:queue_flushed emission (the Zone 1 → Zone
	// 2 transition for the dispatched message).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !app.triage.HasQueuedFlushItems(thread.ID) {
			for _, c := range rec.snapshot() {
				if c.Channel != "provider:queue_flushed" {
					continue
				}
				evt, ok := c.Data.(QueueFlushedEvent)
				if !ok {
					t.Fatalf("provider:queue_flushed payload is %T, want QueueFlushedEvent", c.Data)
				}
				for _, it := range evt.Items {
					if it.QueueItemID == "queue:idle" {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("queued item was not flushed after StartSession — an idle replacement session strands requeued messages")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSessionDeathRetriesStaleRowCleanupForQueuedItem pins the
// death-drain half of R11-1: a requeued item caught STILL QUEUED by a
// later session death carries StaleUserItemID through the drain, and
// the drain retries the cleanup — the message restores to the draft
// only once the stale row is gone.
func TestSessionDeathRetriesStaleRowCleanupForQueuedItem(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("death-stale-retry")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID: "user:0:flush:1", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "survived one death", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID: "queue:stale", Message: "survived one death",
		Payload: json.RawMessage(`{}`), EnqueuedAt: 10,
		StaleUserItemID: "user:0:flush:1",
	})

	app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:0:flush:1"); err != nil {
		t.Fatalf("GetThreadItem stale: %v", err)
	} else if found {
		t.Error("stale flush row survived the death-drain cleanup retry")
	}
	if app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("queued flush items still live after restore")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "survived one death" {
		t.Fatalf("draft content = %q, want the restored message", draft.Content)
	}
}

// TestTeardownDeadPreInitSession_KeepsRequeuedItems pins R13-3 (round
// 13, D13-1): the pre-init death teardown restores the queue BEFORE
// CleanupThreadIfEpoch, but a cleanup-failed item is REQUEUED by that
// restore — into exactly the map the cleanup's clearFlushQueueLocked
// then wipes. The teardown must re-register requeued items after the
// cleanup so the replacement session's start-funnel flush (R12-2)
// still finds them. The store is closed to make the stale-row cleanup
// fail, which is the production shape that produces a requeue.
func TestTeardownDeadPreInitSession_KeepsRequeuedItems(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("preinit-requeue")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID: "queue:survivor", Message: "keep me", Payload: json.RawMessage(`{}`),
		EnqueuedAt: 10, StaleUserItemID: "user:0:flush:1",
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "preinit-tok",
	})
	if err := app.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	app.teardownDeadPreInitSession(thread.ID, "preinit-tok")

	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("queued items after pre-init teardown: got %d, want 1 — the cleanup sweep wiped the requeued message", len(requeued))
	}
	if requeued[0].ID != "queue:survivor" || requeued[0].StaleUserItemID != "user:0:flush:1" {
		t.Fatalf("surviving item = %+v, want queue:survivor with its stale marker", requeued[0])
	}
}

// TestQueuedFlushItemFromUnconfirmed_CarriesStaleMarker pins the
// conversion plumbing of R11-1: a failed-cleanup item's requeue must
// keep StaleUserItemID so the redispatch (or the next death drain)
// still knows about the stale row.
func TestQueuedFlushItemFromUnconfirmed_CarriesStaleMarker(t *testing.T) {
	queued, ok := queuedFlushItemFromUnconfirmed(triage.UnconfirmedFlushItem{
		QueueItemID:     "queue:x",
		Message:         "still stale",
		EnqueuedAt:      10,
		StaleUserItemID: "user:2:flush:1",
	})
	if !ok {
		t.Fatal("conversion rejected a message-bearing item")
	}
	if queued.StaleUserItemID != "user:2:flush:1" {
		t.Fatalf("StaleUserItemID = %q, want carried through", queued.StaleUserItemID)
	}
}

func TestSessionDeathDedupeDispatchCurrentAndPendingFlush(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("restore-current-dedupe")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	settled := 0
	settlement := triage.NewFlushSettlement(func() { settled++ })
	app.flushDispatch.mu.Lock()
	app.ensureFlushDispatchMapsLocked()
	app.flushDispatch.current[thread.ID] = flushDispatchBatch{
		items: []triage.QueuedFlushItem{{
			ID:         "queue:same",
			Message:    "queued payload copy",
			Payload:    json.RawMessage(`{}`),
			EnqueuedAt: 10,
			Settlement: settlement,
		}},
		generation: app.flushDispatch.generation[thread.ID],
	}
	app.flushDispatch.inflightItems[thread.ID] = 1
	app.flushDispatch.mu.Unlock()
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:same", store.Item{
		ID:        "user:1:flush:1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "pending richer copy",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}, 10, triage.PendingSendExpectation{ProviderItemID: ""})

	app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "pending richer copy" {
		t.Fatalf("draft content = %q, want one pending copy", draft.Content)
	}
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 {
		t.Fatalf("queue restored events = %+v, want one", restored)
	}
	if strings.Join(restored[0].QueueItemIDs, ",") != "queue:same" {
		t.Fatalf("restored queue ids = %+v", restored[0].QueueItemIDs)
	}
	if strings.Join(restored[0].UserItemIDs, ",") != "user:1:flush:1" {
		t.Fatalf("restored user ids = %+v", restored[0].UserItemIDs)
	}
	if settled != 1 {
		t.Fatalf("dedupe dropped or repeated the dispatch copy's settlement: got %d, want 1", settled)
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
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventModelFallback,
		ThreadID:  thread.ID,
		ItemID:    "model-fallback:req-live-state",
		Meta:      json.RawMessage(`{"originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","reason":"safeguards flagged this message"}`),
		Timestamp: time.UnixMilli(1_700_000_000_200),
	}); err != nil {
		t.Fatalf("model fallback: %v", err)
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue-flushed", store.Item{
		ID:        "user:7:flush:1",
		ThreadID:  thread.ID,
		TurnIndex: 7,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "flushed text",
	}, 0, triage.PendingSendExpectation{})

	state, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	if state.ThreadID != thread.ID {
		t.Fatalf("ThreadID = %q, want %q", state.ThreadID, thread.ID)
	}
	if state.EffectiveModel != "claude-opus-4-8" {
		t.Fatalf("EffectiveModel = %q, want claude-opus-4-8", state.EffectiveModel)
	}
	if state.EffectiveModelRevision == 0 {
		t.Fatal("EffectiveModelRevision = 0, want live projection revision")
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

// TestCodexResendAfterInterrupt_AmbiguousTimeoutKeepsPendingConfirmation
// pins R14-1 (round 14, D14-2): a turn/start JSON-RPC timeout on the
// interrupt resend is delivery-ambiguous — the request was written and
// the turn (with its user-message echo) may already be running. The old
// failure path cleared the pending marker and requeued, so a flush after
// a merely-slow ack re-sent content Codex had already consumed. The
// resend must instead leave the pending confirmation for the echo.
func TestCodexResendAfterInterrupt_AmbiguousTimeoutKeepsPendingConfirmation(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("codex-resend-ambiguous")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "start-timeout")
	codex.SetRequestTimeoutForTest(sess, 25*time.Millisecond)
	appSess := session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Codex:    sess,
		Liveness: newSessionLiveness(time.Now()),
	}
	app.sessionManager().put(thread.ID, appSess)

	deferred := store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "resend me",
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:q1", deferred, 10, triage.PendingSendExpectation{ProviderItemID: ""})
	tok := app.triage.MarkFlushSendsInterrupted(thread.ID, -1)

	app.eagerPersistFlushSendsOnInterrupt(thread.ID, appSess, -1, tok)

	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil || !found {
		t.Fatalf("eager row user:1:flush:1: found=%v err=%v", found, err)
	}
	// The pending confirmation survives the ambiguous timeout...
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("pending-send marker cleared on ambiguous turn/start timeout — a late echo can no longer settle it")
	}
	// ...and nothing is requeued: a redispatch would double-send.
	if requeued := app.triage.QueuedFlushItems(thread.ID); len(requeued) != 0 {
		t.Fatalf("requeued items = %+v, want none on ambiguous timeout", requeued)
	}
}

// TestCodexResendAfterInterrupt_MissingAttachmentRestoresDraft pins
// R14-3 (round 14, CT14-2) under the composer-restore recovery: the
// interrupt resend must deliver the persisted rows' attachments, not
// silently send text-only. An attachment that no longer resolves
// aborts the resend into the draft restore — content and attachment
// ref both land back in the composer instead of degrading.
func TestCodexResendAfterInterrupt_MissingAttachmentRestoresDraft(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	attStore, err := attachment.NewStore(attachment.Config{
		RootDir: filepath.Join(t.TempDir(), "attachments"),
	}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore

	thread := testThread("codex-resend-missing-att")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess := installSteerTestSession(t, app, thread, "ok")
	appSess := session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Codex:    sess,
		Liveness: newSessionLiveness(time.Now()),
	}
	app.sessionManager().put(thread.ID, appSess)

	deferred := store.Item{
		ID: "user:1:flush:1", ThreadID: thread.ID, TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "resend with image",
		Meta:    `{"attachments":[{"id":"att-gone","threadId":"` + thread.ID + `","filename":"x.png","mimeType":"image/png","size":1}]}`,
	}
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "queue:q1", deferred, 10, triage.PendingSendExpectation{ProviderItemID: ""})
	tok := app.triage.MarkFlushSendsInterrupted(thread.ID, -1)

	app.eagerPersistFlushSendsOnInterrupt(thread.ID, appSess, -1, tok)

	// The unresolvable attachment must abort into the draft restore,
	// not produce a successful text-only send.
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:1:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Error("eager row survived the aborted resend")
	}
	if requeued := app.triage.QueuedFlushItems(thread.ID); len(requeued) != 0 {
		t.Fatalf("requeued items = %+v, want none — recovery is the composer draft", requeued)
	}
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("pending-send entry live after aborted resend — its echo can never arrive")
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "resend with image" {
		t.Fatalf("draft content = %q, want %q", draft.Content, "resend with image")
	}
	if draft.Attachments != `["att-gone"]` {
		t.Fatalf("draft attachments = %s, want the restored ref", draft.Attachments)
	}
	restored := emittedQueueRestored(rec)
	if len(restored) != 1 || restored[0].Reason != "resend_failed" {
		t.Fatalf("queue restored events = %+v, want one with reason resend_failed", restored)
	}
}

// TestDispatchFlush_CodexTurnStartTimeoutKeepsPendingConfirmation is
// the turn/start twin of the steer-timeout test above (round 14,
// D14-2): when the steer loses the NoActiveTurn race and the fallback
// Send's turn/start ack times out after the write, the dispatch must
// treat delivery as ambiguous — keep the re-registered pending entry,
// persist no error row, and requeue nothing.
func TestDispatchFlush_CodexTurnStartTimeoutKeepsPendingConfirmation(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-start-timeout")
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

	sess := installSteerTestSession(t, app, thread, "no-active-turn+start-timeout")
	codex.SetRequestTimeoutForTest(sess, 25*time.Millisecond)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    sess,
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:start-timeout", Message: "eventually accepted"},
	})

	if flushed := emittedQueueFlushed(rec); len(flushed) != 1 {
		t.Fatalf("ambiguous timeout should still enter Zone 2, queue_flushed=%+v", flushed)
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("pending-send marker cleared on ambiguous Codex turn/start timeout")
	}
	if requeued := app.triage.QueuedFlushItems(thread.ID); len(requeued) != 0 {
		t.Fatalf("requeued items = %+v, want none on ambiguous timeout", requeued)
	}
	for _, turnIndex := range []int{0, 1} {
		items, err := app.store.ListItemsForTurn(thread.ID, turnIndex)
		if err != nil {
			t.Fatalf("ListItemsForTurn(%d): %v", turnIndex, err)
		}
		for _, it := range items {
			if it.Kind == "error" {
				t.Fatalf("ambiguous turn/start timeout persisted error row: %+v", it)
			}
		}
	}
}

// TestFlushPayloadFromUserMeta_DropsBakedRevisionCommentIDs pins R14-3
// (round 14, CT14-4): the requeue payload is rebuilt from a persisted
// row whose content ALREADY carries the revision comment excerpts the
// original dispatch appended. Carrying the comment-ID lists again would
// make the redispatch append the excerpts a second time — the rebuilt
// payload keeps the source refs (provenance) and attachment ids but
// drops the comment IDs.
func TestFlushPayloadFromUserMeta_DropsBakedRevisionCommentIDs(t *testing.T) {
	meta := `{
		"attachments":[{"id":"att-1","threadId":"t1","filename":"a.png","mimeType":"image/png","size":9}],
		"sourceProposedPlan":{"itemId":"plan-src"},
		"revisionSourceProposedPlan":{"itemId":"plan-rev"},
		"revisionSourceCommentIds":["c1","c2"],
		"revisionSourceDiffCommentIds":["d1"],
		"expandComposerCommands":true
	}`
	raw, err := flushPayloadFromUserMeta(meta)
	if err != nil {
		t.Fatalf("flushPayloadFromUserMeta: %v", err)
	}
	var payload flushQueuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.AttachmentIDs) != 1 || payload.AttachmentIDs[0] != "att-1" {
		t.Errorf("AttachmentIDs = %v, want [att-1]", payload.AttachmentIDs)
	}
	if payload.SourceProposedPlan == nil || payload.SourceProposedPlan.ItemID != "plan-src" {
		t.Errorf("SourceProposedPlan = %+v, want plan-src ref", payload.SourceProposedPlan)
	}
	if payload.RevisionSourceProposedPlan == nil || payload.RevisionSourceProposedPlan.ItemID != "plan-rev" {
		t.Errorf("RevisionSourceProposedPlan = %+v, want plan-rev ref", payload.RevisionSourceProposedPlan)
	}
	if len(payload.RevisionSourceCommentIDs) != 0 {
		t.Errorf("RevisionSourceCommentIDs = %v, want none — excerpts are already baked into the content", payload.RevisionSourceCommentIDs)
	}
	if len(payload.RevisionSourceDiffCommentIDs) != 0 {
		t.Errorf("RevisionSourceDiffCommentIDs = %v, want none — excerpts are already baked into the content", payload.RevisionSourceDiffCommentIDs)
	}
	if !payload.ExpandComposerCommands {
		t.Error("ExpandComposerCommands = false, want composer provenance preserved across requeue")
	}
}

func TestQueuePayloadFromUserItemPreservesComposerCommandProvenance(t *testing.T) {
	raw := queuePayloadFromUserItem(store.Item{
		Meta: `{"expandComposerCommands":true}`,
	}, nil)
	var payload flushQueuePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !payload.ExpandComposerCommands {
		t.Error("ExpandComposerCommands = false, want true")
	}
}

// TestFlushUserMetaHelpers_RejectNullMeta pins the round-14 strictness
// guard (C14-5): a persisted meta of literal JSON null unmarshals into
// the zero struct without error, which would silently drop attachments
// and provenance. Both decode helpers must fail loudly instead.
func TestFlushUserMetaHelpers_RejectNullMeta(t *testing.T) {
	if _, err := flushPayloadFromUserMeta("null"); err == nil {
		t.Error("flushPayloadFromUserMeta(null) = nil error, want corrupt-meta failure")
	}
	if _, err := attachmentIDsFromUserMeta("null"); err == nil {
		t.Error("attachmentIDsFromUserMeta(null) = nil error, want corrupt-meta failure")
	}
	if _, err := flushPayloadFromUserMeta(""); err != nil {
		t.Errorf("flushPayloadFromUserMeta(empty) = %v, want nil — absent meta is legitimate", err)
	}
}

// TestDispatchFlush_SettlesOnlyAfterTheProviderWrite pins the seam
// an app-internal injector's durable bookkeeping hangs off
// (triage.QueuedFlushItem.Settlement, used by the workflow wake). The queue
// in front of the dispatcher is process memory, so the callback must fire on
// the SUCCESS return of dispatchFlushItem and on nothing earlier: a failed
// dispatch requeues the message, and an injector that had already recorded it
// as delivered would go silent about a message nobody ever received.
func TestDispatchFlush_SettlesOnlyAfterTheProviderWrite(t *testing.T) {
	t.Run("a successful dispatch fires it", func(t *testing.T) {
		app, _ := newAppForFlushQueueRPC(t)

		thread := testThread("flush-ondispatched-ok")
		thread.Provider = string(provider.Codex)
		thread.WorkspacePath = t.TempDir()
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}
		if err := app.store.InsertTurn(store.Turn{
			TurnID: "turn-0", ThreadID: thread.ID, TurnIndex: 0,
			StartedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertTurn: %v", err)
		}
		sess := installSteerTestSession(t, app, thread, "ok")
		app.sessionManager().put(thread.ID, session{
			Provider: string(provider.Codex), Token: "flush-token", Codex: sess,
		})

		fired := 0
		app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
			ID: "queue:ok", Message: "wake", Payload: json.RawMessage(`{}`),
			Settlement: triage.NewFlushSettlement(func() { fired++ }),
		}})

		if fired != 1 {
			t.Fatalf("settlement fired %d times, want 1 after the provider write", fired)
		}
	})

	t.Run("a failed dispatch leaves it unfired and requeued", func(t *testing.T) {
		app, _ := newAppForFlushQueueRPC(t)

		thread := testThread("flush-ondispatched-fail")
		thread.Provider = string(provider.Claude)
		thread.WorkspacePath = t.TempDir()
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}

		fired := 0
		// No session registered: dispatch fails at session resolution.
		app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
			ID: "queue:fail", Message: "wake", Payload: json.RawMessage(`{}`),
			Settlement: triage.NewFlushSettlement(func() { fired++ }),
		}})

		if fired != 0 {
			t.Fatalf("settlement fired %d times on a failed dispatch, want 0", fired)
		}
		// The requeued copy keeps the callback, so the retry that succeeds is
		// still the one that records the delivery.
		requeued := app.triage.QueuedFlushItems(thread.ID)
		if len(requeued) != 1 {
			t.Fatalf("requeued items = %d, want 1", len(requeued))
		}
		if requeued[0].Settlement == nil {
			t.Fatal("the requeued item lost its settlement — its retry would deliver unrecorded")
		}
	})
}
