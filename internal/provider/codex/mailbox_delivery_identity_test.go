package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const encryptedFinalAnswerHeader = "Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nFound one allocation issue."

// One encrypted delivery, both carriers. The raw `agent_message` response item
// carries the plaintext header plus an `encrypted_content` block; the durable
// `inter_agent_communication` rollout record carries the same header as a plain
// string and no tail at all. They must agree on the delivery id, or the same
// delivery lands on two rows — the exact duplicate the content key exists to
// prevent.
func TestEncryptedDeliveryHasOneIDAcrossBothCarriers(t *testing.T) {
	header, err := json.Marshal(encryptedFinalAnswerHeader)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]json.RawMessage{
		"type":      json.RawMessage(`"agent_message"`),
		"author":    json.RawMessage(`"/root/review_perf"`),
		"recipient": json.RawMessage(`"/root"`),
		"content": json.RawMessage(`[{"type":"input_text","text":` + string(header) + `},` +
			`{"type":"encrypted_content","data":"gAAAAABn0000"}]`),
	}
	fromRaw, ok := extractSubagentCompletionFromRawAgentMessageItem(raw)
	if !ok {
		t.Fatal("expected the encrypted two-block envelope to parse")
	}

	communication := map[string]json.RawMessage{
		"author":       json.RawMessage(`"/root/review_perf"`),
		"recipient":    json.RawMessage(`"/root"`),
		"content":      json.RawMessage(header),
		"trigger_turn": json.RawMessage(`false`),
	}
	fromRollout, ok := extractSubagentCompletionFromInterAgentCommunication(communication)
	if !ok {
		t.Fatal("expected the rollout carrier to parse")
	}

	if fromRaw.DeliveryID == "" || fromRaw.DeliveryID != fromRollout.DeliveryID {
		t.Fatalf("delivery ids diverged: raw=%q rollout=%q", fromRaw.DeliveryID, fromRollout.DeliveryID)
	}
}

// A MESSAGE progress note keeps the tail: its payload never leaves the
// ciphertext, so the tail is the only thing separating two beats.
func TestEncryptedProgressDeliveriesStayDistinct(t *testing.T) {
	item := func(tail string) map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"type":      json.RawMessage(`"agent_message"`),
			"author":    json.RawMessage(`"/root/review_perf"`),
			"recipient": json.RawMessage(`"/root"`),
			"content": json.RawMessage(`[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/review_perf\nPayload:\n"},` +
				`{"type":"encrypted_content","data":"` + tail + `"}]`),
		}
	}
	first, ok := extractSubagentCompletionFromRawAgentMessageItem(item("aaaa"))
	if !ok {
		t.Fatal("first progress delivery did not parse")
	}
	second, ok := extractSubagentCompletionFromRawAgentMessageItem(item("bbbb"))
	if !ok {
		t.Fatal("second progress delivery did not parse")
	}
	if first.DeliveryID == second.DeliveryID {
		t.Fatalf("two progress deliveries collapsed onto %q", first.DeliveryID)
	}
	repeat, ok := extractSubagentCompletionFromRawAgentMessageItem(item("aaaa"))
	if !ok || repeat.DeliveryID != first.DeliveryID {
		t.Fatalf("the same progress record must keep one id: %q vs %q", repeat.DeliveryID, first.DeliveryID)
	}
}

// A child woken by `followup_task` can answer identically twice. The provider
// deduper must not eat the second answer; only a repeat inside the SAME child
// turn is the live-stream/rollout-tail duplicate it exists for.
func TestIdenticalDeliveriesSurviveAcrossChildTurns(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-1",
		onEvent:  func(event provider.ProviderEvent) { events = append(events, event) },
		collab: sessionCollabState{
			childParentByAgentPath: map[string]string{"/root/review_perf": "spawn-1"},
			agentPathByThread:      map[string]string{"child-1": "/root/review_perf"},
		},
	}
	line := []byte(`{"type":"inter_agent_communication","payload":{"author":"/root/review_perf","recipient":"/root","content":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/review_perf\nPayload:\nDone.","trigger_turn":false}}`)

	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("first answer must emit")
	}
	if s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("the same record on a second carrier must be deduped")
	}
	if len(events) != 1 {
		t.Fatalf("events after one child turn = %d", len(events))
	}

	s.emitChildLifecycleEvents("turn/started", json.RawMessage(`{"threadId":"child-1"}`), "spawn-1")
	events = nil

	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("an identical answer from a NEW child turn must emit")
	}
	if len(events) != 1 || events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("events after the resume = %+v", events)
	}
}

// The MESSAGE half of the same question, and the one the FINAL_ANSWER carve-out
// left open.
//
// An ENCRYPTED progress beat's payload never leaves the ciphertext: the
// plaintext header stops at "Payload:\n", so (agent path, type, payload) is
// identical for every beat that child ever sends, and the tail digest is the
// only thing that separates them. Only ONE carrier has a tail — the raw
// `agent_message` response item carries the `encrypted_content` block, while
// the durable `inter_agent_communication` rollout record carries a plain string
// and nothing else.
//
// So the durable carrier cannot name an encrypted progress beat at all. Letting
// it mint an id from an empty tail gives ONE delivery TWO ids: the raw carrier's
// tail-keyed one and this degenerate one, which is a duplicate timeline
// activity — and every later beat from that child then collapses onto the same
// degenerate id, so the timeline shows one stuck entry instead of a conversation.
// It refuses instead.
func TestDurableCarrierRefusesAnEncryptedProgressBeatItCannotIdentify(t *testing.T) {
	// The plaintext header of an encrypted MESSAGE: everything after
	// "Payload:\n" is in the ciphertext block the rollout record does not have.
	header := "Message Type: MESSAGE\nTask name: /root\nSender: /root/review_perf\nPayload:\n"
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	communication := map[string]json.RawMessage{
		"author":       json.RawMessage(`"/root/review_perf"`),
		"recipient":    json.RawMessage(`"/root"`),
		"content":      json.RawMessage(encoded),
		"trigger_turn": json.RawMessage(`false`),
	}
	if got, ok := extractSubagentCompletionFromInterAgentCommunication(communication); ok {
		t.Fatalf("the durable carrier minted %q for a beat only the raw carrier can identify",
			got.DeliveryID)
	}

	// The raw carrier still reports it, because it has the tail that names it.
	raw := map[string]json.RawMessage{
		"type":      json.RawMessage(`"agent_message"`),
		"author":    json.RawMessage(`"/root/review_perf"`),
		"recipient": json.RawMessage(`"/root"`),
		"content": json.RawMessage(`[{"type":"input_text","text":` + string(encoded) + `},` +
			`{"type":"encrypted_content","data":"gAAAAABn1111"}]`),
	}
	fromRaw, ok := extractSubagentCompletionFromRawAgentMessageItem(raw)
	if !ok {
		t.Fatal("the raw carrier must still report an encrypted progress beat")
	}
	if fromRaw.DeliveryID == "" {
		t.Fatal("the raw carrier produced no delivery id")
	}
}

// The refusal is scoped to the identity-incomplete case only. A PLAINTEXT
// progress beat has its body in the header, so both carriers see the same
// bytes and agree on one id — refusing it would drop a progress activity the
// timeline is supposed to show.
func TestDurableCarrierStillReportsAPlaintextProgressBeat(t *testing.T) {
	header := "Message Type: MESSAGE\nTask name: /root\nSender: /root/review_perf\nPayload:\nHalfway through the sweep."
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	communication := map[string]json.RawMessage{
		"author":       json.RawMessage(`"/root/review_perf"`),
		"recipient":    json.RawMessage(`"/root"`),
		"content":      json.RawMessage(encoded),
		"trigger_turn": json.RawMessage(`false`),
	}
	fromRollout, ok := extractSubagentCompletionFromInterAgentCommunication(communication)
	if !ok {
		t.Fatal("a plaintext progress beat must still reach the card")
	}
	if fromRollout.Message == "" {
		t.Fatal("a plaintext progress beat lost its body")
	}

	// And it is the SAME delivery as the raw carrier's: one beat, one id.
	raw := map[string]json.RawMessage{
		"type":      json.RawMessage(`"agent_message"`),
		"author":    json.RawMessage(`"/root/review_perf"`),
		"recipient": json.RawMessage(`"/root"`),
		"content":   json.RawMessage(`[{"type":"input_text","text":` + string(encoded) + `}]`),
	}
	fromRaw, ok := extractSubagentCompletionFromRawAgentMessageItem(raw)
	if !ok {
		t.Fatal("the raw carrier did not parse the plaintext beat")
	}
	if fromRaw.DeliveryID != fromRollout.DeliveryID {
		t.Fatalf("delivery ids diverged: raw=%q rollout=%q", fromRaw.DeliveryID, fromRollout.DeliveryID)
	}
}
