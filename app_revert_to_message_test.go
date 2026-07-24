package main

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestRevertConversationToMessageTruncatesAndEmits drives the public
// entry point end to end: an idle Claude thread with two completed turns
// is reverted to its second user message. The later turn is truncated
// from SQLite, the provider session is sliced to drop the reverted
// prompt, the reverted text lands back in the composer draft, and the
// `user_message:reverted` event fires so the frontend can mirror the
// truncation. The tail itself (slice granularity, attachments, mid-turn)
// is covered by the rollbackToMessage tests in
// app_conversation_rollback_test.go; this asserts the public method
// wires that tail AND emits.
func TestRevertConversationToMessageTruncatesAndEmits(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := createAppTestThread(t, app, "t-revert", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, app.store, thread.ID, "asst:0", 0, "reply 0")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertAssistantTextItem(t, app.store, thread.ID, "asst:1", 1, "reply 1")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")

	var reverted []UserMessageRevertedEvent
	app.testEmitHook = func(name string, data any) {
		if name != "user_message:reverted" {
			return
		}
		ev, ok := data.(UserMessageRevertedEvent)
		if !ok {
			t.Fatalf("user_message:reverted payload = %T, want UserMessageRevertedEvent", data)
		}
		reverted = append(reverted, ev)
	}

	if err := app.RevertConversationToMessage(thread.ID, "user:1", false); err != nil {
		t.Fatalf("revert: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "asst:0" {
		t.Fatalf("items after revert = %+v, want [user:0 asst:0]", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "second" {
		t.Fatalf("draft = %+v ok=%v, want content=second", draft, ok)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("session ref = %q, want recovered fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef, []string{"first"}, []string{"second"})

	if len(reverted) != 1 {
		t.Fatalf("user_message:reverted fired %d times, want 1", len(reverted))
	}
	if reverted[0].ThreadID != thread.ID || reverted[0].UserItemID != "user:1" || reverted[0].TurnIndex != 1 {
		t.Fatalf("reverted event = %+v, want thread=%s item=user:1 turn=1", reverted[0], thread.ID)
	}
}

// TestRevertConversationToMessageRejectsActiveTurn guards the
// defense-in-depth check: an in-place revert must not truncate a
// timeline the provider is still writing to. The live-turn un-send
// (InterruptAndRevertIfClean) owns that case and interrupts first.
func TestRevertConversationToMessageRejectsActiveTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-active", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	// An incomplete turn row (completed_at NULL) is what GetActiveTurn
	// treats as live.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert active turn: %v", err)
	}

	emitted := false
	app.testEmitHook = func(name string, _ any) {
		if name == "user_message:reverted" {
			emitted = true
		}
	}

	err := app.RevertConversationToMessage(thread.ID, "user:1", false)
	if err == nil {
		t.Fatal("revert succeeded during an active turn, want rejection")
	}
	if !strings.Contains(err.Error(), "turn is in progress") {
		t.Fatalf("error = %q, want active-turn rejection", err)
	}
	if emitted {
		t.Fatal("user_message:reverted fired despite the rejection")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after rejected revert = %+v, want conversation preserved", items)
	}
}

// TestRevertConversationToMessageRejectsNonUserTargets makes the
// message-only contract structural: an assistant row and a wire-only
// user row (a provider-injected envelope the user never composed) are
// both refused rather than silently reverting to the wrong point.
func TestRevertConversationToMessageRejectsNonUserTargets(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-kind", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, app.store, thread.ID, "asst:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, thread.ID, "wire:0", 1, "injected", `{"wire_only":true}`)

	for _, tc := range []struct{ name, itemID string }{
		{"assistant row", "asst:0"},
		{"wire-only user row", "wire:0"},
		{"missing row", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := app.RevertConversationToMessage(thread.ID, tc.itemID, false)
			if err == nil {
				t.Fatalf("revert to %s succeeded, want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), "is not a user message") {
				t.Fatalf("error = %q, want not-a-user-message", err)
			}
		})
	}
}

// TestRevertConversationToMessageRejectsClaudeTUI makes the provider
// contract structural instead of relying on the UI never wiring the
// button: rollbackConversationLocked's claude-tui branch assumes a
// native Esc-revert was already delivered for a live turn, which this
// idle-thread path can never satisfy — accepting the call would
// truncate AO's history cache while the live TUI keeps the full
// conversation.
func TestRevertConversationToMessageRejectsClaudeTUI(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-tui", "claude-tui", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")

	emitted := false
	app.testEmitHook = func(name string, _ any) {
		if name == "user_message:reverted" {
			emitted = true
		}
	}

	err := app.RevertConversationToMessage(thread.ID, "user:1", false)
	if err == nil {
		t.Fatal("revert succeeded on a claude-tui thread, want rejection")
	}
	if !strings.Contains(err.Error(), "does not support in-place revert") {
		t.Fatalf("error = %q, want claude-tui rejection", err)
	}
	if emitted {
		t.Fatal("user_message:reverted fired despite the rejection")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after rejected revert = %+v, want conversation preserved", items)
	}
}

// TestRevertConversationToMessageRefusesPendingSend covers the echo gap
// GetActiveTurn cannot see: a dispatched send registers its pending-send
// marker under the thread lock before the stdin write, but the `turns`
// row only lands when the provider's turn-start echo arrives. A revert
// in that window would kill the in-flight send and truncate its
// just-persisted message, so the marker must refuse it.
func TestRevertConversationToMessageRefusesPendingSend(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := createAppTestThread(t, app, "t-pending", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	app.triage.RegisterPendingSendExpecting(thread.ID, "user:1", 1, "")

	emitted := false
	app.testEmitHook = func(name string, _ any) {
		if name == "user_message:reverted" {
			emitted = true
		}
	}

	err := app.RevertConversationToMessage(thread.ID, "user:0", false)
	if err == nil {
		t.Fatal("revert succeeded with a send awaiting its provider echo, want rejection")
	}
	if !strings.Contains(err.Error(), "awaiting provider confirmation") {
		t.Fatalf("error = %q, want pending-send rejection", err)
	}
	if emitted {
		t.Fatal("user_message:reverted fired despite the rejection")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after rejected revert = %+v, want conversation preserved", items)
	}
}

// TestRevertConversationToMessageRefusesUnconfirmedBackgroundKill makes
// the consent gate structural: with background work running, the revert
// is refused unless the caller explicitly passed
// killRunningBackgroundTasks — regardless of what the frontend
// preflight saw (a task can start between the preflight and the RPC).
func TestRevertConversationToMessageRefusesUnconfirmedBackgroundKill(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createAppTestThread(t, app, "t-bg-refuse", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)

	emitted := false
	app.testEmitHook = func(name string, _ any) {
		if name == "user_message:reverted" {
			emitted = true
		}
	}

	err := app.RevertConversationToMessage(thread.ID, "user:1", false)
	if err == nil {
		t.Fatal("revert succeeded with running background tasks and no consent, want refusal")
	}
	if !strings.Contains(err.Error(), "running background tasks must be killed") {
		t.Fatalf("error = %q, want background-task refusal", err)
	}
	if emitted {
		t.Fatal("user_message:reverted fired despite the refusal")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items after refused revert = %+v, want conversation preserved", items)
	}
}

// TestRevertConversationToMessageConfirmedKillClearsBackgroundRows
// drives the confirmed path: a running background tool call on a turn
// BEFORE the revert point (so its row survives the truncation) is
// flipped to inactive during the rollback — dead work must not survive
// as a stale running spinner — and the tray-change event fires.
func TestRevertConversationToMessageConfirmedKillClearsBackgroundRows(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
`)
	thread := createAppTestThread(t, app, "t-bg-confirm", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")

	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 1 {
		t.Fatalf("precondition: running background tasks = %d (%v), want 1", count, err)
	}

	trayChanged := false
	revertedFired := false
	app.testEmitHook = func(name string, _ any) {
		switch name {
		case "provider:background_tasks_changed":
			trayChanged = true
		case "user_message:reverted":
			revertedFired = true
		}
	}

	if err := app.RevertConversationToMessage(thread.ID, "user:1", true); err != nil {
		t.Fatalf("confirmed revert: %v", err)
	}

	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("running background tasks after confirmed revert = %d (%v), want 0", count, err)
	}
	if !trayChanged {
		t.Fatal("provider:background_tasks_changed did not fire")
	}
	if !revertedFired {
		t.Fatal("user_message:reverted did not fire")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "bg:0" {
		t.Fatalf("items after revert = %+v, want [user:0 bg:0]", items)
	}
}

// TestRevertConversationToMessageMidTurnAnchorEmitsKeptSet drives a
// Claude revert to a MID-TURN user message (a steered prompt at
// item_index > 0). The item-granular cut keeps the turn's prefix in
// SQLite, and the `user_message:reverted` event must carry exactly that
// kept-set so the frontend trims to match instead of hiding rows the
// backend kept (the over-trim bug this field exists to prevent).
func TestRevertConversationToMessageMidTurnAnchorEmitsKeptSet(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	const sessionID = "source-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"steer"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := createAppTestThread(t, app, "t-midturn", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, row := range []store.Item{
		{ID: "user:0", Kind: "user_text", Role: "user", ItemIndex: 0, Summary: "first"},
		{ID: "asst:0", Kind: "assistant_text", Role: "assistant", ItemIndex: 1, Summary: "reply 0"},
		{ID: "user:steer", Kind: "user_text", Role: "user", ItemIndex: 2, Summary: "steer"},
		{ID: "asst:1", Kind: "assistant_text", Role: "assistant", ItemIndex: 3, Summary: "reply 1"},
	} {
		row.ThreadID = thread.ID
		row.TurnIndex = 0
		row.Status = "completed"
		row.CreatedAt = now
		row.UpdatedAt = now
		if _, err := app.store.AppendItem(row); err != nil {
			t.Fatalf("append %s: %v", row.ID, err)
		}
	}
	seedMessageAnchor(t, app.store, thread.ID, "user:steer", 0, "u1", "")

	var reverted []UserMessageRevertedEvent
	app.testEmitHook = func(name string, data any) {
		if name != "user_message:reverted" {
			return
		}
		reverted = append(reverted, data.(UserMessageRevertedEvent))
	}

	if err := app.RevertConversationToMessage(thread.ID, "user:steer", false); err != nil {
		t.Fatalf("revert: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "asst:0" {
		t.Fatalf("items after mid-turn revert = %+v, want [user:0 asst:0]", items)
	}
	if len(reverted) != 1 {
		t.Fatalf("user_message:reverted fired %d times, want 1", len(reverted))
	}
	kept := reverted[0].KeptAnchorTurnItemIDs
	if len(kept) != 2 || kept[0] != "user:0" || kept[1] != "asst:0" {
		t.Fatalf("event kept-set = %v, want [user:0 asst:0]", kept)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok || draft.Content != "steer" {
		t.Fatalf("draft = %+v ok=%v err=%v, want content=steer", draft, ok, err)
	}
	assertClaudeSessionText(t, workspace, mustGetThread(t, app, thread.ID).SessionRef,
		[]string{"first"}, []string{"steer"})
}

func mustGetThread(t *testing.T, app *App, threadID string) store.Thread {
	t.Helper()
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	return thread
}

func TestRevertConversationToMessageValidatesArgs(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

	if err := app.RevertConversationToMessage("", "user:1", false); err == nil ||
		!strings.Contains(err.Error(), "thread id is required") {
		t.Fatalf("empty thread id error = %v, want required", err)
	}
	if err := app.RevertConversationToMessage("t1", "  ", false); err == nil ||
		!strings.Contains(err.Error(), "user item id is required") {
		t.Fatalf("blank item id error = %v, want required", err)
	}
}
