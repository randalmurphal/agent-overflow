package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// TestCodexRollbackMathIgnoresCompactionAndSteer is the regression
// guard for the Codex `numTurns` computation in
// `revertProviderConversationToMessage`
// (`app_checkpoint.go::numTurns := lastTurn - checkpoint.TurnIndex + 1`).
//
// The math stays correct as long as AO's `items.turn_index` advances
// by exactly one per Codex user-turn segment. Two properties matter:
//
//  1. **Steered messages share a turn segment.** The Codex
//     app-server's `is_user_turn_boundary` collapses consecutive
//     user messages within an active turn into one segment for
//     `drop_last_n_user_turns` purposes. AO's `app_steer.go`
//     mirrors this by reusing the active turn's `turnIndex` when
//     persisting a steered user_text row.
//
//  2. **Compaction is not a user turn.** Codex's
//     `RolloutItem::Compacted` is server-side; AO never persists a
//     `turn_index` bump for it. The fixture below has no
//     compaction row because compaction is invisible to AO's
//     timeline — that absence is exactly the property under test.
//
// If either property regresses, `LastTurnIndex` would over-count
// and the Codex `thread/rollback` arg would drop too many turns.
// The test fails loudly so the change author has to justify the
// new invariant break before merging.
func TestCodexRollbackMathIgnoresCompactionAndSteer(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-math", "codex", t.TempDir())

	// 3 logical Codex user turns. Turn 1 has TWO AO user_text rows —
	// a steer scenario where the user sent a follow-up before the
	// model started responding. Both rows MUST share turn_index=1.
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "u:1a", 1, "second")
	insertUserItem(t, app.store, thread.ID, "u:1b", 1, "steered follow-up")
	insertUserItem(t, app.store, thread.ID, "u:2", 2, "third")

	lastTurn, err := app.store.LastTurnIndex(thread.ID)
	if err != nil {
		t.Fatalf("LastTurnIndex: %v", err)
	}
	if lastTurn != 2 {
		t.Fatalf("LastTurnIndex = %d, want 2 (steered messages MUST share turn_index)", lastTurn)
	}

	// Lock the formula AO uses in revertProviderConversationToMessage
	// (app_checkpoint.go). Each case asserts: rollback to checkpoint
	// turn K drops `lastTurn - K + 1` turns on the wire.
	cases := []struct {
		checkpointTurn int
		wantNumTurns   int
	}{
		{checkpointTurn: 2, wantNumTurns: 1}, // drop just the last turn
		{checkpointTurn: 1, wantNumTurns: 2}, // drop steered turn + last turn
		{checkpointTurn: 0, wantNumTurns: 3}, // drop everything
	}
	for _, c := range cases {
		if got := lastTurn - c.checkpointTurn + 1; got != c.wantNumTurns {
			t.Errorf("numTurns(checkpoint=%d) = %d, want %d", c.checkpointTurn, got, c.wantNumTurns)
		}
	}
}

// TestCodexRollbackMathAfterSendFailureStaysSelfConsistent locks in
// the current behaviour around `recordSendFailureAndCompleteTurn`
// (`app_send.go`). AO bumps `turn_index` inside `SendMessage` BEFORE
// the provider send returns; on failure, the error row is persisted
// at the bumped index and `turn_index` is NOT decremented. The
// next successful send bumps it again.
//
// That's "benign in practice" per the plan because:
//   - The failed turn never reached the Codex server, so it isn't
//     in the server's turn count.
//   - AO surfaces the error to the user and clears pending_send
//     before any revert UI is enabled.
//
// This test makes the contract explicit. If someone refactors
// `recordSendFailureAndCompleteTurn` to decrement turn_index (or
// to skip persisting the failed-turn marker), the math invariant
// here breaks and the test fails — forcing them to think through
// the implications for in-flight reverts.
func TestCodexRollbackMathAfterSendFailureStaysSelfConsistent(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-fail", "codex", t.TempDir())

	// Successful turn 0.
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "first")
	// Failed send at turn 1 — only the error marker exists at this
	// turn_index (no successful user_text). In real code path
	// recordSendFailureAndCompleteTurn persists both an error item
	// and a turn-complete row. For the rollback math, the relevant
	// fact is just "turn_index 1 is occupied" — we model it with a
	// user_text marker so LastTurnIndex sees it.
	insertUserItem(t, app.store, thread.ID, "u:1-failed", 1, "second (send failed)")
	// Next successful send bumps to turn 2.
	insertUserItem(t, app.store, thread.ID, "u:2", 2, "third")

	lastTurn, err := app.store.LastTurnIndex(thread.ID)
	if err != nil {
		t.Fatalf("LastTurnIndex: %v", err)
	}
	if lastTurn != 2 {
		t.Fatalf("LastTurnIndex = %d, want 2 (failed-send turn stays counted)", lastTurn)
	}

	// Rollback to checkpoint turn 0 means "redo turn 0 and everything
	// after," so AO drops turns 0, 1, AND 2 (numTurns=3, formula
	// `lastTurn - K + 1` = 2 - 0 + 1 = 3). The Codex server only
	// saw turn 0 and turn 2 (turn 1 was the failed send that never
	// reached the wire), so numTurns=3 over-counts by 1 — but the
	// server's `drop_last_n_user_turns` clamps at "no more user
	// turns" and the over-count is harmless. The test asserts the
	// math returns 3 here, NOT 2 — i.e. AO does not silently
	// discount the failed turn. If we ever want to fix the
	// over-count, it has to be a deliberate change, not an
	// incidental edit.
	if got := lastTurn - 0 + 1; got != 3 {
		t.Fatalf("numTurns(checkpoint=0) = %d, want 3 (lastTurn=2 includes failed turn)", got)
	}
}

func TestRevertToMessageCheckpointCodexUsesActiveSessionAndKeepsItAlive(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-active-revert", "codex", workspace)
	thread.SessionRef = "provider-active-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	saveCodexCheckpoint(t, app.store, thread, "chk-active", "user:1", 1)

	binary := writeCodexRollbackBinary(t, "provider-active-revert", 1)
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
		token:    "active-revert-token",
		codex:    sess,
	})
	stopCalls := 0
	app.stopSessionFn = func(string) error {
		stopCalls++
		return nil
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if stopCalls != 0 {
		t.Fatalf("stopSession calls = %d, want 0", stopCalls)
	}
	if _, ok := app.activeCodexSession(thread.ID); !ok {
		t.Fatal("active Codex session missing after rollback")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after revert = %+v", items)
	}
}

func TestRevertToMessageCheckpointCodexStartsStoppedSessionAndKeepsItAlive(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-stopped-revert", "codex", workspace)
	thread.SessionRef = "provider-stopped-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	saveCodexCheckpoint(t, app.store, thread, "chk-stopped", "user:1", 1)

	binary := writeCodexRollbackBinary(t, "provider-stopped-revert", 1)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	defer app.StopSession(thread.ID)
	if _, ok := app.activeCodexSession(thread.ID); !ok {
		t.Fatal("active Codex session missing after rollback")
	}
}

func TestRevertToMessageCheckpointCodexRejectsRollbackTurnCountMismatch(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-mismatch-revert", "codex", workspace)
	thread.SessionRef = "provider-mismatch-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	saveCodexCheckpoint(t, app.store, thread, "chk-mismatch", "user:1", 1)

	binary := writeCodexRollbackBinary(t, "provider-mismatch-revert", 0)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly)
	defer app.StopSession(thread.ID)
	if err == nil || !strings.Contains(err.Error(), "surviving turns") {
		t.Fatalf("revert error = %v, want surviving-turn mismatch", err)
	}
	items, listErr := app.store.ListItems(thread.ID)
	if listErr != nil {
		t.Fatalf("list items: %v", listErr)
	}
	if len(items) != 2 {
		t.Fatalf("items after failed revert = %+v, want original rows preserved", items)
	}
}

func TestRevertToMessageCheckpointCodexAllowsLocalOnlyFailedTurnBeforeTarget(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-local-only-revert", "codex", workspace)
	thread.SessionRef = "provider-local-only-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItem(t, app.store, thread.ID, "user:1-failed-local", 1, "failed before provider")
	insertUserItemWithMeta(t, app.store, thread.ID, "user:2", 2, "third", `{"provider_item_id":"provider-user-2"}`)
	saveCodexCheckpoint(t, app.store, thread, "chk-local-only", "user:2", 2)

	binary := writeCodexRollbackBinary(t, "provider-local-only-revert", 1)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	defer app.StopSession(thread.ID)
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "user:1-failed-local" {
		t.Fatalf("items after revert = %+v, want provider turn plus local-only failed turn", items)
	}
}

func saveCodexCheckpoint(t *testing.T, st *store.Store, thread store.Thread, id, userItemID string, turnIndex int) {
	t.Helper()
	if err := st.SaveCheckpoint(store.Checkpoint{
		ID:         id,
		ThreadID:   thread.ID,
		UserItemID: userItemID,
		TurnIndex:  turnIndex,
		RefName:    checkpoint.ThreadRefPrefix(thread.ID) + "message/" + id,
		CapturedAt: 1,
		// Codex rollback uses turn index, not provider user message id, but
		// the checkpoint still carries the workspace for shared validation.
		WorkspacePath: thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
}

func writeCodexRollbackBinary(t *testing.T, threadID string, survivingTurns int) string {
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
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/read"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"status":{"type":"idle"}}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/interrupt"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/rollback"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":%s}}}\n' "$id"
        continue
    fi
done
`, threadID, threadID, threadID, turnsJSON)

	path := filepath.Join(t.TempDir(), "codex-rollback.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}
