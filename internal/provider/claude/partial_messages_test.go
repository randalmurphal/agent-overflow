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

// --- Advisor + message_delta usage ---
//
// `message_delta.usage` top-level reports the cumulative sum across
// every parent `type:"message"` iteration in the turn. For a
// single-parent-call turn that equals the sole iteration; for an
// advisor turn (parent → advisor → parent) it is the SUM of the two
// parent iterations.
//
// The top-level sum IS what the Claude CLI's own `compactMetadata.preTokens`
// (the value that drives auto-compact) tracks. Verified across five
// production compactions on Claude 2.1.139 (sessions ef8fb8ee and
// b951a768): preTokens matched top-level within 1-2% on every
// turn — including advisor turns where the last-iteration snapshot
// was ~2× lower. See
// docs/references/fixtures/claude/advisor_pretokens_correlation_20260523.summary.json
// for the per-compaction table.
//
// Earlier the parser extracted `usage.iterations[-1].(type=message)`
// to "avoid an overcount" — that was wrong. The 2× ratio on advisor
// turns is real cumulative processing the CLI counts toward
// compaction; reading the last iteration alone undercounts by half
// and lets compaction trigger before the user-visible meter crosses
// any threshold. Reverted in this commit; previous fix was
// 1c1f9467. Advisor's own per-call usage surfaces solely via
// terminal `result.modelUsage[advisor_model]`, never as a stray
// message_delta into the parent stream. See parse_assistant.go
// advisorOnly for the envelope-level filter that drops advisor's
// standalone assistant frames.

func TestParseStreamEventMessageDeltaUsesTopLevelOnAdvisorTurn(t *testing.T) {
	// Numbers lifted verbatim from /tmp/advisor-spike/out.ndjson
	// (single-advisor capture against Claude 2.1.139):
	//   iter 0 (parent):  input=6, cache_read=18059, cache_create=9816
	//   iter 1 (advisor): input=29179, cache_read=0,   cache_create=0
	//   iter 2 (parent):  input=1, cache_read=27875, cache_create=238
	//   top-level (sum):  input=7, cache_read=45934, cache_create=10054
	// The meter must read TOP-LEVEL (55995 tokens — the cumulative
	// sum across both parent iterations). This matches what the CLI's
	// auto-compact trigger uses (compactMetadata.preTokens), verified
	// against production session data.
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_advisor_call","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_e2e","name":"advisor","input":{}}}}`,
	)); err != nil {
		t.Fatalf("content_block_start parse: %v", err)
	}

	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":137,"cache_read_input_tokens":45934,"cache_creation_input_tokens":10054,"iterations":[{"type":"message","input_tokens":6,"output_tokens":89,"cache_read_input_tokens":18059,"cache_creation_input_tokens":9816},{"type":"advisor_message","model":"claude-opus-4-7","input_tokens":29179,"output_tokens":677,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"message","input_tokens":1,"output_tokens":48,"cache_read_input_tokens":27875,"cache_creation_input_tokens":238}]}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}

	var usageEvent *provider.ProviderEvent
	var sawSoft bool
	for i, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			usageEvent = &events[i]
		}
		if evt.Kind == provider.EventTurnComplete {
			sawSoft = true
		}
	}
	if usageEvent == nil {
		t.Fatalf("expected EventTokenUsage from advisor-containing message_delta: %+v", events)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(usageEvent.Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if want := 7 + 45934 + 10054; window.UsedTokens != want {
		t.Fatalf("UsedTokens: got %d, want %d (top-level cumulative, what compactMetadata.preTokens tracks)", window.UsedTokens, want)
	}
	if !sawSoft {
		t.Fatalf("expected soft EventTurnComplete from message_delta(stop_reason=end_turn): %+v", events)
	}
}

func TestParseStreamEventMessageDeltaUsesTopLevelOnDoubleAdvisor(t *testing.T) {
	// Numbers from /tmp/advisor-spike/double.ndjson (two advisor calls in
	// one turn → 5 iterations: msg, adv, msg, adv, msg). The meter must
	// read top-level (100542 tokens, cumulative across all three parent
	// iterations) — this is what the CLI counts toward compaction.
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_double","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}

	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":8,"output_tokens":185,"cache_read_input_tokens":84960,"cache_creation_input_tokens":15574,"iterations":[{"type":"message","input_tokens":6,"output_tokens":94,"cache_read_input_tokens":18059,"cache_creation_input_tokens":15296},{"type":"advisor_message","model":"claude-opus-4-7","input_tokens":34664,"output_tokens":547},{"type":"message","input_tokens":1,"output_tokens":42,"cache_read_input_tokens":33355,"cache_creation_input_tokens":191},{"type":"advisor_message","model":"claude-opus-4-7","input_tokens":34779,"output_tokens":172},{"type":"message","input_tokens":1,"output_tokens":49,"cache_read_input_tokens":33546,"cache_creation_input_tokens":87}]}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}

	var usageEvent *provider.ProviderEvent
	for i, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			usageEvent = &events[i]
		}
	}
	if usageEvent == nil {
		t.Fatalf("expected EventTokenUsage from double-advisor message_delta: %+v", events)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(usageEvent.Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if want := 8 + 84960 + 15574; window.UsedTokens != want {
		t.Fatalf("UsedTokens: got %d, want %d (top-level cumulative, what compactMetadata.preTokens tracks)", window.UsedTokens, want)
	}
}

func TestParseStreamEventMessageDeltaUsesTopLevelOnProductionAdvisorTurn(t *testing.T) {
	// Production capture from ~/.claude/projects/.../b951a768-*.jsonl
	// line 125, the last assistant message_delta before
	// compactMetadata.preTokens=294675 triggered auto-compact at
	// line 131 (timestamp 2026-05-23T04:58:24Z).
	//
	// Real numbers from the wire — there are 3 iterations:
	//   iter 0 (parent):  input=1,    cache_read=143462, cache_create=287   = 143,750
	//   iter 1 (advisor): input=146484, (separate context window)
	//   iter 2 (parent):  input=1,    cache_read=143749, cache_create=2417  = 146,167
	//   top-level (sum):  input=2,    cache_read=287211, cache_create=2704  = 289,917
	//
	// The CLI compacted at preTokens=294,675. Top-level=289,917 is
	// within 1.6% of that — the remaining gap is the queued user
	// message ("do not run the dev server") + system overhead.
	// iter[-1]=146,167 is ~50% off (would have shown ~15% on the
	// meter while Claude saw ~30% and triggered the user's auto-compact
	// threshold).
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_prod","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":2997,"cache_read_input_tokens":287211,"cache_creation_input_tokens":2704,"iterations":[{"type":"message","input_tokens":1,"output_tokens":1531,"cache_read_input_tokens":143462,"cache_creation_input_tokens":287},{"type":"advisor_message","model":"claude-opus-4-7","input_tokens":146484,"output_tokens":11125,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"message","input_tokens":1,"output_tokens":1466,"cache_read_input_tokens":143749,"cache_creation_input_tokens":2417}]}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	var usageEvent *provider.ProviderEvent
	for i, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			usageEvent = &events[i]
		}
	}
	if usageEvent == nil {
		t.Fatalf("expected EventTokenUsage: %+v", events)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(usageEvent.Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if want := 2 + 287211 + 2704; window.UsedTokens != want {
		t.Fatalf("UsedTokens: got %d, want %d (top-level cumulative; compactMetadata.preTokens=294675 is within 1.6%%)", window.UsedTokens, want)
	}
}

func TestParseStreamEventMessageDeltaTopLevelWithoutIterations(t *testing.T) {
	// A plain non-advisor turn often omits the `iterations` array on
	// `message_delta.usage`. Top-level fields stand alone in that case
	// (and equal iterations[0] when present), so the parser's direct
	// top-level read still produces the right meter value.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_plain","role":"assistant","model":"claude-opus-4-7","content":[],"usage":{"input_tokens":1}}}}`,
	)); err != nil {
		t.Fatalf("message_start parse: %v", err)
	}
	line := []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":50,"cache_read_input_tokens":143000}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("message_delta parse: %v", err)
	}
	var usageEvent *provider.ProviderEvent
	for i, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			usageEvent = &events[i]
		}
	}
	if usageEvent == nil {
		t.Fatalf("expected EventTokenUsage from plain message_delta: %+v", events)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(usageEvent.Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if want := 1 + 143000; window.UsedTokens != want {
		t.Fatalf("UsedTokens: got %d, want %d", window.UsedTokens, want)
	}
}

func TestParseStreamEventMessageDeltaEmitsUsageAcrossMessages(t *testing.T) {
	// Two SSE messages in a row, the first containing an advisor turn,
	// the second text-only. Both message_delta usage snapshots must
	// reach the context meter — the parent's window grew across each
	// round. Meter reads top-level on both.
	parser := NewParser()

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
		`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":7,"output_tokens":137,"cache_read_input_tokens":45934,"cache_creation_input_tokens":10054,"iterations":[{"type":"message","input_tokens":6,"cache_read_input_tokens":18059,"cache_creation_input_tokens":9816},{"type":"advisor_message","input_tokens":29179},{"type":"message","input_tokens":1,"cache_read_input_tokens":27875,"cache_creation_input_tokens":238}]}}}`,
	))
	if err != nil {
		t.Fatalf("advisor message_delta parse: %v", err)
	}
	var advisorUsage []provider.ProviderEvent
	for _, evt := range advisorEvents {
		if evt.Kind == provider.EventTokenUsage {
			advisorUsage = append(advisorUsage, evt)
		}
	}
	if len(advisorUsage) != 1 {
		t.Fatalf("expected 1 EventTokenUsage from advisor message_delta, got %d: %+v", len(advisorUsage), advisorEvents)
	}
	var firstWindow provider.ContextWindow
	if err := json.Unmarshal(advisorUsage[0].Meta, &firstWindow); err != nil {
		t.Fatalf("unmarshal advisor window: %v", err)
	}
	if want := 7 + 45934 + 10054; firstWindow.UsedTokens != want {
		t.Fatalf("first message UsedTokens: got %d, want %d (top-level cumulative)", firstWindow.UsedTokens, want)
	}

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
	var textUsage []provider.ProviderEvent
	for _, evt := range textEvents {
		if evt.Kind == provider.EventTokenUsage {
			textUsage = append(textUsage, evt)
		}
	}
	if len(textUsage) != 1 {
		t.Fatalf("expected 1 EventTokenUsage from text message_delta, got %d: %+v", len(textUsage), textEvents)
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(textUsage[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal window: %v", err)
	}
	if want := 1 + 143000; window.UsedTokens != want {
		t.Fatalf("second message UsedTokens: got %d, want %d", window.UsedTokens, want)
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
