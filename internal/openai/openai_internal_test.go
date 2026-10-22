package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScrubRemovesAnythingShapedLikeAKey(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"Incorrect API key provided: sk-proj-AbCd****************************wxYz.",
		"Incorrect API key provided: sk-abc123.",
		"session sess-AbC_12-x expired",
	} {
		got := scrub(message)
		if strings.Contains(got, "sk-") || strings.Contains(got, "sess-") || !strings.Contains(got, "[key]") {
			t.Fatalf("scrub(%q) = %q", message, got)
		}
	}
	if got := scrub("buffer too small"); got != "buffer too small" {
		t.Fatalf("scrub changed a message without a key: %q", got)
	}
}

func TestTheSessionCarriesEveryOptionAndTurnDetectionOff(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(sessionUpdate(LiveTranscriptionOptions{
		Delay: "high", Languages: []string{"en", "fr"}, Keywords: []string{"Svelte"}, Prompt: "Code talk.",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session":{"audio":{"input":{"format":{"rate":24000,"type":"audio/pcm"},"transcription":{"delay":"high","keywords":["Svelte"],"languages":["en","fr"],"model":"gpt-live-transcribe","prompt":"Code talk."},"turn_detection":null}},"type":"transcription"},"type":"session.update"}`
	if string(payload) != want {
		t.Fatalf("session.update =\n%s\nwant\n%s", payload, want)
	}
	bare, err := json.Marshal(sessionUpdate(LiveTranscriptionOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "delay") || strings.Contains(string(bare), "languages") || strings.Contains(string(bare), "prompt") {
		t.Fatalf("unset options were sent: %s", bare)
	}
}
