package app

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// queuedRow is one row in the fake app-server's provider queue.
type queuedRow struct {
	id       string
	clientID string
	text     string
}

func (r queuedRow) wire() string {
	return fmt.Sprintf(`{"id":%q,"input":[{"type":"text","text":%q}],"clientUserMessageId":%q}`,
		r.id, r.text, r.clientID)
}

// codexQueueBinaryOpts shapes the fake app-server below. The zero value is
// the ordinary queue-native server with an empty queue.
type codexQueueBinaryOpts struct {
	// rows seeds the provider queue. `list` reports what is left and `delete`
	// removes from it, so a test can assert on the queue AFTER a purge rather
	// than only on the delete frames. Nothing here can ADD a row — the whole
	// point of this revert is that AO has no `thread/queue/add` caller left —
	// so a test that wants a queued row states it, which is also how a legacy
	// row written by the 2026-08-21..24 build is modelled.
	rows []queuedRow
	// listError refuses every `thread/queue/list` with an internal error, on a
	// server that DOES have the route. The shape the rollback purge has to
	// survive: the version gate is open, so AO is expected to be able to empty
	// the queue before it truncates, and the one call that could is failing.
	listError bool
	// deleteRefused names one submission id whose `thread/queue/delete` is
	// answered with an internal error while every other row deletes normally.
	// That asymmetry is the partial purge: PurgeQueue goes row by row, so a
	// rollback can abort with some rows already out of codex's queue and
	// nothing left to run them.
	deleteRefused string
	// preQueue makes the handshake report a build from BEFORE the queue
	// family shipped, and refuses every `thread/queue/*` method with the
	// `-32601` a server with no such route answers. Both halves together:
	// the version is what closes `ThreadQueueNative`, and the refusal is what
	// turns a caller that ignored the gate into a visible failure rather than
	// a silently empty queue.
	preQueue bool
	// steerNotSteerable answers `turn/steer` with upstream's
	// `ActiveTurnNotSteerable` refusal — a turn IS running and cannot take
	// input, because it is a review or a compaction — for as long as the
	// toggle file exists. The toggle is what lets one test cover both halves
	// of that rule on ONE session: the refusal requeues the message, and the
	// next boundary drain (after the test removes the file) sends it.
	steerNotSteerable bool
}

// steerRefusalTogglePath is the file whose presence makes the fake refuse a
// steer as non-steerable. Derived from the capture path so the fixture and the
// test agree on it without a second return value.
func steerRefusalTogglePath(capturePath string) string {
	return filepath.Join(filepath.Dir(capturePath), "steer-not-steerable")
}

// writeCodexProviderQueueBinary is a fake app-server that reports a 0.149
// build on the handshake — which is what opens the `thread/queue` gate — and
// records every request frame to capturePath so a test can prove which verb
// the flush dispatcher chose.
//
// Its `thread/queue/add` branch is a TRIPWIRE, mirroring the harness mock
// (cmd/ao-mockprovider/codex_queue.go): the frame is recorded and answered with
// an error, because AO must never write to the provider's queue again and a
// regrown caller has to be a failing test rather than a duplicated turn.
//
// It also answers `thread/resume`, `thread/fork`, and `thread/turns/list`,
// because a rollback cuts
// history through a throwaway session spawned from the same binary and the
// queue purge that rides that connection is only observable on a run that gets
// all the way through. The resumed thread states no `historyMode`, so it is a
// legacy-history thread and the cut is the fork — the purge is what these
// tests are about, not which cut ran.
// Returns the binary path and the path of the queue-state file, which a test
// reads back to assert what is LEFT in the provider's queue.
func writeCodexProviderQueueBinary(
	t *testing.T, threadID, capturePath string, opts codexQueueBinaryOpts,
) (binary, queueStatePath string) {
	t.Helper()
	// The version the handshake reports, and — for a pre-queue build — the
	// blanket refusal that stands in for routes this app-server does not have.
	userAgent := "codex_cli_rs/0.149.0 (test)"
	queueRefusalBranch := ""
	if opts.preQueue {
		userAgent = "codex_cli_rs/0.147.0 (test)"
		queueRefusalBranch = `
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/'; then
        /bin/echo "$line" >> ` + fmt.Sprintf("%q", capturePath) + `
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
        continue
    fi`
	}
	steerBranch := `printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"`
	if opts.steerNotSteerable {
		toggle := steerRefusalTogglePath(capturePath)
		if err := os.WriteFile(toggle, nil, 0o644); err != nil {
			t.Fatalf("arm the steer refusal toggle: %v", err)
		}
		// Upstream's shape: one -32600 whose `error.data.codexErrorInfo` names
		// the `activeTurnNotSteerable` variant. The English message
		// deliberately says nothing about an active turn — the classifier must
		// read the typed field, not the prose.
		steerBranch = fmt.Sprintf(`if [ -f %q ]; then
            printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32600,"message":"a review is running","data":{"message":"a review is running","codexErrorInfo":{"activeTurnNotSteerable":{"turnKind":"review"}}}}}\n' "$id"
        else
            printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        fi`, toggle)
	}
	// The queue's whole state is this file: one `id|json` line per row, which
	// is what lets the mock's list shrink as deletes land without a JSON
	// parser in sh.
	queueState := filepath.Join(t.TempDir(), "queue-state")
	var seed strings.Builder
	for _, row := range opts.rows {
		fmt.Fprintf(&seed, "%s|%s\n", row.id, row.wire())
	}
	if err := os.WriteFile(queueState, []byte(seed.String()), 0o644); err != nil {
		t.Fatalf("write queue state: %v", err)
	}
	listBranch := fmt.Sprintf(
		`rows=$(/usr/bin/cut -d'|' -f2- %q | /usr/bin/paste -sd, -)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[%%s],"nextCursor":null}}\n' "$id" "$rows"`,
		queueState)
	if opts.listError {
		listBranch = `printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"thread store is busy"}}\n' "$id"`
	}
	deleteRefusalBranch := ""
	if opts.deleteRefused != "" {
		deleteRefusalBranch = fmt.Sprintf(`
        if [ "$sub" = %q ]; then
            printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32603,"message":"thread store is busy"}}\n' "$id"
            continue
        fi`, opts.deleteRefused)
	}
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"userAgent":"%s"}}\n' "$id"
        continue
    fi%s
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/add"'; then
        /bin/echo "$line" >> %q
        printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32601,"message":"the client must dispatch mid-turn messages with turn/steer"}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/list"'; then
        /bin/echo "$line" >> %q
        %s
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/queue/delete"'; then
        /bin/echo "$line" >> %q
        sub=$(/bin/echo "$line" | /usr/bin/grep -o '"queuedSubmissionId":"[^"]*"' | /usr/bin/cut -d'"' -f4)%s
        if /usr/bin/grep -q "^$sub|" %q; then
            /usr/bin/grep -v "^$sub|" %q > %q.tmp
            /bin/mv %q.tmp %q
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"deleted":true}}\n' "$id"
        else
            printf '{"jsonrpc":"2.0","id":%%s,"result":{"deleted":false}}\n' "$id"
        fi
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        /bin/echo "$line" >> %q
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        cut=$(/bin/echo "$line" | /usr/bin/grep -o '"lastTurnId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"forked-%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/turns/list"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[{"id":"%%s"}],"nextCursor":null}}\n' "$id" "$cut"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/steer"'; then
        /bin/echo "$line" >> %q
        %s
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, userAgent, queueRefusalBranch, threadID, capturePath, capturePath, listBranch, capturePath,
		deleteRefusalBranch, queueState, queueState, queueState, queueState, queueState,
		capturePath, threadID, threadID, capturePath, steerBranch)

	path := filepath.Join(t.TempDir(), "codex-provider-queue.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex provider-queue binary: %v", err)
	}
	return path, queueState
}

// readQueueState reports the submission ids still in a mock's queue.
func readQueueState(t *testing.T, statePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read queue state: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, _, _ := strings.Cut(line, "|")
		ids = append(ids, id)
	}
	return ids
}

// capturedFrames returns the request frames the fake app-server recorded, or
// nothing at all when it was never asked anything.
func capturedFrames(t *testing.T, capturePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// waitForSteerFrames blocks until the fake app-server has recorded want
// `turn/steer` frames, or fails. The boundary drain hands its batch to the
// per-thread flush worker, so the send it causes lands on another goroutine.
func waitForSteerFrames(t *testing.T, capturePath string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var steered []string
		for _, frame := range capturedFrames(t, capturePath) {
			if strings.Contains(frame, `"method":"turn/steer"`) {
				steered = append(steered, frame)
			}
		}
		if len(steered) >= want {
			return steered
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn/steer frames = %d, want %d:\n%s", len(steered), want, strings.Join(steered, "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoProviderQueueWrite is the per-test half of the repo-wide guard
// below: whatever else a dispatch did, it must not have written to the
// provider's own queue.
func assertNoProviderQueueWrite(t *testing.T, capturePath string) {
	t.Helper()
	for _, frame := range capturedFrames(t, capturePath) {
		if strings.Contains(frame, `"method":"thread/queue/add"`) ||
			strings.Contains(frame, `"method":"thread/queue/start"`) {
			t.Fatalf("a dispatch wrote to the provider's own queue; AO owns exactly one queue:\n\t%s", frame)
		}
	}
}

// foreignQueueClientID is what `codex queue --thread` writes: a v7 uuid, never
// an AO item id (rust-v0.149.0 codex-rs/tui/src/session_queue_commands.rs:48).
const foreignQueueClientID = "0199e3a1-0000-7000-8000-000000000001"

func newProviderQueueSession(t *testing.T, thread store.Thread, capturePath string) *codex.Session {
	t.Helper()
	return newProviderQueueSessionWith(t, thread, capturePath, codexQueueBinaryOpts{})
}

func newProviderQueueSessionWith(
	t *testing.T, thread store.Thread, capturePath string, opts codexQueueBinaryOpts,
) *codex.Session {
	t.Helper()
	binary, _ := writeCodexProviderQueueBinary(t, thread.ID+"-codex", capturePath, opts)
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
	codex.SetActiveTurnIDForTest(sess, "active-turn")
	return sess
}

// newProviderQueueResumeSession starts a session that RESUMES thread.SessionRef
// with the real `Config.BeforeResume` / `Config.OwnsQueuedClientID` wiring the
// app installs (app_session.go), so the test exercises the hook body and the
// handshake gate rather than calling the sunset helper by hand.
func newProviderQueueResumeSession(
	t *testing.T, app *App, thread store.Thread, capturePath string, opts codexQueueBinaryOpts,
) *codex.Session {
	t.Helper()
	binary, _ := writeCodexProviderQueueBinary(t, thread.SessionRef, capturePath, opts)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:         binary,
			WorkDir:        thread.WorkspacePath,
			ResumeThreadID: thread.SessionRef,
			OwnsQueuedClientID: func(clientID string) bool {
				return app.ownsLegacyProviderQueuedClientID(thread.ID, clientID)
			},
			BeforeResume: func(resumed *codex.Session) {
				app.sunsetLegacyProviderQueueRows(thread.ID, resumed)
			},
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// providerQueueReconcileThread is the fixture these tests share: a Codex thread
// with a triage router wired, since most assertions below are about what the
// router and the store agree on after a session start.
func providerQueueReconcileThread(t *testing.T, app *App, name string) store.Thread {
	t.Helper()
	thread := testThread(name)
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.ensureTriageRouter()
	return thread
}

// markedQueueRowMeta returns the meta the retired provider-queue dispatch
// stamped on its row: proven when the add was acked, unproven when it was not.
// Only a legacy row on disk carries either one — nothing in this build writes
// them — which is exactly what the sunset has to recognise.
func markedQueueRowMeta(t *testing.T, proven bool) string {
	t.Helper()
	var (
		meta string
		err  error
	)
	if proven {
		meta, err = itemmeta.MarkProviderQueued("")
	} else {
		meta, err = itemmeta.MarkProviderQueueHandoff("")
	}
	if err != nil {
		t.Fatalf("mark provider-queued meta: %v", err)
	}
	return meta
}

// codexUserEcho feeds triage the `item/completed userMessage` a Codex turn
// emits for input it accepted. clientID is the `clientUserMessageId` the
// app-server echoes back as `clientId`; empty models an echo that names
// nothing, which is what every foreign producer's row looks like.
func codexUserEcho(t *testing.T, app *App, threadID, providerItemID, clientID, text string) {
	t.Helper()
	meta := fmt.Sprintf(`{"provider_item_id":%q}`, providerItemID)
	if clientID != "" {
		meta = fmt.Sprintf(`{"provider_item_id":%q,"client_id":%q}`, providerItemID, clientID)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  threadID,
		Content:   text,
		Meta:      json.RawMessage(meta),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle user echo: %v", err)
	}
}

// TestFlushDispatch_CodexSteersTheMessageIntoTheRunningTurn is the app half of
// the one-queue rule, in the direction the 2026-08-21 change had inverted.
//
// A message sent while a Codex turn is running goes out as `turn/steer`, into
// the turn that is already running, and its row is placed in that turn. It
// does NOT go to the app-server's own `thread/queue/*`: that queue dispatches
// on ITS clock (`QueuedItemService::on_thread_idle`), so a message handed over
// has two dispatchers and one of them is not AO's.
//
// The second half is identity, and it is inseparable from the first. The
// pending send is registered BY the `clientUserMessageId` the steer stamped,
// so the running turn's echo lands on the row the user is already looking at.
// An entry waiting to be named is invisible to an id-less echo — proven here
// by feeding one — which is what stops a foreign producer's message from
// consuming this user's row.
func TestFlushDispatch_CodexSteersTheMessageIntoTheRunningTurn(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := providerQueueReconcileThread(t, app, "flush-codex-steer")

	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-7", ThreadID: thread.ID, TurnIndex: 7, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	// The running turn already holds content, so "the active turn" and "the
	// next turn" are different numbers and the placement assertion means
	// something.
	if err := app.store.InsertItem(store.Item{
		ID: "assistant:7:0", ThreadID: thread.ID, TurnIndex: 7, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "streaming",
		Summary: "working", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSession(t, thread, capture)
	if !codexSess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = false; the 0.149 handshake should have opened the gate")
	}
	sess := session{Provider: string(provider.Codex), Codex: codexSess}
	app.sessionManager().put(thread.ID, sess)

	// Placement: the RUNNING turn. A steered message is context for the turn
	// already underway, so its row belongs above that turn's answer.
	got, active, err := app.resolveFlushTurnPlacement(thread.ID, sess)
	if err != nil {
		t.Fatalf("resolveFlushTurnPlacement: %v", err)
	}
	if !active {
		t.Error("activeAtResolution = false; a steered message joins the running turn")
	}
	if got != 7 {
		t.Errorf("turn index = %d, want 7 (the running turn)", got)
	}

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:steered", Message: "steered", Payload: json.RawMessage(`{}`)},
	})

	frames := capturedFrames(t, capture)
	assertNoProviderQueueWrite(t, capture)
	steers := 0
	for _, frame := range frames {
		if !strings.Contains(frame, `"method":"turn/steer"`) {
			continue
		}
		steers++
		if !strings.Contains(frame, `"clientUserMessageId":"user:7:flush:1"`) {
			t.Errorf("steer carried no client id naming the row it was minted for:\n\t%s", frame)
		}
	}
	if steers != 1 {
		t.Fatalf("turn/steer frames = %d, want exactly 1:\n%s", steers, strings.Join(frames, "\n"))
	}

	// The pending entry is registered BY that id, so an echo that names
	// nothing cannot take it: it is somebody else's message and lands as its
	// own row.
	codexUserEcho(t, app, thread.ID, "codex-foreign", "", "not mine")
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("an id-less echo consumed a send registered by client id; a foreign message would render as the user's own")
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if found {
		t.Fatal("the id-less echo persisted AO's deferred row")
	}

	// And the echo that DOES name it resolves the row, in the running turn.
	codexUserEcho(t, app, thread.ID, "codex-ours", "user:7:flush:1", "steered")
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("the matching echo did not consume the pending send")
	}
	row, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1")
	if err != nil || !found {
		t.Fatalf("the steered row never persisted: found=%v err=%v", found, err)
	}
	if row.Summary != "steered" {
		t.Errorf("row text = %q, want the steered message", row.Summary)
	}
	if row.TurnIndex != 7 {
		t.Errorf("row turn index = %d, want the running turn", row.TurnIndex)
	}
}

// TestFlushDispatch_ANonSteerableTurnKeepsTheMessageQueued is the other
// dispatch outcome the revert has to get right.
//
// `ActiveTurnNotSteerable` means a turn IS running and cannot take input — a
// review or a compaction. There is no verb that delivers the message now:
// re-dispatching as a fresh `turn/start` would interleave it with the running
// review, and `thread/queue/add` is not an option AO has any more. So nothing
// is sent, the message goes back on AO's own queue, and the boundary drain the
// review's own completion raises is what sends it.
//
// The refusal is deliberately NOT an error row. "The queue is waiting for the
// review to finish" is the queue working.
func TestFlushDispatch_ANonSteerableTurnKeepsTheMessageQueued(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := providerQueueReconcileThread(t, app, "flush-codex-not-steerable")

	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-7", ThreadID: thread.ID, TurnIndex: 7, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		steerNotSteerable: true,
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Codex: codexSess, Token: "not-steerable",
	})

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:held", Message: "held for the review", Payload: json.RawMessage(`{}`)},
	})

	// Back on AO's queue, whole: same item id, same text, nothing persisted
	// and nothing pending.
	queued := app.triage.QueuedFlushItems(thread.ID)
	if len(queued) != 1 || queued[0].ID != "queue:held" || queued[0].Message != "held for the review" {
		t.Fatalf("queued items after the refusal = %+v, want the message back on AO's queue", queued)
	}
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("a pending send survived a dispatch that sent nothing; its echo can never arrive")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Kind == "error" {
			t.Errorf("the refusal persisted an error row (%q); a queue waiting for a review is not a failure", item.Summary)
		}
		if item.Kind == "user_text" {
			t.Errorf("the refusal left a user row (%s) for a message the provider never received", item.ID)
		}
	}
	assertNoProviderQueueWrite(t, capture)

	// The review ends. Its turn completion is what raises the boundary drain,
	// and the drain sends the message that was waiting for it — once.
	if err := os.Remove(steerRefusalTogglePath(capture)); err != nil {
		t.Fatalf("release the steer refusal: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     thread.ID,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn completion: %v", err)
	}

	// The drain hands the batch to the per-thread flush worker, so the send
	// lands on another goroutine.
	steered := waitForSteerFrames(t, capture, 2)
	if queued := app.triage.QueuedFlushItems(thread.ID); len(queued) != 0 {
		t.Fatalf("queued items after the boundary drain = %+v, want the message dispatched", queued)
	}
	assertNoProviderQueueWrite(t, capture)
	// Two attempts, one delivery: the refused one carried nothing forward, and
	// the retry is the only send the user's message ever made.
	if len(steered) != 2 {
		t.Fatalf("turn/steer frames = %d, want the refused attempt plus one retry:\n%s",
			len(steered), strings.Join(steered, "\n"))
	}
	if !strings.Contains(steered[1], `"clientUserMessageId":`) {
		t.Errorf("the retry carried no client id; its echo would render as injected provider context:\n\t%s", steered[1])
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Error("the retry registered no pending send; the running turn's echo has no row to claim")
	}
}

// TestCodexSessionStartSunsetsTheLegacyProviderQueueRows is the one thing AO
// still does about rows in the provider's own queue: take its own back out.
//
// The 2026-08-21..24 build handed mid-turn messages to `thread/queue/add`, and
// those rows are durable in codex's SQLite — an idle hook runs them minutes or
// days later, as a turn this connection never asked for, against a thread that
// has moved on. Nothing in this build could attribute such a turn any more. So
// a session start deletes AO's own rows and hands the text back to the person
// who typed it, and the ordering is load-bearing: the DELETE lands before the
// restore, or the composer offers a re-send of a message codex still holds.
//
// The three rows that must NOT move are the point of the fixture: one that
// already ran (proven and absent from the queue — restoring it would offer a
// re-send of something the transcript already answers), and a foreign
// producer's submission, which AO neither deletes nor claims here.
func TestCodexSessionStartSunsetsTheLegacyProviderQueueRows(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-legacy-sunset")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	// Still queued: the provider has it and will run it unless this removes it.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"still queued", markedQueueRowMeta(t, true))
	// Proven and absent: it already dispatched under the old build. History.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:2", 7,
		"already ran", markedQueueRowMeta(t, true))
	// Unproven and absent: the add never landed, so nothing anywhere holds it.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:3", 7,
		"never taken", markedQueueRowMeta(t, false))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	sess := newProviderQueueResumeSession(t, app, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-mine", clientID: "user:7:flush:1", text: "still queued"},
			{id: "sub-theirs", clientID: foreignQueueClientID, text: "somebody else's"},
		},
	})
	if !sess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = false on a 0.149 handshake")
	}

	// The queue keeps exactly what is not AO's.
	binaryQueueState := ""
	for _, frame := range capturedFrames(t, capture) {
		if strings.Contains(frame, `"method":"thread/queue/delete"`) {
			binaryQueueState = frame
		}
	}
	if binaryQueueState == "" {
		t.Fatalf("no thread/queue/delete reached the app-server:\n%s", strings.Join(capturedFrames(t, capture), "\n"))
	}
	if !strings.Contains(binaryQueueState, `"queuedSubmissionId":"sub-mine"`) {
		t.Errorf("the delete did not name AO's own submission:\n\t%s", binaryQueueState)
	}
	if strings.Contains(binaryQueueState, "sub-theirs") {
		t.Errorf("the sunset deleted a foreign producer's queued message:\n\t%s", binaryQueueState)
	}

	// The row it removed is out of the timeline and back in the composer,
	// alongside the one the provider never took.
	for _, id := range []string{"user:7:flush:1", "user:7:flush:3"} {
		if _, found, err := app.store.GetThreadItem(thread.ID, id); err != nil {
			t.Fatalf("GetThreadItem %s: %v", id, err)
		} else if found {
			t.Errorf("row %s is still in the timeline; nothing will ever run it", id)
		}
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:2"); err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	} else if !found {
		t.Error("a message the provider already ran was pulled out of the timeline")
	}

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	for _, want := range []string{"still queued", "never taken"} {
		if !strings.Contains(draft.Content, want) {
			t.Errorf("draft = %q, want it to contain %q", draft.Content, want)
		}
	}
	if strings.Contains(draft.Content, "already ran") {
		t.Errorf("draft = %q, want no copy of a message that already ran", draft.Content)
	}
	if strings.Contains(draft.Content, "somebody else's") {
		t.Errorf("draft = %q; a foreign producer's message was put in this user's composer", draft.Content)
	}

	restored := emittedQueueRestored(rec)
	if len(restored) != 1 || restored[0].Reason != queueRestoredReasonNeverQueued {
		t.Fatalf("queue_restored events = %+v, want one with the never-taken reason", restored)
	}
	if len(restored[0].UserItemIDs) != 2 {
		t.Errorf("restored ids = %v, want both rows nothing else will run", restored[0].UserItemIDs)
	}
}

// TestSunsetLeavesTheRowsAPreQueueAppServerCannotSee is the version-downgrade
// half. A pre-0.148 app-server has no `thread/queue/*` at all: it can neither
// list nor delete, so the rows stay exactly where the newer build left them for
// a later session to retire. Restoring them here would hand the user a draft of
// a message a newer Codex still holds and will run.
func TestSunsetLeavesTheRowsAPreQueueAppServerCannotSee(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-legacy-downgrade")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"still queued", markedQueueRowMeta(t, true))

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	sess := newProviderQueueResumeSession(t, app, thread, capture, codexQueueBinaryOpts{
		preQueue: true,
		// Seeded but unreachable, which is the point: the row exists and
		// nothing on this connection can see it.
		rows: []queuedRow{{id: "sub-mine", clientID: "user:7:flush:1", text: "still queued"}},
	})
	if sess.ThreadQueueNative() {
		t.Fatal("ThreadQueueNative() = true on a 0.147 handshake; the gate must fail closed")
	}

	// Nothing was asked of the provider queue. A call that ignored the gate
	// would be a -32601 in the capture, not a silent no-op.
	for _, frame := range capturedFrames(t, capture) {
		if strings.Contains(frame, `"method":"thread/queue/`) {
			t.Fatalf("a pre-0.148 session called a thread/queue method:\n\t%s", frame)
		}
	}
	row, found, err := app.store.GetThreadItem(thread.ID, "user:7:flush:1")
	if err != nil || !found {
		t.Fatalf("the row was dropped: found=%v err=%v", found, err)
	}
	if !itemmeta.IsProviderQueued(row.Meta) {
		t.Errorf("row meta = %q, want the provider-queued marker retained", row.Meta)
	}
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if strings.TrimSpace(draft.Content) != "" {
		t.Errorf("draft = %q; a message a newer Codex still holds must not come back as a draft", draft.Content)
	}
	if restored := emittedQueueRestored(rec); len(restored) != 0 {
		t.Errorf("queue restored events = %+v, want none", restored)
	}
}

// TestOwnsLegacyProviderQueuedClientIDRequiresThisAppsRow pins what
// `codex.Config.OwnsQueuedClientID` answers from.
//
// AO's row ids (`user:<turn>:flush:<n>`) are deterministic, so they are not a
// credential: a second Agent Overflow profile against the same Codex home
// mints exactly the same ones, and anything speaking `thread/queue/add` may
// simply supply one. Recognising the GRAMMAR would let a stranger's submission
// be deleted as AO's and their text announced as this user's own. The persisted
// row is the token instead — and only a row this app marked provider-queued
// counts, because that is the only thing the retired add path ever wrote.
func TestOwnsLegacyProviderQueuedClientIDRequiresThisAppsRow(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-ownership")

	insertUserItemWithMeta(t, app.store, thread.ID, "user:7:flush:1", 7,
		"mine", markedQueueRowMeta(t, true))
	// A perfectly ordinary sent message. It was never in anyone's queue, so a
	// submission wearing its id is not this app's.
	insertUserItem(t, app.store, thread.ID, "user:8", 8, "just sent")

	cases := []struct {
		name     string
		clientID string
		want     bool
	}{
		{"a row this app marked provider-queued", "user:7:flush:1", true},
		{"AO's id grammar with no row behind it", "user:9:flush:1", false},
		{"an unmarked row of this app's", "user:8", false},
		{"a foreign producer's uuid", foreignQueueClientID, false},
		{"nothing at all", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := app.ownsLegacyProviderQueuedClientID(thread.ID, tc.clientID); got != tc.want {
				t.Errorf("ownsLegacyProviderQueuedClientID(%q) = %v, want %v", tc.clientID, got, tc.want)
			}
		})
	}
	// And ownership is per THREAD: the same id on another thread is not this
	// thread's row.
	other := providerQueueReconcileThread(t, app, "flush-queue-ownership-other")
	if app.ownsLegacyProviderQueuedClientID(other.ID, "user:7:flush:1") {
		t.Error("a row on another thread answered for this one")
	}
}

// TestNoProductionCodePathReachesTheProviderQueueWrites is the grep-level half
// of the rule the whole revert exists for: AO dispatches every mid-turn message
// with `turn/steer` and must NEVER write to the app-server's own queue, not
// even as a fallback. Two dispatchers for one message is how a queued prompt
// runs twice, hours after the user typed it.
//
// The wrapper is already gone (`codex.Session` has no QueueAdd), so this is a
// guard against it growing back somewhere the compiler cannot see — a raw
// `sendRequest`, a method name assembled from a constant. It reads STRING
// LITERALS out of the AST rather than grepping text, so the many comments that
// legitimately explain the retired path do not have to be phrased around it.
//
// Two files are exempt by design and named here so the exemption stays honest:
// the harness mock, whose refusal of both methods is what turns a regrown
// caller into a failing harness run, and the fake app-server in this package,
// which mirrors it.
func TestNoProductionCodePathReachesTheProviderQueueWrites(t *testing.T) {
	forbidden := []string{"thread/queue/add", "thread/queue/start"}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "frontend": true, "e2e": true,
		"docs": true, ".claude": true, "dist": true, "bin": true, "build": true,
	}
	// The mock's tripwire IS the string, and it must keep being the string —
	// asserted below rather than merely skipped.
	tripwire := filepath.Join("cmd", "ao-mockprovider", "codex_queue.go")

	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || path == tripwire {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, method := range forbidden {
				if strings.Contains(lit.Value, method) {
					t.Errorf("%s: a string literal names %s; Agent Overflow dispatches mid-turn messages with turn/steer and must never write to the provider's queue",
						fset.Position(lit.Pos()), method)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	raw, err := os.ReadFile(tripwire)
	if err != nil {
		t.Fatalf("read the mock's queue tripwire: %v", err)
	}
	for _, method := range forbidden {
		if !strings.Contains(string(raw), method) {
			t.Errorf("%s no longer refuses %s; the exemption above is now hiding nothing", tripwire, method)
		}
	}
}

// TestConversationRollbackForgetsTheProviderThreadCostWhenTheThreadMoves is
// finding 16. `provider_thread_cost` is keyed by the AO thread id but
// DESCRIBES the Codex thread the figure was read from. A rollback that forks
// repoints this thread at a new Codex thread carrying a shorter history, and
// the turn-0 branch drops the reference entirely — either way the stored total
// belongs to a thread this one no longer is, and nothing would correct it
// until some later settled turn happened to re-read.
//
// The in-place revert is the case that must NOT forget, and it is excluded by
// construction rather than by a second fixture: it keeps the thread id, so the
// forget is gated on SessionRef actually changing.
func TestConversationRollbackForgetsTheProviderThreadCostWhenTheThreadMoves(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-cost-forget", "codex", workspace)
	thread.SessionRef = "provider-cost-forget"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	seedMessageAnchor(t, app.store, thread.ID, "user:0", 0, "", "")
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID: thread.ID,
		// The provider thread the figure was read from (migration v68). It is
		// what the rollback moves the thread away from, and what makes the row
		// unreadable from that moment whether or not the delete lands.
		SessionRef:    thread.SessionRef,
		Provider:      string(provider.Codex),
		CostUSDMicros: 4_200_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}

	// Rolling back to the row that opens turn 0 keeps no provider history at
	// all: the reference clears and the next send starts a fresh Codex thread.
	// No app-server is spawned for it, which is also why this needs no binary.
	if err := rollbackToMessage(app, thread.ID, "user:0"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	rolled, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if rolled.SessionRef != "" {
		t.Fatalf("SessionRef after a turn-0 rollback = %q, want cleared", rolled.SessionRef)
	}
	if _, found, err := app.store.GetProviderThreadCost(thread.ID); err != nil {
		t.Fatalf("GetProviderThreadCost: %v", err)
	} else if found {
		t.Error("the provider cost survived a rollback that dropped the Codex thread it described")
	}
}

// TestConversationRollbackPurgesTheProviderQueue is finding 7. AO's own
// flushqueue is cleared in process by the rollback, but a row in the
// PROVIDER's queue is durable in codex's SQLite: it outlives the session stop
// and the app-server's idle hook dispatches it on the next resume — re-running,
// onto a thread the user just truncated, a message they rolled back. Since AO
// stopped adding rows, what this finds is normally a foreign producer's; it is
// dropped anyway, because the hazard is identical and there is no way to leave
// it queued that avoids it. The purge has to run while the connection is still
// live, so it happens BEFORE the stop.
func TestConversationRollbackPurgesTheProviderQueue(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-queue-purge", "codex", workspace)
	thread.SessionRef = "provider-queue-purge"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary, queueState := writeCodexProviderQueueBinary(t, thread.SessionRef, capture, codexQueueBinaryOpts{
		// One legacy row AO left behind, one a `codex queue --thread` run
		// wrote. Both would re-run on resume, so both go.
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:1:flush:1", text: "mine"},
			{id: "sub-2", clientID: foreignQueueClientID, text: "theirs"},
		},
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         binary,
		Model:          "test-model",
		WorkDir:        workspace,
		ResumeThreadID: thread.SessionRef,
		OwnsQueuedClientID: func(clientID string) bool {
			return app.ownsLegacyProviderQueuedClientID(thread.ID, clientID)
		},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "queue-purge-token",
		Codex:    sess,
	})

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	var deleted []string
	for _, frame := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(frame, `"method":"thread/queue/delete"`) {
			continue
		}
		for _, id := range []string{"sub-1", "sub-2"} {
			if strings.Contains(frame, `"queuedSubmissionId":"`+id+`"`) {
				deleted = append(deleted, id)
			}
		}
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted queued submissions = %v, want both rows dropped before the stop:\n%s", deleted, raw)
	}
	if left := readQueueState(t, queueState); len(left) != 0 {
		t.Fatalf("provider queue after the rollback = %v, want empty", left)
	}
	// And the session really is gone afterwards — the purge cannot have
	// happened after the stop, because there is no connection to run it on.
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("Codex session still active after the rollback")
	}
}

// TestConversationRollbackPurgesTheProviderQueueWithNoLiveSession is the
// no-session half of the same hazard. Nothing is running to purge through, but
// the rollback has to resume the thread anyway to cut its history — and an
// in-place revert keeps the thread id, so a row left behind runs a rolled-back
// message on the user's next send. The purge rides the connection the cut
// already opened; nothing extra is spawned.
func TestConversationRollbackPurgesTheProviderQueueWithNoLiveSession(t *testing.T) {
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-queue-purge-cold", "codex", workspace)
	thread.SessionRef = "provider-queue-purge-cold"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	binary, queueState := writeCodexProviderQueueBinary(t, thread.SessionRef, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-1", clientID: "user:1:flush:1", text: "mine"},
			{id: "sub-2", clientID: foreignQueueClientID, text: "theirs"},
		},
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	// No session in the manager at all: the throwaway resume the cut opens is
	// the only connection this rollback gets.
	if _, ok := app.activeCodexSession(thread.ID); ok {
		t.Fatal("the fixture installed a session; this case is the cold one")
	}

	if err := rollbackToMessage(app, thread.ID, "user:1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if left := readQueueState(t, queueState); len(left) != 0 {
		t.Fatalf("provider queue after a no-session rollback = %v, want empty", left)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured requests: %v", err)
	}
	// The purge went first on that connection — ahead of the RESUME, not just
	// ahead of the cut. Resuming is what LOADS the thread, and a loaded
	// thread's idle hook is what dispatches the queue, so a purge issued after
	// it is racing a turn it can no longer stop.
	frames := strings.Split(strings.TrimSpace(string(raw)), "\n")
	firstDelete, firstResume, firstCut := -1, -1, -1
	for i, frame := range frames {
		switch {
		case firstDelete < 0 && strings.Contains(frame, `"method":"thread/queue/delete"`):
			firstDelete = i
		case firstResume < 0 && strings.Contains(frame, `"method":"thread/resume"`):
			firstResume = i
		case firstCut < 0 && (strings.Contains(frame, `"method":"thread/fork"`) ||
			strings.Contains(frame, `"method":"thread/revert"`)):
			firstCut = i
		}
	}
	if firstDelete < 0 {
		t.Fatalf("no queue delete reached the cut connection:\n%s", raw)
	}
	if firstResume >= 0 && firstDelete > firstResume {
		t.Errorf("the purge ran after thread/resume (delete at %d, resume at %d); the thread was already loaded and could dispatch a queued row",
			firstDelete, firstResume)
	}
	if firstCut >= 0 && firstDelete > firstCut {
		t.Errorf("the purge ran after the cut (delete at %d, cut at %d); a loaded thread can dispatch a queued row first",
			firstDelete, firstCut)
	}
}

// TestRollbackRefusesWhenTheProviderQueueCannotBePurged is finding H3.
//
// A queued row survives `stopSession` in codex's own SQLite and its idle hook
// dispatches it on the next resume. Truncating history over a queue that could
// not be emptied therefore arms the removed messages to re-run against the
// shortened thread, silently and possibly days later. Refusing is visible and
// the user can retry; the replay is neither.
func TestRollbackRefusesWhenTheProviderQueueCannotBePurged(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-purge-refusal")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{listError: true})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Codex: codexSess, Token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "queued") {
		t.Errorf("error = %v, want it to name the queued messages as the reason", err)
	}

	// Nothing was mutated: the refusal lands before the session stop, so the
	// user's retry starts from where they were.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after the refused rollback = %d, want both still there: %+v", len(items), items)
	}
	if _, ok := app.sessionManager().get(thread.ID); !ok {
		t.Error("the session was stopped by a rollback that refused; the refusal must land first")
	}
}

// TestRollbackRefusalRestoresTheMessagesThePartialPurgeAlreadyDeleted is K1 at
// the app boundary.
//
// The purge deletes row by row. With A and B queued, deleting A can succeed
// and B fail — and the caller then aborts the rollback and leaves history
// untouched, which is what the refusal promises. But A is already out of
// codex's queue: no idle hook will dispatch it, no echo will claim its row,
// and every recovery path steps around a row marked provider-queued. Without
// this, abandoning a rollback silently eats a message the user queued.
func TestRollbackRefusalRestoresTheMessagesThePartialPurgeAlreadyDeleted(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-partial-purge")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	// A legacy row the old build handed to the provider's queue, proven and
	// unclaimed — the shape the sunset would have retired at the next session
	// start, caught here by a rollback instead.
	insertUserItemWithMeta(t, app.store, thread.ID, "user:2:flush:1", 2,
		"queued and then purged", markedQueueRowMeta(t, true))
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	var restored []QueueRestoredEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:queue_restored" {
			return
		}
		if evt, ok := data.(QueueRestoredEvent); ok {
			restored = append(restored, evt)
		}
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			{id: "sub-mine", clientID: "user:2:flush:1", text: "queued and then purged"},
			{id: "sub-stuck", clientID: foreignQueueClientID, text: "somebody else's"},
		},
		deleteRefused: "sub-stuck",
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Codex: codexSess, Token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "composer") {
		t.Errorf("error = %v, want it to say where the already-deleted message went", err)
	}

	// History is untouched, and the session is still up: the refusal lands
	// before either mutation.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.ID == "user:2:flush:1" {
			t.Fatal("the purged row is still in the timeline; nothing will ever run it")
		}
	}
	if len(items) != 2 {
		t.Fatalf("items after the refused rollback = %d, want the two history rows: %+v", len(items), items)
	}
	if _, ok := app.sessionManager().get(thread.ID); !ok {
		t.Error("the session was stopped by a rollback that refused")
	}

	// The message the purge deleted is back where the user can send it.
	draft, found, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !found {
		t.Fatalf("thread draft after the refused rollback: found=%v err=%v", found, err)
	}
	if !strings.Contains(draft.Content, "queued and then purged") {
		t.Fatalf("draft = %q, want the purged message returned to the composer", draft.Content)
	}
	if len(restored) != 1 || restored[0].Reason != queueRestoredReasonPurgeAborted {
		t.Fatalf("queue_restored events = %+v, want one with the purge-aborted reason", restored)
	}
	if len(restored[0].UserItemIDs) != 1 || restored[0].UserItemIDs[0] != "user:2:flush:1" {
		t.Fatalf("restored ids = %v, want exactly AO's own purged row", restored[0].UserItemIDs)
	}
}

// TestPurgeRefusalDoesNotRestoreAForeignSubmissionItDeleted is the other half
// of the same rule. A foreign producer's row is deleted by the purge too — it
// carries the same replay hazard — but AO cannot put it back: there is no
// `thread/queue/add` caller left anywhere in this app, and re-adding it would
// render that author's text as this user's own message. So it is reported and
// counted, never restored.
func TestPurgeRefusalDoesNotRestoreAForeignSubmissionItDeleted(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := providerQueueReconcileThread(t, app, "flush-queue-foreign-purge")
	thread.SessionRef = thread.ID + "-codex"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	var restored []QueueRestoredEvent
	app.testEmitHook = func(name string, data any) {
		if name != "provider:queue_restored" {
			return
		}
		if evt, ok := data.(QueueRestoredEvent); ok {
			restored = append(restored, evt)
		}
	}

	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	codexSess := newProviderQueueSessionWith(t, thread, capture, codexQueueBinaryOpts{
		rows: []queuedRow{
			// Deleted first, and not this app's: no store row backs it.
			{id: "sub-foreign", clientID: foreignQueueClientID, text: "somebody else's"},
			{id: "sub-stuck", clientID: "0199e3a1-0000-7000-8000-000000000002", text: "also theirs"},
		},
		deleteRefused: "sub-stuck",
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex), Codex: codexSess, Token: "live",
	})

	err := rollbackToMessage(app, thread.ID, "user:1")
	if err == nil {
		t.Fatal("the rollback truncated history over a queue it could not empty")
	}
	if !strings.Contains(err.Error(), "not added by Agent Overflow") &&
		!strings.Contains(err.Error(), "outside Agent Overflow") {
		t.Errorf("error = %v, want it to say the dropped message was not this app's and is unrecoverable", err)
	}
	if len(restored) != 0 {
		t.Fatalf("queue_restored events = %+v, want none; a foreign submission has no AO row to restore", restored)
	}
	draft, found, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if found && strings.Contains(draft.Content, "somebody else's") {
		t.Fatalf("draft = %q; a foreign producer's message was put in this user's composer", draft.Content)
	}
}
