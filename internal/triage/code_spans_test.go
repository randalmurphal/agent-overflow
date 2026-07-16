package triage

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func TestEnrichCodeSpansMergesUnderKey(t *testing.T) {
	r := &Router{}
	r.SetCodeSpanEnricher(func(text string) json.RawMessage {
		if text != "hello" {
			t.Fatalf("enricher received %q, want the item summary", text)
		}
		return json.RawMessage(`{"hv":"v","blocks":[]}`)
	})

	item := store.Item{Kind: itemKindAssistantText, Summary: "hello",
		Meta: `{"pathRefs":[{"path":"a.go"}]}`}
	r.enrichCodeSpans(&item)

	var meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("meta not valid JSON after merge: %v", err)
	}
	if string(meta["codeSpans"]) != `{"hv":"v","blocks":[]}` {
		t.Fatalf("codeSpans = %s", meta["codeSpans"])
	}
	// Sibling keys round-trip untouched.
	if string(meta["pathRefs"]) != `[{"path":"a.go"}]` {
		t.Fatalf("pathRefs sibling changed: %s", meta["pathRefs"])
	}
}

// TestSettleStreamingTextEnrichesCodeSpans is the integration-shaped
// counterpart of the pathRefs settle test: the enricher fires from the
// settle path with the item's FINAL summary and its output lands under
// meta.codeSpans on the persisted row.
func TestSettleStreamingTextEnrichesCodeSpans(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.SetCodeSpanEnricher(func(text string) json.RawMessage {
		// Echo the received text so the assertion proves the enricher
		// saw the final summary, not an intermediate flush.
		blob, err := json.Marshal(map[string]string{"got": text})
		if err != nil {
			t.Fatalf("marshal echo: %v", err)
		}
		return blob
	})

	const content = "```python\npass\n```"
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: content, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	var meta struct {
		CodeSpans json.RawMessage `json:"codeSpans"`
	}
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta %q: %v", row.Meta, err)
	}
	want := fmt.Sprintf(`{"got":%q}`, content)
	if string(meta.CodeSpans) != want {
		t.Fatalf("codeSpans = %s, want %s", meta.CodeSpans, want)
	}
}

func TestEnrichCodeSpansGates(t *testing.T) {
	// Nil enricher: no-op.
	r := &Router{}
	item := store.Item{Kind: itemKindAssistantText, Summary: "x", Meta: "{}"}
	r.enrichCodeSpans(&item)
	if item.Meta != "{}" {
		t.Fatalf("nil enricher must not touch meta, got %q", item.Meta)
	}

	// Non-assistant-text kinds: enricher never runs.
	r.SetCodeSpanEnricher(func(string) json.RawMessage {
		t.Fatal("enricher must not run for non-text kinds")
		return nil
	})
	tool := store.Item{Kind: itemKindToolCall, Summary: "x"}
	r.enrichCodeSpans(&tool)

	// Empty enricher result: nothing stored.
	r.SetCodeSpanEnricher(func(string) json.RawMessage { return nil })
	item = store.Item{Kind: itemKindAssistantText, Summary: "x", Meta: ""}
	r.enrichCodeSpans(&item)
	if item.Meta != "" {
		t.Fatalf("empty enrichment must not touch meta, got %q", item.Meta)
	}

	// Corrupt existing meta: bytes preserved, key skipped (never
	// overwrite siblings we cannot round-trip).
	r.SetCodeSpanEnricher(func(string) json.RawMessage { return json.RawMessage(`{}`) })
	item = store.Item{Kind: itemKindAssistantText, Summary: "x", Meta: "not-json"}
	r.enrichCodeSpans(&item)
	if item.Meta != "not-json" {
		t.Fatalf("corrupt meta must be preserved, got %q", item.Meta)
	}
}
