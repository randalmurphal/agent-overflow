package store

import (
	"errors"
	"testing"
	"time"
)

// AppendItemUnderParent writes a row only while its parent is in history,
// in the parent's turn whatever turn the caller named, and writes nothing
// once a cut removed the parent.
func TestAppendItemUnderParentFollowsItsParent(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t", base)
	for turn := 0; turn < 2; turn++ {
		if err := s.InsertItem(Item{ID: "user-" + string(rune('0'+turn)), ThreadID: "t", TurnIndex: turn, Kind: "user_text", Role: "user", CreatedAt: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertItem(Item{ID: "spawn", ThreadID: "t", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	row := Item{ID: "cmd", ThreadID: "t", TurnIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "spawn", CreatedAt: base, UpdatedAt: base}
	if err := s.WithSubagentCard("t", "spawn", func(card *SubagentCard) error {
		row.SubagentCard = card
		persisted, written, err := s.AppendItemUnderParent(row, &Payload{ID: "out", Kind: "command_output", Data: []byte("done\n"), CreatedAt: base})
		if err != nil || !written || persisted.TurnIndex != 0 || persisted.PayloadID != "out" {
			t.Fatalf("under a present parent: %+v written=%v err=%v, want turn 0 with its payload", persisted, written, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.DeleteConversationFromTurn("t", 0); err != nil {
		t.Fatal(err)
	}
	row.ID = "cmd-late"
	if err := s.WithSubagentCard("t", "spawn", func(card *SubagentCard) error {
		row.SubagentCard = card
		_, written, err := s.AppendItemUnderParent(row, nil)
		if err != nil || written {
			t.Fatalf("under a cut parent: written=%v err=%v, want nothing written", written, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetThreadItem("t", "cmd-late"); err != nil || found {
		t.Fatalf("row under a cut parent stored: found=%v err=%v", found, err)
	}
	if _, _, err := s.AppendItemUnderParent(Item{ID: "orphan", ThreadID: "t", Kind: "tool_call"}, nil); err == nil {
		t.Fatalf("a row without a parent was accepted")
	}
}

// AppendItemUnderParent only appends: it refuses, writing nothing, a parent
// that is not a tool_call, a parent whose chain holds the row, and any id
// already stored, wherever it sits and whatever it is: a row in the same
// place, a scoped user prompt or a nested spawn with the same parent and
// turn, a row on another turn (as the newest-turn fallback before the
// scoped write could leave one), and a row under another parent.
func TestAppendItemUnderParentOnlyAppends(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t", base)
	seed := []Item{
		{ID: "user-0", TurnIndex: 0, Kind: "user_text", Role: "user"},
		{ID: "spawn", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed"},
		{ID: "text", TurnIndex: 0, ItemIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "hello"},
		{ID: "other", TurnIndex: 0, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed"},
		{ID: "user-1", TurnIndex: 1, Kind: "user_text", Role: "user"},
	}
	for _, item := range seed {
		item.ThreadID, item.CreatedAt = "t", base
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	children := []Item{
		{ID: "kept", TurnIndex: 0, ItemIndex: 4, ParentID: "spawn", Summary: "kept"},
		{ID: "prompt", TurnIndex: 0, ItemIndex: 7, ParentID: "spawn", Summary: "do the thing", Kind: "user_text", Role: "user"},
		{ID: "nested-spawn", TurnIndex: 0, ItemIndex: 8, ParentID: "spawn", Summary: "spawn_agent", ToolName: "collab_agent"},
		{ID: "wrong-turn", TurnIndex: 1, ItemIndex: 1, ParentID: "spawn", Summary: "wrong turn"},
		{ID: "wrong-turn-free", TurnIndex: 1, ItemIndex: 40, ParentID: "spawn", Summary: "wrong turn, index free in turn 0"},
		{ID: "under-other", TurnIndex: 0, ItemIndex: 5, ParentID: "other", Summary: "under other"},
		{ID: "loop", TurnIndex: 0, ItemIndex: 6, ParentID: "cycled", Summary: "loop"},
	}
	for _, item := range children {
		item.ThreadID, item.Status, item.CreatedAt = "t", "completed", base
		if item.Kind == "" {
			item.Kind, item.Role = "tool_call", "assistant"
		}
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	appendRow := func(id, parent string) (bool, error) {
		t.Helper()
		var written bool
		var appendErr error
		err := s.WithSubagentCard("t", parent, func(card *SubagentCard) error {
			_, written, appendErr = s.AppendItemUnderParent(Item{
				ID: id, ThreadID: "t", TurnIndex: 1, Kind: "tool_call", Role: "assistant",
				Status: "completed", Summary: "rewritten", ParentID: parent, CreatedAt: base, UpdatedAt: base + 1,
				SubagentCard: card,
			}, nil)
			return nil
		})
		if err != nil {
			t.Fatalf("card for %s: %v", parent, err)
		}
		return written, appendErr
	}

	for _, tc := range []struct{ name, id, parent string }{
		{"a parent that is not a tool_call", "new-under-text", "text"},
		{"a parent whose chain holds the row", "cycled", "loop"},
		{"a stored row in another turn, its index taken in the parent's", "wrong-turn", "spawn"},
		{"a stored row in another turn, its index free in the parent's", "wrong-turn-free", "spawn"},
		{"a stored row under another parent", "under-other", "spawn"},
		{"a stored row in the same place", "kept", "spawn"},
		{"a scoped user prompt in the same place", "prompt", "spawn"},
		{"a nested spawn in the same place", "nested-spawn", "spawn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, existed, err := s.GetThreadItem("t", tc.id)
			if err != nil {
				t.Fatal(err)
			}
			written, err := appendRow(tc.id, tc.parent)
			if !errors.Is(err, ErrItemNotAppended) || written {
				t.Fatalf("append %s under %s: written=%v err=%v, want ErrItemNotAppended", tc.id, tc.parent, written, err)
			}
			after, found, err := s.GetThreadItem("t", tc.id)
			if err != nil || found != existed || (found && (after.TurnIndex != before.TurnIndex || after.ItemIndex != before.ItemIndex || after.ParentID != before.ParentID || after.Kind != before.Kind || after.Summary != before.Summary || after.Status != before.Status)) {
				t.Fatalf("%s changed: %+v -> %+v (found=%v err=%v)", tc.id, before, after, found, err)
			}
		})
	}

}
