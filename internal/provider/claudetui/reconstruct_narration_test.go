package claudetui

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// narrationSignature builds a thinking signature carrying tag, by hand:
// field 2 holds a message whose field 1 holds the tag in field 8.
func narrationSignature(tag string) string {
	header := append([]byte{0x42, byte(len(tag))}, tag...)
	body := append([]byte{0x0a, byte(len(header))}, header...)
	return base64.StdEncoding.EncodeToString(append([]byte{0x12, byte(len(body))}, body...))
}

// The gateway passes the response's frames through as stream_event lines,
// signature_delta included, so the shared parser marks a narration block's
// stop on this path as it does on the headless one.
func TestReconstructMarksANarrationThinkingStop(t *testing.T) {
	for tag, want := range map[string]bool{"narration": true, "thinking": false} {
		t.Run(tag, func(t *testing.T) {
			sse := []string{
				`{"type":"message_start","message":{"id":"msg_n","model":"claude-opus-4-8","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Read the file; one line changes."}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + narrationSignature(tag) + `"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done."}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":20}}`,
				`{"type":"message_stop"}`,
			}
			events := driveAgentRequest(t, agentReqBody, sse)
			marked := map[string]bool{}
			for _, e := range findKind(events, provider.EventContentBlockStop) {
				var meta struct {
					BlockType string `json:"blockType"`
					Narration bool   `json:"narration"`
				}
				if err := json.Unmarshal(e.Meta, &meta); err != nil {
					t.Fatalf("stop meta %s: %v", e.Meta, err)
				}
				if e.ContentPresent {
					t.Fatalf("a streamed block was recovered a second time: %+v", e)
				}
				marked[meta.BlockType] = meta.Narration
			}
			if got, ok := marked["thinking"]; !ok || got != want {
				t.Fatalf("thinking stop narration = %v (present %v), want %v", got, ok, want)
			}
			if marked["text"] {
				t.Fatal("the text block stopped with the marker")
			}
			if th := findKind(events, provider.EventThinking); len(th) != 1 || len(th[0].Meta) != 0 {
				t.Fatalf("thinking events = %+v, want the one delta with no meta", th)
			}
		})
	}
}
