package store

import (
	"fmt"
	"testing"
)

func TestMessageAnchorRoundTrip(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")

	want := MessageAnchor{
		ThreadID:              "t1",
		UserItemID:            "t1-user:1",
		TurnIndex:             1,
		ProviderUserMessageID: "provider-user-1",
		ProviderParentUUID:    "parent-1",
		CreatedAt:             123,
	}
	if err := s.UpsertMessageAnchor(want); err != nil {
		t.Fatalf("upsert anchor: %v", err)
	}

	got, ok, err := s.GetMessageAnchor("t1", "t1-user:1")
	if err != nil {
		t.Fatalf("get anchor: %v", err)
	}
	if !ok {
		t.Fatal("anchor not found")
	}
	if got != want {
		t.Fatalf("anchor mismatch: got %+v want %+v", got, want)
	}

	if _, ok, err := s.GetMessageAnchor("t1", "no-such-item"); err != nil || ok {
		t.Fatalf("missing anchor: ok=%v err=%v, want absent without error", ok, err)
	}
}

// TestUpsertMessageAnchorReplaces — a resend of the same optimistic row
// re-anchors at the newest send: every column is overwritten, not merged.
func TestUpsertMessageAnchorReplaces(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")

	first := MessageAnchor{
		ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0,
		ProviderUserMessageID: "old-uuid", ProviderParentUUID: "old-parent", CreatedAt: 1,
	}
	if err := s.UpsertMessageAnchor(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second := MessageAnchor{
		ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 2,
		CreatedAt: 9,
	}
	if err := s.UpsertMessageAnchor(second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, ok, err := s.GetMessageAnchor("t1", "t1-user:0")
	if err != nil || !ok {
		t.Fatalf("get anchor: ok=%v err=%v", ok, err)
	}
	if got != second {
		t.Fatalf("anchor = %+v, want the replacement row %+v (upsert must not merge)", got, second)
	}
}

func TestUpdateMessageAnchorProviderIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")
	if err := s.UpsertMessageAnchor(MessageAnchor{
		ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("upsert anchor: %v", err)
	}

	if err := s.UpdateMessageAnchorProviderIDs("t1", "t1-user:0", "provider-id", "parent-id"); err != nil {
		t.Fatalf("update provider ids: %v", err)
	}
	got, ok, err := s.GetMessageAnchor("t1", "t1-user:0")
	if err != nil || !ok {
		t.Fatalf("get anchor: ok=%v err=%v", ok, err)
	}
	if got.ProviderUserMessageID != "provider-id" || got.ProviderParentUUID != "parent-id" {
		t.Fatalf("provider IDs = %q/%q", got.ProviderUserMessageID, got.ProviderParentUUID)
	}

	// Empty-string args preserve the stored values — an echo carrying
	// only one id must not blank the other.
	if err := s.UpdateMessageAnchorProviderIDs("t1", "t1-user:0", "", "parent-2"); err != nil {
		t.Fatalf("update parent only: %v", err)
	}
	got, _, err = s.GetMessageAnchor("t1", "t1-user:0")
	if err != nil {
		t.Fatalf("get anchor: %v", err)
	}
	if got.ProviderUserMessageID != "provider-id" || got.ProviderParentUUID != "parent-2" {
		t.Fatalf("provider IDs = %q/%q, want empty-preserves semantics", got.ProviderUserMessageID, got.ProviderParentUUID)
	}
}

// TestDeleteConversationFromTurnScopesAnchors pins the anchor half of
// the combined truncation: anchors from the cut turn onward are deleted
// in the same tx as the conversation rows and other threads' anchors
// are untouched.
func TestDeleteConversationFromTurnScopesAnchors(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")
	mustCreateThreadWithUserItems(t, s, "t2")
	for _, a := range []MessageAnchor{
		{ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, CreatedAt: 0},
		{ThreadID: "t1", UserItemID: "t1-user:1", TurnIndex: 1, CreatedAt: 1},
		{ThreadID: "t1", UserItemID: "t1-user:2", TurnIndex: 2, CreatedAt: 2},
		{ThreadID: "t2", UserItemID: "t2-user:2", TurnIndex: 2, CreatedAt: 2},
	} {
		if err := s.UpsertMessageAnchor(a); err != nil {
			t.Fatalf("upsert %s/%s: %v", a.ThreadID, a.UserItemID, err)
		}
	}

	deleted, _, err := s.DeleteConversationFromTurn("t1", 1)
	if err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted items = %d, want the two cut user rows", deleted)
	}
	list, err := s.ListMessageAnchors("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UserItemID != "t1-user:0" {
		t.Fatalf("remaining anchors = %+v", list)
	}
	other, err := s.ListMessageAnchors("t2")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("other thread anchor was deleted: %+v", other)
	}
}

// TestDeleteConversationFromTurnCoversDriftedAnchors — an anchor whose
// own turn_index sits BELOW the cut but whose item is deleted still
// dies via the items FK cascade; one whose cached turn drifted ABOVE
// the cut while its item survives must NOT be deleted.
func TestDeleteConversationFromTurnCoversDriftedAnchors(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")
	for _, a := range []MessageAnchor{
		// Item at turn 2 (deleted by the cut), anchor turn cached low.
		{ThreadID: "t1", UserItemID: "t1-user:2", TurnIndex: 0, CreatedAt: 0},
		// Item at turn 0 (survives the cut), anchor turn cached high.
		{ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 2, CreatedAt: 0},
	} {
		if err := s.UpsertMessageAnchor(a); err != nil {
			t.Fatalf("upsert drifted anchor %s: %v", a.UserItemID, err)
		}
	}

	if _, _, err := s.DeleteConversationFromTurn("t1", 2); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if _, ok, _ := s.GetMessageAnchor("t1", "t1-user:2"); ok {
		t.Error("deleted item's anchor should cascade away despite its stale-low cached turn")
	}
	if _, ok, err := s.GetMessageAnchor("t1", "t1-user:0"); err != nil || !ok {
		t.Errorf("surviving item's anchor must survive its stale-high cached turn (ok=%v err=%v)", ok, err)
	}
}

// TestUpdateThreadAndRemapProviderIDs pins R6-5 (round 6): the thread
// row, item meta rewrites, and anchor provider-id rewrites commit in
// ONE transaction — a failing rewrite rolls back the thread update
// too, so SessionRef can never move without its uuid remap.
func TestUpdateThreadAndRemapProviderIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")
	if err := s.UpsertMessageAnchor(MessageAnchor{
		ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0,
		ProviderUserMessageID: "old-uuid", ProviderParentUUID: "old-parent", CreatedAt: 0,
	}); err != nil {
		t.Fatalf("upsert anchor: %v", err)
	}
	thread, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	thread.SessionRef = "new-session-ref"
	if err := s.UpdateThreadAndRemapProviderIDs(thread,
		[]ItemMetaUpdate{{ItemID: "t1-user:0", Meta: `{"provider_item_id":"new-uuid"}`}},
		[]MessageAnchorProviderIDsUpdate{{UserItemID: "t1-user:0", ProviderUserMessageID: "new-uuid", ProviderParentUUID: ""}},
	); err != nil {
		t.Fatalf("UpdateThreadAndRemapProviderIDs: %v", err)
	}
	updated, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread updated: %v", err)
	}
	if updated.SessionRef != "new-session-ref" {
		t.Fatalf("SessionRef = %q, want the new ref", updated.SessionRef)
	}
	item, found, err := s.GetThreadItem("t1", "t1-user:0")
	if err != nil || !found {
		t.Fatalf("item: found=%v err=%v", found, err)
	}
	if item.Meta != `{"provider_item_id":"new-uuid"}` {
		t.Fatalf("item meta = %q, want the remapped blob", item.Meta)
	}
	anchors, err := s.ListMessageAnchors("t1")
	if err != nil || len(anchors) != 1 {
		t.Fatalf("anchors: %+v err=%v", anchors, err)
	}
	if anchors[0].ProviderUserMessageID != "new-uuid" {
		t.Fatalf("anchor provider_user_message_id = %q, want remapped", anchors[0].ProviderUserMessageID)
	}
	if anchors[0].ProviderParentUUID != "old-parent" {
		t.Fatalf("anchor provider_parent_uuid = %q, want empty-preserves to keep the stored value", anchors[0].ProviderParentUUID)
	}

	// A failing item rewrite (unknown id) rolls the WHOLE commit back.
	thread.SessionRef = "half-committed-ref"
	err = s.UpdateThreadAndRemapProviderIDs(thread,
		[]ItemMetaUpdate{{ItemID: "no-such-item", Meta: `{}`}}, nil)
	if err == nil {
		t.Fatal("remap against a missing item must error")
	}
	after, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("GetThread after rollback: %v", err)
	}
	if after.SessionRef != "new-session-ref" {
		t.Fatalf("SessionRef = %q after failed remap, want the previous ref (tx rolled back)", after.SessionRef)
	}
}

// TestListMessageAnchorsOrdersByItemTimelinePosition pins R9-3 (round
// 9): an echo-time replace gives an earlier sibling's anchor a LATER
// created_at than a later sibling's, and list order is consumed as
// message order (the fork remap) — so it must follow the linked item's
// (turn_index, item_index), not record time.
func TestListMessageAnchorsOrdersByItemTimelinePosition(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t1")
	if err := s.InsertItem(Item{
		ID: "t1-user:1b", ThreadID: "t1", TurnIndex: 1, ItemIndex: 5,
		Kind: "user_text", Role: "user", CreatedAt: 10,
	}); err != nil {
		t.Fatalf("insert same-turn sibling: %v", err)
	}
	for _, a := range []MessageAnchor{
		{ThreadID: "t1", UserItemID: "t1-user:1", TurnIndex: 1, CreatedAt: 900},
		{ThreadID: "t1", UserItemID: "t1-user:1b", TurnIndex: 1, CreatedAt: 100},
		{ThreadID: "t1", UserItemID: "t1-user:0", TurnIndex: 0, CreatedAt: 500},
	} {
		if err := s.UpsertMessageAnchor(a); err != nil {
			t.Fatalf("upsert %s: %v", a.UserItemID, err)
		}
	}

	list, err := s.ListMessageAnchors("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, len(list))
	for i, a := range list {
		got[i] = a.UserItemID
	}
	want := []string{"t1-user:0", "t1-user:1", "t1-user:1b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("anchor order = %v, want %v (timeline position, not created_at)", got, want)
	}
}

func mustCreateThreadWithUserItems(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateThread(makeThread(id, "claude")); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
	for turn := 0; turn < 3; turn++ {
		if err := s.InsertItem(Item{
			ID:        id + "-user:" + string(rune('0'+turn)),
			ThreadID:  id,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			CreatedAt: int64(turn + 1),
		}); err != nil {
			t.Fatalf("insert user item turn %d for %s: %v", turn, id, err)
		}
	}
}
