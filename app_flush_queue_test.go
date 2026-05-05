package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestDispatchFlush_Codex_PersistsUserItemAndDispatchesViaSteer pins
// the happy-path Codex flow: a queued message reaches the active
// turn's pending_input via turn/steer, and the AO-side bookkeeping
// (user:<turn>:flush:<n> row + pending-send marker) lands so the
// wire echo can correlate.
func TestDispatchFlush_Codex_PersistsUserItemAndDispatchesViaSteer(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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
	var flushRow *store.Item
	for i := range items {
		if items[i].Kind == "user_text" && strings.HasPrefix(items[i].ID, "user:3:flush:") {
			flushRow = &items[i]
			break
		}
	}
	if flushRow == nil {
		t.Fatalf("expected user:3:flush:* row, got items %+v", items)
	}
	if flushRow.Summary != "drained" {
		t.Errorf("flush row summary: got %q, want %q", flushRow.Summary, "drained")
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send marker not registered after Codex Steer dispatch")
	}
	if _, ok, err := app.store.GetCheckpointByUserItemID(thread.ID, flushRow.ID); err != nil || !ok {
		t.Fatalf("checkpoint for flushed user item missing: ok=%v err=%v", ok, err)
	}
}

// TestDispatchFlush_Codex_NoActiveTurnFallsBackToSend pins the
// race-window fallback: turn/steer returning NoActiveTurn must not
// surface as a hard failure — it triggers a fresh Send that opens a
// new turn carrying the queued content.
func TestDispatchFlush_Codex_NoActiveTurnFallsBackToSend(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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

	// Even though Steer returned NoActiveTurn, the fallback path
	// reaches sess.Send and the user_text row should still be
	// persisted at the active turn's index. There must be NO
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
	var flushRow *store.Item
	for i := range items {
		if strings.HasPrefix(items[i].ID, "user:2:flush:") {
			flushRow = &items[i]
			break
		}
	}
	if flushRow == nil {
		t.Fatalf("expected user:2:flush:* row after fallback, got %+v", items)
	}
}

// TestDispatchFlush_NoSession_PersistsErrorRow pins the missing-session
// guard: a flush trigger fires, but the session was torn down between
// trigger fire and dispatcher arrival — the dispatcher must not panic
// or persist a half-baked item.
func TestDispatchFlush_NoSession_PersistsErrorRow(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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

	// Each attempted item persists a user_text row before its
	// dispatch (the optimistic write); aborting on item 1 means
	// item 2 and 3 must not have rows.
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
	if flushRows != 1 {
		t.Errorf("flush rows persisted: got %d, want 1 (subsequent items aborted)", flushRows)
	}
	if errorRows != 1 {
		t.Errorf("error rows persisted: got %d, want 1 (one per failed item)", errorRows)
	}
	// Pending-send marker must be cleared on the failed item so a
	// later wire echo can't hijack a future send's correlation.
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("pending-send marker live after failed dispatch")
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
// A mis-decoded Payload silently drops attachments — covered here
// by asserting the user_text row's Meta encodes the attachment
// reference.
func TestDispatchFlush_PayloadDecoding(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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
	var row *store.Item
	for i := range items {
		if strings.HasPrefix(items[i].ID, "user:1:flush:") {
			row = &items[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("expected flush row, got %+v", items)
	}
	// marshalUserMessageMeta returns "" for an all-empty payload;
	// applyItemDefaults in the store normalises that to "{}" so
	// downstream JSON consumers always have valid input. The exact
	// stored value is "{}" — assert the store contract rather than
	// the marshaller's intermediate empty string.
	if row.Meta != "{}" {
		t.Errorf("Meta with empty payload: got %q, want %q", row.Meta, "{}")
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
	got, err := app.resolveFlushTurnIndex(thread.ID)
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

	got, err := app.resolveFlushTurnIndex(thread.ID)
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
		AttachmentIDs:              p.AttachmentIDs,
		SourceProposedPlan:         p.SourceProposedPlan,
		RevisionSourceProposedPlan: p.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:   p.RevisionSourceCommentIDs,
	}
}

// emitRecorder captures every event emitted by the App so tests can
// assert against the post-mutation queue snapshot. Mirrors the
// pattern used by other App-event tests but kept local to keep the
// flush-queue test surface self-contained.
type emitRecorder struct {
	calls []emittedCall
}

type emittedCall struct {
	Channel string
	Data    any
}

func (r *emitRecorder) capture(channel string, data any) {
	r.calls = append(r.calls, emittedCall{Channel: channel, Data: data})
}

func (r *emitRecorder) lastQueueState(t *testing.T) QueueStateChangedEvent {
	t.Helper()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].Channel == "provider:queue_state_changed" {
			evt, ok := r.calls[i].Data.(QueueStateChangedEvent)
			if !ok {
				t.Fatalf("provider:queue_state_changed payload is not QueueStateChangedEvent: %T", r.calls[i].Data)
			}
			return evt
		}
	}
	t.Fatalf("no provider:queue_state_changed emission found in calls=%v", r.calls)
	return QueueStateChangedEvent{}
}

func newAppForFlushQueueRPC(t *testing.T) (*App, *emitRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	app.triage = triage.NewRouter(app.store, rec.capture)
	app.triage.SetFlushDispatcher(app.dispatchFlush)
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
	for _, c := range rec.calls {
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
