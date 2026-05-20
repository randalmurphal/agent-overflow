package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func requireSoftRoundClose(t *testing.T, evt provider.ProviderEvent) provider.SoftRoundCloseMeta {
	t.Helper()
	switch meta := evt.TurnComplete.(type) {
	case *provider.SoftRoundCloseMeta:
		if meta != nil {
			return *meta
		}
	}
	t.Fatalf("turn complete meta type = %T, want SoftRoundCloseMeta", evt.TurnComplete)
	return provider.SoftRoundCloseMeta{}
}

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

func TestParseStreamEventMessageDeltaUsageUpdatesContextWindow(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":3}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventTokenUsage)
	}

	var window provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal context window: %v", err)
	}
	if window.UsedTokens != 123 {
		t.Fatalf("UsedTokens: got %d, want 123", window.UsedTokens)
	}
}

func TestParseStreamEventSubagentMessageDeltaUsageDoesNotUpdateParentContext(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","parent_tool_use_id":"task_tool_sub","data":{"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("subagent message_delta emitted parent context update: %+v", evt)
		}
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

// --- Soft round-close: message_delta.stop_reason ---
//
// Without these tests the working indicator stays stuck whenever a
// local_agent (Task) subagent is in flight at parent end_turn — Claude
// CLI withholds the `result` envelope until the subagent completes.
// The wire-typed signal that the parent has stopped emitting for the
// round is `stream_event.message_delta.delta.stop_reason` (gated on
// parent_tool_use_id == ""). See invariants.md §27.

func TestParseStreamEventMessageDeltaEndTurnEmitsSoftTurnComplete(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null},"usage":{"input_tokens":6,"output_tokens":34}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var sawTurnComplete bool
	for _, e := range events {
		if e.Kind != provider.EventTurnComplete {
			continue
		}
		sawTurnComplete = true
		meta := requireSoftRoundClose(t, e)
		if meta.StopReason != "end_turn" {
			t.Errorf("StopReason: got %q, want %q", meta.StopReason, "end_turn")
		}
		if e.ParentToolUseID != "" {
			t.Errorf("ParentToolUseID: got %q, want empty (parent only)", e.ParentToolUseID)
		}
	}
	if !sawTurnComplete {
		t.Fatalf("expected EventTurnComplete from message_delta stop_reason=end_turn, got %+v", events)
	}
}

func TestParseStreamEventMessageDeltaStopSequenceEmitsSoftTurnComplete(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"stop_sequence"},"usage":{"input_tokens":6,"output_tokens":1}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			found = true
		}
	}
	if !found {
		t.Fatalf("stop_sequence should fire soft turn-complete: %+v", events)
	}
}

func TestParseStreamEventMessageDeltaRefusalEmitsSoftTurnComplete(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"input_tokens":6,"output_tokens":1}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal should fire soft turn-complete: %+v", events)
	}
}

func TestParseStreamEventMessageDeltaToolUseDoesNotEmitSoftTurnComplete(t *testing.T) {
	// stop_reason="tool_use" means the model paused to call a tool;
	// more text follows. Firing turn-complete here would close the
	// indicator mid-round, then re-open it on the next message_start
	// for cosmetically jarring "Done → Working → Done" flicker —
	// AND it would cascade into per-round emission semantics that
	// don't match the model's actual state.
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":6,"output_tokens":493}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("tool_use must NOT fire soft turn-complete: %+v", events)
		}
	}
}

func TestParseStreamEventMessageDeltaPauseTurnDoesNotEmitSoftTurnComplete(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"input_tokens":6,"output_tokens":1}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("pause_turn must NOT fire soft turn-complete: %+v", events)
		}
	}
}

func TestParseStreamEventMessageDeltaMaxTokensDoesNotEmitSoftTurnComplete(t *testing.T) {
	// max_tokens means the model truncated; the harness may auto-continue
	// (Claude does this for some configurations). Firing turn-complete
	// here would clear the indicator, then a fresh message_start would
	// re-open it — same flicker problem as tool_use.
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":6,"output_tokens":64000}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("max_tokens must NOT fire soft turn-complete: %+v", events)
		}
	}
}

func TestParseStreamEventSubagentMessageDeltaEndTurnDoesNotEmitParentSoftTurnComplete(t *testing.T) {
	// A subagent's own message_delta carries parent_tool_use_id != null.
	// Firing the parent's turn-complete from a subagent's end_turn would
	// close the parent's indicator while the parent is still active —
	// confusing UI and breaking the round-id allocation.
	line := []byte(`{"type":"stream_event","event":"message_delta","parent_tool_use_id":"toolu_subagent","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":6,"output_tokens":1}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("subagent message_delta must NOT fire parent soft turn-complete: %+v", events)
		}
	}
}

// --- Advisor: message_delta usage suppression ---
//
// Claude's `message_delta` carries CUMULATIVE usage across every API
// call that SSE message generated. When the message contains an
// advisor block (`server_tool_use` / `advisor_tool_result`) the
// advisor's separate API call inflates `cache_read_input_tokens` by
// roughly the parent's context size (the advisor loads the parent's
// cache too), so the message_delta total is ~2x the parent's actual
// window. Pushing that to EventTokenUsage clobbers the parent's
// context meter with a doubled value. The parser tracks
// per-message "has advisor block?" between message_start and
// message_delta and suppresses the usage emit when set — the
// parent's correct per-call usage is still emitted from the
// non-advisor assistant envelopes.
//
// Wire shape (paraphrased from the May-19 capture of
// srvtoolu_01XrvFDa22jtChJqLhSoXnTH on msg_01TRHGRTMvngb8sahiv9):
//   - assistant envelopes for thinking/text/tool_use blocks all
//     report the parent's true cache_read=142901
//   - the trailing message_delta reports cache_read=286173 (~2×)
// We assert by counting EventTokenUsage events emitted from the
// stream path — the message_delta must contribute 0 when an advisor
// was involved.

func TestParseStreamEventMessageDeltaSuppressesUsageWhenAdvisorCalled(t *testing.T) {
	parser := NewParser()

	// New SSE message begins.
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_advisor_call","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}

	// content_block_start for the server_tool_use (advisor call).
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_e2e","name":"advisor","input":{}}}}`,
	)); err != nil {
		t.Fatalf("content_block_start parse: %v", err)
	}

	// Trailing message_delta with the inflated cumulative usage.
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":531,"cache_read_input_tokens":286173,"cache_creation_input_tokens":1619}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("advisor-containing message_delta must NOT emit EventTokenUsage: %+v", evt)
		}
	}
	// The soft turn-complete still fires — the usage gate is independent.
	var sawSoft bool
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			sawSoft = true
		}
	}
	if !sawSoft {
		t.Fatalf("expected soft EventTurnComplete from message_delta(stop_reason=end_turn): %+v", events)
	}
}

func TestParseStreamEventMessageDeltaSuppressesUsageWhenAdvisorResult(t *testing.T) {
	// The advisor result envelope shares the same msg_id as the
	// call but rides its own message_start/content_block_start
	// sequence on the wire. The flag must be set by the
	// advisor_tool_result block type too — not just server_tool_use
	// — because the cumulative usage on the result-message's
	// message_delta is just as inflated.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_advisor_result","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"advisor_tool_result","tool_use_id":"srvtoolu_e2e","content":{"type":"advisor_result","text":"hi"}}}}`,
	)); err != nil {
		t.Fatalf("content_block_start parse: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":531,"cache_read_input_tokens":286173}}}`,
	))
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("advisor_tool_result message_delta must NOT emit EventTokenUsage: %+v", evt)
		}
	}
}

func TestParseStreamEventMessageDeltaSuppressesUsageWhenMixedContentIncludesAdvisor(t *testing.T) {
	// In the real wire one SSE message carries text/thinking blocks
	// alongside the advisor server_tool_use (the model writes a few
	// lines, then calls the advisor). The message_delta's cumulative
	// usage is still inflated, so ANY advisor block in the message
	// must trigger suppression — not only the advisor-only case.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_mixed","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	)); err != nil {
		t.Fatalf("text block start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_mix","name":"advisor","input":{}}}}`,
	)); err != nil {
		t.Fatalf("advisor block start parse: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":531,"cache_read_input_tokens":286173}}}`,
	))
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("mixed (text+advisor) message_delta must NOT emit EventTokenUsage: %+v", evt)
		}
	}
}

func TestParseStreamEventMessageDeltaEmitsUsageAfterAdvisorMessageEnds(t *testing.T) {
	// Regression guard for the flag-leak case: an advisor message
	// followed by a clean text-only message must NOT carry the
	// advisor suppression into the next message_delta. message_start
	// is the boundary that resets the flag.
	parser := NewParser()

	// First message: advisor — usage must be suppressed.
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_advisor","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("advisor message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_a","name":"advisor","input":{}}}}`,
	)); err != nil {
		t.Fatalf("advisor block start parse: %v", err)
	}
	advisorEvents, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":531,"cache_read_input_tokens":286173}}}`,
	))
	if err != nil {
		t.Fatalf("advisor message_delta parse: %v", err)
	}
	for _, evt := range advisorEvents {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("advisor message_delta must NOT emit EventTokenUsage: %+v", evt)
		}
	}

	// Second message: text-only — usage MUST emit.
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_text","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("text message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	)); err != nil {
		t.Fatalf("text block start parse: %v", err)
	}
	textEvents, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":50,"cache_read_input_tokens":143000}}}`,
	))
	if err != nil {
		t.Fatalf("text message_delta parse: %v", err)
	}
	var usageEvents []provider.ProviderEvent
	for _, evt := range textEvents {
		if evt.Kind == provider.EventTokenUsage {
			usageEvents = append(usageEvents, evt)
		}
	}
	if len(usageEvents) != 1 {
		t.Fatalf("expected 1 EventTokenUsage after clean message_start reset, got %d: %+v", len(usageEvents), textEvents)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(usageEvents[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if window.UsedTokens != 1+143000 {
		t.Fatalf("UsedTokens: got %d, want %d", window.UsedTokens, 1+143000)
	}
}

func TestParseStreamEventMessageDeltaEmitsUsageWhenNoAdvisorEverSeen(t *testing.T) {
	// Plain text-only message: the advisor flag is never set, so the
	// existing message_delta path must continue to emit
	// EventTokenUsage. Mirrors the legacy behavior pinned by
	// TestParseStreamEventMessageDeltaUsageUpdatesContextWindow but
	// goes through the full preamble (message_start +
	// content_block_start) so the gate is exercised end-to-end.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_plain","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	)); err != nil {
		t.Fatalf("content_block_start parse: %v", err)
	}
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":50,"cache_read_input_tokens":100,"cache_creation_input_tokens":5}}}`,
	))
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	var saw bool
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("non-advisor message_delta must emit EventTokenUsage: %+v", events)
	}
}

func TestParseStreamEventMessageDeltaWithoutStopReasonDoesNotEmitTurnComplete(t *testing.T) {
	// Some message_delta envelopes carry only usage updates (no
	// delta.stop_reason). These are mid-message accounting snapshots,
	// not round-end signals. Existing context-meter behavior must not
	// regress — usage still emits an EventTokenUsage; nothing else.
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			t.Fatalf("message_delta without stop_reason must NOT fire turn-complete: %+v", events)
		}
	}
}

func TestParseStreamEventMessageDeltaWithoutUsageStillEmitsSoftTurnComplete(t *testing.T) {
	// Defensive: message_delta with stop_reason but no usage should
	// still fire the soft turn-complete. The two fields are
	// independent — a malformed/partial envelope shouldn't strand the
	// indicator.
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == provider.EventTurnComplete {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected soft turn-complete even without usage: %+v", events)
	}
}

// TestParseStreamSoftTurnCompleteWithoutPriorAssistantHasNoAssistantID
// pins the defensive case where message_delta arrives before any
// assistant envelope (degenerate ordering / fresh session attach).
// The peeked id is empty; soft fires with an empty assistant_message_id;
// triage's late-payload fold writes the trailing wire `result`'s id onto
// the empty column.
func TestParseStreamSoftTurnCompleteWithoutPriorAssistantHasNoAssistantID(t *testing.T) {
	parser := NewParser()
	deltaLine := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`)
	events, err := parser.ParseLine(testThread, deltaLine)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var softFound bool
	for _, e := range events {
		if e.Kind != provider.EventTurnComplete {
			continue
		}
		softFound = true
		meta := requireSoftRoundClose(t, e)
		if meta.AssistantMessageID != "" {
			t.Errorf("expected empty assistant_message_id when parser has not seen any assistant envelope yet, got %q", meta.AssistantMessageID)
		}
	}
	if !softFound {
		t.Fatalf("expected soft EventTurnComplete: %+v", events)
	}
}

// TestParseStreamUnknownStopReasonDoesNotEmitTurnComplete pins the
// closed-set behavior of `isSoftRoundCloseStopReason`. A future SDK
// addition (or a typo in the wire) must NOT fire the soft — the
// trailing wire `result` envelope still settles the turn correctly.
// Under-firing on an unknown is the safer failure mode.
func TestParseStreamUnknownStopReasonDoesNotEmitTurnComplete(t *testing.T) {
	for _, reason := range []string{"future_unknown", "model_overloaded", "", "END_TURN"} {
		t.Run(reason, func(t *testing.T) {
			line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}}`)
			events, err := ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			for _, e := range events {
				if e.Kind == provider.EventTurnComplete {
					t.Fatalf("unknown stop_reason %q must NOT fire soft turn-complete: %+v", reason, events)
				}
			}
		})
	}
}

// TestParseStreamSoftTurnCompleteCarriesPeekedAssistantMessageID pins
// the contract that the soft EventTurnComplete includes the parser's
// `lastAssistantMessageID` (peeked, not consumed) so the persisted
// turn row's `assistant_message_id` is populated on the FIRST settle.
// Without this, the trailing wire `result` envelope folds the id in
// later via `persistLateTurnPayload`, but the frontend's in-memory
// `latestSettledTurn.assistantMessageId` projection — which only
// reacts to `provider:turn_completed` — would stay null until the
// next thread switch / page refresh hydrated it from the store.
//
// The peek (rather than take) is load-bearing: the trailing real
// `result`'s `parseResult` consumes via takeLastAssistantMessageID and
// the parser's per-session "last id from this turn" invariant
// (cleared at turn boundary so it doesn't leak into the next turn)
// stays intact.
func TestParseStreamSoftTurnCompleteCarriesPeekedAssistantMessageID(t *testing.T) {
	parser := NewParser()
	// Emit an assistant envelope first so the parser tracks the id.
	assistantLine := []byte(`{"type":"assistant","message":{"id":"msg_peekABC","role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	if _, err := parser.ParseLine(testThread, assistantLine); err != nil {
		t.Fatalf("parse assistant: %v", err)
	}

	// Soft round-close from message_delta.
	deltaLine := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`)
	events, err := parser.ParseLine(testThread, deltaLine)
	if err != nil {
		t.Fatalf("parse message_delta: %v", err)
	}

	var softMeta provider.SoftRoundCloseMeta
	var found bool
	for _, e := range events {
		if e.Kind != provider.EventTurnComplete {
			continue
		}
		softMeta = requireSoftRoundClose(t, e)
		found = true
	}
	if !found {
		t.Fatalf("expected soft turn-complete: %+v", events)
	}
	if softMeta.AssistantMessageID != "msg_peekABC" {
		t.Errorf("expected peeked assistant_message_id=%q, got %q", "msg_peekABC", softMeta.AssistantMessageID)
	}

	// The trailing `result` envelope must still observe the id (peek
	// did not consume).
	resultLine := []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	rEvents, err := parser.ParseLine(testThread, resultLine)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	var resultAMID string
	for _, e := range rEvents {
		if e.Kind != provider.EventTurnComplete {
			continue
		}
		resultAMID = requireWireTurnComplete(t, []provider.ProviderEvent{e}).AssistantMessageID
	}
	if resultAMID != "msg_peekABC" {
		t.Errorf("result envelope must still consume the id: got %q, want %q", resultAMID, "msg_peekABC")
	}
}
