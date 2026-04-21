package triage

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// TestPersistItemAlwaysRerendersHighlightedContent pins the invariant that
// persistItem renders item.HighlightedContent unconditionally against the
// current Summary. Callers that load a row from the store (which populates
// HighlightedContent with the previously rendered HTML), mutate Summary,
// and call persistItem without clearing HighlightedContent must end up
// with HTML that matches the NEW Summary — not the stale pre-mutation
// render. This is the same class of bug that c4a33fc closed for
// tool_result diff upgrade; guarding it at persistItem eliminates the
// class for every future caller.
func TestPersistItemAlwaysRerendersHighlightedContent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	fresh := store.Item{
		ID:        "text:0:0",
		ThreadID:  "t1",
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "streaming",
		Summary:   "# hello",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.persistItem(fresh, nil); err != nil {
		t.Fatalf("initial persist: %v", err)
	}

	original := firstItemByKind(t, st, "t1", "assistant_text")
	if !strings.Contains(original.HighlightedContent, "hello") {
		t.Fatalf("original HighlightedContent missing 'hello': %q", original.HighlightedContent)
	}
	if strings.Contains(original.HighlightedContent, "goodbye") {
		t.Fatalf("original HighlightedContent unexpectedly contains 'goodbye': %q", original.HighlightedContent)
	}

	// Simulate a caller that loaded the row from the store (which populates
	// HighlightedContent with the previous render), then mutated Summary
	// without touching HighlightedContent. This is exactly the shape of the
	// bug c4a33fc addressed. The stored HTML contains 'hello'; the caller's
	// new Summary is 'goodbye'. If persistItem preserves non-empty
	// HighlightedContent (the old contract), the row ends with HTML
	// describing 'hello' next to Summary 'goodbye' — the stale-HTML bug.
	stale := original
	stale.Summary = "# goodbye"
	stale.UpdatedAt = now + 1
	if err := router.persistItem(stale, nil); err != nil {
		t.Fatalf("stale-HTML persist: %v", err)
	}

	refreshed := firstItemByKind(t, st, "t1", "assistant_text")
	if !strings.Contains(refreshed.HighlightedContent, "goodbye") {
		t.Fatalf("expected re-rendered HTML with 'goodbye', got: %q", refreshed.HighlightedContent)
	}
	if strings.Contains(refreshed.HighlightedContent, "hello") {
		t.Fatalf("expected stale 'hello' to be gone from re-rendered HTML, got: %q", refreshed.HighlightedContent)
	}
}
