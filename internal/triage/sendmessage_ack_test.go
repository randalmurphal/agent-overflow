package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Drives one SendMessage launch + completion through the Router on a
// Claude thread and returns the settled row. The completion carries the
// wire's own `is_error:false` in every case — the CLI never flags a
// refused send on the tool_result block — so the verdict can only have
// come from the ack.
func settleSendMessage(t *testing.T, router *Router, st *store.Store, threadID, itemID, to string, toolUseResult string) store.Item {
	t.Helper()
	launchMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": to, "message": "status?", "recipient": to, "content": "status?"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID, ItemType: "SendMessage",
		Meta: launchMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	completion := json.RawMessage(`{"is_error":false,"tool_use_result":` + toolUseResult + `}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: itemID, ItemType: "SendMessage",
		Content: "ack", Meta: completion, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, it := range findItemsByKind(t, st, threadID, itemKindToolCall) {
		if it.ID == itemID {
			return it
		}
	}
	t.Fatalf("no tool_call row %s", itemID)
	return store.Item{}
}

func decodeMeta(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode meta %q: %v", raw, err)
	}
	return obj
}

// A refused send arrives as a normal `is_error:false` tool_result whose
// ack says `success:false`. The row must read as errored — status,
// summary suffix, stored meta and the payload header all agreeing — and
// carry the CLI's own one-line reason for the frontend to show red.
func TestSendMessageRefusalReadsTheAckNotTheWireFlag(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	item := settleSendMessage(t, router, st, "t1", "toolu_send_1", "A",
		`{"success":false,"message":"No agent named \"A\".","display":"No agent named \"A\" in this session. Use the agent's id, or /agents to list them."}`)

	if item.Status != statusErrored {
		t.Fatalf("status = %q, want errored", item.Status)
	}
	if !strings.HasSuffix(item.Summary, "(error)") {
		t.Errorf("summary = %q, want the error suffix", item.Summary)
	}
	meta := decodeMeta(t, item.Meta)
	if meta["is_error"] != true {
		t.Errorf("meta.is_error = %v, want true (the ack's verdict must survive the wire's false flag)", meta["is_error"])
	}
	if got := meta[MetaSendReplyKey]; got != `No agent named "A" in this session. Use the agent's id, or /agents to list them.` {
		t.Errorf("meta.send_reply = %v, want the display text", got)
	}
	if _, stamped := meta[MetaRecipientDescriptionKey]; stamped {
		t.Errorf("meta.recipient_description stamped on a refusal that named no agent: %v", meta)
	}
	if item.PayloadID == "" {
		t.Fatal("no completion payload persisted")
	}
	payloadMeta := decodeMeta(t, item.PayloadMeta)
	if payloadMeta["isError"] != true {
		t.Errorf("payload header isError = %v, want true", payloadMeta["isError"])
	}
}

// A queued send names the agent it resolved to (`pin.id`). When that
// agent was launched in this thread, the row is stamped with the
// launch's description so the card reads "To <name>" rather than the
// raw id, and it stays a completed row: queued is delivered.
func TestSendMessageQueuedStampsTheRecipientLaunchDescription(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	launchMeta, _ := json.Marshal(map[string]any{
		"toolName":        "Agent",
		"subagent_launch": true,
		"task_id":         "ab487a02304913d06",
		"input": map[string]any{
			"description":   "Frontend transitive suppression fix",
			"subagent_type": "general-purpose",
			"prompt":        "Fix it.",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_agent_1", ItemType: "Agent",
		Meta: launchMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent launch: %v", err)
	}

	item := settleSendMessage(t, router, st, "t1", "toolu_send_1", "ab487a02304913d06",
		`{"success":true,"message":"Message queued for delivery to ab487a02304913d06 at its next tool round.","pin":{"id":"ab487a02304913d06","name":"Frontend transitive suppression fix","ref":"ab487a02304913d06"}}`)

	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", item.Status)
	}
	meta := decodeMeta(t, item.Meta)
	if got := meta[MetaRecipientDescriptionKey]; got != "Frontend transitive suppression fix" {
		t.Errorf("meta.recipient_description = %v, want the launch's description", got)
	}
	if got := meta[MetaRecipientTaskIDKey]; got != "ab487a02304913d06" {
		t.Errorf("meta.recipient_task_id = %v, want the pin", got)
	}
	if got := meta[MetaSendReplyKey]; got != "Message queued for delivery to ab487a02304913d06 at its next tool round." {
		t.Errorf("meta.send_reply = %v, want the CLI's reply line", got)
	}
	if meta["is_error"] == true {
		t.Error("meta.is_error = true on a queued send")
	}
}

// A send to a peer session or a name this thread never launched resolves
// to no launch: the reply is kept, nothing is stamped, and the row shows
// the recipient as typed.
func TestSendMessageToAPeerStampsNoRecipient(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	item := settleSendMessage(t, router, st, "t1", "toolu_send_1", "reviewer",
		`{"success":true,"message":"Message sent to reviewer."}`)

	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", item.Status)
	}
	meta := decodeMeta(t, item.Meta)
	if _, stamped := meta[MetaRecipientDescriptionKey]; stamped {
		t.Errorf("recipient_description stamped for a peer: %v", meta)
	}
	if _, stamped := meta[MetaRecipientTaskIDKey]; stamped {
		t.Errorf("recipient_task_id stamped for a peer: %v", meta)
	}
	if got := meta[MetaSendReplyKey]; got != "Message sent to reviewer." {
		t.Errorf("meta.send_reply = %v", got)
	}
}
