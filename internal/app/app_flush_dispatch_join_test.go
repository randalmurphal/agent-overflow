package app

import (
	"agent-overflow/internal/testutil/mockexec"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
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
	mockexec.Write(t, path, script)
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
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	home := testProviderHome(t, app)
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
	items3, err := app.store.ListTurnItems(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListTurnItems: %v", err)
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
	assertClaudeSessionText(t, app, workspace, updated.SessionRef,
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
	remaining, err := app.store.ListTurnItems(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListTurnItems after rollback: %v", err)
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
	t.Parallel()
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
	items, err := app.store.ListTurnItems(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListTurnItems: %v", err)
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
	t.Parallel()
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
	items, err := app.store.ListTurnItems(thread.ID, 3)
	if err != nil {
		t.Fatalf("ListTurnItems: %v", err)
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
	t.Parallel()
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

// reviewDispatch is a queued message carrying review drafts that an active
// Claude turn persisted as a quiet row, on a fake CLI that records every
// stdin envelope. Several queued messages join into that one message.
type reviewDispatch struct {
	app     *App
	rec     *emitRecorder
	dbPath  string
	logPath string
	thread  store.Thread
	drafts  []store.DiffReviewComment
	row     store.Item
	// texts are the joined messages' own texts, and hunks the PR hunk
	// excerpts their comments render with; the message carries each once.
	texts []string
	hunks []string
}

const reviewDispatchText = "Please address these."

// reviewMessage is one queued message of a reviewDispatch: its text and
// the review drafts it sends, on sourceKey (the PR-scope default when
// empty), with a PR context giving each a hunk excerpt when pr is set.
type reviewMessage struct {
	text      string
	sourceKey string
	bodies    []string
	pr        bool
}

func startReviewDispatch(t *testing.T, name string) reviewDispatch {
	t.Helper()
	return startReviewDispatchOf(t, name, reviewMessage{text: reviewDispatchText, bodies: []string{"Rename the counter.", "Check the nil map."}})
}

// startReviewDispatchOf queues messages, the first being reviewDispatchText,
// and dispatches them as one joined message.
func startReviewDispatchOf(t *testing.T, name string, messages ...reviewMessage) reviewDispatch {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	app.triage = triage.NewRouter(app.store, rec.captureChannel)
	app.configureTriageQueueCallbacks()
	d := reviewDispatch{app: app, rec: rec, dbPath: dbPath, logPath: filepath.Join(t.TempDir(), "stdin.log")}
	d.thread = testThread(name)
	d.thread.Provider = string(provider.Claude)
	d.thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(d.thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	var items []triage.QueuedFlushItem
	for i, message := range messages {
		d.texts = append(d.texts, message.text)
		var ids []string
		var pr *store.DiffReviewPRContext
		if message.pr {
			pr = &store.DiffReviewPRContext{Number: 5, URL: "https://github.com/acme/widgets/pull/5"}
		}
		var source *SourceDiffReview
		for j, body := range message.bodies {
			input := diffReviewPRInput(body)
			if message.sourceKey != "" {
				input.SourceKey = message.sourceKey
			}
			draft, err := app.CreateDiffReviewComment(d.thread.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			d.drafts = append(d.drafts, draft)
			ids = append(ids, draft.ID)
			source = &SourceDiffReview{ThreadID: d.thread.ID, Scope: draft.Scope, SourceKey: draft.SourceKey, PR: pr}
			if pr != nil {
				hunk := fmt.Sprintf("@@ -%d +%d @@\n+hunk of message %d comment %d", j+1, j+1, i, j)
				pr.Comments = append(pr.Comments, store.DiffReviewPRContextEntry{CommentID: draft.ID, HunkExcerpt: hunk})
				d.hunks = append(d.hunks, hunk)
			}
		}
		id := "queue:review"
		if i > 0 {
			id = fmt.Sprintf("queue:review-%d", i)
		}
		items = append(items, triage.QueuedFlushItem{
			ID: id, Message: message.text, EnqueuedAt: int64(i + 1),
			Payload: flushQueuePayloadJSON(t, flushQueuePayload{RevisionSourceDiffReview: source, RevisionSourceDiffCommentIDs: ids}),
		})
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-0", ThreadID: d.thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	sess, err := claude.NewSession(context.Background(), d.thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, d.logPath), WorkDir: d.thread.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(d.thread.ID, session{Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now())})
	app.dispatchFlush(d.thread.ID, items)
	d.row = d.messageRow(t)
	d.wantEnvelope(t, 1)
	return d
}

// messageRow is the one user row carrying the message.
func (d reviewDispatch) messageRow(t *testing.T) store.Item {
	t.Helper()
	items, err := d.app.store.ListItems(d.thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.Item
	for _, item := range items {
		if item.Kind == "user_text" && strings.Contains(item.Summary, reviewDispatchText) {
			rows = append(rows, item)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("rows carrying the message = %+v, want one", rows)
	}
	return rows[0]
}

// wantEnvelope fails unless the fake CLI saw n envelopes, the last of which
// carries every message's text, each review comment and each hunk excerpt
// exactly once.
func (d reviewDispatch) wantEnvelope(t *testing.T, n int) {
	t.Helper()
	text := d.envelope(t, n)
	want := slices.Concat(d.texts, d.hunks)
	for _, draft := range d.drafts {
		want = append(want, draft.Body)
	}
	for _, want := range want {
		if got := strings.Count(text, want); got != 1 {
			t.Fatalf("envelope %d carries %q %d times, want once:\n%s", n, want, got, text)
		}
	}
}

// envelope is the text of the nth envelope, once the fake CLI saw n.
func (d reviewDispatch) envelope(t *testing.T, n int) string {
	t.Helper()
	envelopes := waitForRecordedUserEnvelopes(t, d.logPath, n)
	if len(envelopes) != n {
		t.Fatalf("outbound envelopes = %d, want %d", len(envelopes), n)
	}
	text, _ := envelopeText(t, envelopes[n-1])
	return text
}

// resend dispatches the message the restore put back on the queue, as the
// next drain would.
func (d reviewDispatch) resend(t *testing.T) {
	t.Helper()
	item, ok := d.app.triage.RemoveQueuedFlushItem(d.thread.ID, "queue:review")
	if !ok {
		t.Fatal("the restore did not put the message back on the queue")
	}
	d.app.dispatchFlush(d.thread.ID, []triage.QueuedFlushItem{item})
}

// wantStatus fails unless every review comment has status.
func (d reviewDispatch) wantStatus(t *testing.T, app *App, status, when string) {
	t.Helper()
	for _, draft := range d.drafts {
		got, err := app.store.GetDiffReviewComment(store.ThreadReviewOwner(d.thread.ID), draft.ID)
		if err != nil || got.Status != status {
			t.Fatalf("%s: comment %q = %+v (%v), want %s", when, draft.Body, got, err, status)
		}
	}
}

// A session death whose composer write fails requeues the message it took
// back. Its row is gone and its comments are drafts again; the requeue sends
// the text the row rendered and claims the comments again: the provider sees
// the text once, with each comment once.
func TestRequeueAfterAFailedDraftWriteSendsTheReviewCommentsOnce(t *testing.T) {
	t.Parallel()
	d := startReviewDispatch(t, "requeue-failed-draft-write")
	execOnStore(t, d.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
	execOnStore(t, d.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
	if requeued := d.app.restoreUnconfirmedQueueOnSessionDeath(d.thread.ID); len(requeued) != 1 {
		t.Fatalf("requeued = %+v, want the message back on the queue", requeued)
	}
	d.wantStatus(t, d.app, "draft", "after the restore")
	execOnStore(t, d.dbPath, `DROP TRIGGER refuse_draft_insert`)
	execOnStore(t, d.dbPath, `DROP TRIGGER refuse_draft_update`)

	d.resend(t)
	d.wantEnvelope(t, 2)
	waitForCondition(t, "the resend to claim the comments", func() bool {
		got, err := d.app.store.GetDiffReviewComment(store.ThreadReviewOwner(d.thread.ID), d.drafts[1].ID)
		return err == nil && got.Status == "sent"
	})
	d.wantStatus(t, d.app, "sent", "after the resend")
	row := d.messageRow(t)
	for _, draft := range d.drafts {
		if got := strings.Count(row.Summary, draft.Body); got != 1 {
			t.Fatalf("resent row renders %q %d times, want once:\n%s", draft.Body, got, row.Summary)
		}
	}
}

// A session death that cannot delete the message's row requeues it with
// the row still holding its comments. The resend retires the old row and
// claims again the comments it returns: the provider sees each once.
func TestRequeueAfterAFailedRowCleanupResolvesItsReviewComments(t *testing.T) {
	t.Parallel()
	d := startReviewDispatch(t, "requeue-failed-cleanup")
	execOnStore(t, d.dbPath, fmt.Sprintf(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = '%s' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`, d.row.ID))
	requeued := d.app.restoreUnconfirmedQueueOnSessionDeath(d.thread.ID)
	if len(requeued) != 1 || requeued[0].StaleUserItemID != d.row.ID {
		t.Fatalf("requeued = %+v, want the message back on the queue naming its row", requeued)
	}
	d.wantStatus(t, d.app, "sent", "while the old row holds them")
	execOnStore(t, d.dbPath, `DROP TRIGGER refuse_row_delete`)

	d.resend(t)
	d.wantEnvelope(t, 2)
	waitForCondition(t, "the resend to replace the old row", func() bool {
		items, err := d.app.store.ListItems(d.thread.ID)
		if err != nil {
			return false
		}
		n := 0
		for _, item := range items {
			if item.Kind == "user_text" && strings.Contains(item.Summary, reviewDispatchText) {
				n++
			}
		}
		return n == 1
	})
	d.wantStatus(t, d.app, "sent", "after the resend")
	if released, err := d.app.store.ReleaseUnsettledDiffReviewClaims(); err != nil || released != 2 {
		// The resend's claims wait for the provider's confirmation, which
		// the fake CLI never sends; both belong to the new row.
		t.Fatalf("ReleaseUnsettledDiffReviewClaims = %d, %v; want the resend's two claims", released, err)
	}
}

// reviewRequeueFailure is a session death that requeues the message instead
// of restoring it to the composer: inject makes the store fail that way and
// returns what undoes it. reregister repeats the requeue a teardown makes
// after its triage cleanup wiped the queue.
type reviewRequeueFailure struct {
	name       string
	inject     func(t *testing.T, d reviewDispatch) (undo func())
	reregister bool
}

func refuseDraftWrites(t *testing.T, d reviewDispatch) func() {
	execOnStore(t, d.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
	execOnStore(t, d.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
	return func() {
		execOnStore(t, d.dbPath, `DROP TRIGGER refuse_draft_insert`)
		execOnStore(t, d.dbPath, `DROP TRIGGER refuse_draft_update`)
	}
}

func refuseRowDelete(t *testing.T, d reviewDispatch) func() {
	execOnStore(t, d.dbPath, fmt.Sprintf(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = '%s' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`, d.row.ID))
	return func() { execOnStore(t, d.dbPath, `DROP TRIGGER refuse_row_delete`) }
}

// reviewRequeueFailures are the two ways a restore requeues: the row is
// retired by the restore, or left for the resend to retire.
var reviewRequeueFailures = []reviewRequeueFailure{
	{name: "failed draft write", inject: refuseDraftWrites},
	{name: "failed row cleanup", inject: refuseRowDelete},
}

// reviewRequeueAfterTeardown is a failed draft write whose requeue a
// teardown registers again after wiping the queue.
var reviewRequeueAfterTeardown = reviewRequeueFailure{name: "failed draft write then a teardown", inject: refuseDraftWrites, reregister: true}

// requeue takes the message back by a session death that fails as failure
// says, then lets the store work again.
func (d reviewDispatch) requeue(t *testing.T, failure reviewRequeueFailure) {
	t.Helper()
	undo := failure.inject(t, d)
	requeued := d.app.restoreUnconfirmedQueueOnSessionDeath(d.thread.ID)
	if len(requeued) != 1 {
		t.Fatalf("requeued = %+v, want the message back on the queue", requeued)
	}
	if failure.reregister {
		if _, ok := d.app.triage.RemoveQueuedFlushItem(d.thread.ID, "queue:review"); !ok {
			t.Fatal("the restore did not put the message back on the queue")
		}
		d.app.requeueUnconfirmedFlushItems(d.thread.ID, requeued)
	}
	undo()
}

// wantClaimedByResend fails unless the one row the resend landed as claims
// exactly the comments ids, with the bodies the resend carries: its retire
// returns those.
func (d reviewDispatch) wantClaimedByResend(t *testing.T, ids ...string) {
	t.Helper()
	var row store.Item
	waitForCondition(t, "the resend's row", func() bool {
		items, err := d.app.store.ListItems(d.thread.ID)
		if err != nil {
			return false
		}
		var rows []store.Item
		for _, item := range items {
			if item.Kind == "user_text" && strings.Contains(item.Summary, reviewDispatchText) {
				rows = append(rows, item)
			}
		}
		if len(rows) != 1 {
			return false
		}
		row = rows[0]
		return true
	})
	returned, err := d.app.store.RetireUndeliveredUserRow(d.thread.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, source := range returned {
		for _, comment := range source.Comments {
			if !strings.Contains(row.Summary, comment.Body) {
				t.Fatalf("the resend claims %q, which its row does not render:\n%s", comment.Body, row.Summary)
			}
			got = append(got, comment.ID)
		}
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(ids))
	if !slices.Equal(got, want) {
		t.Fatalf("the resend's row claims %v, want %v", got, want)
	}
}

func (d reviewDispatch) draftIDs() []string {
	ids := make([]string, 0, len(d.drafts))
	for _, draft := range d.drafts {
		ids = append(ids, draft.ID)
	}
	return ids
}

// A joined message whose members carry comments of two review sources is
// requeued whole: the resend carries both members' text and every comment
// once, and its row claims every comment of both sources.
func TestRequeueOfAJoinedMessageResendsEveryReviewSource(t *testing.T) {
	t.Parallel()
	for _, failure := range append(slices.Clone(reviewRequeueFailures), reviewRequeueAfterTeardown) {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatchOf(t, "requeue-joined-two-sources",
				reviewMessage{text: reviewDispatchText, bodies: []string{"Rename the counter.", "Check the nil map."}},
				reviewMessage{text: "And the other change.", sourceKey: "pr:github:acme/widgets#6", bodies: []string{"Drop the retry."}},
			)
			d.requeue(t, failure)
			d.resend(t)
			d.wantEnvelope(t, 2)
			d.wantStatus(t, d.app, "sent", "after the resend")
			d.wantClaimedByResend(t, d.draftIDs()...)
		})
	}
}

// A joined message may carry more comments of one source than one selection
// may: each member's selection is bounded, the join is not. Its resend
// claims every comment it renders.
func TestRequeueOfAJoinedMessageClaimsMoreCommentsThanOneSelection(t *testing.T) {
	t.Parallel()
	bodies := func(member, n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("Comment %d of message %d.", i, member))
		}
		return out
	}
	for _, failure := range reviewRequeueFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatchOf(t, "requeue-joined-over-selection",
				reviewMessage{text: reviewDispatchText, bodies: bodies(0, 26)},
				reviewMessage{text: "And the rest.", bodies: bodies(1, 25)},
			)
			if len(d.drafts) <= store.MaxDiffReviewCommentIDs {
				t.Fatalf("%d comments, want more than one selection's %d", len(d.drafts), store.MaxDiffReviewCommentIDs)
			}
			d.requeue(t, failure)
			d.resend(t)
			d.wantEnvelope(t, 2)
			d.wantStatus(t, d.app, "sent", "after the resend")
			d.wantClaimedByResend(t, d.draftIDs()...)
		})
	}
}

// A PR-scope message renders each comment with its hunk excerpt, which only
// its first dispatch had; the resend carries them.
func TestRequeueResendsTheHunkExcerptsOfAPRScopeMessage(t *testing.T) {
	t.Parallel()
	for _, failure := range reviewRequeueFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatchOf(t, "requeue-pr-hunks",
				reviewMessage{text: reviewDispatchText, bodies: []string{"Rename the counter.", "Check the nil map."}, pr: true})
			d.requeue(t, failure)
			d.resend(t)
			d.wantEnvelope(t, 2)
			d.wantClaimedByResend(t, d.draftIDs()...)
		})
	}
}

// A comment edited while its message waits to be resent was not sent with
// its new text: the resend carries the text the message rendered, and the
// edit stays a draft.
func TestRequeueLeavesACommentEditedMeanwhileADraft(t *testing.T) {
	t.Parallel()
	const edited = "Use a sync.Map instead."
	for _, failure := range reviewRequeueFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatch(t, "requeue-edited-meanwhile")
			d.requeue(t, failure)
			if _, err := d.app.UpdateDiffReviewComment(d.thread.ID, d.drafts[1].ID, store.DiffReviewCommentUpdate{Body: edited}); err != nil {
				t.Fatal(err)
			}
			d.resend(t)
			d.wantEnvelope(t, 2)
			if text := d.envelope(t, 2); strings.Contains(text, edited) {
				t.Fatalf("the resend carries the edit:\n%s", text)
			}
			got, err := d.app.store.GetDiffReviewComment(store.ThreadReviewOwner(d.thread.ID), d.drafts[1].ID)
			if err != nil || got.Status != "draft" || got.Body != edited {
				t.Fatalf("edited comment = %+v (%v), want a draft with the edit", got, err)
			}
			d.wantClaimedByResend(t, d.drafts[0].ID)
		})
	}
}

// A resend that fails after it retired the old row is requeued with the
// comments that row returned, so the next resend still claims them.
func TestRequeueKeepsTheReviewCommentsAcrossAFailedResend(t *testing.T) {
	t.Parallel()
	for _, failure := range reviewRequeueFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatch(t, "requeue-failed-resend")
			d.requeue(t, failure)
			execOnStore(t, d.dbPath, `CREATE TRIGGER refuse_message_insert BEFORE INSERT ON items WHEN NEW.kind = 'user_text' BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`)
			d.resend(t)
			execOnStore(t, d.dbPath, `DROP TRIGGER refuse_message_insert`)
			d.wantStatus(t, d.app, "draft", "after the failed resend")
			d.resend(t)
			d.wantEnvelope(t, 2)
			d.wantClaimedByResend(t, d.draftIDs()...)
		})
	}
}

// A requeued message taken back by a later session death returns its own
// text to the composer and its comments to drafts, like the first restore.
func TestRequeuedMessageRestoresTheTextWithoutItsReviewComments(t *testing.T) {
	t.Parallel()
	for _, failure := range reviewRequeueFailures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()
			d := startReviewDispatch(t, "requeued-restore")
			d.requeue(t, failure)
			d.app.restoreUnconfirmedQueueOnSessionDeath(d.thread.ID)
			draft, _, err := d.app.store.GetThreadDraft(d.thread.ID)
			if err != nil || draft.Content != reviewDispatchText {
				t.Fatalf("composer draft = %q (%v), want the message's own text", draft.Content, err)
			}
			d.wantStatus(t, d.app, "draft", "after the second restore")
		})
	}
}

// A message taken back by a session death returns its own text to the
// composer and its review comments to drafts, never the rendered comments
// with them; a boot keeps that state.
func TestSessionDeathRestoresTheTextWithoutItsReviewComments(t *testing.T) {
	t.Parallel()
	d := startReviewDispatch(t, "restore-text-without-comments")
	d.app.restoreUnconfirmedQueueOnSessionDeath(d.thread.ID)
	wantRestored := func(app *App, when string) {
		t.Helper()
		draft, _, err := app.store.GetThreadDraft(d.thread.ID)
		if err != nil || draft.Content != reviewDispatchText {
			t.Fatalf("%s: composer draft = %q (%v), want the message's own text", when, draft.Content, err)
		}
		d.wantStatus(t, app, "draft", when)
	}
	wantRestored(d.app, "after the restore")
	if err := d.app.store.Close(); err != nil {
		t.Fatal(err)
	}
	rebooted := newTestAppAtStorePath(t, d.dbPath)
	rebooted.triage = triage.NewRouter(rebooted.store, func(eventchan.Channel, any) {})
	rebooted.settlePriorInstance()
	wantRestored(rebooted, "after a boot")
}

// A direct send Claude retires unrun returns the same way: its text to the
// composer, its review comments to drafts.
func TestUndeliveredDirectSendRestoresTheTextWithoutItsReviewComments(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("undelivered-direct-review")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	var ids []string
	var bodies []string
	for _, body := range []string{"Rename the counter.", "Check the nil map."} {
		draft, err := app.CreateDiffReviewComment(thread.ID, diffReviewPRInput(body))
		if err != nil {
			t.Fatal(err)
		}
		ids, bodies = append(ids, draft.ID), append(bodies, body)
	}
	source := &SourceDiffReview{ThreadID: thread.ID, Scope: string(store.DiffReviewScopePR), SourceKey: diffReviewPRInput("").SourceKey}
	resolved, err := app.resolveUserMessageEnvelope(thread.ID, reviewDispatchText, userMessageInputs{revisionSourceDiffReview: source, revisionSourceDiffCommentIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.claimDiffReviewDelivery(thread.ID, 0, "user:0", resolved, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{ID: "user:0", ThreadID: thread.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: resolved.content, Meta: resolved.userMessageMeta, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	app.triage.RegisterPendingSendWithExpectation(thread.ID, "user:0", 0, triage.PendingSendExpectation{ProviderItemID: "u-direct"})
	for _, state := range []provider.CommandLifecycleState{provider.CommandQueued, provider.CommandCancelled} {
		meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: "u-direct", State: state})
		if err != nil {
			t.Fatal(err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventCommandLifecycle, ThreadID: thread.ID, ItemID: "u-direct", Meta: meta, Timestamp: time.Now()}); err != nil {
			t.Fatalf("lifecycle %s: %v", state, err)
		}
	}
	waitForCondition(t, "the undelivered send in the composer", func() bool {
		draft, _, err := app.store.GetThreadDraft(thread.ID)
		return err == nil && draft.Content != ""
	})
	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || draft.Content != reviewDispatchText {
		t.Fatalf("composer draft = %q (%v), want the message's own text", draft.Content, err)
	}
	for i, id := range ids {
		wantReviewDraft(t, app, store.ThreadReviewOwner(thread.ID), id, "after the restore of "+bodies[i])
	}
}

// A joined message records its text before the review comments any member
// rendered, joined by the same walk as its summary, so a restore of the one
// row returns every member's own text and none of the rendered comments,
// whichever source each member's comments came from.
func TestJoinFlushMembersRecordsTheTextBeforeReviewComments(t *testing.T) {
	t.Parallel()
	own, third := "first, own", "third, own"
	members := []flushMember{
		{resolved: resolvedUserMessage{content: own + "\n\nrendered comment", providerContent: own + "\n\nrendered comment", textBeforeReviewComments: &own,
			revisionSourceDiff: &SourceDiffReview{ThreadID: "t", Scope: "pr", SourceKey: "a"}, revisionDiffCommentIDs: []string{"a1"}}},
		{resolved: resolvedUserMessage{content: "second", providerContent: "second"}},
		{resolved: resolvedUserMessage{content: third + "\n\nother rendered comment", providerContent: third + "\n\nother rendered comment", textBeforeReviewComments: &third,
			revisionSourceDiff: &SourceDiffReview{ThreadID: "t", Scope: "pr", SourceKey: "b"}, revisionDiffCommentIDs: []string{"b1"}}},
	}
	joined, err := joinFlushMembers(members)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := usermessage.FromItem(store.Item{Meta: joined.meta})
	if err != nil {
		t.Fatal(err)
	}
	want := own + joinedFlushSeparator + "second" + joinedFlushSeparator + third
	if got := meta.TextWithoutReviewComments(joined.content); got != want {
		t.Fatalf("joined text without review comments = %q, want %q", got, want)
	}
	for _, rendered := range []string{"\n\nrendered comment", "other rendered comment"} {
		if !strings.Contains(joined.content, rendered) {
			t.Fatalf("joined summary = %q, want %q", joined.content, rendered)
		}
	}
}
