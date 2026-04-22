package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// --- Gap 4: --include-partial-messages stream event handling ---
//
// With --include-partial-messages, Claude surfaces a richer set of
// stream_event envelopes. Previously the parser only accepted
// content_block_delta/text_delta. These tests confirm the parser covers:
//   - thinking_delta  → EventThinking
//   - input_json_delta → silently skipped (no downstream event)
//   - content_block_start/stop → explicit lifecycle events
//   - parent_tool_use_id on stream_event → propagated to emitted events

func TestBuildArgsIncludesPartialMessagesFlag(t *testing.T) {
	args := buildArgs(Config{})

	for _, arg := range args {
		if arg == "--include-partial-messages" {
			return
		}
	}
	t.Fatalf("missing --include-partial-messages flag; args=%v", args)
}

// TestAssistantEnvelopeDoesNotDuplicateStreamedText pins the contract
// that — with `--include-partial-messages` on — the parser emits
// EventTextDelta (and EventThinking) only from the stream_event path,
// never from the final `assistant` envelope. Both envelope types carry
// the same block content; emitting from both produces a cumulative
// summary that contains the text twice. This was the root cause of a
// user-visible rendering artefact where a single mermaid diagram in an
// agent response was persisted and rendered twice.
func TestAssistantEnvelopeDoesNotDuplicateStreamedText(t *testing.T) {
	parser := NewParser()

	// Stream the text deltas first — this is the source-of-truth path.
	streamDelta := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`)
	deltaEvents, err := parser.ParseLine(testThread, streamDelta)
	if err != nil {
		t.Fatalf("parse stream delta: %v", err)
	}
	if len(deltaEvents) != 1 || deltaEvents[0].Kind != provider.EventTextDelta {
		t.Fatalf("expected one EventTextDelta from stream path, got %+v", deltaEvents)
	}

	// Now the coalesced assistant envelope arrives with the full text.
	// The parser MUST NOT emit a second EventTextDelta — triage would
	// append the content onto the already-streamed summary and the
	// final row would contain duplicated text.
	assistantLine := []byte(`{"type":"assistant","message":{"id":"msg_abc","role":"assistant","content":[{"type":"text","text":"hello world"}]}}`)
	assistantEvents, err := parser.ParseLine(testThread, assistantLine)
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	for _, e := range assistantEvents {
		if e.Kind == provider.EventTextDelta {
			t.Fatalf("assistant envelope emitted EventTextDelta, duplicating stream path: content=%q", e.Content)
		}
		if e.Kind == provider.EventThinking {
			t.Fatalf("assistant envelope emitted EventThinking, duplicating stream path: content=%q", e.Content)
		}
	}
}

// TestAssistantEnvelopeStillEmitsToolUseAndUsage confirms the skip is
// scoped to text/thinking — tool_use blocks and usage metadata are not
// streamed via stream_event and MUST still come from the assistant
// envelope.
func TestAssistantEnvelopeStillEmitsToolUseAndUsage(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"assistant","message":{"id":"msg_abc","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":20}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawToolStart, sawUsage bool
	for _, e := range events {
		switch e.Kind {
		case provider.EventToolStart:
			sawToolStart = true
		case provider.EventTokenUsage:
			sawUsage = true
		}
	}
	if !sawToolStart {
		t.Errorf("assistant envelope did not emit EventToolStart: %+v", events)
	}
	if !sawUsage {
		t.Errorf("assistant envelope did not emit EventUsage: %+v", events)
	}
}

func TestParseStreamEventThinkingDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"considering..."}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventThinking {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventThinking)
	}
	if events[0].Content != "considering..." {
		t.Errorf("content: got %q, want %q", events[0].Content, "considering...")
	}
}

func TestParseStreamEventInputJSONDeltaSkipped(t *testing.T) {
	// input_json_delta carries incremental tool-call input JSON. The raw
	// NDJSON parser surfaces one EventToolStart per tool_use block on the
	// assistant message — partial inputs would confuse downstream consumers,
	// so they are swallowed at this layer.
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\""}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for input_json_delta, got %d: %+v", len(events), events)
	}
}

func TestParseStreamEventContentBlockStartEmitsLifecycleEvent(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"sig-1"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for content_block_start, got %d", len(events))
	}
	if events[0].Kind != provider.EventContentBlockStart {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventContentBlockStart)
	}
}

func TestParseStreamEventContentBlockStopEmitsLifecycleEvent(t *testing.T) {
	parser := NewParser()
	startLine := []byte(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
	if _, err := parser.ParseLine(testThread, startLine); err != nil {
		t.Fatalf("parse start: %v", err)
	}

	line := []byte(`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for content_block_stop, got %d", len(events))
	}
	if events[0].Kind != provider.EventContentBlockStop {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventContentBlockStop)
	}
}

func TestParseStreamEventPropagatesParentToolUseID(t *testing.T) {
	// Partial text deltas for a Task-tool subagent carry parent_tool_use_id
	// at the top level of the stream_event envelope. The parser must
	// propagate it onto the emitted text delta so triage can group child
	// turns under the parent Task.
	line := []byte(`{"type":"stream_event","event":"content_block_delta","parent_tool_use_id":"task_tool_sub","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTextDelta)
	}
	if events[0].ParentToolUseID != "task_tool_sub" {
		t.Errorf("parentToolUseID: got %q, want %q",
			events[0].ParentToolUseID, "task_tool_sub")
	}
}

func TestParseStreamEventThinkingDeltaCarriesParentToolUseID(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","parent_tool_use_id":"task_tool_sub","data":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"sub-thinking"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "task_tool_sub" {
		t.Errorf("parentToolUseID: got %q, want %q",
			events[0].ParentToolUseID, "task_tool_sub")
	}
}

func TestParseStreamEventEmptyDeltaText(t *testing.T) {
	// An empty text_delta is legal and must still emit a zero-length delta
	// event so that the accumulator is woken up on some providers — but the
	// current router treats zero-length as a no-op. Keep behavior: no event.
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty text_delta, got %d", len(events))
	}
}
