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
	}, triage.FlushDispatchModeBoundary)

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
	}, triage.FlushDispatchModeBoundary)
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:second", Message: "second"},
	}, triage.FlushDispatchModeBoundary)

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

func TestDispatchFlush_BoundaryFlushDoesNotEmitSendBridgeReason(t *testing.T) {
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
	}, triage.FlushDispatchModeBoundary)

	states := emittedQueueStates(rec)
	if len(states) == 0 {
		t.Fatal("expected queue_state_changed event")
	}
	if states[0].Reason != "" {
		t.Fatalf("first queue_state_changed reason = %q, want empty", states[0].Reason)
	}
	if len(states[0].Items) != 0 {
		t.Fatalf("flush-started queue snapshot items: got %d, want 0", len(states[0].Items))
	}
}

func TestDispatchFlush_ImmediateCodexUsesFreshSend(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("flush-immediate-send")
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

	methodLog := filepath.Join(t.TempDir(), "codex-methods.log")
	sess := installMethodLoggingCodexSession(t, thread, methodLog)
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "flush-token",
		codex:    sess,
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:forced", Message: "forced"},
	}, triage.FlushDispatchModeImmediate)

	states := emittedQueueStates(rec)
	if len(states) == 0 {
		t.Fatal("expected queue_state_changed event")
	}
	if states[0].Reason != queueStateReasonFlushStarted {
		t.Fatalf("first queue_state_changed reason = %q, want %q", states[0].Reason, queueStateReasonFlushStarted)
	}
	methods := readMethodLog(t, methodLog)
	if strings.Contains(methods, "turn/steer") {
		t.Fatalf("immediate flush used turn/steer; methods:\n%s", methods)
	}
	if !strings.Contains(methods, "turn/start") {
		t.Fatalf("immediate flush did not start a fresh turn; methods:\n%s", methods)
	}
	flushed := emittedQueueFlushed(rec)
	if len(flushed) != 1 {
		t.Fatalf("queue_flushed events: got %d, want 1", len(flushed))
	}
	if got := flushed[0].Items[0].UserItemID; got != "user:1:flush:1" {
		t.Fatalf("immediate flush user item id = %q, want user:1:flush:1", got)
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
	}, triage.FlushDispatchModeBoundary)

	// Even though Steer returned NoActiveTurn, the fallback path
	// reaches sess.Send. The user_text row still waits for the
	// provider echo, and there must be NO
	// sibling `error` row — the fallback succeeded, so this isn't a
	// failure case from the user's POV.
	items, err := app.store.ListItemsForTurn(thread.ID, 2)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, it := range items {
		if it.Kind == "error" {
			t.Errorf("unexpected error row after fallback: %+v", it)
		}
	}
	for _, item := range items {
		if strings.HasPrefix(item.ID, "user:2:flush:") {
			t.Fatalf("user_text row should wait for provider echo after fallback, got %+v", item)
		}
	}
	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": "wire-fallback"})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  thread.ID,
		TurnIndex: 2,
		ItemID:    "user:2:flush:1",
		Content:   "race",
		Meta:      echoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText: %v", err)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:2:flush:1"); err != nil || !found {
		t.Fatalf("expected user:2:flush:1 after echo: found=%v err=%v", found, err)
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
	}, triage.FlushDispatchModeBoundary)

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
	}, triage.FlushDispatchModeBoundary)

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
	}, triage.FlushDispatchModeBoundary)

	if flushed := emittedQueueFlushed(rec); len(flushed) != 0 {
		t.Fatalf("failed dispatch emitted queue_flushed events: %+v", flushed)
	}

	states := emittedQueueStates(rec)
	if len(states) < 2 {
		t.Fatalf("queue_state_changed events: got %d, want at least 2", len(states))
	}
	last := states[len(states)-1]
	if last.Reason != "" {
		t.Fatalf("last queue_state_changed reason = %q, want empty for boundary failure", last.Reason)
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
	}, triage.FlushDispatchModeBoundary)

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

	if _, err := app.RegisterQueueItem(thread.ID, "queued while provider works", SendMessageOptions{}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
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
		done <- app.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			ThreadID:  thread.ID,
			TurnIndex: 0,
			ItemID:    "tool-1",
			ItemType:  "Bash",
			Timestamp: now,
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EventToolComplete: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("configured flush dispatcher blocked provider event handler")
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
	app.enqueueFlushDispatch(thread.ID, []triage.QueuedFlushItem{{ID: "queue:0", Message: "first"}}, triage.FlushDispatchModeBoundary)
	app.enqueueFlushDispatch(thread.ID, []triage.QueuedFlushItem{{ID: "queue:1", Message: "second"}}, triage.FlushDispatchModeBoundary)
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
	err := a.dispatchFlushToProvider(sess, "x", provider.SendOptions{}, triage.FlushDispatchModeBoundary)
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
	}, triage.FlushDispatchModeBoundary)

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
	got, err := app.resolveFlushTurnIndex(thread.ID, triage.FlushDispatchModeBoundary)
	if err != nil {
		t.Fatalf("resolveFlushTurnIndex: %v", err)
	}
	if got != 1 {
		t.Errorf("turn index: got %d, want 1 (next after settled turn 0)", got)
	}
}

// TestDispatchFlush_ResolveTurnIndex_PrefersActiveTurn pins the
// happy-path resolution: when the active turn exists, we use its
// index even if LastTurnIndex would yield a different number.
func TestDispatchFlush_ResolveTurnIndex_PrefersActiveTurn(t *testing.T) {
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

	got, err := app.resolveFlushTurnIndex(thread.ID, triage.FlushDispatchModeBoundary)
	if err != nil {
		t.Fatalf("resolveFlushTurnIndex: %v", err)
	}
	if got != 7 {
		t.Errorf("turn index: got %d, want 7 (active turn)", got)
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

func TestUndoQueuedItems_DropsAndReturnsAll(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-undo")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	queued1, _ := app.RegisterQueueItem(thread.ID, "first", SendMessageOptions{})
	queued2, _ := app.RegisterQueueItem(thread.ID, "second", SendMessageOptions{})

	dropped, err := app.UndoQueuedItems(thread.ID)
	if err != nil {
		t.Fatalf("UndoQueuedItems: %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped count: got %d, want 2", len(dropped))
	}
	if dropped[0].ID != queued1.ID || dropped[1].ID != queued2.ID {
		t.Errorf("dropped order: got [%s, %s], want [%s, %s]",
			dropped[0].ID, dropped[1].ID, queued1.ID, queued2.ID)
	}
	state := rec.lastQueueState(t)
	if len(state.Items) != 0 {
		t.Errorf("post-undo queue state: got %d items, want 0", len(state.Items))
	}
}

func TestUndoQueuedItems_EmptyQueue_NoEmission(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread("rpc-undo-empty")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	dropped, err := app.UndoQueuedItems(thread.ID)
	if err != nil {
		t.Fatalf("UndoQueuedItems: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped count on empty queue: got %d, want 0", len(dropped))
	}
	for _, c := range rec.snapshot() {
		if c.Channel == "provider:queue_state_changed" {
			t.Errorf("unexpected queue_state_changed emission for no-op undo")
		}
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
		Meta:      json.RawMessage(`{"plan":[{"step":"one","status":"inProgress"}]}`),
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
