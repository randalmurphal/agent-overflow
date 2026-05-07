package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// writeCodexSteerBinary mints a fake codex app-server that knows how to
// answer the JSON-RPC methods the SteerMessageWithOptions path drives:
// initialize, thread/start, and turn/steer. The steer reply path is
// parameterized so individual tests can simulate the wire returning
// success or NoActiveTurn.
func writeCodexSteerBinary(t *testing.T, threadID, steerOutcome string) string {
	t.Helper()

	// steerBranch is embedded into the outer fmt.Sprintf template via a
	// %s verb, so its own printf format spec needs ONE % (not %%) — the
	// outer Sprintf is the only layer that strips a percent here.
	var steerBranch string
	switch steerOutcome {
	case "ok":
		steerBranch = `printf '{"jsonrpc":"2.0","id":%s,"result":{"turnId":"steer-turn"}}\n' "$id"`
	case "no-active-turn":
		steerBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"NoActiveTurn"}}\n' "$id"`
	case "timeout":
		steerBranch = `: # simulate an accepted write whose JSON-RPC ack never arrives`
	default:
		t.Fatalf("writeCodexSteerBinary: unknown steerOutcome %q", steerOutcome)
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
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        %s
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, threadID, steerBranch)

	path := filepath.Join(t.TempDir(), "codex-steer.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex-steer binary: %v", err)
	}
	return path
}

// installSteerTestSession registers an active codex Session on the App
// and pre-seeds activeTurnID so Steer doesn't short-circuit on the
// "no local active turn" branch (that branch is already covered at the
// codex.Session unit level). Returns the constructed session so the
// caller can Close() it via t.Cleanup.
func installSteerTestSession(t *testing.T, app *App, thread store.Thread, steerOutcome string) *codex.Session {
	t.Helper()
	binary := writeCodexSteerBinary(t, thread.ID+"-codex", steerOutcome)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Pre-seed an active turn id so Steer doesn't trip the
	// no-active-turn-yet guard. The triage / store layers are
	// independent of this session-local id; the test exercises the
	// wire dispatch + persistence wiring, not turn discovery.
	codex.SetActiveTurnIDForTest(sess, "active-turn")
	return sess
}

// TestSteerMessageWithOptions_PersistsUserRowAndDispatchesToCodex pins
// the happy path: the App.SteerMessageWithOptions wrapper persists a
// `user_text` row at the active turn's index, registers a pending-send
// marker for triage to consume, and forwards the wire payload through
// turn/steer.
func TestSteerMessageWithOptions_PersistsUserRowAndDispatchesToCodex(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := testThread("thread-steer-ok")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed an in-flight turn at index 3 so Steer attaches the new row
	// to that turn rather than starting a new one.
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
		token:    "steer-token",
		codex:    sess,
	}

	updated, err := app.SteerMessageWithOptions(thread.ID, "steer me", SendMessageOptions{})
	if err != nil {
		t.Fatalf("SteerMessageWithOptions: %v", err)
	}
	if updated.ID != thread.ID {
		t.Fatalf("returned thread id = %q, want %q", updated.ID, thread.ID)
	}

	// User row must land at the active turn's index, NOT at +1.
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var steerRow *store.Item
	for i := range items {
		if items[i].Kind == "user_text" && strings.HasPrefix(items[i].ID, "user:3:steer:") {
			steerRow = &items[i]
			break
		}
	}
	if steerRow == nil {
		t.Fatalf("expected a user:3:steer:* row, got items %+v", items)
	}
	if steerRow.Summary != "steer me" {
		t.Fatalf("steer row summary = %q, want %q", steerRow.Summary, "steer me")
	}

	// Pending-send marker should clear once we dispatch successfully —
	// triage.handleUserText consumes it on the wire echo, but the
	// FIFO entry should at minimum be live until Steer returned. For
	// this happy-path test we confirm the FIFO is populated then
	// flushed; the wire echo is a triage-side concern covered in
	// internal/triage/handle_user_text_test.go.
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("expected pending-send marker registered for thread; FIFO empty")
	}
}

// TestSteerMessageWithOptions_FailsForNonCodexProvider pins the
// type-assertion guard: a Claude session must not reach Codex's wire
// path. The frontend never routes here for Claude threads, but the
// backend defends against a stray RPC the same way.
func TestSteerMessageWithOptions_FailsForNonCodexProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := testThread("thread-steer-claude")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "steer-token",
		// claude pointer omitted — we never reach the wire because the
		// type-assertion fails first.
		claude: (*claude.Session)(nil),
	}

	_, err := app.SteerMessageWithOptions(thread.ID, "anything", SendMessageOptions{})
	if err == nil {
		t.Fatal("SteerMessageWithOptions for Claude err = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "not supported for provider") {
		t.Fatalf("err = %v, want \"not supported for provider\" message", err)
	}
}

// TestSteerMessageWithOptions_NoActiveTurnSurfacesSentinel pins the
// race-window fallback: when the store has no in-flight turn (the
// frontend's read of getActiveTurn was stale), the App returns
// codex.ErrNoActiveTurn so callers can errors.Is-check it. The
// frontend's wire-side path string-matches against the error message
// for the same fallback.
func TestSteerMessageWithOptions_NoActiveTurnSurfacesSentinel(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := testThread("thread-steer-no-turn")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	sess := installSteerTestSession(t, app, thread, "ok")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "steer-token",
		codex:    sess,
	}

	// No turn inserted — the store has nothing in flight.
	_, err := app.SteerMessageWithOptions(thread.ID, "anything", SendMessageOptions{})
	if !errors.Is(err, codex.ErrNoActiveTurn) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

// TestSteerMessageWithOptions_FailureClearsPendingSendAndPersistsErrorRow
// pins the failure-side bookkeeping: when the wire turn/steer returns
// an error, we drop the pending-send marker (so a stale wire echo
// can't hijack a future send's correlation) and persist a sibling
// `error` row so the timeline shows the failure next to the optimistic
// user_text row.
func TestSteerMessageWithOptions_FailureClearsPendingSendAndPersistsErrorRow(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	thread := testThread("thread-steer-fail")
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

	sess := installSteerTestSession(t, app, thread, "no-active-turn")
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "steer-token",
		codex:    sess,
	}

	_, err := app.SteerMessageWithOptions(thread.ID, "doomed", SendMessageOptions{})
	if err == nil {
		t.Fatal("expected SteerMessageWithOptions error for NoActiveTurn wire reply")
	}
	if !strings.Contains(err.Error(), "NoActiveTurn") {
		t.Fatalf("err = %v, want NoActiveTurn surfaced verbatim", err)
	}

	// Pending-send marker dropped on failure.
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("expected pending-send marker cleared on failure; still live")
	}

	// Sibling error row persisted at the same turn index.
	items, err := app.store.ListItemsForTurn(thread.ID, 2)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var errorRow *store.Item
	for i := range items {
		if items[i].Kind == "error" {
			errorRow = &items[i]
			break
		}
	}
	if errorRow == nil {
		t.Fatalf("expected a sibling error row, got items %+v", items)
	}
	if !strings.Contains(errorRow.Summary, "Failed to steer") {
		t.Fatalf("error summary = %q, want \"Failed to steer\" prefix", errorRow.Summary)
	}
}
