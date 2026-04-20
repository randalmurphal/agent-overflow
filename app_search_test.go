package main

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// TestSearchThreadMessagesBindingReturnsEmptyOnBlankQuery — the binding
// must always return a non-nil slice so the JSON payload shape is
// stable for the frontend.
func TestSearchThreadMessagesBindingReturnsEmptyOnBlankQuery(t *testing.T) {
	app := newTestAppWithStore(t)
	hits, err := app.SearchThreadMessages("", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if hits == nil {
		t.Errorf("expected non-nil empty slice on blank query")
	}
	if len(hits) != 0 {
		t.Errorf("expected empty result, got %+v", hits)
	}
}

// TestSearchThreadMessagesBindingNormalizesNilToEmpty — even when the
// underlying store returns nil for a no-match search, the binding
// elevates it to an empty slice so the frontend contract is uniform.
func TestSearchThreadMessagesBindingNormalizesNilToEmpty(t *testing.T) {
	app := newTestAppWithStore(t)
	hits, err := app.SearchThreadMessages("nothing-matches", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if hits == nil {
		t.Errorf("expected non-nil empty slice, got nil")
	}
}

// TestSearchThreadMessagesBindingSurfacesResults — happy-path round trip
// verifying the binding delegates correctly and the caller sees typed hits.
func TestSearchThreadMessagesBindingSurfacesResults(t *testing.T) {
	app := newTestAppWithStore(t)
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID: "t1", ProjectID: defaultTestProjectID, Title: "Hello world", Provider: "codex",
		WorkspacePath: "/tmp", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: "i1", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Summary: "found a bug",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	hits, err := app.SearchThreadMessages("bug", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %+v", hits)
	}
	h := hits[0]
	if h.ThreadID != "t1" || h.ItemID != "i1" {
		t.Errorf("wrong hit: %+v", h)
	}
	if h.MatchType != "item" {
		t.Errorf("matchType: got %q, want item", h.MatchType)
	}
	if h.Summary != "found a bug" {
		t.Errorf("summary: got %q", h.Summary)
	}
}
