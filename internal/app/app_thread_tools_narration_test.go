package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A thinking block the provider marked as prose is stored as assistant
// text under its thinking ids (triage narration.go). The transcript reads
// it as the assistant's message, body included, with no thinking include.
func TestThreadToolsTranscriptReadsANarrationRowAsAssistantText(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "narration-thread")
	text := "quokka opens the note. " + strings.Repeat("The summary keeps going. ", 40)
	f.turn(t, thread.ID, 0, 1_000,
		textItem("u0", "user_text", "summarise"),
		store.Item{
			ID: "think:0:0", Kind: "assistant_text", Role: "assistant", Summary: text, Meta: `{"narration":true}`,
			PayloadID: "thinking:think:0:0", PayloadKind: "assistant_text", PayloadMeta: text, CreatedAt: 1, UpdatedAt: 1,
		},
		store.Item{ID: "think:0:1", Kind: "thinking", Role: "assistant", Summary: "pondering", CreatedAt: 1, UpdatedAt: 1})
	ctx := t.Context()
	bounds, err := f.adapter.ResolveWindow(ctx, threadtools.WindowQuery{ThreadID: thread.ID, Kind: threadtools.WindowAll})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	page, err := f.adapter.Transcript(ctx, threadtools.TranscriptQuery{ThreadID: thread.ID, From: bounds.From, To: bounds.To, Limit: 10})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("page = %#v", page.Items)
	}
	narration, thinking := page.Items[1], page.Items[2]
	if narration.ID != "think:0:0" || narration.Kind != "assistant_text" || narration.Role != "assistant" ||
		narration.Text != text || narration.Clipped || narration.Size != int64(len(text)) {
		t.Fatalf("narration row = %#v", narration)
	}
	if thinking.Kind != "thinking" || thinking.Text != "" {
		t.Fatalf("thinking row = %#v", thinking)
	}
}
