package app

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// appendTurnRow appends one row of the given kind to a thread turn.
func appendTurnRow(t *testing.T, st *store.Store, row store.Item) {
	t.Helper()
	now := time.Now().UnixMilli()
	if row.Status == "" {
		row.Status = "completed"
	}
	if row.Role == "" {
		row.Role = "assistant"
	}
	row.CreatedAt = now
	row.UpdatedAt = now
	if _, err := st.AppendItem(row); err != nil {
		t.Fatalf("append %s %s: %v", row.Kind, row.ID, err)
	}
}

func insertSettledTurn(t *testing.T, st *store.Store, threadID string, turnIndex int, stopReason string) {
	t.Helper()
	turnID := threadID + "-turn"
	now := time.Now().UnixMilli()
	if err := st.InsertTurn(store.Turn{TurnID: turnID, ThreadID: threadID, TurnIndex: turnIndex, StartedAt: now}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := st.UpdateTurnCompleted(turnID, now, stopReason, "", "", ""); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
}

// Only the model's unfinished reasoning and request-level retry/error rows
// may share a turn with the message an early Stop un-sends. Anything else in
// the turn is content the provider conversation already holds, and a cut
// would destroy it.
func TestUnsendPredicateAcceptsOnlyCompanionKinds(t *testing.T) {
	cases := []struct {
		name     string
		row      store.Item
		eligible bool
	}{
		{name: "thinking", row: store.Item{Kind: "thinking"}, eligible: true},
		{name: "api_retry", row: store.Item{Kind: "api_retry", Role: "system"}, eligible: true},
		{name: "api_error", row: store.Item{Kind: "api_error"}, eligible: true},
		{name: "error", row: store.Item{Kind: "error", Role: "system"}, eligible: true},
		{name: "assistant_text", row: store.Item{Kind: "assistant_text"}},
		{name: "tool_call", row: store.Item{Kind: "tool_call", Status: "running"}},
		{name: "tool_completion", row: store.Item{Kind: "tool_completion", IsBackground: true}},
		{name: "notification", row: store.Item{Kind: "notification", Role: "system"}},
		{name: "compaction", row: store.Item{Kind: "compaction", Role: "system"}},
		{name: "compaction_reasoning", row: store.Item{Kind: "compaction_reasoning"}},
		{name: "command_result", row: store.Item{Kind: "command_result"}},
		{name: "terminal_interaction", row: store.Item{Kind: "terminal_interaction"}},
		{name: "wire-only user_text", row: store.Item{Kind: "user_text", Role: "user", Meta: `{"wire_only":true}`}},
		{name: "subagent prompt", row: store.Item{Kind: "user_text", Role: "user", ParentID: "task:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			thread := createAppTestThread(t, app, "unsend-kind", "claude", t.TempDir())
			insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
			tc.row.ID = "row:1"
			tc.row.ThreadID = thread.ID
			tc.row.ItemIndex = 1
			appendTurnRow(t, app.store, tc.row)

			ok, userItem, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if ok != tc.eligible {
				t.Fatalf("eligible = %v (reason %q), want %v", ok, reason, tc.eligible)
			}
			if tc.eligible && userItem.ID != "u:0" {
				t.Fatalf("user item = %q, want u:0", userItem.ID)
			}
			if !tc.eligible && reason != "turn holds "+tc.row.Kind {
				t.Fatalf("reason = %q, want %q", reason, "turn holds "+tc.row.Kind)
			}
		})
	}
}

// A message is only undoable while its turn has never settled. After a
// round of the turn completes (a plain Stop, a finished reply), a later
// round on the same turn index does not make it undoable again.
func TestUnsendPredicateRequiresAnUnsettledTurn(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "unsend-settled", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	turnID := thread.ID + "-turn"
	if err := app.store.InsertTurn(store.Turn{TurnID: turnID, ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	if ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID); err != nil || !ok {
		t.Fatalf("open turn: eligible = %v reason = %q err = %v, want eligible", ok, reason, err)
	}

	if err := app.store.UpdateTurnCompleted(turnID, time.Now().UnixMilli(), "interrupted", "", "", ""); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok || reason != "turn already settled" {
		t.Fatalf("settled turn: eligible = %v reason = %q, want declined with \"turn already settled\"", ok, reason)
	}
}

// The reported sequence: a background command outlives turn 0, the user
// sends turn 1 and presses Stop (a plain interrupt, because the command is
// still running), the command finishes, and Claude answers the task
// notification inside turn 1 with only thinking so far. A second Stop must
// interrupt, not un-send the message and delete the command's completion.
func TestStopAfterBackgroundCompletionReRoundKeepsMessage(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := createAppTestThread(t, app, "unsend-reround", "claude", t.TempDir())
	thread.SessionRef = "live-session"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "run the tests")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "while we wait")
	insertSettledTurn(t, app.store, thread.ID, 1, "interrupted")
	appendTurnRow(t, app.store, store.Item{ID: "think:1", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 1, Kind: "thinking"})
	appendTurnRow(t, app.store, store.Item{ID: "complete:bg:0", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 2, Kind: "tool_completion", IsBackground: true, CompletionOf: "bg:0"})
	appendTurnRow(t, app.store, store.Item{ID: "task-notification:t0:n0", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 3, Kind: "notification", Role: "system"})
	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("precondition: running background tasks = %d (%v), want 0", count, err)
	}
	stops := 0
	app.stopSessionFn = func(string) error { stops++; return nil }

	result, err := app.InterruptAndRevertIfClean(thread.ID, InterruptRevertOptions{})
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if result.Reverted {
		t.Fatal("Stop un-sent a message whose turn had already settled")
	}
	if stops != 0 {
		t.Fatalf("session stopped %d times, want a plain interrupt", stops)
	}
	items, err := app.store.ListTurnItems(thread.ID, 1)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	want := []string{"u:1", "think:1", "complete:bg:0", "task-notification:t0:n0"}
	if !slices.Equal(ids, want) {
		t.Fatalf("turn 1 items = %v, want %v", ids, want)
	}
	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("running background tasks after Stop = %d (%v), want 0", count, err)
	}
	if _, found, err := app.store.GetThreadDraft(thread.ID); err != nil || found {
		t.Fatalf("draft found = %v (%v), want none", found, err)
	}
	if got := mustGetThread(t, app, thread.ID).SessionRef; got != "live-session" {
		t.Fatalf("SessionRef = %q, want live-session", got)
	}
}

// Headless Claude re-reads the turn after its session stops. Content that
// landed after the first read (here the reply's first text) declines the
// un-send, keeps the message, and clears the revert marker so the turn's
// completion is not reported as a revert.
func TestClaudeUnsendRechecksTurnAfterStoppingSession(t *testing.T) {
	app := newTestApp(t)
	var completions []triage.TurnCompletedEvent
	app.triage = triage.NewRouter(app.store, app.emit)
	app.testEmitHook = func(name string, data any) {
		if evt, ok := data.(triage.TurnCompletedEvent); ok && name == "provider:turn_completed" {
			completions = append(completions, evt)
		}
	}
	thread := createAppTestThread(t, app, "unsend-recheck", "claude", t.TempDir())
	thread.SessionRef = "live-session"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnID:    "turn-0",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	app.stopSessionFn = func(string) error {
		appendTurnRow(t, app.store, store.Item{ID: "a:0", ThreadID: thread.ID, TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Summary: "Sure"})
		return nil
	}

	result, err := app.InterruptAndRevertIfClean(thread.ID, InterruptRevertOptions{})
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if result.Reverted || result.Reason != "turn holds assistant_text" {
		t.Fatalf("result = %+v, want declined with \"turn holds assistant_text\"", result)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "u:0"); err != nil || !found {
		t.Fatalf("user message found = %v (%v), want kept", found, err)
	}
	if _, found, err := app.store.GetThreadDraft(thread.ID); err != nil || found {
		t.Fatalf("draft found = %v (%v), want none", found, err)
	}
	if got := mustGetThread(t, app, thread.ID).SessionRef; got != "live-session" {
		t.Fatalf("SessionRef = %q, want live-session", got)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     thread.ID,
		TurnID:       "turn-0",
		TurnIndex:    0,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	if len(completions) != 1 || completions[0].RevertedUserMessage {
		t.Fatalf("completions = %+v, want one completion not marked reverted", completions)
	}
}

// Editing an earlier message cuts a later turn that holds the completion of
// a background command launched before the cut. The launch must not come
// back as running: the session that owned it is gone.
func TestRevertAndResendSettlesLaunchWhoseCompletionWasCut(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-revived")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 9)
	appendTurnRow(t, app.store, store.Item{ID: "complete:bg:0", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 5, Kind: "tool_completion", IsBackground: true, CompletionOf: "bg:0"})
	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("precondition: running background tasks = %d (%v), want 0", count, err)
	}

	if err := revertAndResendForTest(app, context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("running background tasks after revert = %d (%v), want 0", count, err)
	}
	completion := settledCompletionFor(t, app, thread.ID, "bg:0")
	if completion.TurnIndex != 0 {
		t.Fatalf("settle sibling turn = %d, want the last kept turn 0", completion.TurnIndex)
	}
}

// When the cut keeps part of the anchor turn, the settle sibling lands in
// that surviving turn, so the kept set sent to clients must name it or
// clients drop it as a pre-cut row.
func TestRevertAndResendKeptSetIncludesSettledSibling(t *testing.T) {
	app, bus := newResendTestApp(t)
	workspace := t.TempDir()
	writeClaudeProjectSession(t, os.Getenv("HOME"), workspace, resendSourceSessionID,
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"steer"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := e2eThread("t-resend-kept-settle", string(provider.Claude), workspace)
	thread.SessionRef = resendSourceSessionID
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 1)
	for _, row := range []store.Item{
		{ID: "asst:0", Kind: "assistant_text", ItemIndex: 2, Summary: "reply 0"},
		{ID: "user:steer", Kind: "user_text", Role: "user", ItemIndex: 3, Summary: "steer"},
		{ID: "complete:bg:0", Kind: "tool_completion", ItemIndex: 4, IsBackground: true, CompletionOf: "bg:0"},
		{ID: "asst:1", Kind: "assistant_text", ItemIndex: 5, Summary: "reply 1"},
	} {
		row.ThreadID = thread.ID
		appendTurnRow(t, app.store, row)
	}
	seedMessageAnchor(t, app.store, thread.ID, "user:steer", 0, "u1", "")

	if err := revertAndResendForTest(app, context.Background(), thread.ID, "user:steer", RevertAndResendOptions{Content: "rewritten steer"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	completion := settledCompletionFor(t, app, thread.ID, "bg:0")
	_, ev := findRevertedEvent(t, bus)
	want := []string{"user:0", "bg:0", "asst:0", completion.ID}
	if !slices.Equal(ev.KeptAnchorTurnItemIDs, want) {
		t.Fatalf("event kept-set = %v, want %v", ev.KeptAnchorTurnItemIDs, want)
	}
}

// settledCompletionFor returns the single completion sibling of launchID,
// failing unless it is the session-end settle's.
func settledCompletionFor(t *testing.T, app *App, threadID, launchID string) store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var found []store.Item
	for _, item := range items {
		if item.CompletionOf == launchID {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		t.Fatalf("completion siblings of %s = %+v, want exactly one", launchID, found)
	}
	if !strings.Contains(found[0].Meta, "session_died") {
		t.Fatalf("completion of %s is not the session-end settle: %+v", launchID, found[0])
	}
	return found[0]
}
