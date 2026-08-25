package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestEvaluateInterruptRevertPredicateNoItems covers the empty-thread
// branch — no items at all means there's nothing to revert.
func TestEvaluateInterruptRevertPredicateNoItems(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-empty", "claude", t.TempDir())

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "no items" {
		t.Fatalf("reason = %q, want \"no items\"", reason)
	}
}

// TestEvaluateInterruptRevertPredicateSingleUserItem is the happy path:
// the only item on the latest turn is a single user_text row, so the
// predicate matches and the user_item is returned.
func TestEvaluateInterruptRevertPredicateSingleUserItem(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-single", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")

	ok, userItem, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !ok {
		t.Fatalf("expected predicate true, got false (reason=%q)", reason)
	}
	if userItem.ID != "u:0" {
		t.Fatalf("user item id = %q, want u:0", userItem.ID)
	}
	if userItem.TurnIndex != 0 {
		t.Fatalf("turn index = %d, want 0", userItem.TurnIndex)
	}
}

// TestEvaluateInterruptRevertPredicateRejectsAssistantText guards the
// Claude-Code-parity rule: an assistant_text row in the latest turn
// means the agent has produced visible output and the revert would
// discard real work.
func TestEvaluateInterruptRevertPredicateRejectsAssistantText(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-asst", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "a:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "I am the agent",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "agent content present" {
		t.Fatalf("reason = %q, want \"agent content present\"", reason)
	}
}

// TestEvaluateInterruptRevertPredicateRejectsToolCall covers the same
// "agent has acted" gate for tool calls — even a started-but-incomplete
// tool call blocks the revert because the model already decided to act.
func TestEvaluateInterruptRevertPredicateRejectsToolCall(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-tool", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "tc:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "running",
		Summary:   "Read",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append tool_call: %v", err)
	}

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "agent content present" {
		t.Fatalf("reason = %q, want \"agent content present\"", reason)
	}
}

// TestEvaluateInterruptRevertPredicateAllowsThinking confirms that
// thinking rows DO NOT block the revert (matches Claude Code's
// messagesAfterAreOnlySynthetic — only assistant_text and tool_call
// count as "the agent has responded").
func TestEvaluateInterruptRevertPredicateAllowsThinking(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-think", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "think:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "thinking",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   "let me think...",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append thinking: %v", err)
	}

	ok, userItem, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !ok {
		t.Fatalf("expected predicate true, got false (reason=%q)", reason)
	}
	if userItem.ID != "u:0" {
		t.Fatalf("user item id = %q, want u:0", userItem.ID)
	}
}

// TestEvaluateInterruptRevertPredicateRejectsMultiUser covers the
// steered-turn case: multiple user_text rows on a single turn mean the
// user steered mid-round, and reverting one would break ordering.
func TestEvaluateInterruptRevertPredicateRejectsMultiUser(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-multi", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1", 0, "second")

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "turn has steered user messages" {
		t.Fatalf("reason = %q, want \"turn has steered user messages\"", reason)
	}
}

// TestEvaluateInterruptRevertPredicateRejectsQueuedFlush guards the
// "let the queue drain" rule: when triage has a pending flush item,
// Stop should let the queued follow-up reach the provider rather than
// discard everything via revert.
func TestEvaluateInterruptRevertPredicateRejectsQueuedFlush(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-queue", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{
		ID:      "queue:1",
		Message: "follow-up message",
	})

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "queued follow-up messages" {
		t.Fatalf("reason = %q, want \"queued follow-up messages\"", reason)
	}
}

func TestEvaluateInterruptRevertPredicateRejectsInflightFlushDispatch(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-dispatch", "codex", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.flushDispatch.mu.Lock()
	app.ensureFlushDispatchMapsLocked()
	app.flushDispatch.inflightItems[thread.ID] = 1
	app.flushDispatch.mu.Unlock()

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatalf("expected predicate false, got true")
	}
	if reason != "queued follow-up messages" {
		t.Fatalf("reason = %q, want \"queued follow-up messages\"", reason)
	}
}

// TestRegisterQueueItemSerializesInterruptRevertAcrossFlushHandoff is the
// regression guard for a TOCTOU race in the revert-on-interrupt predicate.
//
// A queued flush is tracked by exactly one of three signals at any moment:
// triage's queuedFlushItems (QueuedFlushItemCount), the app's
// a.flushDispatch.inflightItems counter, or the persisted SQLite row.
// tryFlushQueue hands off from the first to the second by deleting the item
// from the triage queue (under r.mu), releasing r.mu, and THEN invoking the
// dispatcher — and the inflight counter is only bumped inside that dispatcher
// (enqueueFlushDispatch). Between the queue delete and the bump, the item is
// invisible to both counters and not yet in SQLite. If InterruptAndRevertIfClean
// could run its predicate in that window it would see a turn with a lone
// user_text and no pending flush work, report it cleanly revertable, and
// DeleteConversationFromTurn the very turn the queued message was about to
// extend — discarding the prompt that started it.
//
// The fix makes App.RegisterQueueItem hold App.flushDispatch.handoffMu across the queue
// append and flush handoff. The revert predicate reads the queued / in-flight
// counters under the same mutex (pendingFlushWorkCount), so
// InterruptAndRevertIfClean cannot observe the window: its predicate read blocks
// until RegisterQueueItem releases, by which point the message is counted as
// in-flight and the predicate correctly refuses. This test interposes at the
// dispatcher to widen the handoff window, then proves a concurrent Stop blocks
// rather than reverts.
//
// Without the fix, the Stop returns immediately during the window (reverting
// P0); with it, the Stop blocks on the lock and then refuses.
func TestRegisterQueueItemSerializesInterruptRevertAcrossFlushHandoff(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "register-serialize", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "original prompt P0")

	// RegisterQueueItem only flushes immediately when a session is attached.
	// The stub carries no provider handle, so the eligible/refuse paths'
	// provider Interrupt calls are no-ops — this test exercises only the
	// predicate and the locking. Written before any goroutine starts.
	app.sessions[thread.ID] = session{provider: "claude"}

	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	inGap := make(chan struct{})
	release := make(chan struct{})
	// Interpose at the exact point tryFlushQueue invokes the dispatcher: the
	// item is already deleted from the triage queue, and the real
	// enqueueFlushDispatch (which would bump the inflight counter) has not run.
	// Signal that we're in the handoff window, block, then replicate the
	// inflight bump enqueueFlushDispatch would have done so the post-handoff
	// state matches production — the message visible via the inflight counter.
	app.triage.SetFlushDispatcher(func(threadID string, items []triage.QueuedFlushItem) {
		close(inGap)
		<-release
		app.flushDispatch.mu.Lock()
		app.ensureFlushDispatchMapsLocked()
		app.flushDispatch.inflightItems[threadID] += len(items)
		app.flushDispatch.mu.Unlock()
	})

	// Queue M. With the fix, RegisterQueueItem holds a.flushDispatch.handoffMu across the
	// flush, so this goroutine parks inside the interpose with the mutex held.
	registered := make(chan error, 1)
	go func() {
		_, err := app.RegisterQueueItem(thread.ID, "follow-up M", SendMessageOptions{})
		registered <- err
	}()

	select {
	case <-inGap:
	case <-time.After(5 * time.Second):
		t.Fatal("flush dispatcher never reached the handoff window")
	}

	// The user hits Stop while M is mid-handoff. InterruptAndRevertIfClean must
	// block on a.flushDispatch.handoffMu — which it acquires inside its revert-predicate
	// read while RegisterQueueItem holds it — so it must not observe the window.
	type iarOutcome struct {
		res InterruptAndRevertResult
		err error
	}
	result := make(chan iarOutcome, 1)
	go func() {
		res, err := app.InterruptAndRevertIfClean(thread.ID)
		result <- iarOutcome{res, err}
	}()

	select {
	case out := <-result:
		// Returned without us releasing the handoff: it observed the window
		// instead of serializing behind RegisterQueueItem's mutex. The bug.
		close(release)
		t.Fatalf("BUG: InterruptAndRevertIfClean ran during the flush handoff window "+
			"(Reverted=%v, err=%v); expected it to block on flushHandoffMu held by "+
			"RegisterQueueItem and never revert the turn-starting prompt", out.res.Reverted, out.err)
	case <-time.After(500 * time.Millisecond):
		// Correctly blocked on a.flushDispatch.handoffMu — the fix is serializing.
	}

	// Release the handoff. RegisterQueueItem records M in-flight, drops
	// a.flushDispatch.handoffMu, and the parked Stop proceeds — now seeing pending flush work.
	close(release)
	if err := <-registered; err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	out := <-result
	if out.err != nil {
		t.Fatalf("interrupt-and-revert: %v", out.err)
	}
	if out.res.Reverted {
		t.Fatalf("after handoff: expected refuse (M in flight), got Reverted=true; " +
			"the turn-starting prompt would have been discarded")
	}

	// Assert the invariant the race threatens, not just the Reverted==false
	// proxy for it: the turn-starting prompt P0 must still be in SQLite. (The
	// refuse path with a handle-less session mutates nothing, so this also
	// confirms no stray row was written for M.)
	items, err := app.store.ListTurnItems(thread.ID, 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("turn-starting prompt user:0 must survive the refused Stop; got %d items: %+v", len(items), items)
	}
}

func TestEvaluateInterruptRevertPredicateRejectsRunningBackgroundTasks(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-background", "claude", t.TempDir())
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 0)
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "hello")

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatal("expected predicate false, got true")
	}
	if reason != "running background tasks" {
		t.Fatalf("reason = %q, want \"running background tasks\"", reason)
	}
}

func TestEvaluateInterruptRevertPredicateRejectsStashedBackgroundTasks(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pred-stashed-background", "claude", t.TempDir())
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 0)
	if err := app.store.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
		ThreadID:  thread.ID,
		TaskID:    "task-bg-0",
		ToolUseID: "bg:0",
		Status:    "completed",
		Source:    "task_updated",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("upsert pending terminal: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "hello")

	ok, _, reason, err := app.evaluateInterruptRevertPredicate(thread.ID)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ok {
		t.Fatal("expected predicate false, got true")
	}
	if reason != "running background tasks" {
		t.Fatalf("reason = %q, want \"running background tasks\"", reason)
	}
}

// TestResolveMessageAnchorReturnsPersisted is the simple lookup case
// when the at-send record wrote an anchor keyed by the user item id
// and matching turn index.
func TestResolveMessageAnchorReturnsPersisted(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "rc-found", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	seedMessageAnchor(t, app.store, thread.ID, "u:0", 0, "provider-u-0", "")

	userItem, ok, err := app.store.GetThreadItem(thread.ID, "u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveMessageAnchor("test", thread.ID, userItem)
	// The provider id only exists on the persisted row (the item meta is
	// empty), so seeing it proves the lookup returned the stored anchor
	// rather than synthesizing.
	if got.ProviderUserMessageID != "provider-u-0" {
		t.Fatalf("anchor provider id = %q, want persisted provider-u-0", got.ProviderUserMessageID)
	}
	if got.TurnIndex != 0 {
		t.Fatalf("turn index = %d, want 0", got.TurnIndex)
	}
}

// TestResolveMessageAnchorSynthesizesWhenMissing covers the case where
// the at-send record didn't land. The provider rollback helpers only
// need TurnIndex plus whatever ids the item meta carries, so a minimal
// record is synthesized from the row.
func TestResolveMessageAnchorSynthesizesWhenMissing(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "rc-missing", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")

	userItem, ok, err := app.store.GetThreadItem(thread.ID, "u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveMessageAnchor("test", thread.ID, userItem)
	if got.UserItemID != "u:0" {
		t.Fatalf("synthesized userItemID = %q, want u:0", got.UserItemID)
	}
	if got.TurnIndex != 0 {
		t.Fatalf("synthesized turnIndex = %d, want 0", got.TurnIndex)
	}
	if got.ProviderUserMessageID != "" {
		t.Fatalf("synthesized provider id = %q, want empty (item meta carries none)", got.ProviderUserMessageID)
	}
}

// TestRunPlainInterruptLockedNoSessionIsNoOp asserts the
// "Stop on a stale thread" path: no active session means no work to do,
// no error returned. This is the fallback the InterruptAndRevertIfClean
// method takes when the predicate fails.
func TestRunPlainInterruptLockedNoSessionIsNoOp(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "no-session", "claude", t.TempDir())

	if err := app.runPlainInterruptLocked(thread.ID); err != nil {
		t.Fatalf("runPlainInterruptLocked returned err = %v, want nil", err)
	}
}

// TestInterruptAndRevertIfCleanRejectsEmptyThreadID is the input-validation
// gate. The frontend always passes a non-empty thread id, but the
// binding still must refuse empty values rather than silently no-op.
func TestInterruptAndRevertIfCleanRejectsEmptyThreadID(t *testing.T) {
	app := newTestApp(t)

	_, err := app.InterruptAndRevertIfClean("")
	if err == nil {
		t.Fatalf("expected error for empty thread id, got nil")
	}
}

// TestInterruptAndRevertIfCleanFallsBackWhenAssistantPresent covers
// the predicate-false race: the frontend's local predicate said the
// turn was clean (no agent output), but by the time the backend ran
// under the per-thread lock a streaming assistant_text row had landed.
// The method should NOT revert; it should fall through to the plain
// interrupt path and return Reverted=false with a populated reason.
func TestInterruptAndRevertIfCleanFallsBackWhenAssistantPresent(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "fallback", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:        "a:0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   "I am responding",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if result.Reverted {
		t.Fatalf("expected Reverted=false, got true")
	}
	if result.Reason != "agent content present" {
		t.Fatalf("Reason = %q, want \"agent content present\"", result.Reason)
	}
	// The user message must still be in SQLite — the fallback path
	// MUST NOT delete anything.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after fallback = %d, want 2", len(items))
	}
}

// TestInterruptAndRevertIfCleanRevertsClaudeFirstTurn is the happy
// path for a first-turn (TurnIndex==0) Claude revert. With turn 0 the
// Claude path just clears SessionRef + PendingForkRef on the thread
// row — no JSONL fork file needed. The conversation is truncated and
// the composer draft is repopulated from the reverted user item.
func TestInterruptAndRevertIfCleanRevertsClaudeFirstTurn(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := createAppTestThread(t, app, "revert-first", "claude", t.TempDir())
	// Even though TurnIndex==0 short-circuits in the Claude revert path,
	// SessionRef is set to confirm it gets cleared.
	thread.SessionRef = "stale-session"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "the original prompt")

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}
	if result.UserItemID != "u:0" {
		t.Fatalf("UserItemID = %q, want u:0", result.UserItemID)
	}
	if result.TurnIndex != 0 {
		t.Fatalf("TurnIndex = %d, want 0", result.TurnIndex)
	}
	if result.HistoryEpoch <= 0 || result.HistoryRev <= 0 {
		t.Fatalf("result history stamp = %d:%d, want a committed positive stamp", result.HistoryEpoch, result.HistoryRev)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after revert = %d, want 0", len(items))
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok {
		t.Fatalf("expected draft to be upserted")
	}
	if draft.Content != "the original prompt" {
		t.Fatalf("draft.Content = %q, want \"the original prompt\"", draft.Content)
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "" {
		t.Fatalf("SessionRef = %q, want empty after revert", updated.SessionRef)
	}
}

// A rollback receives the thread snapshot loaded when the interrupt began.
// Session slicing can take long enough for a narrow thread setting update to
// land before that snapshot is committed. Finishing the rollback must change
// only provider-resume state, never restore unrelated fields from its stale
// snapshot.
func TestInterruptRollbackPreservesConcurrentEffortChange(t *testing.T) {
	for _, providerName := range []string{"claude", "codex"} {
		t.Run(providerName, func(t *testing.T) {
			app := newTestApp(t)

			thread := createAppTestThread(t, app, "revert-preserves-effort-"+providerName, providerName, t.TempDir())
			thread.ReasoningEffort = "high"
			thread.SessionRef = "stale-session"
			if err := app.store.UpdateThread(thread); err != nil {
				t.Fatalf("seed thread: %v", err)
			}
			rollbackSnapshot, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatalf("load rollback snapshot: %v", err)
			}
			if err := app.store.UpdateReasoningEffort(thread.ID, "xhigh"); err != nil {
				t.Fatalf("update effort during rollback: %v", err)
			}

			anchor := store.MessageAnchor{ThreadID: thread.ID, UserItemID: "user:0", TurnIndex: 0}
			userItem := store.Item{
				ID: "user:0", ThreadID: thread.ID, TurnIndex: 0,
				Kind: "user_text", Role: "user",
			}
			switch providerName {
			case "claude":
				err = app.rollbackClaudeThreadToMessage(rollbackSnapshot, anchor, userItem)
			case "codex":
				err = app.rollbackCodexThreadToMessage(rollbackSnapshot, anchor)
			}
			if err != nil {
				t.Fatalf("finish rollback: %v", err)
			}

			updated, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatalf("reload thread: %v", err)
			}
			if updated.ReasoningEffort != "xhigh" {
				t.Fatalf("effort after rollback = %q, want xhigh", updated.ReasoningEffort)
			}
			if updated.SessionRef != "" {
				t.Fatalf("session ref after turn-zero rollback = %q, want empty", updated.SessionRef)
			}
		})
	}
}

// TestInterruptAndRevertIfCleanRevertsClaudeTUIWithoutKillingSession covers the
// claude-tui revert-on-interrupt path. The interactive TUI reverts the just-sent
// prompt natively on the Esc that InterruptAndRevertIfClean delivers (LIVE:
// spike/claude-mitm/probe_hook_escrevert.py), so AO mirrors it — truncate the
// turn + restore the draft — WITHOUT the headless-Claude steps of stopping the
// session and rewriting a session file (claude-tui owns its own conversation; AO
// has no fork file to write). Before the claude-tui branch existed this hit the
// generic else and failed with "unsupported provider claude-tui" from
// revertProviderConversationToMessage, killing the live session on the way.
func TestInterruptAndRevertIfCleanRevertsClaudeTUIWithoutKillingSession(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := createAppTestThread(t, app, "revert-tui", "claude-tui", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "the original prompt")

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}
	if result.UserItemID != "u:0" {
		t.Fatalf("UserItemID = %q, want u:0", result.UserItemID)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after revert = %d, want 0 (turn truncated to mirror the native revert)", len(items))
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "the original prompt" {
		t.Fatalf("draft = (ok=%v, %q), want (true, \"the original prompt\")", ok, draft.Content)
	}
}

// TestInterruptAndRevertIfCleanRevertsWithSynthesizedAnchor covers
// the "at-send record failed" path: no message-anchor row exists, the
// revert helper synthesizes one with just TurnIndex. The revert should
// still succeed because the Claude TurnIndex==0 path doesn't need any
// anchor metadata.
func TestInterruptAndRevertIfCleanRevertsWithSynthesizedAnchor(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := createAppTestThread(t, app, "revert-synth", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "synth prompt")
	// Deliberately do NOT seed a message anchor — the predicate is still
	// eligible because it only depends on items + queue, and the revert
	// helper synthesizes a record from the user item.

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after revert = %d, want 0", len(items))
	}
}

// TestInterruptAndRevertIfCleanCodexStopsSessionWithActiveTurn: the
// Esc auto-revert with a turn still in flight. The fork-at-turn revert
// does NOT wait for the interrupted turn to settle — it stops the
// session immediately, which flips the stopped-thread gate (invariant
// 29) so any straggler wire events from the aborted turn are dropped
// instead of landing on the truncated timeline. Reverting the first
// message needs no fork; SessionRef clears for a fresh thread on the
// next send.
func TestInterruptAndRevertIfCleanCodexStopsSessionWithActiveTurn(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-interrupt-revert", "codex", workspace)
	thread.SessionRef = "provider-codex-interrupt"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:         "turn-active",
		ProviderTurnID: "turn-active",
		ThreadID:       thread.ID,
		TurnIndex:      0,
		StartedAt:      time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert active turn: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "codex prompt")

	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-codex-interrupt",
		forkedThreadID:  "forked-provider-thread",
	})
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         binary,
		Model:          "test-model",
		WorkDir:        workspace,
		ResumeThreadID: thread.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex),
		token:    "codex-interrupt-token",
		codex:    sess,
	})

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}
	if result.UserItemID != "u:0" || result.TurnIndex != 0 {
		t.Fatalf("result = %+v, want reverted u:0 turn 0", result)
	}
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("Codex session still active after interrupt revert; revert must stop it")
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "" {
		t.Fatalf("SessionRef = %q, want cleared for turn-zero revert", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after revert = %+v, want empty", items)
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if !ok || draft.Content != "codex prompt" {
		t.Fatalf("draft = %+v ok=%v, want restored codex prompt", draft, ok)
	}
}

func TestInterruptAndRevertIfCleanCodexMarksCompletionDuringInterruptAsReverted(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, app.emit)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-interrupt-marker", "codex", workspace)
	thread.SessionRef = "provider-codex-marker"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "codex prompt")

	var completions []triage.TurnCompletedEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:turn_completed" {
			return
		}
		if evt, ok := data.(triage.TurnCompletedEvent); ok {
			completions = append(completions, evt)
		}
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnID:    "turn-active",
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	binary := writeCodexInterruptCompleteBinary(t, "provider-codex-marker", "turn-active")
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         binary,
		Model:          "test-model",
		WorkDir:        workspace,
		ResumeThreadID: thread.SessionRef,
	}, func(evt provider.ProviderEvent) {
		_ = app.triage.Handle(evt)
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	app.sessionManager().put(thread.ID, session{
		provider: string(provider.Codex),
		token:    "codex-marker-token",
		codex:    sess,
	})

	if _, err := app.InterruptAndRevertIfClean(thread.ID); err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if len(completions) == 0 {
		t.Fatal("expected provider:turn_completed emission")
	}
	if !completions[0].RevertedUserMessage {
		t.Fatalf("RevertedUserMessage = false, want true: %+v", completions[0])
	}
}

// TestInterruptAndRevertIfCleanSurvivesCompactBoundary pins the
// compact-boundary slice for interrupt-revert. The "synthesize an
// anchor when none was recorded" branch (`resolveMessageAnchor`)
// lifts `provider_item_id` off the user item's Meta so the
// synthesized record drives the same UUID-keyed slice.
//
// Scenario: 3 logical turns, /compact summary on disk between turn
// 0's assistant and turn 1's user prompt (the placement that
// triggers the ordinal off-by-N). User sends turn 2 and immediately
// hits Stop before any agent output. AO synthesizes an anchor
// (no at-send row exists). The revert must keep turn 1's
// full assistant response.
func TestInterruptAndRevertIfCleanSurvivesCompactBoundary(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "compact-interrupt-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"compact-interrupt-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"compact-interrupt-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"cs1","parentUuid":"a0","sessionId":"compact-interrupt-session","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}
{"type":"user","uuid":"u1","parentUuid":"cs1","sessionId":"compact-interrupt-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"compact-interrupt-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"compact-interrupt-session","message":{"role":"user","content":"third"}}
`)
	thread := createAppTestThread(t, app, "interrupt-compact", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "second")
	// u:2 carries provider_item_id=u2 — the wire-stamp the synthesize
	// path will read to populate the synthesized anchor.
	insertUserItemWithMeta(t, app.store, thread.ID, "u:2", 2, "third", `{"provider_item_id":"u2"}`)

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third"})
}

// TestInterruptAndRevertIfCleanSurvivesPriorInterruptMarker is the
// end-to-end regression guard for the bug the user hit on thread
// 3da09d16: send a message, hit Stop, and the revert lands one turn too
// early. The trigger conjunction is (a) a `[Request interrupted by
// user]` marker in kept history — Claude writes one whenever an earlier
// turn was interrupted — AND (b) the revert taking the ORDINAL fallback
// because no provider_item_id was stamped yet (the fast send→escape
// race: the wire echo that stamps the UUID hasn't arrived). With both,
// the ordinal walk used to count the interrupt marker as a real prompt
// and slice the resumed session a full turn too far back.
//
// Unlike TestInterruptAndRevertIfCleanSurvivesCompactBoundary above,
// this deliberately inserts the final user item with NO provider_item_id
// (plain insertUserItem) so writeClaudeSessionSlice's empty-anchor
// branch routes through WriteForkFileForLastKeptTurn — the ordinal path
// where the bug lived. The marker is written as ARRAY content, exactly
// as createUserInterruptionMessage emits it
// (claude-code-source-code/src/utils/messages.ts:545-560).
//
// Scenario: turn 0 "first"/"reply 0", a prior interrupt marker, turn 1
// "second"/"reply 1", then turn 2 "third" sent and immediately Stopped.
// The revert must keep turns 0 AND 1 in full and drop only "third".
// Before the fix it dropped "second"/"reply 1" too.
func TestInterruptAndRevertIfCleanSurvivesPriorInterruptMarker(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	const sessionID = "prior-interrupt-session"
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"prior-interrupt-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"prior-interrupt-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"int1","parentUuid":"a0","sessionId":"prior-interrupt-session","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
{"type":"user","uuid":"u1","parentUuid":"int1","sessionId":"prior-interrupt-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"prior-interrupt-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"prior-interrupt-session","message":{"role":"user","content":"third"}}
`)
	thread := createAppTestThread(t, app, "interrupt-marker", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "second")
	// No provider_item_id on u:2 — forces the ordinal fallback (the buggy
	// path), reproducing the fast send→escape race where the wire echo
	// hasn't stamped the UUID yet.
	insertUserItem(t, app.store, thread.ID, "u:2", 2, "third")

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want sliced fork session", updated.SessionRef)
	}
	// turn 1's "second"/"reply 1" MUST survive; only "third" is dropped.
	// Before the fix, the interrupt marker shifted the ordinal count and
	// "second"/"reply 1" were sliced away too.
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"first", "reply 0", "second", "reply 1"},
		[]string{"third"})
}

// TestResolveMessageAnchorSynthesizesProviderUserMessageID is the
// fine-grained unit test for the synthesize path: when no persisted
// anchor exists, the helper must lift the wire id off the user item's
// Meta so the downstream Claude rollback can do UUID-keyed slicing.
// Regression guard for the bug where an anchor-less row fell back to
// the legacy ordinal walk even after the fix.
func TestResolveMessageAnchorSynthesizesProviderUserMessageID(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "synth-uuid", "claude", t.TempDir())
	const meta = `{"provider_item_id":"wire-u1"}`
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1", 1, "second", meta)

	item := store.Item{ID: "u:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Meta: meta}
	anchor := app.resolveMessageAnchor("test", thread.ID, item)
	if anchor.ProviderUserMessageID != "wire-u1" {
		t.Fatalf("synthesized ProviderUserMessageID = %q, want %q", anchor.ProviderUserMessageID, "wire-u1")
	}
	if anchor.UserItemID != "u:1" {
		t.Fatalf("synthesized UserItemID = %q, want u:1", anchor.UserItemID)
	}
	if anchor.TurnIndex != 1 {
		t.Fatalf("synthesized TurnIndex = %d, want 1", anchor.TurnIndex)
	}
}

// writeCodexInterruptCompleteBinary mocks an app-server that emits the
// turn/completed notification synchronously while answering the
// turn/interrupt request — the ordering that forces MarkTurnReverted
// to run before the provider interrupt.
func writeCodexInterruptCompleteBinary(t *testing.T, threadID, turnID string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/interrupt"'; then
        printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"id":"%s","status":"completed"}}}\n'
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
done
`, threadID, threadID, turnID)

	path := filepath.Join(t.TempDir(), "codex-interrupt-complete.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}
