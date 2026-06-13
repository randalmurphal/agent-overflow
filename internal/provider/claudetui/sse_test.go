package claudetui

import (
	"encoding/json"
	"testing"
)

func TestSSEScannerReassemblesAcrossChunks(t *testing.T) {
	var got []string
	s := newSSEScanner(func(ev json.RawMessage) { got = append(got, string(ev)) })

	// A realistic frame stream, deliberately split at awkward byte boundaries:
	// mid-JSON, mid-line, and across the blank-line separators.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		": keep-alive comment line\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	chunks := splitEvery(stream, 7) // 7 bytes: guarantees mid-line/mid-JSON splits
	for _, c := range chunks {
		s.write([]byte(c))
	}

	want := []string{
		`{"type":"message_start","message":{"id":"m1"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_stop"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestSSEScannerSkipsDoneAndNonJSON(t *testing.T) {
	var got int
	s := newSSEScanner(func(json.RawMessage) { got++ })
	s.write([]byte("data: [DONE]\n\ndata: not json here\n\ndata: {\"type\":\"ok\"}\n\n"))
	if got != 1 {
		t.Fatalf("expected only the one valid JSON event, got %d", got)
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
