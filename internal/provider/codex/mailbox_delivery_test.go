package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestAuditNestedMailboxMustBeRecognized(t *testing.T) {
	_, ok := parseInterAgentMailboxEnvelope("/root/worker/helper", "/root/worker", "Message Type: FINAL_ANSWER\nTask name: /root/worker\nSender: /root/worker/helper\nPayload:\ndone")
	if !ok {
		t.Fatal("valid nested child-to-parent delivery rejected")
	}
}
func TestAuditDistinctProviderDeliveryIDsMustRemainDistinct(t *testing.T) {
	parse := func(id string) subagentNotification {
		b, _ := json.Marshal(map[string]any{"id": id, "type": "agent_message", "author": "/root/worker", "recipient": "/root", "content": []map[string]string{{"type": "input_text", "text": "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/worker\nPayload:\nDone."}}})
		var item map[string]json.RawMessage
		_ = json.Unmarshal(b, &item)
		n, ok := extractSubagentCompletionFromRawAgentMessageItem(item)
		if !ok {
			t.Fatal("parse")
		}
		return n
	}
	if parse("message-1").DeliveryID == parse("message-2").DeliveryID {
		t.Fatal("distinct upstream message IDs collapse to one content delivery ID")
	}
}

func TestChildRuntimeIgnoresStaleAndDuplicateTurnBoundaries(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	s.registerChildOwnership("root-provider-thread", "child", "/root/worker", "spawn")
	for _, frame := range []string{
		`{"threadId":"child","turn":{"id":"A"}}`,
		`{"threadId":"child","turn":{"id":"B"}}`,
		`{"threadId":"child","turn":{"id":"B"}}`,
	} {
		s.emitChildLifecycleEvents("turn/started", json.RawMessage(frame), "spawn")
	}
	s.emitChildLifecycleEvents("turn/completed", json.RawMessage(`{"threadId":"child","turn":{"id":"A","status":"completed"}}`), "spawn")
	if len(events) != 2 || events[1].TurnID != "B" {
		t.Fatalf("lifecycle events = %+v", events)
	}
	if runtime := s.collab.childRuntimeByThread["child"]; runtime.turnID != "B" || runtime.phase != childRuntimeRunning {
		t.Fatalf("runtime = %+v", runtime)
	}
}

func TestNestedMailboxRoutesToRecipientAndDeduplicatesNativeID(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	s.registerChildOwnership("root-provider-thread", "worker", "/root/worker", "spawn-worker")
	s.registerChildOwnership("worker", "helper", "/root/worker/helper", "spawn-helper")
	makeParams := func(id, recipient, author string) json.RawMessage {
		b, err := json.Marshal(map[string]any{"threadId": "worker", "item": map[string]any{"id": id, "type": "agent_message", "author": author, "recipient": recipient, "content": []map[string]string{{"type": "input_text", "text": "Message Type: MESSAGE\nTask name: " + recipient + "\nSender: " + author + "\nPayload:\n"}, {"type": "encrypted_content", "encrypted_content": "secret-ciphertext"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	frame := makeParams("message-1", "/root/worker", "/root/worker/helper")
	s.dispatchNotification("rawResponseItem/completed", frame)
	s.advanceChildTurnGeneration("helper")
	s.dispatchNotification("rawResponseItem/completed", frame)
	s.dispatchNotification("rawResponseItem/completed", makeParams("message-2", "/root/worker", "/root/worker/helper"))
	s.dispatchNotification("rawResponseItem/completed", makeParams("spoof", "/root", "/root/worker/helper"))
	if len(events) != 2 {
		t.Fatalf("deliveries=%+v", events)
	}
	for _, e := range events {
		if e.ParentToolUseID != "spawn-worker" || e.ItemID != "spawn-helper" || !strings.Contains(string(e.Meta), `"encrypted":true`) || strings.Contains(string(e.Meta), "secret-ciphertext") {
			t.Fatalf("delivery = %+v", e)
		}
	}
}

func TestChildThreadStatusIsScopedAndPreservesActiveFlags(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	s.registerChildOwnership("root-provider-thread", "child", "/root/worker", "spawn")
	s.dispatchNotification("thread/status/changed", json.RawMessage(`{"threadId":"child","status":{"type":"active","activeFlags":["waitingOnApproval"]}}`))
	if len(events) != 1 || events[0].Kind != provider.EventSubagentStatus || events[0].ItemID != "spawn" || !strings.Contains(string(events[0].Meta), "waitingOnApproval") {
		t.Fatalf("events=%+v", events)
	}
}

func TestRecoveredExecutionRetainsNativeStartAndBlockingFlags(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	_, err := s.reconcileCollabHistoryTerminal(collabHistoryJob{Ownership: collabHistoryOwnership{ParentItemID: "spawn", ChildThreadID: "child"}}, collabThreadSnapshot{ThreadID: "child", Status: "active", LatestTurnID: "B", LatestTurnStatus: "inProgress", StartedAt: 1234, ActiveFlags: []string{"waitingOnUserInput"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TurnID != "B" || !strings.Contains(string(events[0].Meta), `"started_at":1234000`) || !strings.Contains(string(events[0].Meta), "waitingOnUserInput") {
		t.Fatalf("recovery = %+v", events)
	}
}

func TestUnrepresentedCollabResultRemainsVisibleWithoutInventingSuccess(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	s.recordAppServerVersion(json.RawMessage(`{"userAgent":"codex_cli_rs/0.153.4"}`))
	s.dispatchNotification("rawResponseItem/completed", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"root-turn","item":{"type":"function_call","namespace":"collaboration","name":"send_message","call_id":"send","arguments":"{\"target\":\"missing\",\"message\":\"secret-ciphertext\"}"}}`))
	s.dispatchNotification("rawResponseItem/completed", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"root-turn","item":{"type":"function_call_output","call_id":"send","output":"target not found"}}`))
	if len(events) != 1 || events[0].ItemID != "send" || events[0].Content != "target not found" || !strings.Contains(string(events[0].Meta), `"outcome":"unknown"`) || strings.Contains(string(events[0].Meta), "secret-ciphertext") {
		t.Fatalf("operation outcome=%+v", events)
	}
	if len(s.collab.childParentByThread) != 0 {
		t.Fatal("failed send created an agent")
	}
}

func TestRootMailboxTailRetainsUnknownSenderAndRejectsForeignRecipient(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	line := []byte(`{"timestamp":"2026-09-08T10:11:12Z","type":"response_item","payload":{"id":"native-unknown","type":"agent_message","author":"/root/unknown","recipient":"/root","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/unknown\nPayload:\nhello"}]}}`)
	if !s.emitSubagentNotificationsFromRolloutLine(line) || len(events) != 1 {
		t.Fatalf("unknown sender receipt lost: %+v", events)
	}
	if events[0].ItemID != "" || events[0].ParentToolUseID != "" || events[0].Timestamp.Format(time.RFC3339) != "2026-09-08T10:11:12Z" {
		t.Fatalf("receipt=%+v", events[0])
	}
	s.registerHistoricalChildOwnership("root-provider-thread", "late-child", "/root/unknown", "late-spawn")
	if s.emitSubagentNotificationsFromRolloutLine(line) || len(events) != 1 {
		t.Fatal("late sender ownership duplicated native delivery")
	}
	foreign := []byte(strings.ReplaceAll(string(line), `"recipient":"/root"`, `"recipient":"/root/other"`))
	if s.emitSubagentNotificationsFromRolloutLine(foreign) || len(events) != 1 {
		t.Fatal("foreign recipient leaked into root timeline")
	}
}
