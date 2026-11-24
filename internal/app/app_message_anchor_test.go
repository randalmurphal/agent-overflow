package app

import (
	"testing"
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
// synthesized from the row. A wire id on the item's Meta is lifted, so the
// Claude rollback can do UUID-keyed slicing instead of the ordinal walk.
func TestResolveMessageAnchorSynthesizesWhenMissing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, id, meta, wantProviderID string
		turn                           int
	}{
		{name: "meta carries no wire id", id: "u:0", turn: 0},
		{name: "meta carries the wire id", id: "u:1", turn: 1, meta: `{"provider_item_id":"wire-u1"}`, wantProviderID: "wire-u1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t)
			thread := createAppTestThread(t, app, "rc-missing", "claude", t.TempDir())
			insertUserItemWithMeta(t, app.store, thread.ID, tc.id, tc.turn, "hello", tc.meta)

			userItem, ok, err := app.store.GetThreadItem(thread.ID, tc.id)
			if err != nil {
				t.Fatalf("get item: %v", err)
			}
			if !ok {
				t.Fatalf("user item %s missing", tc.id)
			}
			got := app.resolveMessageAnchor("test", thread.ID, userItem)
			if got.UserItemID != tc.id {
				t.Fatalf("synthesized userItemID = %q, want %s", got.UserItemID, tc.id)
			}
			if got.TurnIndex != tc.turn {
				t.Fatalf("synthesized turnIndex = %d, want %d", got.TurnIndex, tc.turn)
			}
			if got.ProviderUserMessageID != tc.wantProviderID {
				t.Fatalf("synthesized provider id = %q, want %q", got.ProviderUserMessageID, tc.wantProviderID)
			}
		})
	}
}
