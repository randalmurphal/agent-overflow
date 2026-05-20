package claude

import (
	"strconv"
	"strings"
	"testing"
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
