package store

import (
	"testing"
	"time"
)

// UpsertItemUnderParent writes a row only while its parent is in history,
// in the parent's turn whatever turn the caller named, and writes nothing
// once a cut removed the parent.
func TestUpsertItemUnderParentFollowsItsParent(t *testing.T) {
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
		persisted, written, err := s.UpsertItemUnderParent(row, &Payload{ID: "out", Kind: "command_output", Data: []byte("done\n"), CreatedAt: base})
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
		_, written, err := s.UpsertItemUnderParent(row, nil)
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
	if _, _, err := s.UpsertItemUnderParent(Item{ID: "orphan", ThreadID: "t", Kind: "tool_call"}, nil); err == nil {
		t.Fatalf("a row without a parent was accepted")
	}
}
