package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The tests in this file pin the turn model for work the Claude CLI does
// with no AO message behind it: every CLI turn is a logical turn of its
// own, a turn settles at the CLI's `result`, and nothing writes into a
// settled turn (docs/architecture/turn-lifecycle.md §Turns the provider
// starts).

// assertNoStreamingRows fails when any row of the thread is still
// streaming or running.
func assertNoStreamingRows(t *testing.T, st *store.Store, threadID string) {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.Status == statusStreaming || it.Status == "running" {
			t.Errorf("row %s (turn %d, %s) left %s", it.ID, it.TurnIndex, it.Kind, it.Status)
		}
	}
}

// settleTurnZero runs turn 0 to its result.
func settleTurnZero(t *testing.T, router *Router) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
}

// TestSelfWakeStreamKeepsItsTurnWhenASendPersistsLater reproduces thread
// 774f2b92 turn 278: the CLI woke itself after a turn settled, and while
// its thinking streamed an AO send persisted a user row at the next turn.
// The stream's turn was read from the newest stored row, so the next
// delta opened a second thinking row in the send's turn and the first
// never settled; the pane withheld everything behind it. The wake's turn
// is now the open turn, so the whole block stays one row in it.
func TestSelfWakeStreamKeepsItsTurnWhenASendPersistsLater(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("self-wake init: %v", err)
	}
	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("thinking block start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", Content: "Batch 5 is green; ", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking delta 1: %v", err)
	}

	// A send persists its row at the next free turn mid-stream.
	next, err := st.NextTurnIndex("t1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:direct", ThreadID: "t1", TurnIndex: next, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "Oracle lane: reviewer B traced it", CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist direct send row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:direct", next, PendingSendExpectation{ProviderItemID: "ao-direct"})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", Content: "check the xdist worker count.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking delta 2: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking block stop: %v", err)
	}
	router.WaitForPendingSettles()

	thinking := findItemsByKind(t, st, "t1", itemKindThinking)
	if len(thinking) != 1 {
		t.Fatalf("thinking rows = %+v, want one row for one block", thinking)
	}
	if thinking[0].TurnIndex != 1 {
		t.Errorf("thinking row turn = %d, want the wake's turn 1", thinking[0].TurnIndex)
	}
	if thinking[0].Status != statusCompleted {
		t.Errorf("thinking row status = %q, want completed", thinking[0].Status)
	}
	if next != 2 {
		t.Errorf("send's turn = %d, want 2 (after the wake's turns row)", next)
	}
	assertNoStreamingRows(t, st, "t1")
}

// TestStopCutsProviderStartedTurn: Stop during a self-woken turn cuts that
// turn. With the work filed under a settled turn, InterruptTurnIndex read
// -1 and the stop recorded nothing.
func TestStopCutsProviderStartedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("self-wake init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "wake-up work in progress", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wake text: %v", err)
	}

	sampled := router.InterruptTurnIndex("t1")
	if sampled != 1 {
		t.Fatalf("InterruptTurnIndex = %d, want the wake's turn 1", sampled)
	}
	tok := router.MarkFlushSendsInterrupted("t1", sampled)
	errID, err := router.MarkUserInterrupt("t1", sampled, tok)
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
	text, found, err := st.GetThreadItem("t1", "text:1:0")
	if err != nil || !found {
		t.Fatalf("wake text row missing: found=%v err=%v", found, err)
	}
	if text.Status != statusErrored || !isStopped(text.Summary) {
		t.Errorf("wake text after stop = (%s, %q), want errored and stopped", text.Status, text.Summary)
	}
}

// TestSoftCloseContinuationStaysInItsTurn: a ToolSearch or ExitPlanMode
// continuation ends the model's message with end_turn and resumes the same
// CLI turn with no init (thread a20f339e). The soft close settles the turn,
// so the thread reads idle; the continuation reopens the same turn, so it
// reads active again, a send's row persisted in between does not move it,
// and the result settles the turn again.
func TestSoftCloseContinuationStaysInItsTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("soft close: %v", err)
	}
	if active, found, err := st.GetActiveTurn("t1"); err != nil || found {
		t.Fatalf("GetActiveTurn after soft close = (%+v, %v, %v), want idle", active, found, err)
	}

	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "sent during the gap", CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist send row: %v", err)
	}

	emissions.reset()
	if err := router.Handle(parentContentBlockStart("t1", "thinking")); err != nil {
		t.Fatalf("continuation block start: %v", err)
	}
	starts := filterEmissions(emissions.snapshot(), "provider:turn_started")
	if len(starts) != 1 || starts[0].data.(TurnStartedEvent).TurnIndex != 0 {
		t.Fatalf("continuation turn_started = %+v, want one round on turn 0", starts)
	}
	if active, found, err := st.GetActiveTurn("t1"); err != nil || !found || active.TurnIndex != 0 {
		t.Fatalf("GetActiveTurn during the continuation = (%+v, %v, %v), want turn 0 active", active, found, err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", Content: "loading the deferred tool", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("continuation thinking: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("result: %v", err)
	}

	thinking := findItemsByKind(t, st, "t1", itemKindThinking)
	if len(thinking) != 1 || thinking[0].TurnIndex != 0 {
		t.Fatalf("thinking rows = %+v, want one row in turn 0", thinking)
	}
	turn, found, err := st.GetTurn("t1:0")
	if err != nil || !found || turn.CompletedAt == nil {
		t.Fatalf("turn 0 not settled by the result: found=%v row=%+v err=%v", found, turn, err)
	}
	assertNoStreamingRows(t, st, "t1")
}

// TestTurnStartSettlesOpenPredecessor: a turn start for an AO message
// while the previous turn is still open. The single result that follows
// settles only the new turn, so the predecessor settles at the start.
func TestTurnStartSettlesOpenPredecessor(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "turn 0 answer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 text: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "next question", CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist send row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("init for the send: %v", err)
	}

	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("OpenTurnIndex = %d, want 1", got)
	}
	prev, found, err := st.GetTurn("t1:0")
	if err != nil || !found || prev.CompletedAt == nil {
		t.Fatalf("turn 0 not settled when turn 1 started: found=%v row=%+v err=%v", found, prev, err)
	}
	if prev.StopReason != "end_turn" {
		t.Errorf("turn 0 stop_reason = %q, want end_turn", prev.StopReason)
	}
	text, found, err := st.GetThreadItem("t1", "text:0:0")
	if err != nil || !found || text.Status != statusCompleted {
		t.Errorf("turn 0 text = (%+v, %v, %v), want completed", text, found, err)
	}
}

// Parent content with no init after a settled turn, while a sent message
// waits for its turn, opens that message's turn as its init would.
func TestParentContentWithoutInitOpensPendingSendTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})
	if err := router.Handle(parentContentBlockStart("t1", "text")); err != nil {
		t.Fatal(err)
	}
	if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
		t.Fatalf("open turn = %d/%v, want the pending send's turn 1", open, ok)
	}
	pending, ok := router.turnOpeningPendingSend("t1")
	if ok {
		t.Fatalf("pending send %s at turn %d still waits for a turn", pending.AOItemID, pending.TurnIndex)
	}
}

func openerInit(t *testing.T, opener provider.TurnOpener) provider.ProviderEvent {
	t.Helper()
	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: opener})
	if err != nil {
		t.Fatal(err)
	}
	return provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Meta: meta, Timestamp: time.Now()}
}

func persistSendRow(t *testing.T, router *Router, id string, turnIndex int) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: id, ThreadID: "t1", TurnIndex: turnIndex, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "sent", CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist send row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", id, turnIndex, PendingSendExpectation{})
}

// A wake that arrives while a send is being placed waits for the turn
// allocation lock, then finds the registered send and takes its turn
// instead of opening a provider-started turn at the index the send chose.
func TestWakeDuringSendPlacementTakesTheSendsTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)

	unlock := router.LockTurnAllocation("t1")
	done := make(chan error, 1)
	go func() {
		done <- router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()})
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("wake init finished while a send held the allocation lock (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	persistSendRow(t, router, "user:1", 1)
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("wake init: %v", err)
	}

	if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
		t.Fatalf("open turn = %d/%v, want the send's turn 1", open, ok)
	}
	if pending, ok := router.turnOpeningPendingSend("t1"); ok {
		t.Fatalf("send %s at turn %d still waits for a turn: the wake opened another", pending.AOItemID, pending.TurnIndex)
	}
}

// An AO internal command's init opens no turn and leaves a message
// waiting for its turn unclaimed; the command's result settles no turn
// that has not started; the message's own init opens its turn.
func TestInternalCommandInitLeavesPendingSendUnclaimed(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)
	persistSendRow(t, router, "user:1", 1)

	if err := router.Handle(openerInit(t, provider.TurnOpenerInternalCommand)); err != nil {
		t.Fatalf("internal command init: %v", err)
	}
	if open, ok := router.openTurnIndex("t1"); ok {
		t.Fatalf("internal command opened turn %d", open)
	}
	if _, ok := router.turnOpeningPendingSend("t1"); !ok {
		t.Fatal("internal command init claimed the waiting send")
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("internal command result: %v", err)
	}
	if router.isTurnSettled("t1", 1) {
		t.Fatal("internal command result settled the send's turn before it started")
	}

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("send init: %v", err)
	}
	if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
		t.Fatalf("open turn = %d/%v, want the send's turn 1", open, ok)
	}
}

// A native command sent while the previous turn's result is withheld
// writes its launch row before its init. The row belongs to the command's
// turn, not to the soft-closed one.
func TestCommandLaunchAfterSoftCloseLandsInItsTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	persistSendRow(t, router, "user:1", 1)

	meta, _ := json.Marshal(map[string]any{"toolName": "Command", "input": map[string]any{"command": "/context"}})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "claude-command:c1", ItemType: "Command", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command launch: %v", err)
	}
	row, found, err := st.GetThreadItem("t1", "claude-command:c1")
	if err != nil || !found {
		t.Fatalf("command launch row: found=%v err=%v", found, err)
	}
	if row.TurnIndex != 1 {
		t.Fatalf("command launch on turn %d, want its own turn 1", row.TurnIndex)
	}
}

// After a resume, a completion that matches no turn of the new session (an
// AO internal command's result while a sent message waits for its turn)
// rewrites neither the previous session's finished turn nor the waiting
// message's.
func TestCompletionAfterResumeLeavesPreviousSessionTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)
	before, found, err := st.GetTurnByThreadIndex("t1", 0)
	if err != nil || !found || before.CompletedAt == nil {
		t.Fatalf("turn 0: found=%v err=%v %+v", found, err, before)
	}

	router.MarkThreadActive("t1")
	persistSendRow(t, router, "user:1", 1)
	if err := router.Handle(openerInit(t, provider.TurnOpenerInternalCommand)); err != nil {
		t.Fatalf("internal command init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1", Timestamp: time.Now().Add(time.Minute),
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", AssistantMessageID: "msg-internal"},
	}); err != nil {
		t.Fatalf("internal command result: %v", err)
	}

	after, _, err := st.GetTurnByThreadIndex("t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if *after.CompletedAt != *before.CompletedAt || after.AssistantMessageID != before.AssistantMessageID || after.StopReason != before.StopReason {
		t.Fatalf("previous session's turn 0 rewritten: before %+v after %+v", before, after)
	}
	if router.isTurnSettled("t1", 1) {
		t.Fatal("the completion settled the waiting message's turn")
	}
}

// On a thread's first send, a completion that arrives before the message's
// turn starts (an AO internal command's result) does not settle the turn
// the message is waiting for.
func TestCompletionBeforeFirstTurnLeavesWaitingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	persistSendRow(t, router, "user:0", 0)
	if err := router.Handle(openerInit(t, provider.TurnOpenerInternalCommand)); err != nil {
		t.Fatalf("internal command init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("internal command result: %v", err)
	}
	if router.isTurnSettled("t1", 0) {
		t.Fatal("the completion settled the waiting message's turn 0")
	}
}
