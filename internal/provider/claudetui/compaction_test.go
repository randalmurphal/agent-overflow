package claudetui

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// newParserBackedRec builds a reconstructor whose emitted envelopes run through
// the REAL claude.Parser, returning the reconstructor and a pointer to the
// accumulated ProviderEvents — the same parity wiring driveAgentRequest uses,
// exposed so a test can interleave compaction hooks with agent requests.
func newParserBackedRec(t *testing.T) (*reconstructor, *[]provider.ProviderEvent) {
	t.Helper()
	parser := claude.NewParser()
	var out []provider.ProviderEvent
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		out = append(out, evs...)
	})
	return rec, &out
}

func jsonStr(s string) string { return string(mustMarshal(s)) }

// summarizerSSE is the compaction summarizer's response shape confirmed on
// 2.1.170 (spike/claude-mitm): one thinking block then one text block (the
// summary), ending end_turn. text empty omits the text block (a thinking-only
// partial). thinking empty omits the thinking block.
func summarizerSSE(thinking, text string) []string {
	sse := []string{
		`{"type":"message_start","message":{"id":"msg_compact","model":"claude-haiku","role":"assistant","usage":{"input_tokens":100,"output_tokens":1}}}`,
	}
	idx := 0
	if thinking != "" {
		sse = append(sse,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":`+jsonStr(thinking)+`}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
		)
		idx = 1
	}
	if text != "" {
		sse = append(sse,
			`{"type":"content_block_start","index":`+itoa(idx)+`,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":`+itoa(idx)+`,"delta":{"type":"text_delta","text":`+jsonStr(text)+`}}`,
			`{"type":"content_block_stop","index":`+itoa(idx)+`}`,
		)
	}
	sse = append(sse,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":50}}`,
		`{"type":"message_stop"}`,
	)
	return sse
}

func itoa(i int) string { return string(rune('0' + i)) }

// captureSummarizer drives one armed summarizer through the capture path: it
// claims the request (must be non-nil because armed), streams its SSE, and ends
// it — exactly what the gateway does for an armed classAgent request.
func captureSummarizer(t *testing.T, rec *reconstructor, thinking, text string) {
	t.Helper()
	ar := rec.beginCompactionCapture()
	if ar == nil {
		t.Fatal("beginCompactionCapture returned nil while armed")
	}
	for _, s := range summarizerSSE(thinking, text) {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()
}

func boundaryMeta(t *testing.T, e provider.ProviderEvent) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(e.Meta, &m); err != nil {
		t.Fatalf("compact-boundary meta unmarshal: %v (meta=%s)", err, e.Meta)
	}
	return m
}

// TestCompactionStreamsReasoningAndCommitsSummary is the headline behavior: the
// armed summarizer's reasoning STREAMS live as compaction-scoped thinking (the
// "compact" tail above the divider), while the single EventCompactBoundary
// carries only the committed PostCompact summary — never the reasoning.
func TestCompactionStreamsReasoningAndCommitsSummary(t *testing.T) {
	rec, out := newParserBackedRec(t)

	rec.armCompaction()
	captureSummarizer(t, rec, "Let me review the conversation so far.", "SSE summary text")
	rec.finalizeCompaction("auto", "Committed summary of the conversation.")

	// Reasoning streams live as compaction-scoped thinking, NOT a main-loop block.
	thinking := findKind(*out, provider.EventThinking)
	if len(thinking) == 0 {
		t.Fatalf("summarizer reasoning did not stream as EventThinking (kinds %v)", kindsOf(*out))
	}
	streamed := ""
	for _, e := range thinking {
		if e.ParentToolUseID != provider.CompactionReasoningScope {
			t.Errorf("reasoning thinking carried scope %q, want the reserved compaction scope", e.ParentToolUseID)
		}
		streamed += e.Content
	}
	if streamed != "Let me review the conversation so far." {
		t.Errorf("streamed reasoning = %q, want the summarizer's thinking", streamed)
	}

	// The boundary carries the committed summary + trigger, and NOT the reasoning.
	boundaries := findKind(*out, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("want exactly 1 compact-boundary, got %d (kinds %v)", len(boundaries), kindsOf(*out))
	}
	meta := boundaryMeta(t, boundaries[0])
	if meta["summary"] != "Committed summary of the conversation." {
		t.Errorf("boundary summary = %q, want the committed PostCompact summary", meta["summary"])
	}
	if meta["trigger"] != "auto" {
		t.Errorf("boundary trigger = %q, want auto", meta["trigger"])
	}
	if got, ok := meta["thinking"]; ok {
		t.Errorf("boundary must not carry thinking (it streamed live), got %q", got)
	}
}

// TestCompactionSummarizerSuppressesTurn proves the summarizer never surfaces as
// an agent TURN: none of init / streamed summary text / tool starts /
// turn-complete appear. Its reasoning DOES stream, but every thinking and
// content-block event carries the reserved compaction scope, so triage routes it
// to the compact tail rather than the main timeline.
func TestCompactionSummarizerSuppressesTurn(t *testing.T) {
	rec, out := newParserBackedRec(t)

	rec.armCompaction()
	captureSummarizer(t, rec, "internal reasoning", "the summary")
	rec.finalizeCompaction("auto", "the summary")

	for _, k := range []provider.EventKind{
		provider.EventInit,
		provider.EventTextDelta,
		provider.EventToolStart,
		provider.EventTurnComplete,
	} {
		if got := findKind(*out, k); len(got) != 0 {
			t.Errorf("summarizer leaked %d %s event(s); it must not render as a turn", len(got), k)
		}
	}
	// The reasoning streams, but only as reserved-scope events (never main-loop).
	scoped := 0
	for _, k := range []provider.EventKind{
		provider.EventThinking,
		provider.EventContentBlockStart,
		provider.EventContentBlockStop,
	} {
		for _, e := range findKind(*out, k) {
			if e.ParentToolUseID != provider.CompactionReasoningScope {
				t.Errorf("%s carried scope %q, want the reserved compaction scope", k, e.ParentToolUseID)
			}
			scoped++
		}
	}
	if scoped == 0 {
		t.Error("summarizer reasoning did not stream (expected scoped thinking / content-block events)")
	}
	if got := findKind(*out, provider.EventCompactBoundary); len(got) != 1 {
		t.Fatalf("want 1 compact-boundary, got %d (kinds %v)", len(got), kindsOf(*out))
	}
}

// TestCompactionSummaryFallsBackToCapturedText proves that when the PostCompact
// hook carries no committed summary, the captured SSE text block is used.
func TestCompactionSummaryFallsBackToCapturedText(t *testing.T) {
	rec, out := newParserBackedRec(t)

	rec.armCompaction()
	captureSummarizer(t, rec, "thinking", "fallback summary from the SSE")
	rec.finalizeCompaction("manual", "") // hook carried no compact_summary

	boundaries := findKind(*out, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("want 1 compact-boundary, got %d", len(boundaries))
	}
	if got := boundaryMeta(t, boundaries[0])["summary"]; got != "fallback summary from the SSE" {
		t.Errorf("boundary summary = %q, want the captured SSE text fallback", got)
	}
}

// TestCompactionAbortRetryKeepsCompletedSummary reproduces the abort/retry the
// auto-compaction probe surfaced: a user submitting mid-compaction aborts the
// first summarizer and a fresh PreCompact + retry follows. Reasoning streams
// live per attempt, but the boundary's committed summary must come from the
// COMPLETED retry — the re-arm resets the pending capture so the aborted
// attempt's summary can't leak into the boundary fallback.
func TestCompactionAbortRetryKeepsCompletedSummary(t *testing.T) {
	rec, out := newParserBackedRec(t)

	// Attempt 1: arm, summarizer aborted (re-armed before it finalized).
	rec.armCompaction()
	captureSummarizer(t, rec, "ABORTED partial reasoning", "aborted summary")

	// Attempt 2: a fresh PreCompact (re-arm) discards attempt 1's pending
	// capture, then the completed summarizer runs and PostCompact finalizes with
	// no committed summary — so the boundary falls back to attempt 2's summary.
	rec.armCompaction()
	captureSummarizer(t, rec, "COMPLETED reasoning", "completed summary")
	rec.finalizeCompaction("auto", "")

	boundaries := findKind(*out, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("want exactly 1 compact-boundary (one finalize), got %d", len(boundaries))
	}
	if got := boundaryMeta(t, boundaries[0])["summary"]; got != "completed summary" {
		t.Errorf("boundary summary = %q, want the completed retry's summary (the aborted partial must be discarded)", got)
	}
}

// TestCompactionFailureEmitsNoBoundary proves the "drop silently" decision: a
// captured summarizer with no PostCompact (a failed compaction) renders no
// boundary, and a subsequent real turn reconstructs normally because the
// summarizer already disarmed the capture.
func TestCompactionFailureEmitsNoBoundary(t *testing.T) {
	rec, out := newParserBackedRec(t)

	rec.armCompaction()
	captureSummarizer(t, rec, "reasoning that never commits", "summary that never commits")
	// No finalizeCompaction — the compaction failed.

	if got := findKind(*out, provider.EventCompactBoundary); len(got) != 0 {
		t.Fatalf("a failed compaction must emit no boundary, got %d", len(got))
	}

	// The next real turn: gateway tries the capture path first, but we're no
	// longer armed (the summarizer disarmed), so it renders as a normal turn.
	if ar := rec.beginCompactionCapture(); ar != nil {
		t.Fatal("beginCompactionCapture should be nil after the summarizer disarmed")
	}
	req := mustClassifyAgent(t, agentReqBody)
	ar := rec.beginAgentRequest(req)
	for _, s := range []string{
		`{"type":"message_start","message":{"id":"msg_next","model":"claude-haiku","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	} {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()

	if got := findKind(*out, provider.EventTurnComplete); len(got) == 0 {
		t.Error("the real turn after a failed compaction must reconstruct normally (no turn-complete seen)")
	}
}

func mustClassifyAgent(t *testing.T, body string) *messagesRequest {
	t.Helper()
	class, req := classifyRequest([]byte(body))
	if class != classAgent {
		t.Fatalf("classifyRequest = %v, want classAgent", class)
	}
	return req
}
