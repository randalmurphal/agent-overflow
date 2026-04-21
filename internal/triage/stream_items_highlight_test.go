package triage

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestTextDeltaStreamPopulatesMarkdownHTML emits a stream of text_delta
// events that together form a fenced code block and asserts every
// provider:item_upsert carries populated HighlightedContent with the
// expected Chroma span after the fence closes.
func TestTextDeltaStreamPopulatesMarkdownHTML(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Split across deltas so we exercise the firstBlock +
	// AppendItemSummaryAndHighlight code paths together.
	deltas := []string{
		"Here is some Go:\n",
		"```go\n",
		"package main\n\n",
		"func main() {}\n",
		"```\n",
		"Done.",
	}
	for _, d := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  "t1",
			Content:   d,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle text_delta %q: %v", d, err)
		}
	}

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatalf("expected item_upsert emissions, got none")
	}
	for i, e := range upserts {
		item, ok := e.data.(store.Item)
		if !ok {
			t.Fatalf("upsert[%d] type = %T, want store.Item", i, e.data)
		}
		if item.Kind != "assistant_text" {
			continue
		}
		if item.HighlightedContent == "" {
			t.Fatalf("upsert[%d] assistant_text has empty HighlightedContent (summary=%q)", i, item.Summary)
		}
	}

	// Final upsert contains the closed fence, so goldmark should have
	// handed it to Chroma and the output must carry at least one
	// `ch-`-prefixed span plus the code keyword.
	final, _ := upserts[len(upserts)-1].data.(store.Item)
	if !strings.Contains(final.HighlightedContent, `class="ch-`) {
		t.Fatalf("final HighlightedContent missing ch- class prefix; got: %q", final.HighlightedContent)
	}
	if !strings.Contains(final.HighlightedContent, "package") {
		t.Fatalf("final HighlightedContent missing 'package' keyword; got: %q", final.HighlightedContent)
	}
}

// TestThinkingStreamPopulatesANSIHTML emits thinking deltas that carry
// ANSI escape sequences and asserts every upsert has HighlightedContent
// populated with at least one terminal-to-html term- span.
func TestThinkingStreamPopulatesANSIHTML(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Split across deltas. The color sequence straddles the boundary so
	// the append path runs the cumulative renderer over spliced bytes.
	deltas := []string{
		"reasoning: ",
		"\x1b[31mred\x1b[0m",
		" then normal\n",
	}
	for _, d := range deltas {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventThinking,
			ThreadID:  "t1",
			Content:   d,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("handle thinking %q: %v", d, err)
		}
	}

	upserts := filterEmissions(*emissions, "provider:item_upsert")
	if len(upserts) == 0 {
		t.Fatalf("expected item_upsert emissions, got none")
	}
	for i, e := range upserts {
		item, ok := e.data.(store.Item)
		if !ok {
			t.Fatalf("upsert[%d] type = %T, want store.Item", i, e.data)
		}
		if item.Kind != "thinking" {
			continue
		}
		if item.HighlightedContent == "" {
			t.Fatalf("upsert[%d] thinking has empty HighlightedContent (summary=%q)", i, item.Summary)
		}
	}

	// The final upsert spans the full "red" color segment so the
	// renderer must have emitted a term- classed span over it.
	final, _ := upserts[len(upserts)-1].data.(store.Item)
	if !strings.Contains(final.HighlightedContent, "term-") {
		t.Fatalf("final thinking HighlightedContent missing term- class; got: %q", final.HighlightedContent)
	}
	if !strings.Contains(final.HighlightedContent, "red") {
		t.Fatalf("final thinking HighlightedContent missing 'red' payload; got: %q", final.HighlightedContent)
	}
}
