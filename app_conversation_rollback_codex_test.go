package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-anchor", "codex", t.TempDir())

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
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-anchor-skip", "codex", t.TempDir())

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
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-anchor-fresh", "codex", t.TempDir())

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
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-anchor-hole", "codex", t.TempDir())

	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)

	_, _, err := app.resolveCodexForkAnchor(thread.ID, 0)
	if err == nil || !strings.Contains(err.Error(), "no recorded provider turn id") {
		t.Fatalf("resolveCodexForkAnchor error = %v, want legacy-data-hole error", err)
	}
}

func TestConversationRollbackCodexForksAtAnchorAndStopsSession(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-active-revert", "codex", workspace)
	thread.SessionRef = "provider-active-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

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

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
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

func TestConversationRollbackCodexForksThroughTempSessionWhenStopped(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-stopped-revert", "codex", workspace)
	thread.SessionRef = "provider-stopped-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-stopped-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
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

func TestConversationRollbackCodexRejectsForkTailMismatch(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-mismatch-revert", "codex", workspace)
	thread.SessionRef = "provider-mismatch-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-mismatch-revert",
		forkedThreadID:  "forked-provider-thread",
		forkTailTurnID:  "turn-wrong",
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	err := rollbackToMessage(app, thread.ID, "user:1")
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

func TestConversationRollbackCodexAnchorSkipsLocalOnlyFailedTurn(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-local-only-revert", "codex", workspace)
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
	seedMessageAnchor(t, app.store, thread.ID, "user:2", 2, "", "")

	requestLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID: "provider-local-only-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  requestLog,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if err := rollbackToMessage(app, thread.ID, "user:2"); err != nil {
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

// TestConversationRollbackCodexTurnZeroClearsSessionRef: reverting
// the very first message needs no fork — SessionRef clears, the session
// stops, and the next send starts a fresh Codex thread.
func TestConversationRollbackCodexTurnZeroClearsSessionRef(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-turn-zero-revert", "codex", workspace)
	thread.SessionRef = "provider-turn-zero-revert"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "", "")

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

	if err := rollbackToMessage(app, thread.ID, "user:0"); err != nil {
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

// TestConversationRollbackCodexLocalOnlyThreadNeedsNoSessionRef:
// a thread whose sends all failed before reaching the provider has no
// SessionRef and no provider-backed turns. Reverting past turn 0 must
// take the fresh-thread path (no fork, cursor stays empty) instead of
// failing on the missing thread reference.
func TestConversationRollbackCodexLocalOnlyThreadNeedsNoSessionRef(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-local-only-thread", "codex", workspace)
	insertUserItem(t, app.store, thread.ID, "user:0-failed", 0, "first (send failed)")
	insertUserItem(t, app.store, thread.ID, "user:1-failed", 1, "second (send failed)")
	seedMessageAnchor(t, app.store, thread.ID, "user:1-failed", 1, "", "")

	if err := rollbackToMessage(app, thread.ID, "user:1-failed"); err != nil {
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

// A 0.148+ mock answers `thread/queue/list` with an empty queue because a
// real one always answers it: the rollback purges the provider-owned queue
// before it cuts, and a purge that cannot complete now REFUSES the rollback
// (purgeCodexProviderQueue). A mock that stayed silent would be asserting the
// old best-effort behavior by omission.
//
// codexForkMock configures writeCodexForkAtBinary, the fake app-server
// behind the Codex history-cut tests. It answers `thread/fork` by
// echoing the requested lastTurnId back as the fork's tail turn (the
// shape a real cut returns); forkTailTurnID overrides that echo to
// simulate a server whose fork survives through a different turn than
// the requested anchor.
//
// The zero value is a pre-0.148 legacy-history server: no userAgent (so
// AO reads the build as unknown and every per-method floor fails closed)
// and no historyMode on the resumed thread. That is deliberately the
// shape AO's own threads have today — see the revert tests for what it
// takes to opt in.
type codexForkMock struct {
	resumedThreadID string
	forkedThreadID  string
	requestLogPath  string
	forkTailTurnID  string
	// userAgent is what the server states at `initialize`; the ONLY
	// in-band statement of its build, and what appServerAtLeast reads.
	userAgent string
	// historyMode is the resumed thread's persisted history contract.
	// `thread/revert` needs "paginated"; anything else is refused
	// upstream, so anything else must keep AO on the fork cut.
	historyMode string
	// revertLogPath captures the `thread/revert` request frame.
	revertLogPath string
	// revertErrorMessage makes `thread/revert` answer invalid_request
	// with this message instead of succeeding.
	revertErrorMessage string
}

func writeCodexForkAtBinary(t *testing.T, mock codexForkMock) string {
	t.Helper()
	logForkRequest := ":"
	if mock.requestLogPath != "" {
		logForkRequest = fmt.Sprintf(`/bin/echo "$line" >> '%s'`, mock.requestLogPath)
	}
	logRevertRequest := ":"
	if mock.revertLogPath != "" {
		logRevertRequest = fmt.Sprintf(`/bin/echo "$line" >> '%s'`, mock.revertLogPath)
	}
	tailExpr := `"$cut"`
	if mock.forkTailTurnID != "" {
		tailExpr = fmt.Sprintf(`'%s'`, mock.forkTailTurnID)
	}
	initializeResult := "{}"
	if mock.userAgent != "" {
		initializeResult = fmt.Sprintf(`{"userAgent":"%s"}`, mock.userAgent)
	}
	resumedHistoryMode := ""
	if mock.historyMode != "" {
		resumedHistoryMode = fmt.Sprintf(`,"historyMode":"%s"`, mock.historyMode)
	}
	// A real app-server answers the revert and THEN emits thread/reverted
	// on the same connection; the mock keeps that order because the RPC
	// waits for the echo.
	revertReply := fmt.Sprintf(
		`printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"%s,"turns":[]},"turnsBackwardsCursor":"turns-cursor","itemsBackwardsCursor":"items-cursor"}}\n' "$id"
        printf '{"jsonrpc":"2.0","method":"thread/reverted","params":{"threadId":"%s"}}\n'`,
		mock.resumedThreadID, resumedHistoryMode, mock.resumedThreadID)
	if mock.revertErrorMessage != "" {
		revertReply = fmt.Sprintf(
			`printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32600,"message":"%s"}}\n' "$id"`,
			mock.revertErrorMessage)
	}
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":%s}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"%s,"turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/revert"'; then
        %s
        %s
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/list"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[],"nextCursor":null}}\n' "$id"
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
`, initializeResult, mock.resumedThreadID, resumedHistoryMode, logRevertRequest, revertReply,
		logForkRequest, tailExpr, mock.forkedThreadID, mock.forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-fork-at.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}

// readCodexRevertRequest reads the single `thread/revert` frame the mock
// captured. Same one-request contract as readCodexForkRequest: a second
// line would mean the rollback cut history twice.
func readCodexRevertRequest(t *testing.T, requestLogPath string) string {
	t.Helper()
	data, err := os.ReadFile(requestLogPath)
	if err != nil {
		t.Fatalf("read revert request log: %v", err)
	}
	request := strings.TrimSpace(string(data))
	if request == "" || strings.Contains(request, "\n") {
		t.Fatalf("revert request log = %q, want exactly one thread/revert request", request)
	}
	return request
}

// requireNoCodexRequest asserts a capture file was never written — the
// mock only creates it when the method it guards was actually called, so
// its absence is the proof that AO chose the other cut.
func requireNoCodexRequest(t *testing.T, requestLogPath, method string) {
	t.Helper()
	if _, err := os.Stat(requestLogPath); err == nil {
		t.Fatalf("%s was sent; this rollback must not have used it", method)
	}
}

// paginatedRevertThread is the fixture every revert test shares: two
// provider-backed turns on a paginated thread served by a 0.149
// app-server, with the second one about to be rolled back.
func paginatedRevertThread(t *testing.T, app *App, name string, mock codexForkMock) store.Thread {
	t.Helper()
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, name, "codex", workspace)
	thread.SessionRef = mock.resumedThreadID
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	binary := writeCodexForkAtBinary(t, mock)
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	return thread
}

// TestConversationRollbackCodexRevertsInPlaceWhenSupported is the whole
// feature: a 0.148+ app-server serving a paginated thread cuts history
// with `thread/revert` and the thread KEEPS its provider identity —
// SessionRef unchanged, no fork request on the wire at all.
func TestConversationRollbackCodexRevertsInPlaceWhenSupported(t *testing.T) {
	app := newTestApp(t)
	logDir := t.TempDir()
	mock := codexForkMock{
		resumedThreadID: "provider-inplace-revert",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  filepath.Join(logDir, "fork-requests.jsonl"),
		revertLogPath:   filepath.Join(logDir, "revert-requests.jsonl"),
		userAgent:       "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		historyMode:     "paginated",
	}
	thread := paginatedRevertThread(t, app, "codex-inplace-revert", mock)

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("revert: %v", err)
	}

	revertRequest := readCodexRevertRequest(t, mock.revertLogPath)
	// The EXCLUSIVE anchor: the first dropped turn, not the fork's last
	// kept one.
	if !strings.Contains(revertRequest, `"beforeTurnId":"turn-b"`) {
		t.Fatalf("revert request = %s, want beforeTurnId turn-b", revertRequest)
	}
	if !strings.Contains(revertRequest, `"threadId":"provider-inplace-revert"`) {
		t.Fatalf("revert request = %s, want the thread's own id", revertRequest)
	}
	requireNoCodexRequest(t, mock.requestLogPath, "thread/fork")

	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	// Thread identity is the whole point of the in-place cut.
	if updated.SessionRef != "provider-inplace-revert" {
		t.Fatalf("SessionRef = %q, want the SAME provider thread", updated.SessionRef)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].ID != "user:0" {
		t.Fatalf("items after revert = %+v, want only the kept prefix", items)
	}
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("the cut session must not survive the revert")
	}
}

// TestConversationRollbackCodexForksBelowTheRevertFloor: 0.147 has no
// `thread/revert` at all, so the fork cut has to still be there.
func TestConversationRollbackCodexForksBelowTheRevertFloor(t *testing.T) {
	app := newTestApp(t)
	logDir := t.TempDir()
	mock := codexForkMock{
		resumedThreadID: "provider-old-server",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  filepath.Join(logDir, "fork-requests.jsonl"),
		revertLogPath:   filepath.Join(logDir, "revert-requests.jsonl"),
		userAgent:       "codex_cli_rs/0.147.0 (Ubuntu 24.04; x86_64) some/1.0",
		historyMode:     "paginated",
	}
	thread := paginatedRevertThread(t, app, "codex-old-server", mock)

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	requireNoCodexRequest(t, mock.revertLogPath, "thread/revert")
	if forkRequest := readCodexForkRequest(t, mock.requestLogPath); !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want the inclusive anchor turn-a", forkRequest)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "forked-provider-thread" {
		t.Fatalf("SessionRef = %q, want the fork", updated.SessionRef)
	}
}

// TestConversationRollbackCodexForksOnLegacyHistoryThreads is the case
// that matters today: AO does not ask for paginated history on
// thread/start, upstream defaults to legacy, and upstream refuses a
// legacy revert. A 0.149 binary alone must NOT flip the cut.
func TestConversationRollbackCodexForksOnLegacyHistoryThreads(t *testing.T) {
	app := newTestApp(t)
	logDir := t.TempDir()
	mock := codexForkMock{
		resumedThreadID: "provider-legacy-history",
		forkedThreadID:  "forked-provider-thread",
		requestLogPath:  filepath.Join(logDir, "fork-requests.jsonl"),
		revertLogPath:   filepath.Join(logDir, "revert-requests.jsonl"),
		userAgent:       "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		historyMode:     "legacy",
	}
	thread := paginatedRevertThread(t, app, "codex-legacy-history", mock)

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	requireNoCodexRequest(t, mock.revertLogPath, "thread/revert")
	if forkRequest := readCodexForkRequest(t, mock.requestLogPath); !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want the inclusive anchor turn-a", forkRequest)
	}
}

// TestConversationRollbackCodexFallsBackToForkOnRefusedRevert covers
// version skew the local gate cannot see: the server reported a
// paginated thread and still refused. Upstream raises that refusal
// before it mutates anything, so the fork must complete the rollback on
// the same connection.
func TestConversationRollbackCodexFallsBackToForkOnRefusedRevert(t *testing.T) {
	app := newTestApp(t)
	logDir := t.TempDir()
	mock := codexForkMock{
		resumedThreadID:    "provider-refused-revert",
		forkedThreadID:     "forked-provider-thread",
		requestLogPath:     filepath.Join(logDir, "fork-requests.jsonl"),
		revertLogPath:      filepath.Join(logDir, "revert-requests.jsonl"),
		userAgent:          "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		historyMode:        "paginated",
		revertErrorMessage: "thread/revert only supports paginated threads",
	}
	thread := paginatedRevertThread(t, app, "codex-refused-revert", mock)

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if revertRequest := readCodexRevertRequest(t, mock.revertLogPath); !strings.Contains(revertRequest, `"beforeTurnId":"turn-b"`) {
		t.Fatalf("revert request = %s, want the attempt to have happened", revertRequest)
	}
	if forkRequest := readCodexForkRequest(t, mock.requestLogPath); !strings.Contains(forkRequest, `"lastTurnId":"turn-a"`) {
		t.Fatalf("fork request = %s, want the fallback cut", forkRequest)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef != "forked-provider-thread" {
		t.Fatalf("SessionRef = %q, want the fallback fork", updated.SessionRef)
	}
}

// TestConversationRollbackCodexAbortsOnNonRefusalRevertFailure: any
// revert error that is NOT a pre-mutation refusal may have left the
// thread half-cut, so falling back to a fork built on it would silently
// disagree with both. The rollback fails with everything untouched.
func TestConversationRollbackCodexAbortsOnNonRefusalRevertFailure(t *testing.T) {
	app := newTestApp(t)
	logDir := t.TempDir()
	mock := codexForkMock{
		resumedThreadID:    "provider-broken-revert",
		forkedThreadID:     "forked-provider-thread",
		requestLogPath:     filepath.Join(logDir, "fork-requests.jsonl"),
		revertLogPath:      filepath.Join(logDir, "revert-requests.jsonl"),
		userAgent:          "codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		historyMode:        "paginated",
		revertErrorMessage: "timed out shutting down thread before revert",
	}
	thread := paginatedRevertThread(t, app, "codex-broken-revert", mock)

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil || !strings.Contains(err.Error(), "cut history") {
		t.Fatalf("rollback error = %v, want the revert failure to abort the rollback", err)
	}
	requireNoCodexRequest(t, mock.requestLogPath, "thread/fork")
	updated, getErr := app.store.GetThread(thread.ID)
	if getErr != nil {
		t.Fatalf("get thread: %v", getErr)
	}
	if updated.SessionRef != "provider-broken-revert" {
		t.Fatalf("SessionRef = %q, want it untouched", updated.SessionRef)
	}
	items, listErr := app.store.ListItems(thread.ID)
	if listErr != nil {
		t.Fatalf("list items: %v", listErr)
	}
	if len(items) != 2 {
		t.Fatalf("items after failed rollback = %+v, want the original rows", items)
	}
}

// TestResolveCodexRevertAnchorPicksEarliestDroppedProviderTurn is the
// mirror of the fork-anchor contract: the anchor for "drop turns >= K"
// is the provider turn id of the EARLIEST provider-backed turn at or
// after K.
func TestResolveCodexRevertAnchorPicksEarliestDroppedProviderTurn(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-revert-anchor", "codex", t.TempDir())

	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	insertCodexTurn(t, app.store, thread.ID, 2, "turn-c")

	for _, c := range []struct {
		firstDropped int
		wantAnchor   string
	}{
		{firstDropped: 0, wantAnchor: "turn-a"},
		{firstDropped: 1, wantAnchor: "turn-b"},
		{firstDropped: 2, wantAnchor: "turn-c"},
	} {
		anchor, found, err := app.resolveCodexRevertAnchor(thread.ID, c.firstDropped)
		if err != nil {
			t.Fatalf("resolveCodexRevertAnchor(%d): %v", c.firstDropped, err)
		}
		if !found || anchor != c.wantAnchor {
			t.Errorf("resolveCodexRevertAnchor(%d) = (%q, %v), want (%q, true)", c.firstDropped, anchor, found, c.wantAnchor)
		}
	}
}

// TestResolveCodexRevertAnchorSkipsTurnsWithoutProviderID: a send that
// failed before the wire occupies an AO turn index the server never saw.
// Naming it would be an anchor upstream cannot resolve, so the walk
// continues UP to the next real one.
func TestResolveCodexRevertAnchorSkipsTurnsWithoutProviderID(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-revert-anchor-skip", "codex", t.TempDir())

	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	// Turn 1 failed before the wire — no turns row at all.
	insertCodexTurn(t, app.store, thread.ID, 2, "turn-c")

	anchor, found, err := app.resolveCodexRevertAnchor(thread.ID, 1)
	if err != nil {
		t.Fatalf("resolveCodexRevertAnchor(1): %v", err)
	}
	if !found || anchor != "turn-c" {
		t.Fatalf("resolveCodexRevertAnchor(1) = (%q, %v), want (turn-c, true)", anchor, found)
	}
}

// TestResolveCodexRevertAnchorMissesPastTheTail: nothing being dropped
// reached the provider. That is a miss, not an error — the caller falls
// back to the fork cut, which describes the same boundary from the
// surviving side.
func TestResolveCodexRevertAnchorMissesPastTheTail(t *testing.T) {
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "codex-revert-anchor-miss", "codex", t.TempDir())

	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")

	anchor, found, err := app.resolveCodexRevertAnchor(thread.ID, 1)
	if err != nil {
		t.Fatalf("resolveCodexRevertAnchor(1): %v", err)
	}
	if found || anchor != "" {
		t.Fatalf("resolveCodexRevertAnchor(1) = (%q, %v), want a miss", anchor, found)
	}
	// A thread with no turn rows at all is the same answer, without a
	// ceiling to walk to. Created directly because createAppTestThread
	// also creates project "p1", which already exists in this store.
	now := time.Now().UnixMilli()
	empty := store.Thread{
		ID: "codex-revert-anchor-empty", ProjectID: "p1", Title: "empty",
		Provider: "codex", WorkspacePath: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.CreateThread(empty); err != nil {
		t.Fatalf("create empty thread: %v", err)
	}
	if anchor, found, err := app.resolveCodexRevertAnchor(empty.ID, 0); err != nil || found || anchor != "" {
		t.Fatalf("resolveCodexRevertAnchor on an empty thread = (%q, %v, %v), want a clean miss", anchor, found, err)
	}
}
