package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// claudeMergeEchoEvent builds the EventUserText a replayed Claude envelope
// produces for the given content blocks, by running the real parser over a
// real wire line. Used to feed the two shapes a queue-boundary merge emits:
// the merged-away member's own blocks (no `timestamp`), and the survivor's ack
// carrying every member's blocks concatenated.
func claudeMergeEchoEvent(t *testing.T, threadID, uuid string, texts []string, withTimestamp bool) provider.ProviderEvent {
	t.Helper()
	blocks := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	envelope := map[string]any{
		"type":     "user",
		"isReplay": true,
		"uuid":     uuid,
		"message":  map[string]any{"role": "user", "content": blocks},
	}
	if withTimestamp {
		envelope["timestamp"] = "2026-09-16T12:00:00.000Z"
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal echo envelope: %v", err)
	}
	events, err := claude.NewParser().ParseLine(threadID, line)
	if err != nil {
		t.Fatalf("parse echo envelope: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("echo envelope produced %d events, want 1: %+v", len(events), events)
	}
	return events[0]
}

// TestDispatchFlush_Claude_FoldsCLIMergeAcrossDrains is the end-to-end pin for
// the residual merge AO cannot join at dispatch. Two messages are flushed at
// SEPARATE drains — so each leaves AO as its own envelope under its own uuid —
// and the CLI, which had not consumed the first when the second's drain hit
// the turn boundary, merges them into one transcript entry under the second
// uuid. AO folds its two rows into that one message, and a revert to it then
// cuts SQLite and the provider transcript at the same place.
func TestDispatchFlush_Claude_FoldsCLIMergeAcrossDrains(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)

	const sessionID = "cli-merge-fold-session"
	thread := testThread("flush-claude-cli-merge")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	thread.SessionRef = sessionID
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:3", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "original prompt", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user item: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := app.store.AppendItem(store.Item{
		ID: "toolu_sleep", ThreadID: thread.ID, TurnIndex: 3,
		Kind: "tool_call", Role: "assistant", Status: "running",
		Summary: "sleep 10", CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
	}

	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: workspace},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	messages := []string{"first queued", "second queued"}
	sendIDs := []string{"send-a", "send-b"}
	// TWO drains, one message each: the join at dispatch does not apply, so
	// each message reaches the CLI as its own envelope under its own uuid.
	for i, message := range messages {
		app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
			ID:      fmt.Sprintf("queue:%d", i),
			Message: message,
			Payload: flushQueuePayloadJSON(t, flushQueuePayload{
				SendID:                 sendIDs[i],
				ExpandComposerCommands: true,
			}),
			EnqueuedAt: now + int64(2+i),
		}})
	}

	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 2)
	if len(envelopes) != 2 {
		t.Fatalf("stdin envelopes: got %d, want one per drain:\n%+v", len(envelopes), envelopes)
	}
	uuids := make([]string, 0, 2)
	for i, envelope := range envelopes {
		id, _ := envelope["uuid"].(string)
		if id == "" {
			t.Fatalf("envelope %d carries no uuid: %+v", i, envelope)
		}
		uuids = append(uuids, id)
	}

	rows := flushRowsForTurn(t, app, thread.ID, 3)
	if len(rows) != 2 {
		t.Fatalf("flush rows before the fold: got %d, want one per separately drained message: %+v", len(rows), rows)
	}
	firstRowID, survivorRowID := rows[0].ID, rows[1].ID

	// The CLI's boundary drain: the first message's uuid is acknowledged on
	// stdout with its OWN blocks and never written to the session file, then
	// the batch is acknowledged under the LAST uuid with both members'
	// blocks concatenated.
	if err := app.triage.Handle(claudeMergeEchoEvent(t, thread.ID, uuids[0], messages[:1], false)); err != nil {
		t.Fatalf("merged-away echo: %v", err)
	}
	if err := app.triage.Handle(claudeMergeEchoEvent(t, thread.ID, uuids[1], messages, true)); err != nil {
		t.Fatalf("survivor echo: %v", err)
	}

	// ONE row left, holding both texts and both send ids.
	rows = flushRowsForTurn(t, app, thread.ID, 3)
	if len(rows) != 1 {
		t.Fatalf("flush rows after the fold: got %d, want the one row the transcript holds: %+v", len(rows), rows)
	}
	survivor := rows[0]
	if survivor.ID != survivorRowID {
		t.Errorf("surviving row is %s, want the message the transcript names (%s)", survivor.ID, survivorRowID)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, firstRowID); err != nil || found {
		t.Fatalf("merged-away row %s still present (found=%v err=%v)", firstRowID, found, err)
	}
	wantJoined := strings.Join(messages, usermessage.JoinSeparator)
	if survivor.Summary != wantJoined {
		t.Errorf("survivor summary:\n got %q\nwant %q", survivor.Summary, wantJoined)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	if !slices.Equal(meta.JoinedSendIDs, sendIDs) {
		t.Errorf("joinedSendIds: got %v, want %v", meta.JoinedSendIDs, sendIDs)
	}
	// A retry of either message must resolve to the folded row rather than
	// send a second copy.
	for _, sendID := range sendIDs {
		accepted, _, found, err := app.triage.FindAcceptedUserMessageBySendID(thread.ID, sendID)
		if err != nil {
			t.Fatalf("FindAcceptedUserMessageBySendID(%s): %v", sendID, err)
		}
		if !found || accepted.ID != survivor.ID {
			t.Errorf("send id %s resolves to %q (found=%v), want the folded row %s", sendID, accepted.ID, found, survivor.ID)
		}
	}
	// ONE anchor, under the uuid the transcript actually contains.
	if _, found, err := app.store.GetMessageAnchor(thread.ID, firstRowID); err != nil || found {
		t.Errorf("merged-away row kept its anchor (found=%v err=%v)", found, err)
	}
	anchor, found, err := app.store.GetMessageAnchor(thread.ID, survivor.ID)
	if err != nil || !found {
		t.Fatalf("survivor has no anchor (found=%v err=%v)", found, err)
	}
	if anchor.ProviderUserMessageID != uuids[1] {
		t.Errorf("anchor provider uuid: got %q, want %q", anchor.ProviderUserMessageID, uuids[1])
	}

	// Response-turn accounting. Each drain reserved its own response turn
	// index while its send was pending, but the merge means the provider
	// runs ONE turn. The survivor keeps ITS display position in the turn it
	// was queued into, and no turn row is materialized for either
	// reservation — the provider's own turn start owns that.
	if survivor.TurnIndex != 3 {
		t.Errorf("survivor display turn = %d, want the turn it was queued into (3)", survivor.TurnIndex)
	}
	if opened := turnsAbove(t, app, thread.ID, 3); len(opened) != 0 {
		t.Errorf("turns opened above the queued-into turn = %+v, want none before the provider starts one", opened)
	}
	// Both reservations are released, so the next send is placed at the
	// plain next index rather than skipping past a reservation the merge
	// consumed.
	if index, pending := app.triage.MaxPendingSendTurnIndex(thread.ID); pending {
		t.Errorf("pending send reservation survives the fold at turn %d", index)
	}
	next, err := app.nextSendTurnIndex(thread.ID)
	if err != nil {
		t.Fatalf("nextSendTurnIndex: %v", err)
	}
	if next != 4 {
		t.Errorf("next send turn index = %d, want 4", next)
	}

	// The transcript the CLI wrote: ONE user entry, under the LAST uuid,
	// carrying both members' blocks. The first uuid appears nowhere.
	writeClaudeProjectSession(t, home, workspace, sessionID, fmt.Sprintf(
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":%q,"message":{"role":"user","content":"original prompt"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":%q,"message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}
{"type":"user","uuid":%q,"parentUuid":"a0","sessionId":%q,"message":{"role":"user","content":[{"type":"text","text":%q},{"type":"text","text":%q}]}}
{"type":"assistant","uuid":"a1","parentUuid":%q,"sessionId":%q,"message":{"role":"assistant","content":[{"type":"text","text":"answer to both"}]}}
`, sessionID, sessionID, uuids[1], sessionID, messages[0], messages[1], uuids[1], sessionID))

	if err := rollbackToMessage(app, thread.ID, survivor.ID); err != nil {
		t.Fatalf("rollback to the folded row: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want a sliced fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"original prompt", "working on it"},
		[]string{messages[0], messages[1], "answer to both"})
	if _, found, err := app.store.GetThreadItem(thread.ID, survivor.ID); err != nil || found {
		t.Fatalf("folded row still present after rollback (found=%v err=%v)", found, err)
	}
	for _, kept := range []string{"user:3", "toolu_sleep"} {
		if _, found, err := app.store.GetThreadItem(thread.ID, kept); err != nil || !found {
			t.Fatalf("rollback removed %s from the shared turn (found=%v err=%v)", kept, found, err)
		}
	}
	remaining, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn after rollback: %v", err)
	}
	for _, it := range remaining {
		for _, message := range messages {
			if strings.Contains(it.Summary, message) {
				t.Errorf("row %s still holds %q after rollback: %q", it.ID, message, it.Summary)
			}
		}
	}
	// SQLite cuts where the session did: nothing above the cut survives.
	if left := turnsAbove(t, app, thread.ID, 3); len(left) != 0 {
		t.Errorf("turns above the cut survive the rollback: %+v", left)
	}
}

// turnsAbove lists the thread's turn rows past turnIndex, ascending.
func turnsAbove(t *testing.T, app *App, threadID string, turnIndex int) []store.Turn {
	t.Helper()
	turns, err := app.store.ListRecentTurns(threadID, 16)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	var above []store.Turn
	for _, turn := range turns {
		if turn.TurnIndex > turnIndex {
			above = append(above, turn)
		}
	}
	slices.SortFunc(above, func(a, b store.Turn) int { return a.TurnIndex - b.TurnIndex })
	return above
}

// flushRowsForTurn lists the turn's flush-shaped user rows in timeline order.
func flushRowsForTurn(t *testing.T, app *App, threadID string, turnIndex int) []store.Item {
	t.Helper()
	items, err := app.store.ListItemsForTurn(threadID, turnIndex)
	if err != nil {
		t.Fatalf("ListItemsForTurn(%d): %v", turnIndex, err)
	}
	var rows []store.Item
	for _, item := range items {
		if item.Kind == "user_text" && strings.Contains(item.ID, ":flush:") {
			rows = append(rows, item)
		}
	}
	return rows
}
