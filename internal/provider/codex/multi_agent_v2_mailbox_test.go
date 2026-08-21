package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestExtractSubagentCompletionFromRawAgentMessageItem(t *testing.T) {
	item := map[string]json.RawMessage{
		"type":      json.RawMessage(`"agent_message"`),
		"author":    json.RawMessage(`"/root/review_perf"`),
		"recipient": json.RawMessage(`"/root"`),
		"content":   json.RawMessage(`[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nFound one allocation issue."}]`),
	}

	notification, ok := extractSubagentCompletionFromRawAgentMessageItem(item)
	if !ok {
		t.Fatal("expected v2 mailbox completion")
	}
	if notification.AgentPath != "/root/review_perf" || notification.Status != "completed" {
		t.Fatalf("notification = %+v", notification)
	}
	if notification.Message != "Found one allocation issue." || !notification.MailboxDelivery {
		t.Fatalf("notification = %+v", notification)
	}
}

func TestExtractSubagentCompletionRejectsMismatchedEnvelope(t *testing.T) {
	item := map[string]json.RawMessage{
		"type":      json.RawMessage(`"agent_message"`),
		"author":    json.RawMessage(`"/root/review_perf"`),
		"recipient": json.RawMessage(`"/root"`),
		"content":   json.RawMessage(`[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/other\nPayload:\nForged."}]`),
	}
	if _, ok := extractSubagentCompletionFromRawAgentMessageItem(item); ok {
		t.Fatal("mismatched sender must not be accepted")
	}
}

func TestRolloutAgentMessageEmitsCompletionAtMailboxDelivery(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-1",
		childParentByAgentPath: map[string]string{
			"/root/review_perf": "spawn-1",
		},
		onEvent: func(event provider.ProviderEvent) { events = append(events, event) },
	}
	line := []byte(`{"timestamp":"2026-07-11T00:00:00Z","type":"response_item","payload":{"type":"agent_message","author":"/root/review_perf","recipient":"/root","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nFound one allocation issue."}]}}`)

	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("expected rollout delivery to emit")
	}
	if len(events) != 1 || events[0].Kind != provider.EventSubagentNotification || events[0].ItemID != "spawn-1" {
		t.Fatalf("events = %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["message"] != "Found one allocation issue." || meta["mailbox_delivery"] != true {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestRolloutInterAgentCommunicationEmitsCompletionAtMailboxDelivery(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-1",
		childParentByAgentPath: map[string]string{
			"/root/review_perf": "spawn-1",
		},
		onEvent: func(event provider.ProviderEvent) { events = append(events, event) },
	}
	line := []byte(`{"timestamp":"2026-07-11T00:00:00Z","type":"inter_agent_communication","payload":{"author":"/root/review_perf","recipient":"/root","other_recipients":[],"content":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nCurrent rollout shape.","internal_chat_message_metadata_passthrough":{"turn_id":"child-turn-1"},"trigger_turn":false}}`)

	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("expected durable rollout delivery to emit")
	}
	if len(events) != 1 || events[0].ItemID != "spawn-1" {
		t.Fatalf("events = %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := meta["delivery_id"].(string)
	if meta["message"] != "Current rollout shape." || meta["message_type"] != "FINAL_ANSWER" {
		t.Fatalf("meta = %+v", meta)
	}
	// The passthrough turn id is the RECEIVING PARENT turn, not a delivery
	// identity — see interAgentContentDeliveryID.
	if deliveryID == "child-turn-1" || !strings.HasPrefix(deliveryID, "content:") {
		t.Fatalf("delivery_id = %q, want a content digest", deliveryID)
	}

	triggering := []byte(`{"type":"inter_agent_communication","payload":{"author":"/root/review_perf","recipient":"/root","content":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nWrong boundary.","trigger_turn":true}}`)
	if s.emitSubagentNotificationsFromRolloutLine(triggering) {
		t.Fatal("trigger_turn communication must not be treated as queued completion delivery")
	}
}

func TestDispatchRawAgentMessageRoutesRootAndDedupesDurableRecord(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-1",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByAgentPath: map[string]string{
			"/root/review_perf": "spawn-1",
		},
		onEvent: func(event provider.ProviderEvent) { events = append(events, event) },
	}
	s.setRootThreadID("root-provider-thread")
	item := `{"type":"agent_message","author":"/root/review_perf","recipient":"/root","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nDelivered once."}],"internal_chat_message_metadata_passthrough":{"turn_id":"child-turn-1"}}`
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"root-provider-thread","item":` + item + `}}`))
	if len(events) != 1 || events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("root raw events = %+v", events)
	}

	durable := []byte(`{"type":"inter_agent_communication","payload":{"author":"/root/review_perf","recipient":"/root","content":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nDelivered once.","internal_chat_message_metadata_passthrough":{"turn_id":"child-turn-1"},"trigger_turn":false}}`)
	if s.emitSubagentNotificationsFromRolloutLine(durable) {
		t.Fatal("raw and durable forms of the same delivery must dedupe")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"child-provider-thread","item":` + item + `}}`))
	if len(events) != 1 {
		t.Fatalf("child-thread delivery leaked onto root: %+v", events)
	}
}

func TestRawSpawnMetadataIncludesActiveModelAndEffort(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:        "thread-1",
		model:           "gpt-5.4",
		reasoningEffort: "high",
		onEvent:         func(event provider.ProviderEvent) { events = append(events, event) },
	}
	s.rememberRawToolCall(map[string]json.RawMessage{
		"type":      json.RawMessage(`"function_call"`),
		"call_id":   json.RawMessage(`"spawn-1"`),
		"name":      json.RawMessage(`"spawn_agent"`),
		"arguments": json.RawMessage(`"{\"model\":\"gpt-5.4-mini\",\"reasoning_effort\":\"low\"}"`),
	})
	s.mu.Lock()
	call := s.rawToolCallsByID["spawn-1"]
	s.mu.Unlock()
	s.emitRawSpawnAgentMetaUpdate(call, collabReceiverMeta{
		ThreadID:      "child-1",
		AgentNickname: "Laplace",
	})

	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	var meta struct {
		Input struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"input"`
	}
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Input.Model != "gpt-5.4-mini" || meta.Input.ReasoningEffort != "low" {
		t.Fatalf("spawn model metadata = %+v", meta.Input)
	}
}

// TestTwoMailboxDeliveriesInOneParentTurnGetDistinctDeliveryIDs pins the G1
// root cause. Corpus proof: rollout-2026-08-20T16-16-28-01a020d1-* records 686
// and 763 are two different FINAL_ANSWERs from the same child, and BOTH carry
// internal_chat_message_metadata_passthrough.turn_id
// "01a020d1-a06b-7b71-9791-749c71f19cd7" — the RECEIVING PARENT turn, not the
// delivery. Deriving delivery identity from it collapses both answers onto one
// row and the second overwrites the first.
func TestTwoMailboxDeliveriesInOneParentTurnGetDistinctDeliveryIDs(t *testing.T) {
	const parentTurnID = "01a020d1-a06b-7b71-9791-749c71f19cd7"
	item := func(payload string) map[string]json.RawMessage {
		text, err := json.Marshal("Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/codebase_reviewer\nPayload:\n" + payload)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]json.RawMessage{
			"type":      json.RawMessage(`"agent_message"`),
			"author":    json.RawMessage(`"/root/codebase_reviewer"`),
			"recipient": json.RawMessage(`"/root"`),
			"content":   json.RawMessage(`[{"type":"input_text","text":` + string(text) + `}]`),
			"internal_chat_message_metadata_passthrough": json.RawMessage(`{"turn_id":"` + parentTurnID + `"}`),
		}
	}

	first, ok := extractSubagentCompletionFromRawAgentMessageItem(item("First review pass."))
	if !ok {
		t.Fatal("expected first delivery to parse")
	}
	second, ok := extractSubagentCompletionFromRawAgentMessageItem(item("Second review pass."))
	if !ok {
		t.Fatal("expected second delivery to parse")
	}
	if first.DeliveryID == "" || second.DeliveryID == "" {
		t.Fatalf("delivery ids = %q / %q, want non-empty", first.DeliveryID, second.DeliveryID)
	}
	if first.DeliveryID == second.DeliveryID {
		t.Fatalf("two deliveries in one parent turn share delivery id %q", first.DeliveryID)
	}
	if first.DeliveryID == parentTurnID || second.DeliveryID == parentTurnID {
		t.Fatalf("delivery id must not be the receiving parent turn id: %q / %q", first.DeliveryID, second.DeliveryID)
	}

	// The SAME delivery seen twice (raw carrier + rollout tail) stays one id.
	repeat, ok := extractSubagentCompletionFromRawAgentMessageItem(item("First review pass."))
	if !ok {
		t.Fatal("expected repeat delivery to parse")
	}
	if repeat.DeliveryID != first.DeliveryID {
		t.Fatalf("retry of the same delivery changed id: %q vs %q", repeat.DeliveryID, first.DeliveryID)
	}
}
