package codex

import (
	"encoding/json"
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
	if meta["message"] != "Current rollout shape." || meta["delivery_id"] != "child-turn-1" {
		t.Fatalf("meta = %+v", meta)
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
