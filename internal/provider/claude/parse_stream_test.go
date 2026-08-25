package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestParseStreamEvent(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`)

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
	if events[0].Content != "hello " {
		t.Errorf("content: got %q, want %q", events[0].Content, "hello ")
	}
}

func TestParseStreamEventMessageBoundariesDoNotEmitLifecycleEvents(t *testing.T) {
	for _, line := range [][]byte{
		[]byte(`{"type":"stream_event","event":"message_start","data":{"type":"message_start"}}`),
		[]byte(`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`),
	} {
		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("expected 0 events, got %d", len(events))
		}
	}
}
