package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// writeClaudeStdinRecorderBinary is a fake CLI that appends every stdin line
// to logPath. The passthrough binary discards stdin, so tests that must see
// the outbound envelope (how many were written, what content blocks they
// carry) use this one.
func writeClaudeStdinRecorderBinary(t *testing.T, logPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stdin-recorder.sh")
	script := fmt.Sprintf("#!/bin/sh\nset -u\nwhile IFS= read -r line; do\n    printf '%%s\\n' \"$line\" >> '%s'\ndone\n", logPath)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write recorder binary: %v", err)
	}
	return path
}

// recordedUserEnvelopes returns the `type:"user"` lines the fake CLI saw.
// Control requests (permission mode) share the stream and are ignored.
func recordedUserEnvelopes(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read stdin log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("stdin line is not JSON (%v): %s", err, line)
		}
		if kind, _ := envelope["type"].(string); kind == "user" {
			out = append(out, envelope)
		}
	}
	return out
}

// waitForRecordedUserEnvelopes polls the log until the fake CLI's shell has
// consumed want envelopes. The stdin write returns before the child reads it,
// so the count is only stable after the child has echoed it to the file.
func waitForRecordedUserEnvelopes(t *testing.T, logPath string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []map[string]any
	for time.Now().Before(deadline) {
		got = recordedUserEnvelopes(t, logPath)
		if len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// envelopeText concatenates the text of an outbound user envelope's content
// blocks and reports how many blocks there were.
func envelopeText(t *testing.T, envelope map[string]any) (string, int) {
	t.Helper()
	message, ok := envelope["message"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no message object: %+v", envelope)
	}
	switch content := message["content"].(type) {
	case string:
		return content, 1
	case []any:
		var text strings.Builder
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := block["type"].(string); kind != "text" {
				continue
			}
			s, _ := block["text"].(string)
			text.WriteString(s)
		}
		return text.String(), len(content)
	default:
		t.Fatalf("envelope content has unexpected type %T", content)
		return "", 0
	}
}

func flushQueuePayloadJSON(t *testing.T, payload flushQueuePayload) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal flush payload: %v", err)
	}
	return raw
}

// TestDispatchFlush_Claude_JoinsDrainIntoOneMessage is the end-to-end pin for
// the boundary-drain join. Three messages queued while Claude is mid-turn are
// drained together, and because the headless CLI would merge them into one
// transcript entry under the LAST uuid anyway (claude-wire.md §Queued-message
// consumption), AO sends and records them as ONE message: one stdin envelope,
// one uuid, one row carrying all three texts and all three send ids, one
// anchor, one response turn. Revert to that row then cuts the SQLite timeline
// and the provider transcript at the same place.
func TestDispatchFlush_Claude_JoinsDrainIntoOneMessage(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := initGitRepo(t)

	const sessionID = "joined-flush-session"
	thread := testThread("flush-claude-join")
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

	sendIDs := []string{"send-a", "send-b", "send-c"}
	messages := []string{"first queued", "second queued", "third queued"}
	items := make([]triage.QueuedFlushItem, 0, len(messages))
	for i, message := range messages {
		items = append(items, triage.QueuedFlushItem{
			ID:      fmt.Sprintf("queue:%d", i),
			Message: message,
			Payload: flushQueuePayloadJSON(t, flushQueuePayload{
				SendID:                 sendIDs[i],
				ExpandComposerCommands: true,
			}),
			EnqueuedAt: now + int64(2+i),
		})
	}
	app.dispatchFlush(thread.ID, items)

	wantJoined := strings.Join(messages, joinedFlushSeparator)

	// ONE envelope on the wire, carrying all three texts in queue order.
	// Several envelopes would be several uuids, and the CLI would still
	// merge them into one entry under the last one.
	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("stdin envelopes: got %d, want 1 joined message:\n%+v", len(envelopes), envelopes)
	}
	gotText, blockCount := envelopeText(t, envelopes[0])
	if gotText != wantJoined {
		t.Errorf("envelope text:\n got %q\nwant %q", gotText, wantJoined)
	}
	if blockCount != 1 {
		t.Errorf("envelope content blocks: got %d, want 1 text block holding the joined parts", blockCount)
	}

	// ONE row, holding the three texts separated by the visible rule.
	items3, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var flushRow *store.Item
	for i, it := range items3 {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			if flushRow != nil {
				t.Fatalf("two flush rows persisted for one joined drain: %s and %s", flushRow.ID, it.ID)
			}
			flushRow = &items3[i]
		}
	}
	if flushRow == nil {
		t.Fatalf("joined flush row not found at turn 3; items: %+v", items3)
	}
	if flushRow.Summary != wantJoined {
		t.Errorf("flush row summary:\n got %q\nwant %q", flushRow.Summary, wantJoined)
	}
	if !strings.Contains(flushRow.Summary, joinedFlushSeparator) {
		t.Errorf("flush row summary has no visible separator: %q", flushRow.Summary)
	}

	// Every member's send id is on the row, and a retry of ANY of them
	// resolves to that row instead of sending a second copy.
	meta, err := usermessage.FromItem(*flushRow)
	if err != nil {
		t.Fatalf("decode row meta: %v", err)
	}
	if meta.SendID != sendIDs[0] {
		t.Errorf("row sendId: got %q, want the first member %q", meta.SendID, sendIDs[0])
	}
	if strings.Join(meta.JoinedSendIDs, ",") != strings.Join(sendIDs, ",") {
		t.Errorf("row joinedSendIds: got %v, want %v", meta.JoinedSendIDs, sendIDs)
	}
	for _, sendID := range sendIDs {
		accepted, queued, found, err := app.triage.FindAcceptedUserMessageBySendID(thread.ID, sendID)
		if err != nil {
			t.Fatalf("FindAcceptedUserMessageBySendID(%s): %v", sendID, err)
		}
		if !found {
			t.Fatalf("send id %s does not resolve after the join — a retry would duplicate the message", sendID)
		}
		if queued.ID != "" {
			t.Errorf("send id %s still resolves to a queued row %s after dispatch", sendID, queued.ID)
		}
		if accepted.ID != flushRow.ID {
			t.Errorf("send id %s resolves to %s, want the joined row %s", sendID, accepted.ID, flushRow.ID)
		}
	}

	// ONE pending send, at the response turn, under one minted uuid.
	head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
	if !ok {
		t.Fatalf("no pending send registered for the joined dispatch")
	}
	if head.TurnIndex != 4 {
		t.Errorf("pending send TurnIndex: got %d, want 4 (one response turn for one message)", head.TurnIndex)
	}
	echoID := head.ExpectedProviderItemID
	if echoID == "" {
		t.Fatal("joined dispatch registered no ExpectedProviderItemID")
	}
	if gotUUID, _ := envelopes[0]["uuid"].(string); gotUUID != echoID {
		t.Errorf("envelope uuid %q does not match the registered expectation %q", gotUUID, echoID)
	}

	// Zone 1 clears for all three queue ids; Zone 2 shows the one row.
	var flushedItems []QueueFlushedItem
	for _, call := range rec.snapshot() {
		if call.Channel != "provider:queue_flushed" {
			continue
		}
		evt, ok := call.Data.(QueueFlushedEvent)
		if !ok {
			t.Fatalf("queue_flushed payload type %T", call.Data)
		}
		flushedItems = append(flushedItems, evt.Items...)
	}
	if len(flushedItems) != 3 {
		t.Fatalf("queue_flushed acknowledgements: got %d, want one per queued message", len(flushedItems))
	}
	for i, flushed := range flushedItems {
		if flushed.QueueItemID != fmt.Sprintf("queue:%d", i) {
			t.Errorf("acknowledgement %d names queue item %q", i, flushed.QueueItemID)
		}
		if flushed.UserItemID != flushRow.ID {
			t.Errorf("acknowledgement %d points at %q, want the joined row %q", i, flushed.UserItemID, flushRow.ID)
		}
	}

	// The echo stamps the one row and gives it the one anchor.
	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": echoID})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: thread.ID,
		TurnIndex: 3, ItemID: flushRow.ID, Content: wantJoined,
		Meta: echoMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventUserText echo: %v", err)
	}
	anchor, ok, err := app.store.GetMessageAnchor(thread.ID, flushRow.ID)
	if err != nil || !ok {
		t.Fatalf("joined row has no anchor after echo (ok=%v err=%v)", ok, err)
	}
	if anchor.ProviderUserMessageID != echoID {
		t.Errorf("anchor provider_user_message_id: got %q, want %q", anchor.ProviderUserMessageID, echoID)
	}

	// The transcript the CLI wrote for this batch: ONE user entry under the
	// minted uuid whose content is the three texts. Reverting to the joined
	// row must cut SQLite and that transcript at the same message.
	writeClaudeProjectSession(t, home, workspace, sessionID, fmt.Sprintf(
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":%q,"message":{"role":"user","content":"original prompt"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":%q,"message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}
{"type":"user","uuid":%q,"parentUuid":"a0","sessionId":%q,"message":{"role":"user","content":[{"type":"text","text":%q},{"type":"text","text":%q},{"type":"text","text":%q}]}}
{"type":"assistant","uuid":"a1","parentUuid":%q,"sessionId":%q,"message":{"role":"assistant","content":[{"type":"text","text":"answer to all three"}]}}
`, sessionID, sessionID, echoID, sessionID, messages[0], messages[1], messages[2], echoID, sessionID))

	if err := rollbackToMessage(app, thread.ID, flushRow.ID); err != nil {
		t.Fatalf("rollback to the joined row: %v", err)
	}
	updated, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if updated.SessionRef == "" || updated.SessionRef == sessionID {
		t.Fatalf("thread session ref = %q, want a sliced fork session", updated.SessionRef)
	}
	// Provider cut: the joined entry and everything after it is gone, the
	// work before it survives.
	assertClaudeSessionText(t, workspace, updated.SessionRef,
		[]string{"original prompt", "working on it"},
		[]string{messages[0], messages[1], messages[2], "answer to all three"})
	// SQLite cut agrees: one row removed, not one of three with two orphans.
	if _, found, err := app.store.GetThreadItem(thread.ID, flushRow.ID); err != nil || found {
		t.Fatalf("joined row still present after rollback (found=%v err=%v)", found, err)
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
		if strings.Contains(it.Summary, messages[1]) || strings.Contains(it.Summary, messages[2]) {
			t.Errorf("row %s still holds a joined member's text after rollback: %q", it.ID, it.Summary)
		}
	}
}

// TestDispatchFlush_Claude_JoinFailureBeforeWriteRequeuesEveryMember pins the
// lossless half of the join. A joined group is ONE message, so a resolution
// failure on its third member must leave nothing on the wire, nothing
// persisted, and all three messages back on the queue in order.
func TestDispatchFlush_Claude_JoinFailureBeforeWriteRequeuesEveryMember(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-join-fail")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
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

	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: thread.WorkspacePath},
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

	// The third member references an attachment that no longer exists, so
	// its envelope cannot be resolved.
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{ID: "queue:0", Message: "first queued", Payload: flushQueuePayloadJSON(t, flushQueuePayload{SendID: "send-a"})},
		{ID: "queue:1", Message: "second queued", Payload: flushQueuePayloadJSON(t, flushQueuePayload{SendID: "send-b"})},
		{ID: "queue:2", Message: "third queued", Payload: flushQueuePayloadJSON(t, flushQueuePayload{
			SendID:        "send-c",
			AttachmentIDs: []string{"att-gone"},
		})},
	})

	if envelopes := recordedUserEnvelopes(t, stdinLog); len(envelopes) != 0 {
		t.Fatalf("a member failed to resolve but %d envelope(s) reached the provider: %+v", len(envelopes), envelopes)
	}
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	for _, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			t.Fatalf("a flush row was persisted for a group that never reached the provider: %+v", it)
		}
	}
	requeued := app.triage.QueuedFlushItems(thread.ID)
	if len(requeued) != 3 {
		t.Fatalf("requeued items: got %d, want all 3 members of the failed group", len(requeued))
	}
	for i, item := range requeued {
		wantID := fmt.Sprintf("queue:%d", i)
		if item.ID != wantID {
			t.Errorf("requeued[%d] = %q, want %q (queue order preserved)", i, item.ID, wantID)
		}
		if item.StaleUserItemID != "" {
			t.Errorf("requeued[%d] carries stale row %q, but nothing was persisted", i, item.StaleUserItemID)
		}
	}
	// Nothing settled: every message is still owed a delivery.
	for _, sendID := range []string{"send-a", "send-b", "send-c"} {
		if _, _, found, err := app.triage.FindAcceptedUserMessageBySendID(thread.ID, sendID); err != nil || found {
			t.Errorf("send id %s resolved to an accepted message after a failed group (found=%v err=%v)", sendID, found, err)
		}
	}
}

// TestDispatchFlush_Claude_RedispatchKeepsInheritedSendIDs covers the second
// life of a joined message. A session death that cannot clean up the joined
// row requeues it as ONE queued message carrying every send id the row
// answered for; re-dispatching it must keep all of them on the fresh row, or a
// retry of a member other than the first would send a duplicate.
func TestDispatchFlush_Claude_RedispatchKeepsInheritedSendIDs(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("flush-claude-rejoin")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
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

	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: thread.WorkspacePath},
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

	restored := "first queued" + joinedFlushSeparator + "second queued"
	sendIDs := []string{"send-a", "send-b"}
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID:      "queue:restored",
		Message: restored,
		Payload: flushQueuePayloadJSON(t, flushQueuePayload{
			SendID:        sendIDs[0],
			JoinedSendIDs: sendIDs,
		}),
	}})

	if envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1); len(envelopes) != 1 {
		t.Fatalf("stdin envelopes: got %d, want 1", len(envelopes))
	}
	items, err := app.store.ListItemsForTurn(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	var row *store.Item
	for i, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.ID, ":flush:") {
			row = &items[i]
		}
	}
	if row == nil {
		t.Fatalf("re-dispatched row not found; items: %+v", items)
	}
	if row.Summary != restored {
		t.Errorf("row summary:\n got %q\nwant %q", row.Summary, restored)
	}
	meta, err := usermessage.FromItem(*row)
	if err != nil {
		t.Fatalf("decode row meta: %v", err)
	}
	if strings.Join(meta.JoinedSendIDs, ",") != strings.Join(sendIDs, ",") {
		t.Fatalf("row joinedSendIds: got %v, want the inherited %v", meta.JoinedSendIDs, sendIDs)
	}
	for _, sendID := range sendIDs {
		accepted, _, found, err := app.triage.FindAcceptedUserMessageBySendID(thread.ID, sendID)
		if err != nil || !found || accepted.ID != row.ID {
			t.Fatalf("send id %s after re-dispatch: found=%v id=%q err=%v", sendID, found, accepted.ID, err)
		}
	}
}

// capturedUserEnvelopeParts returns each outbound `user` envelope as ordered
// (kind, payload) pairs, where a text block's payload is its text and an image
// block's payload is its base64 data. Positional binding of images is the whole
// point of the marker renumbering, so the data has to come back too.
func capturedUserEnvelopeParts(t *testing.T, capturePath string) [][][2]string {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture: %v", err)
	}
	var out [][][2]string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source struct {
						Data string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.Type != "user" {
			continue
		}
		parts := make([][2]string, 0, len(envelope.Message.Content))
		for _, block := range envelope.Message.Content {
			payload := block.Text
			if block.Type == "image" {
				payload = block.Source.Data
			}
			parts = append(parts, [2]string{block.Type, payload})
		}
		out = append(out, parts)
	}
	return out
}

func waitForCapturedUserEnvelopeParts(t *testing.T, capturePath string, want int) [][][2]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		envelopes := capturedUserEnvelopeParts(t, capturePath)
		if len(envelopes) >= want {
			return envelopes
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d outbound user messages; got %d", want, len(envelopes))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDispatchFlush_Claude_JoinRenumbersImageMarkers pins the marker/attachment
// binding across the join. Each member numbers its own image `[Image #1]`, so a
// plain concatenation would leave two `#1` markers against a two-image list:
// the bubble would label both images "1" beside a #1/#2 attachment strip, and
// any re-dispatch of that text (edit-resend, session-death requeue) would bind
// both markers to image 1 and append image 2 at the end, losing its drop point.
// The STORED summary is renumbered by the same walk as the wire text, so both
// stay true to the joined attachment order.
func TestDispatchFlush_Claude_JoinRenumbersImageMarkers(t *testing.T) {
	app := newMixedTurnApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-join-images")

	firstImage := uploadTestAttachment(t, app, thread.ID, "one.png", "image/png", append(tinyPNG(), 0x01))
	secondImage := uploadTestAttachment(t, app, thread.ID, "two.png", "image/png", append(tinyPNG(), 0x02))
	firstData := base64.StdEncoding.EncodeToString(append(tinyPNG(), 0x01))
	secondData := base64.StdEncoding.EncodeToString(append(tinyPNG(), 0x02))

	// An open turn makes the dispatch persist its row immediately (the quiet
	// shape a mid-turn queued message takes), which is the row the bubble
	// renders and a requeue rebuilds from.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID,
		TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}

	const firstMessage = "look at [Image #1] first"
	const secondMessage = "then [Image #1] second"
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{
		{
			ID: "queue:0", Message: firstMessage,
			Payload: flushQueuePayloadJSON(t, flushQueuePayload{
				SendID: "send-a", AttachmentIDs: []string{firstImage.ID},
			}),
		},
		{
			ID: "queue:1", Message: secondMessage,
			Payload: flushQueuePayloadJSON(t, flushQueuePayload{
				SendID: "send-b", AttachmentIDs: []string{secondImage.ID},
			}),
		},
	})

	wantSummary := "look at [Image #1] first" + joinedFlushSeparator + "then [Image #2] second"
	// Markers are consumed by the split, so the wire text is the summary with
	// each marker replaced by its image block at that exact position.
	wantParts := [][2]string{
		{"text", "look at "},
		{"image", firstData},
		{"text", " first" + joinedFlushSeparator + "then "},
		{"image", secondData},
		{"text", " second"},
	}

	envelopes := waitForCapturedUserEnvelopeParts(t, capturePath, 1)
	if len(envelopes) != 1 {
		t.Fatalf("outbound envelopes: got %d, want 1", len(envelopes))
	}
	if !reflect.DeepEqual(envelopes[0], wantParts) {
		t.Fatalf("wire parts:\n got %v\nwant %v", envelopes[0], wantParts)
	}

	row := joinedFlushRow(t, app, thread.ID)
	if row.Summary != wantSummary {
		t.Fatalf("row summary:\n got %q\nwant %q", row.Summary, wantSummary)
	}
	meta, err := usermessage.FromItem(row)
	if err != nil {
		t.Fatalf("decode row meta: %v", err)
	}
	if len(meta.Attachments) != 2 ||
		meta.Attachments[0].ID != firstImage.ID || meta.Attachments[1].ID != secondImage.ID {
		t.Fatalf("row attachments = %+v, want [%s %s] in queue order", meta.Attachments, firstImage.ID, secondImage.ID)
	}

	// Second life: a session death that could not clean the row up requeues it
	// as ONE message rebuilt from the row (queuePayloadFromUserItem). The
	// re-dispatch resolves the summary's markers against the same attachment
	// list, so the wire must come out identical.
	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID:              "queue:restored",
		Message:         row.Summary,
		Payload:         queuePayloadFromUserItem(row, nil),
		StaleUserItemID: row.ID,
	}})
	envelopes = waitForCapturedUserEnvelopeParts(t, capturePath, 2)
	if len(envelopes) != 2 {
		t.Fatalf("outbound envelopes after requeue: got %d, want 2", len(envelopes))
	}
	if !reflect.DeepEqual(envelopes[1], wantParts) {
		t.Fatalf("re-dispatched wire parts:\n got %v\nwant %v", envelopes[1], wantParts)
	}
	// The stale row was cleaned up and replaced, so the message is still one
	// row reading exactly as before.
	rebuilt := joinedFlushRow(t, app, thread.ID)
	if rebuilt.Summary != wantSummary {
		t.Fatalf("re-dispatched row summary:\n got %q\nwant %q", rebuilt.Summary, wantSummary)
	}
	rebuiltMeta, err := usermessage.FromItem(rebuilt)
	if err != nil {
		t.Fatalf("decode re-dispatched meta: %v", err)
	}
	if len(rebuiltMeta.Attachments) != 2 ||
		rebuiltMeta.Attachments[0].ID != firstImage.ID || rebuiltMeta.Attachments[1].ID != secondImage.ID {
		t.Fatalf("re-dispatched attachments = %+v, want [%s %s] in order", rebuiltMeta.Attachments, firstImage.ID, secondImage.ID)
	}
}

// joinedFlushRow returns the single flush-persisted user row on the thread.
func joinedFlushRow(t *testing.T, app *App, threadID string) store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var found []store.Item
	for _, item := range items {
		if item.Kind == "user_text" && strings.Contains(item.ID, ":flush:") {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		t.Fatalf("flush rows: got %d, want exactly 1 joined row; items: %+v", len(found), items)
	}
	return found[0]
}
