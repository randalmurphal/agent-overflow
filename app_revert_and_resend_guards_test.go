package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// The guards below all reject BEFORE the saga touches anything: no draft
// is staged, no rollback runs, no send dispatches. They therefore run on
// the light newTestApp fixture, which has no provider mock: a call that
// slipped past a guard would reach sendMessageLocked and spawn the
// poisoned binary newTestApp installs, failing the test from that
// fixture's spawn check rather than passing quietly — and never
// reaching a real CLI or the developer's provider homes.

func TestRevertAndResendValidatesArgs(t *testing.T) {
	app := newTestApp(t)

	if err := app.RevertConversationAndResendMessage("", "user:1", RevertAndResendOptions{Content: "edited"}); err == nil ||
		!strings.Contains(err.Error(), "thread id is required") {
		t.Fatalf("empty thread id error = %v, want required", err)
	}
	if err := app.RevertConversationAndResendMessage("t1", "  ", RevertAndResendOptions{Content: "edited"}); err == nil ||
		!strings.Contains(err.Error(), "user item id is required") {
		t.Fatalf("blank item id error = %v, want required", err)
	}
	// An edit-resend with nothing to resend is a caller bug: this method
	// replaces a message, so a blank replacement has no meaning.
	if err := app.RevertConversationAndResendMessage("t1", "user:1", RevertAndResendOptions{Content: "  \n\t "}); err == nil ||
		!strings.Contains(err.Error(), "content is required") {
		t.Fatalf("blank content error = %v, want required", err)
	}
}

// TestRevertAndResendRefusesDuringShutdown pins the teardown guard.
// Shutdown stops provider sessions and closes the store underneath
// whatever is running, so a saga admitted here could truncate the
// timeline and then find no store to resend or settle the draft
// through. The typed sentinel is part of the contract: callers
// distinguish "the app is going away" from a rejection they could
// retry.
func TestRevertAndResendRefusesDuringShutdown(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "t-shutdown", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	app.shuttingDown.Store(true)

	err := assertRevertAndResendRejected(t, app, thread.ID, "user:1", false, "shutting down", 2)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("error = %v, want ErrShuttingDown so callers can tell teardown from a retryable guard", err)
	}
}

// TestRevertAndResendRejectsActiveTurn guards the defense-in-depth
// check: an in-place revert must not truncate a timeline the provider is
// still writing to. The live-turn un-send (InterruptAndRevertIfClean)
// owns that case and lets Codex thread/revert perform shutdown and cut
// together.
func TestRevertAndResendRejectsActiveTurn(t *testing.T) {
	app := newTestApp(t)
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

	assertRevertAndResendRejected(t, app, thread.ID, "user:1", false, "turn is in progress", 2)
}

// TestRevertAndResendRejectsNonUserTargets makes the message-only
// contract structural: an assistant row and a wire-only user row (a
// provider-injected envelope the user never composed) are both refused
// rather than silently reverting to the wrong point.
func TestRevertAndResendRejectsNonUserTargets(t *testing.T) {
	app := newTestApp(t)
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
			assertRevertAndResendRejected(t, app, thread.ID, tc.itemID, false, "is not a user message", 3)
		})
	}
}

// TestRevertAndResendRejectsClaudeTUI makes the provider contract
// structural instead of relying on the UI never wiring the button:
// rollbackConversationLocked's claude-tui branch assumes a native
// Esc-revert was already delivered for a live turn, which this
// idle-thread path can never satisfy — accepting the call would truncate
// AO's history cache while the live TUI keeps the full conversation.
func TestRevertAndResendRejectsClaudeTUI(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "t-tui", "claude-tui", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")

	assertRevertAndResendRejected(t, app, thread.ID, "user:1", false, "does not support in-place revert", 2)
}

// TestRevertAndResendRejectsWorkflowThread makes the workflow exclusion
// structural. A send into a workflow thread has to detach the run first,
// and that preparation re-acquires this thread's action lock — which
// this saga holds across the whole sequence. Reaching the send tail on a
// workflow thread must fail loudly, never half-run the takeover.
func TestRevertAndResendRejectsWorkflowThread(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "t-workflow", "claude", t.TempDir())
	thread.Mode = threadmode.ModeWorkflow
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread mode: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")

	assertRevertAndResendRejected(t, app, thread.ID, "user:1", false, "workflow threads cannot", 2)
}

// TestRevertAndResendRefusesPendingSend covers the echo gap
// GetActiveTurn cannot see: a dispatched send registers its pending-send
// marker under the thread lock before the stdin write, but the `turns`
// row only lands when the provider's turn-start echo arrives. A revert
// in that window would kill the in-flight send and truncate its
// just-persisted message, so the marker must refuse it.
// The marker is thread-scoped, not item-scoped, so BOTH targets are
// refused: an earlier message (whose revert would truncate the
// in-flight one away) and the in-flight message itself (whose revert
// would kill the send it is the row for). The release side of this
// transition — the marker resolving and the same revert then
// succeeding — is
// TestRevertAndResendProceedsOncePendingSendResolves.
func TestRevertAndResendRefusesPendingSend(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"earlier message", "user:0"},
		{"the in-flight message itself", "user:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
			thread := createAppTestThread(t, app, "t-pending", "claude", t.TempDir())
			insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
			insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
			app.triage.RegisterPendingSendWithExpectation(thread.ID, "user:1", 1, triage.PendingSendExpectation{ProviderItemID: ""})

			assertRevertAndResendRejected(t, app, thread.ID, tc.target, false, "awaiting provider confirmation", 2)
		})
	}
}

// TestRevertAndResendRefusesUnconfirmedBackgroundKill makes the consent
// gate structural: with background work running, the revert is refused
// unless the caller explicitly passed killRunningBackgroundTasks —
// regardless of what the frontend preflight saw (a task can start
// between the preflight and the RPC).
func TestRevertAndResendRefusesUnconfirmedBackgroundKill(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "t-bg-refuse", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)

	assertRevertAndResendRejected(t, app, thread.ID, "user:1", false, "running background tasks must be killed", 3)
}

// assertRevertAndResendRejected drives a rejected call and asserts the
// three properties every guard shares: the wanted error, no
// `user_message:reverted` emission, and a conversation left exactly as
// it was. It also asserts the draft row was never staged — a guard that
// let the crash copy land would leave the user's composer holding text
// they never sent. The rejection is returned so a caller can assert on
// its identity as well as its text.
func assertRevertAndResendRejected(
	t *testing.T, app *App, threadID, userItemID string, killBackground bool, wantErr string, wantItems int,
) error {
	t.Helper()
	emitted := false
	app.testEmitHook = func(name string, _ any) {
		if name == "user_message:reverted" {
			emitted = true
		}
	}
	defer func() { app.testEmitHook = nil }()

	rejection := app.RevertConversationAndResendMessage(threadID, userItemID, RevertAndResendOptions{
		Content:                    "edited replacement",
		KillRunningBackgroundTasks: killBackground,
	})
	if rejection == nil {
		t.Fatalf("revert and resend succeeded, want rejection containing %q", wantErr)
	}
	if !strings.Contains(rejection.Error(), wantErr) {
		t.Fatalf("error = %q, want it to contain %q", rejection, wantErr)
	}
	if emitted {
		t.Fatal("user_message:reverted fired despite the rejection")
	}
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != wantItems {
		t.Fatalf("items after rejected revert = %+v, want %d preserved", items, wantItems)
	}
	if _, ok, err := app.store.GetThreadDraft(threadID); err != nil {
		t.Fatalf("get draft: %v", err)
	} else if ok {
		t.Fatal("a rejected revert staged the edited message into the composer draft")
	}
	return rejection
}
