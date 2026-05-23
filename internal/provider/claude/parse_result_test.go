package claude

import (
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

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

// TestParseResult_DoesNotEmitContextWindowFromModelUsage pins the
// deliberate non-behavior: `result.modelUsage[parent_model]` is the
// cumulative parent-only sum across every `type:"message"` iteration
// in the turn — the same value the trailing message_delta's
// top-level usage carries before parse_stream.go's
// lastParentIterationUsage narrows it to the last parent iteration.
// Emitting from the result envelope would either re-introduce the
// N×-overcount (raw modelUsage) or duplicate an already-correct
// meter reading (parsing iterations a second time). `result` only
// emits EventTurnComplete.
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
