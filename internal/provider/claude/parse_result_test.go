package claude

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestParseResult_StructuredOutputPresent(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"answer":42}}}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	want := []byte(`{"status":"done","outputs":{"answer":42}}`)
	if !bytes.Equal(events[0].StructuredOutput, want) {
		t.Fatalf("StructuredOutput = %s, want %s", events[0].StructuredOutput, want)
	}
}

func TestParseResult_StructuredOutputAbsentIsNil(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].StructuredOutput != nil {
		t.Fatalf("StructuredOutput = %s, want nil", events[0].StructuredOutput)
	}
}

func TestParseResult_SuccessIsErrorUsesResultFallback(t *testing.T) {
	parser := NewParser()
	const message = "API Error: 529 Overloaded. This is a server-side issue, usually temporary - try again in a moment. If it persists, check status.claude.com."
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"result":` + strconv.Quote(message) + `,"stop_reason":"stop_sequence"}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if meta.StopReason != "error" {
		t.Fatalf("StopReason: got %q, want error", meta.StopReason)
	}
	if meta.ErrorMessage != message {
		t.Fatalf("ErrorMessage: got %q, want %q", meta.ErrorMessage, message)
	}
}

func TestParseResult_ResultFallbackIsBounded(t *testing.T) {
	parser := NewParser()
	message := strings.Repeat("x", maxJoinedErrorChars+10)
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"result":` + strconv.Quote(message) + `}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	want := strings.Repeat("x", maxJoinedErrorChars) + "..."
	if meta.ErrorMessage != want {
		t.Fatalf("ErrorMessage length/content mismatch: got len=%d want len=%d", len(meta.ErrorMessage), len(want))
	}
}

// ede2_1_170InterruptResultLine is a verbatim `result` envelope from an
// interrupted turn on claude 2.1.170 (spike 2026-06-10, 6/6 runs
// identical shape): subtype=error_during_execution, is_error=true, and
// errors[] carrying only the "[ede_diagnostic] ..." marker — neither
// "aborted" nor "interrupted", so the string heuristic alone misreads
// it as a hard error. See claude-wire.md §result.
const ede2_1_170InterruptResultLine = `{"type":"result","subtype":"error_during_execution","duration_ms":1183,"duration_api_ms":0,"is_error":true,"num_turns":2,"stop_reason":null,"session_id":"2a1d04b1-e85f-445e-8cee-d55b3741b796","total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":"","iterations":[],"speed":"standard"},"modelUsage":{},"permission_denials":[],"terminal_reason":"aborted_streaming","fast_mode_state":"off","uuid":"9bab7771-7499-4fac-9895-3db7526ef7dd","errors":["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"]}`

// TestParseResult_InterruptAckClassifiesEdeDiagnosticAsInterrupted is
// the 2.1.170 regression: with the interrupt ack flagged, the
// ede_diagnostic result classifies as a user abort (stop_reason
// "interrupted", no error message), not a hard error.
func TestParseResult_InterruptAckClassifiesEdeDiagnosticAsInterrupted(t *testing.T) {
	parser := NewParser()
	parser.MarkInterruptAcked()
	events, err := parser.ParseLine(testThread, []byte(ede2_1_170InterruptResultLine))
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if !meta.Aborted {
		t.Fatalf("Aborted = false — acked interrupt's ede result classified as hard error")
	}
	if meta.StopReason != "interrupted" {
		t.Fatalf("StopReason = %q, want interrupted", meta.StopReason)
	}
	if meta.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty (interrupt wins over error)", meta.ErrorMessage)
	}
}

// TestParseResult_InterruptAckConsumedByAnyResult pins the take
// semantics: the FIRST result after the ack consumes it — even a
// success result (interrupt landed after the model already finished) —
// so a raced ack can never reclassify a later turn. Close clears too.
func TestParseResult_InterruptAckConsumedByAnyResult(t *testing.T) {
	parser := NewParser()
	parser.MarkInterruptAcked()
	events, err := parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("success result: %v", err)
	}
	if meta := requireWireTurnComplete(t, events); meta.Aborted {
		t.Fatalf("success result after ack marked Aborted — ack must not override a finished turn")
	}

	// The consumed ack must not leak into the next turn's ede result.
	events, err = parser.ParseLine(testThread, []byte(ede2_1_170InterruptResultLine))
	if err != nil {
		t.Fatalf("ede result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if meta.Aborted {
		t.Fatalf("ede result after a consumed ack marked Aborted — take semantics leaked across turns")
	}
	if meta.ErrorMessage == "" {
		t.Fatalf("unacked ede result lost its error message")
	}

	// Close clears a pending ack (parser-state contract).
	parser.MarkInterruptAcked()
	parser.Close()
	events, err = parser.ParseLine(testThread, []byte(ede2_1_170InterruptResultLine))
	if err != nil {
		t.Fatalf("ede result after close: %v", err)
	}
	if meta := requireWireTurnComplete(t, events); meta.Aborted {
		t.Fatalf("ack survived Close")
	}
}

// TestParseResult_LegacyAbortedStringStillInterrupted pins the string
// heuristic fallback: an ede result whose errors[] says "aborted"
// classifies as interrupted even with no ack (pre-2.1.170 CLIs, or an
// ack that raced a control-request timeout).
func TestParseResult_LegacyAbortedStringStillInterrupted(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Request was aborted."]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if !meta.Aborted {
		t.Fatalf("Aborted = false, want legacy aborted-string heuristic to classify as interrupted")
	}
	if meta.StopReason != "interrupted" {
		t.Fatalf("StopReason = %q, want interrupted", meta.StopReason)
	}
}

// TestParseResult_DoesNotEmitContextWindowFromModelUsage pins the
// deliberate non-behavior: `result.modelUsage[parent_model]` carries
// the same cumulative parent-only sum the trailing message_delta's
// top-level usage already emitted as EventTokenUsage. Emitting again
// from `result` would be a duplicate. `result` only emits
// EventTurnComplete.
func TestParseResult_DoesNotEmitContextWindowFromModelUsage(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(
		`{"type":"system","subtype":"init","session_id":"s1","cwd":"/tmp","tools":[],"model":"claude-opus-4-6[1m]","uuid":"u1"}`,
	)); err != nil {
		t.Fatalf("init parse: %v", err)
	}

	line := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":19716,"cache_read_input_tokens":98723,"cache_creation_input_tokens":57915},"modelUsage":{"claude-opus-4-6[1m]":{"inputTokens":9,"outputTokens":19716,"cacheReadInputTokens":98723,"cacheCreationInputTokens":57915,"contextWindow":1000000}}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result parse: %v", err)
	}

	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			t.Fatalf("result envelope must not emit EventTokenUsage: %+v", evt)
		}
	}
	requireWireTurnComplete(t, events)
}

func TestParseResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Done","session_id":"s1"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
}

// TestParseResult_IsErrorTrueEmitsTurnComplete pins the post-cleanup
// behaviour: an `is_error: true` result no longer branches into an
// EventError. The Python SDK's SDKResultError shape has no bare
// `error` field — only `errors: string[]` — so the previous branch
// always produced an EventError with empty content. The branch was
// dead (docs/references/claude-wire.md §result). Interrupted turns
// are still detected via detectInterrupted; other error subtypes
// settle as a normal EventTurnComplete whose stop_reason carries
// the shape signal.
func TestParseResult_IsErrorTrueWithSubtypeEmitsTurnComplete(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["something broke"]}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
}
