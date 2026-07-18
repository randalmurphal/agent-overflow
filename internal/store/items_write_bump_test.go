package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/itemmeta"
)

func insertBumpFixture(t *testing.T, s *Store, threadID string, base int64) {
	t.Helper()
	createDeleteConversationThread(t, s, threadID, base)
	for i := 0; i < 3; i++ {
		if err := s.InsertItem(Item{
			ID: threadID + "-row-" + string(rune('a'+i)), ThreadID: threadID,
			TurnIndex: 0, ItemIndex: i, Kind: "user_text", Role: "user",
			Meta: `{"draftSource":"queue"}`, CreatedAt: base + int64(i),
		}); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
}

func decodeMetaMap(t *testing.T, meta string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	return m
}

// mergeMetaKey mimics the echo-time provider_item_id stamp: read the
// current meta, set one key, keep everything else.
func mergeMetaKey(key, value string) func(string) (string, error) {
	return func(raw string) (string, error) {
		m := map[string]any{}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				return "", err
			}
		}
		m[key] = value
		out, err := json.Marshal(m)
		return string(out), err
	}
}

// TestBumpItemToTurnEndMovesRowAndTransformsMeta — the interrupt promote
// relies on bump + promotion marker landing in ONE transaction: the row
// moves to MAX(item_index)+1 for its turn and the transformed meta (with
// pre-existing keys preserved) commits with it.
func TestBumpItemToTurnEndMovesRowAndTransformsMeta(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	insertBumpFixture(t, s, "t-bump", base)

	item, err := s.BumpItemToTurnEnd("t-bump", "t-bump-row-a", itemmeta.MarkPromotedAtInterrupt, base+100)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if item.ItemIndex != 3 {
		t.Errorf("item_index = %d, want 3 (past rows at 0..2)", item.ItemIndex)
	}
	if item.UpdatedAt != base+100 {
		t.Errorf("updated_at = %d, want %d", item.UpdatedAt, base+100)
	}
	state, err := itemmeta.DecodePromotionState(item.Meta)
	if err != nil {
		t.Fatalf("decode promotion state from %q: %v", item.Meta, err)
	}
	if !state.Promoted {
		t.Errorf("meta %q should carry the promotion marker", item.Meta)
	}
	if m := decodeMetaMap(t, item.Meta); m["draftSource"] != "queue" {
		t.Errorf("pre-existing meta keys must survive the transform: %q", item.Meta)
	}
}

// TestBumpItemToTurnEndTransformErrorLeavesRowUntouched — a failing
// transform aborts the whole transaction: neither the index move nor
// any partial meta write may land.
func TestBumpItemToTurnEndTransformErrorLeavesRowUntouched(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	insertBumpFixture(t, s, "t-bumperr", base)

	boom := errors.New("boom")
	_, err := s.BumpItemToTurnEnd("t-bumperr", "t-bumperr-row-a", func(string) (string, error) {
		return "", boom
	}, base+100)
	if !errors.Is(err, boom) {
		t.Fatalf("bump error = %v, want wrapped boom", err)
	}

	items, err := s.ListItems("t-bumperr")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if items[0].ID != "t-bumperr-row-a" || items[0].ItemIndex != 0 {
		t.Errorf("row moved despite failed transform: %+v", items[0])
	}
	if items[0].Meta != `{"draftSource":"queue"}` {
		t.Errorf("meta changed despite failed transform: %q", items[0].Meta)
	}
}

// TestUpdateItemMetaMergeReportsChangeAndComposes — the echo-time stamp
// and the interrupt promote both merge into the same row's meta; each
// merge must read the current meta inside its transaction so sequential
// merges compose, and the changed flag must distinguish a real write
// from a duplicate echo (which must not bump updated_at).
func TestUpdateItemMetaMergeReportsChangeAndComposes(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	insertBumpFixture(t, s, "t-merge", base)

	promote := func(raw string) (string, error) { return itemmeta.MarkPromotedAtInterrupt(raw) }
	stamp := mergeMetaKey("providerItemId", "u-echo")

	item, changed, err := s.UpdateItemMetaMerge("t-merge", "t-merge-row-a", promote, base+10)
	if err != nil || !changed {
		t.Fatalf("promote merge: changed=%v err=%v", changed, err)
	}
	item, changed, err = s.UpdateItemMetaMerge("t-merge", "t-merge-row-a", stamp, base+20)
	if err != nil || !changed {
		t.Fatalf("stamp merge: changed=%v err=%v", changed, err)
	}
	if item.UpdatedAt != base+20 {
		t.Errorf("updated_at = %d, want %d", item.UpdatedAt, base+20)
	}

	state, err := itemmeta.DecodePromotionState(item.Meta)
	if err != nil {
		t.Fatalf("decode promotion state: %v", err)
	}
	m := decodeMetaMap(t, item.Meta)
	if !state.Promoted || m["providerItemId"] != "u-echo" || m["draftSource"] != "queue" {
		t.Errorf("merges did not compose: %q", item.Meta)
	}

	item, changed, err = s.UpdateItemMetaMerge("t-merge", "t-merge-row-a", stamp, base+30)
	if err != nil {
		t.Fatalf("duplicate stamp merge: %v", err)
	}
	if changed {
		t.Error("duplicate echo reported changed=true")
	}
	if item.UpdatedAt != base+20 {
		t.Errorf("duplicate echo bumped updated_at to %d, want %d untouched", item.UpdatedAt, base+20)
	}
}

// TestUpsertItemAtTurnHead — the deferred-prompt persist (round-7,
// R7-4): a new row lands at MIN(item_index)-1 for an occupied turn and
// at 0 for an empty one (identical to the append path there); an
// existing row updates in place with index and created_at preserved,
// exactly like UpsertItem.
func TestUpsertItemAtTurnHead(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	insertBumpFixture(t, s, "t-head", base)

	// Occupied turn (rows at 0..2): head insert takes -1.
	prompt := Item{
		ID: "t-head-prompt", ThreadID: "t-head", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "retried prompt", CreatedAt: base + 10, UpdatedAt: base + 10,
	}
	persisted, err := s.UpsertItemAtTurnHead(prompt)
	if err != nil {
		t.Fatalf("head upsert into occupied turn: %v", err)
	}
	if persisted.ItemIndex != -1 {
		t.Errorf("item_index = %d, want -1 (above rows at 0..2)", persisted.ItemIndex)
	}
	items, err := s.ListItems("t-head")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if items[0].ID != "t-head-prompt" {
		t.Errorf("head-inserted prompt does not sort first: %+v", items[0])
	}

	// Empty turn: head insert takes 0, same as the append path.
	empty := Item{
		ID: "t-head-first", ThreadID: "t-head", TurnIndex: 5,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "turn-opening prompt", CreatedAt: base + 20, UpdatedAt: base + 20,
	}
	persisted, err = s.UpsertItemAtTurnHead(empty)
	if err != nil {
		t.Fatalf("head upsert into empty turn: %v", err)
	}
	if persisted.ItemIndex != 0 {
		t.Errorf("empty-turn item_index = %d, want 0", persisted.ItemIndex)
	}

	// Existing row: update in place, index and created_at preserved.
	update := prompt
	update.Summary = "retried prompt v2"
	update.CreatedAt = base + 99
	update.UpdatedAt = base + 99
	persisted, err = s.UpsertItemAtTurnHead(update)
	if err != nil {
		t.Fatalf("head upsert of existing row: %v", err)
	}
	if persisted.ItemIndex != -1 || persisted.CreatedAt != base+10 {
		t.Errorf("existing row not updated in place: index=%d created_at=%d, want -1/%d",
			persisted.ItemIndex, persisted.CreatedAt, base+10)
	}
	if persisted.Summary != "retried prompt v2" {
		t.Errorf("summary = %q, want the updated text", persisted.Summary)
	}
}
