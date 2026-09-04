package claudetui

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// TestReconstructThinkingDeltaEmitsThinking is the thinking parity guard for the
// --thinking-display fix. With the flag set the wire carries thinking_delta text;
// this drives a summarized-mode turn (thinking block with real deltas + a text
// block) through the shared claude.Parser and asserts the thinking surfaces as
// EventThinking, exactly as the headless path produces it.
func TestReconstructThinkingDeltaEmitsThinking(t *testing.T) {
	sse := []string{
		`{"type":"message_start","message":{"id":"msg_t","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I'll work through "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"this step by step."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2lnbmF0dXJl"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The answer is 42."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}
	events := driveAgentRequest(t, agentReqBody, sse)

	var thinking strings.Builder
	for _, e := range findKind(events, provider.EventThinking) {
		thinking.WriteString(e.Content)
	}
	if got := thinking.String(); got != "I'll work through this step by step." {
		t.Fatalf("assembled thinking = %q, want %q; kinds=%v",
			got, "I'll work through this step by step.", kindsOf(events))
	}

	var text strings.Builder
	for _, e := range findKind(events, provider.EventTextDelta) {
		text.WriteString(e.Content)
	}
	if got := text.String(); got != "The answer is 42." {
		t.Fatalf("assembled text = %q, want %q", got, "The answer is 42.")
	}
}

// TestReconstructOmittedThinkingBlockKeepsTextIntact is a regression guard built
// from a turn captured live on 2.1.170 (the CLI capture spike thinking_title
// with AO_NO_THINK_DISPLAY=1): in omitted mode the wire emits an EMPTY,
// signature-only thinking block at index 0, then the real text at block index 1.
// This proves that empty block does NOT corrupt the following text — the text
// streams intact under a single content block — so the user's phantom/truncation
// was not introduced by the reconstruction layer. (The thinking fix removes this
// shape entirely: with display=summarized a thinking block carries real text or
// is absent, never empty-with-signature.)
func TestReconstructOmittedThinkingBlockKeepsTextIntact(t *testing.T) {
	const want = "Here is a basic flowchart. Want one tailored to something specific - e.g. the Agent Overflow IPC flow, a sequence diagram, or something else?"
	sse := []string{
		`{"type":"message_start","message":{"id":"msg_o","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		// Empty signature-only thinking block (the omitted-mode shape).
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"ErcBdGhpbmtpbmdfc2ln"}}`,
		`{"type":"content_block_stop","index":0}`,
		// Real text at block index 1, streamed in fragments (incl. a lone "the").
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Here is a basic flowchart. "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Want one tailored to something specific - e.g. "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" Agent Overflow IPC flow, a sequence diagram, or something else?"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":80}}`,
		`{"type":"message_stop"}`,
	}
	events := driveAgentRequest(t, agentReqBody, sse)

	// The full text streams intact — no split, no truncation.
	var text strings.Builder
	for _, e := range findKind(events, provider.EventTextDelta) {
		text.WriteString(e.Content)
	}
	if got := text.String(); got != want {
		t.Fatalf("assembled text = %q, want %q", got, want)
	}

	// Exactly two content blocks open: the thinking block and the text block.
	starts := findKind(events, provider.EventContentBlockStart)
	if len(starts) != 2 {
		t.Fatalf("content_block_start count = %d, want 2; kinds=%v", len(starts), kindsOf(events))
	}

	// The empty thinking block surfaces no thinking text (nothing to show).
	if th := findKind(events, provider.EventThinking); len(th) != 0 {
		t.Errorf("expected no thinking events for an empty thinking block, got %d", len(th))
	}

	// The turn still completes.
	if tc := findKind(events, provider.EventTurnComplete); len(tc) == 0 {
		t.Error("expected a turn-complete event")
	}
}
