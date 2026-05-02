package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseUser_ReplayEnvelopeEmitsEventUserText covers the canonical
// SDK shape (`SDKUserMessageReplaySchema`): `isReplay:true` with
// `message.content` as a plain string. The parser promotes this to a
// single EventUserText whose meta carries `provider_item_id` set to
// `message.id`. Phase E reads that key to stamp the AO-owned
// `user:<turnIndex>` row.
func TestParseUser_ReplayEnvelopeEmitsEventUserText(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_abc123","content":"hello world"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}

	got := events[0]
	if got.Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", got.Kind, provider.EventUserText)
	}
	if got.ThreadID != testThread {
		t.Fatalf("ThreadID: got %q, want %q", got.ThreadID, testThread)
	}
	if got.Content != "hello world" {
		t.Fatalf("Content: got %q, want %q", got.Content, "hello world")
	}
	if got.ItemID != "" {
		t.Fatalf("ItemID: got %q, want empty (triage owns the AO row id)", got.ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "msg_abc123" {
		t.Fatalf("meta.provider_item_id: got %v, want msg_abc123", meta["provider_item_id"])
	}
}

// TestParseUser_ReplayEnvelopeWithBlockContent covers the defensive
// secondary shape — `content` as a `[{type:"text",text:"..."}]` array.
// Real captures haven't shown this for replay envelopes, but the
// SDK's content union allows it and extractToolResultText already
// handles both shapes; the parser must not treat array-content as a
// drop just because string is documented.
func TestParseUser_ReplayEnvelopeWithBlockContent(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_block","content":[{"type":"text","text":"hello"}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "hello" {
		t.Fatalf("Content: got %q, want %q", events[0].Content, "hello")
	}
}

// TestParseUser_ReplayEnvelopeMissingMessageID covers the
// missing-uuid case — the parser must still emit EventUserText
// (the wire echo carries semantic value) but must NOT emit an
// empty-string `provider_item_id`. Phase E treats absence-of-key
// and empty-string differently; we collapse them here so triage
// sees one shape.
func TestParseUser_ReplayEnvelopeMissingMessageID(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"content":"no uuid"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "no uuid" {
		t.Fatalf("Content: got %q, want %q", events[0].Content, "no uuid")
	}

	// Either absent meta or meta without provider_item_id is acceptable;
	// what is NOT acceptable is meta carrying an empty-string value for
	// the key.
	if len(events[0].Meta) == 0 {
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["provider_item_id"]; ok {
		if s, isStr := v.(string); !isStr || s == "" {
			t.Fatalf("meta.provider_item_id present but empty/invalid: %v", v)
		}
	}
}

// TestParseUser_NonReplayUserEnvelopeWithStringContent_StillDrops
// pins the byte-for-byte preservation of today's behaviour: a
// string-content user envelope WITHOUT the replay flag still drops
// silently (no events). The replay flag is the gate; without it
// nothing changes.
func TestParseUser_NonReplayUserEnvelopeWithStringContent_StillDrops(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","message":{"content":"some user text"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for non-replay string-content user envelope, got %d: %+v", len(events), events)
	}
}

// TestParseUser_NonReplayUserEnvelopeWithToolResult_RoutesThroughParseUser
// confirms tool_result envelopes still flow through the existing
// parseUser path unchanged. Pre-check must NOT swallow them.
func TestParseUser_NonReplayUserEnvelopeWithToolResult_RoutesThroughParseUser(t *testing.T) {
	parser := NewParser()
	// Prime an inline tool_use so the tool_result correlates.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-inline","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`)); err != nil {
		t.Fatalf("seed tool_use: %v", err)
	}

	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tool-inline","type":"tool_result","content":"127.0.0.1"}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (EventToolComplete), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-inline" {
		t.Fatalf("ItemID: got %q, want tool-inline", events[0].ItemID)
	}
}

// TestParseUser_ReplayWithBothToolResultAndIsReplay defends against
// a future SDK shape that combines both signals. We choose
// `isReplay:true` wins — exactly one EventUserText emits, no
// EventToolComplete. This shouldn't appear in practice; the test
// pins the choice so a future change has to be deliberate.
func TestParseUser_ReplayWithBothToolResultAndIsReplay(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_replay","content":[{"type":"tool_result","tool_use_id":"tool-x","content":"would-be-completion"}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event (replay wins), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q (replay must win over tool_result)", events[0].Kind, provider.EventUserText)
	}
	for _, ev := range events {
		if ev.Kind == provider.EventToolComplete {
			t.Fatalf("EventToolComplete must NOT emit when isReplay:true is present, got %+v", ev)
		}
	}
}
