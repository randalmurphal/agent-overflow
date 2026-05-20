package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// TestEvaluateInterruptRevertPredicateNoItems covers the empty-thread
// branch — no items at all means there's nothing to revert.
func TestEvaluateInterruptRevertPredicateNoItems(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-empty", "claude", t.TempDir())

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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-single", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-asst", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-tool", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-think", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-multi", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-queue", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	app.triage = triage.NewRouter(app.store, func(string, any) {})
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "pred-dispatch", "codex", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	app.flushDispatchMu.Lock()
	app.ensureFlushDispatchMapsLocked()
	app.flushDispatchInflightItems[thread.ID] = 1
	app.flushDispatchMu.Unlock()

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

// TestResolveRevertCheckpointReturnsPersisted is the simple lookup case
// when the at-send checkpoint capture wrote a row keyed by the user
// item id and matching turn index.
func TestResolveRevertCheckpointReturnsPersisted(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "rc-found", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	want := store.Checkpoint{
		ID:                    "chk-0",
		ThreadID:              thread.ID,
		UserItemID:            "u:0",
		TurnIndex:             0,
		ProviderUserMessageID: "provider-u-0",
		RefName:               checkpoint.ThreadRefPrefix(thread.ID) + "message/some",
		WorkspacePath:         thread.WorkspacePath,
		CapturedAt:            time.Now().UnixMilli(),
	}
	if err := app.store.SaveCheckpoint(want); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	userItem, ok, err := app.store.GetItem("u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveRevertCheckpoint(thread.ID, userItem)
	if got.ID != want.ID {
		t.Fatalf("checkpoint id = %q, want %q", got.ID, want.ID)
	}
	if got.TurnIndex != want.TurnIndex {
		t.Fatalf("turn index = %d, want %d", got.TurnIndex, want.TurnIndex)
	}
}

// TestResolveRevertCheckpointSynthesizesWhenMissing covers the case
// where at-send capture didn't write a row (e.g. workspace is not a
// git repo). The provider revert helpers only need TurnIndex, so we
// synthesize a minimal record.
func TestResolveRevertCheckpointSynthesizesWhenMissing(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "rc-missing", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")

	userItem, ok, err := app.store.GetItem("u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveRevertCheckpoint(thread.ID, userItem)
	if got.UserItemID != "u:0" {
		t.Fatalf("synthesized userItemID = %q, want u:0", got.UserItemID)
	}
	if got.TurnIndex != 0 {
		t.Fatalf("synthesized turnIndex = %d, want 0", got.TurnIndex)
	}
	if got.ID != "" {
		t.Fatalf("synthesized record should have empty ID, got %q", got.ID)
	}
}

// TestRunPlainInterruptLockedNoSessionIsNoOp asserts the
// "Stop on a stale thread" path: no active session means no work to do,
// no error returned. This is the fallback the InterruptAndRevertIfClean
// method takes when the predicate fails.
func TestRunPlainInterruptLockedNoSessionIsNoOp(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "no-session", "claude", t.TempDir())

	if err := app.runPlainInterruptLocked(thread.ID); err != nil {
		t.Fatalf("runPlainInterruptLocked returned err = %v, want nil", err)
	}
}

// TestInterruptAndRevertIfCleanRejectsEmptyThreadID is the input-validation
// gate. The frontend always passes a non-empty thread id, but the
// binding still must refuse empty values rather than silently no-op.
func TestInterruptAndRevertIfCleanRejectsEmptyThreadID(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()

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
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "fallback", "claude", t.TempDir())
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := createCheckpointTestThread(t, app, "revert-first", "claude", t.TempDir())
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

// TestInterruptAndRevertIfCleanRevertsWithSynthesizedCheckpoint covers
// the "at-send capture failed" path: no checkpoint row exists, the
// revert helper synthesizes one with just TurnIndex. The revert should
// still succeed because the Claude TurnIndex==0 path doesn't need any
// checkpoint metadata.
func TestInterruptAndRevertIfCleanRevertsWithSynthesizedCheckpoint(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := createCheckpointTestThread(t, app, "revert-synth", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "synth prompt")
	// Deliberately do NOT call SaveCheckpoint — the predicate is still
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

func TestInterruptAndRevertIfCleanCodexWaitsForActiveTurnAndRollsBackLiveSession(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-interrupt-revert", "codex", workspace)
	thread.SessionRef = "provider-codex-interrupt"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert active turn: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "codex prompt")

	binary := writeCodexRollbackBinary(t, "provider-codex-interrupt", 0)
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(75 * time.Millisecond)
		_ = app.store.UpdateTurnCompleted("turn-active", time.Now().UnixMilli(), "interrupt", "", "", "")
	}()

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	<-done
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got Reverted=false reason=%q", result.Reason)
	}
	if result.UserItemID != "u:0" || result.TurnIndex != 0 {
		t.Fatalf("result = %+v, want reverted u:0 turn 0", result)
	}
	if _, ok := app.activeCodexSession(thread.ID); !ok {
		t.Fatal("active Codex session missing after interrupt rollback")
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
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, app.emit)
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-interrupt-marker", "codex", workspace)
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

	binary := writeCodexRollbackBinaryWithInterruptComplete(t, "provider-codex-marker", "turn-active", 0)
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

// TestInterruptAndRevertIfCleanSurvivesCompactBoundary is the
// interrupt-revert counterpart to TestRevertToMessageCheckpointSurvivesCompactBoundary.
// The "synthesize a checkpoint when none was captured" branch
// (`resolveRevertCheckpoint`) now lifts `provider_item_id` off the
// user item's Meta so the synthesized record drives the same
// UUID-keyed slice — non-git workspaces benefit from the structural
// fix too.
//
// Scenario: 3 logical turns, /compact summary on disk between turn
// 0's assistant and turn 1's user prompt (the placement that
// triggers the ordinal off-by-N). User sends turn 2 and immediately
// hits Stop before any agent output. AO synthesizes a checkpoint
// (no at-send capture row exists). The revert must keep turn 1's
// full assistant response.
func TestInterruptAndRevertIfCleanSurvivesCompactBoundary(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	app.triage = triage.NewRouter(app.store, func(string, any) {})
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
	thread := createCheckpointTestThread(t, app, "interrupt-compact", "claude", workspace)
	thread.SessionRef = sessionID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1", 1, "second")
	// u:2 carries provider_item_id=u2 — the wire-stamp the synthesize
	// path will read to populate the synthesized checkpoint.
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

// TestResolveRevertCheckpointSynthesizesProviderUserMessageID is the
// fine-grained unit test for the synthesize path: when no persisted
// checkpoint exists, the helper must lift the wire id off the user
// item's Meta so the downstream Claude revert can do UUID-keyed
// slicing. Regression guard for the bug where a non-git workspace
// (which never captures git checkpoints) fell back to the legacy
// ordinal walk even after the fix.
func TestResolveRevertCheckpointSynthesizesProviderUserMessageID(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "synth-uuid", "claude", t.TempDir())
	const meta = `{"provider_item_id":"wire-u1"}`
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1", 1, "second", meta)

	item := store.Item{ID: "u:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Meta: meta}
	cp := app.resolveRevertCheckpoint(thread.ID, item)
	if cp.ProviderUserMessageID != "wire-u1" {
		t.Fatalf("synthesized ProviderUserMessageID = %q, want %q", cp.ProviderUserMessageID, "wire-u1")
	}
	if cp.UserItemID != "u:1" {
		t.Fatalf("synthesized UserItemID = %q, want u:1", cp.UserItemID)
	}
	if cp.TurnIndex != 1 {
		t.Fatalf("synthesized TurnIndex = %d, want 1", cp.TurnIndex)
	}
}

func writeCodexRollbackBinaryWithInterruptComplete(t *testing.T, threadID, turnID string, survivingTurns int) string {
	t.Helper()
	turns := make([]string, 0, survivingTurns)
	for i := 0; i < survivingTurns; i++ {
		turns = append(turns, fmt.Sprintf(`{"id":"turn-%d"}`, i))
	}
	turnsJSON := "[" + strings.Join(turns, ",") + "]"
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
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/rollback"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":%s}}}\n' "$id"
        continue
    fi
done
`, threadID, threadID, turnID, threadID, turnsJSON)

	path := filepath.Join(t.TempDir(), "codex-rollback-interrupt-complete.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}
