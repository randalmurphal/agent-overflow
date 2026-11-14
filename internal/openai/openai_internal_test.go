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

// The speech session and request are the shapes measured against OpenAI.
func TestTheSpeechEventsAreTheMeasuredShapes(t *testing.T) {
	t.Parallel()
	update, err := json.Marshal(speechSessionUpdate("marin"))
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := json.Marshal(SpeechInstructions)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session":{"audio":{"input":{"turn_detection":null},"output":{"format":{"rate":24000,"type":"audio/pcm"},"voice":"marin"}},` +
		`"instructions":` + string(instructions) + `,"output_modalities":["audio"],"reasoning":{"effort":"minimal"},"tool_choice":"none","tools":[],"type":"realtime"},"type":"session.update"}`
	if string(update) != want {
		t.Fatalf("session.update =\n%s\nwant\n%s", update, want)
	}
	envelope, err := SpeechEnvelope(`Say "hi" <now>`)
	if err != nil {
		t.Fatal(err)
	}
	if envelope != `{"require_repeat_verbatim":true,"response_text":"Say \"hi\" `+"\\u003cnow\\u003e"+`"}` {
		t.Fatalf("envelope = %s", envelope)
	}
	create, err := json.Marshal(speechResponseCreate(envelope))
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"response":{"conversation":"none","input":[{"content":[{"text":` + string(quoted) + `,"type":"input_text"}],"role":"user","type":"message"}]},"type":"response.create"}`
	if string(create) != want {
		t.Fatalf("response.create =\n%s\nwant\n%s", create, want)
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
