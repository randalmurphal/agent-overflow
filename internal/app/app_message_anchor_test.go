package app

import (
	"testing"

	"agent-overflow/internal/store"
)

// The at-send record wrote an anchor keyed by the user item id and matching
// turn index, and the lookup returns it.
func TestResolveMessageAnchorReturnsPersisted(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "rc-found", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
	seedMessageAnchor(t, app.store, thread.ID, "u:0", 0, "provider-u-0", "")

	userItem, ok, err := app.store.GetThreadItem(thread.ID, "u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveMessageAnchor("test", thread.ID, userItem)
	// The provider id only exists on the persisted row (the item meta is
	// empty), so seeing it proves the lookup returned the stored anchor
	// rather than synthesizing.
	if got.ProviderUserMessageID != "provider-u-0" {
		t.Fatalf("anchor provider id = %q, want persisted provider-u-0", got.ProviderUserMessageID)
	}
	if got.TurnIndex != 0 {
		t.Fatalf("turn index = %d, want 0", got.TurnIndex)
	}
}

// The at-send record didn't land. The provider rollback helpers only need
// TurnIndex plus whatever ids the item meta carries, so a minimal record is
// synthesized from the row.
func TestResolveMessageAnchorSynthesizesWhenMissing(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "rc-missing", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")

	userItem, ok, err := app.store.GetThreadItem(thread.ID, "u:0")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatalf("user item u:0 missing")
	}
	got := app.resolveMessageAnchor("test", thread.ID, userItem)
	if got.UserItemID != "u:0" {
		t.Fatalf("synthesized userItemID = %q, want u:0", got.UserItemID)
	}
	if got.TurnIndex != 0 {
		t.Fatalf("synthesized turnIndex = %d, want 0", got.TurnIndex)
	}
	if got.ProviderUserMessageID != "" {
		t.Fatalf("synthesized provider id = %q, want empty (item meta carries none)", got.ProviderUserMessageID)
	}
}

// With no persisted anchor the helper lifts the wire id off the user item's
// Meta, so the Claude rollback can do UUID-keyed slicing instead of the
// ordinal walk.
func TestResolveMessageAnchorSynthesizesProviderUserMessageID(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "synth-uuid", "claude", t.TempDir())
	const meta = `{"provider_item_id":"wire-u1"}`
	insertUserItemWithMeta(t, app.store, thread.ID, "u:1", 1, "second", meta)

	item := store.Item{ID: "u:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Meta: meta}
	anchor := app.resolveMessageAnchor("test", thread.ID, item)
	if anchor.ProviderUserMessageID != "wire-u1" {
		t.Fatalf("synthesized ProviderUserMessageID = %q, want %q", anchor.ProviderUserMessageID, "wire-u1")
	}
	if anchor.UserItemID != "u:1" {
		t.Fatalf("synthesized UserItemID = %q, want u:1", anchor.UserItemID)
	}
	if anchor.TurnIndex != 1 {
		t.Fatalf("synthesized TurnIndex = %d, want 1", anchor.TurnIndex)
	}
}
