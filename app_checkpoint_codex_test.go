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

// TestResolveCodexForkAnchorPicksLatestProviderBackedTurn locks the
// anchor-resolution contract for the `thread/fork` lastTurnId cut:
// the anchor for "keep turns <= K" is the provider turn id of the
// LATEST provider-backed turn at or before K.
//
// The fixture's one-turn-row-per-index shape is itself a property
// under test: steered messages share a turn segment (app_steer.go
// reuses the active turnIndex), and Codex compaction never bumps
// turn_index, so AO's turns table stays 1:1 with the app-server's
// user-turn segments. If either property regresses, the anchor picked
// here would cut the fork at the wrong turn.
func TestResolveCodexForkAnchorPicksLatestProviderBackedTurn(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-anchor", "codex", t.TempDir())

	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	insertCodexTurn(t, app.store, thread.ID, 2, "turn-c")

	cases := []struct {
		lastKept   int
		wantAnchor string
	}{
		{lastKept: 2, wantAnchor: "turn-c"},
		{lastKept: 1, wantAnchor: "turn-b"},
		{lastKept: 0, wantAnchor: "turn-a"},
	}
	for _, c := range cases {
		anchor, found, err := app.resolveCodexForkAnchor(thread.ID, c.lastKept)
		if err != nil {
			t.Fatalf("resolveCodexForkAnchor(%d): %v", c.lastKept, err)
		}
		if !found || anchor != c.wantAnchor {
			t.Errorf("resolveCodexForkAnchor(%d) = (%q, %v), want (%q, true)", c.lastKept, anchor, found, c.wantAnchor)
		}
	}
}

// TestResolveCodexForkAnchorSkipsTurnsWithoutProviderID covers the
// failed-send hole: `SendMessage` bumps turn_index BEFORE the provider
// send returns, so a send that errors pre-wire occupies an AO turn
// index that the Codex server never saw (no turns row, or a row with
// no provider id). The anchor walk must skip it and land on the
// previous provider-backed turn instead of failing or anchoring on a
// turn the server doesn't know.
func TestResolveCodexForkAnchorSkipsTurnsWithoutProviderID(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-anchor-skip", "codex", t.TempDir())

	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	// Turn 1 failed before the wire — no turns row at all.
	insertCodexTurn(t, app.store, thread.ID, 2, "turn-c")

	anchor, found, err := app.resolveCodexForkAnchor(thread.ID, 1)
	if err != nil {
		t.Fatalf("resolveCodexForkAnchor(1): %v", err)
	}
	if !found || anchor != "turn-a" {
		t.Fatalf("resolveCodexForkAnchor(1) = (%q, %v), want (turn-a, true)", anchor, found)
	}
}

// TestResolveCodexForkAnchorFreshWhenNoProviderTurns: a prefix with no
// provider-backed turns AND no provider-confirmed user items resolves
// to (found=false, nil) — the caller starts a fresh provider thread.
func TestResolveCodexForkAnchorFreshWhenNoProviderTurns(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-anchor-fresh", "codex", t.TempDir())

	// A local-only failed send is the only occupant of the prefix.
	insertUserItem(t, app.store, thread.ID, "u:0-failed", 0, "failed before provider")

	anchor, found, err := app.resolveCodexForkAnchor(thread.ID, 0)
	if err != nil {
		t.Fatalf("resolveCodexForkAnchor(0): %v", err)
	}
	if found || anchor != "" {
		t.Fatalf("resolveCodexForkAnchor(0) = (%q, %v), want fresh-session miss", anchor, found)
	}
}

// TestResolveCodexForkAnchorRejectsLegacyDataHole: provider-confirmed
// user items with NO recorded provider turn ids means the turns rows
// went missing (a fork cloned before turn rows were copied). Silently
// answering "fresh session" would discard provider history, so the
// resolver must fail loudly instead.
func TestResolveCodexForkAnchorRejectsLegacyDataHole(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	thread := createCheckpointTestThread(t, app, "codex-anchor-hole", "codex", t.TempDir())

	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)

	_, _, err := app.resolveCodexForkAnchor(thread.ID, 0)
	if err == nil || !strings.Contains(err.Error(), "no recorded provider turn id") {
		t.Fatalf("resolveCodexForkAnchor error = %v, want legacy-data-hole error", err)
	}
}

func TestRevertToMessageCheckpointCodexForksAtAnchorAndStopsSession(t *testing.T) {
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
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	saveCodexCheckpoint(t, app.store, thread, "chk-active", "user:1", 1)

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-active-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
	})
	// The revert stops the live session BEFORE forking, so the fork
	// always runs through a temp resume session spawned from settings.
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
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

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	// The stop is load-bearing (invariant 29): straggler wire events
	// from the pre-revert thread must hit the stopped-thread gate.
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("Codex session still active after revert; revert must stop it")
	}
	forkRequest := readCodexForkRequest(t, requestLog)
	if !strings.Contains(forkRequest, `"threadId":"provider-active-revert"`) || !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want cut of provider-active-revert at turn-a", forkRequest)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "forked-provider-thread" {
		t.Fatalf("SessionRef = %q, want forked-provider-thread", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after revert = %+v", items)
	}
}

func TestRevertToMessageCheckpointCodexForksThroughTempSessionWhenStopped(t *testing.T) {
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
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	saveCodexCheckpoint(t, app.store, thread, "chk-stopped", "user:1", 1)

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-stopped-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("temp fork session must not survive the revert")
	}
	forkRequest := readCodexForkRequest(t, requestLog)
	if !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want lastTurnId turn-a", forkRequest)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "forked-provider-thread" {
		t.Fatalf("SessionRef = %q, want forked-provider-thread", updated.SessionRef)
	}
}

func TestRevertToMessageCheckpointCodexRejectsForkTailMismatch(t *testing.T) {
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
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	saveCodexCheckpoint(t, app.store, thread, "chk-mismatch", "user:1", 1)

	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-mismatch-revert",
		forkedThreadID:  "forked-provider-thread",
		forkTailTurnID:  "turn-wrong",
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	err := app.RevertToMessageCheckpoint(thread.ID, "user:1", RevertModeConversationOnly)
	if err == nil || !strings.Contains(err.Error(), "expected anchor") {
		t.Fatalf("revert error = %v, want fork tail mismatch", err)
	}
	items, listErr := app.store.ListItems(thread.ID)
	if listErr != nil {
		t.Fatalf("list items: %v", listErr)
	}
	if len(items) != 2 {
		t.Fatalf("items after failed revert = %+v, want original rows preserved", items)
	}
	updated, getErr := app.store.GetThread(thread.ID)
	if getErr != nil {
		t.Fatalf("get thread: %v", getErr)
	}
	if updated.SessionRef != "provider-mismatch-revert" {
		t.Fatalf("SessionRef = %q, want untouched provider-mismatch-revert", updated.SessionRef)
	}
}

func TestRevertToMessageCheckpointCodexAnchorSkipsLocalOnlyFailedTurn(t *testing.T) {
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
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	// Turn 1 never reached the wire — no turns row.
	insertCodexTurn(t, app.store, thread.ID, 2, "turn-c")
	saveCodexCheckpoint(t, app.store, thread, "chk-local-only", "user:2", 2)

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-local-only-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:2", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	forkRequest := readCodexForkRequest(t, requestLog)
	if !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want anchor turn-a (local-only turn 1 skipped)", forkRequest)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "user:1-failed-local" {
		t.Fatalf("items after revert = %+v, want provider turn plus local-only failed turn", items)
	}
}

// TestRevertToMessageCheckpointCodexTurnZeroClearsSessionRef: reverting
// the very first message needs no fork — SessionRef clears, the session
// stops, and the next send starts a fresh Codex thread.
func TestRevertToMessageCheckpointCodexTurnZeroClearsSessionRef(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-turn-zero-revert", "codex", workspace)
	thread.SessionRef = "provider-turn-zero-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	saveCodexCheckpoint(t, app.store, thread, "chk-zero", "user:0", 0)

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-turn-zero-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
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
		token:    "turn-zero-token",
		codex:    sess,
	})

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:0", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("Codex session still active after turn-zero revert")
	}
	if _, err := os.Stat(requestLog); !os.IsNotExist(err) {
		t.Fatalf("turn-zero revert must not issue thread/fork (stat err = %v)", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "" {
		t.Fatalf("SessionRef = %q, want cleared", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items after revert = %+v, want empty", items)
	}
}

// TestRevertToMessageCheckpointCodexLocalOnlyThreadNeedsNoSessionRef:
// a thread whose sends all failed before reaching the provider has no
// SessionRef and no provider-backed turns. Reverting past turn 0 must
// take the fresh-thread path (no fork, cursor stays empty) instead of
// failing on the missing thread reference.
func TestRevertToMessageCheckpointCodexLocalOnlyThreadNeedsNoSessionRef(t *testing.T) {
	app, cleanup := newTestApp(t)
	defer cleanup()
	workspace := t.TempDir()
	thread := createCheckpointTestThread(t, app, "codex-local-only-thread", "codex", workspace)
	insertUserItem(t, app.store, thread.ID, "user:0-failed", 0, "first (send failed)")
	insertUserItem(t, app.store, thread.ID, "user:1-failed", 1, "second (send failed)")
	saveCodexCheckpoint(t, app.store, thread, "chk-no-ref", "user:1-failed", 1)

	if err := app.RevertToMessageCheckpoint(thread.ID, "user:1-failed", RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "" {
		t.Fatalf("SessionRef = %q, want empty", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0-failed" {
		t.Fatalf("items after revert = %+v, want only the first failed send", items)
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
		// Codex fork-at-turn uses turn index, not provider user message id,
		// but the checkpoint still carries the workspace for shared validation.
		WorkspacePath: thread.WorkspacePath,
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
}

// insertCodexTurn seeds a settled provider-backed turns row the way
// the live triage path does for Codex: wire turn id as both row id and
// provider_turn_id, completed so it doesn't trip the active-turn
// revert guard.
func insertCodexTurn(t *testing.T, st *store.Store, threadID string, turnIndex int, providerTurnID string) {
	t.Helper()
	if err := st.InsertTurn(store.Turn{
		TurnID:         providerTurnID,
		ProviderTurnID: providerTurnID,
		ThreadID:       threadID,
		TurnIndex:      turnIndex,
		StartedAt:      int64(turnIndex + 1),
	}); err != nil {
		t.Fatalf("insert codex turn %s: %v", providerTurnID, err)
	}
	if err := st.UpdateTurnCompleted(providerTurnID, int64(turnIndex+2), "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete codex turn %s: %v", providerTurnID, err)
	}
}

func readCodexForkRequest(t *testing.T, requestLogPath string) string {
	t.Helper()
	data, err := os.ReadFile(requestLogPath)
	if err != nil {
		t.Fatalf("read fork request log: %v", err)
	}
	request := strings.TrimSpace(string(data))
	if request == "" || strings.Contains(request, "\n") {
		t.Fatalf("fork request log = %q, want exactly one thread/fork request", request)
	}
	return request
}

// codexForkMock configures writeCodexForkAtBinary. The mock answers
// `thread/fork` by echoing the requested lastTurnId back as the fork's
// tail turn (the shape a real cut returns); forkTailTurnID overrides
// that echo to simulate a server whose fork survives through a
// different turn than the requested anchor.
type codexForkMock struct {
	resumedThreadID string
	forkedThreadID  string
	requestLogPath  string
	forkTailTurnID  string
}

func writeCodexForkAtBinary(t *testing.T, mock codexForkMock) string {
	t.Helper()
	logForkRequest := ":"
	if mock.requestLogPath != "" {
		logForkRequest = fmt.Sprintf(`/bin/echo "$line" >> '%s'`, mock.requestLogPath)
	}
	tailExpr := `"$cut"`
	if mock.forkTailTurnID != "" {
		tailExpr = fmt.Sprintf(`'%s'`, mock.forkTailTurnID)
	}
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
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/read"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"status":{"type":"idle"}}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/interrupt"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        %s
        cut=$(/bin/echo "$line" | /usr/bin/grep -o '"lastTurnId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        tail=%s
        if [ -n "$tail" ]; then
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[{"id":"%%s"}]}}}\n' "$id" "$tail"
        else
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        fi
        continue
    fi
done
`, mock.resumedThreadID, logForkRequest, tailExpr, mock.forkedThreadID, mock.forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-fork-at.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}
