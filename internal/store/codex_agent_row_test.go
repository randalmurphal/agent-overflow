package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// appendCodexAgentRowForTest appends row under its parent's card.
func appendCodexAgentRowForTest(t *testing.T, s *Store, row CodexAgentRow) (Item, bool, error) {
	t.Helper()
	var persisted Item
	var written bool
	var appendErr error
	if err := s.WithSubagentCard(row.ThreadID, row.ParentID, func(card *SubagentCard) error {
		row.Card = card
		persisted, written, appendErr = s.AppendCodexAgentRow(row)
		return nil
	}); err != nil {
		t.Fatalf("card for %s: %v", row.ParentID, err)
	}
	return persisted, written, appendErr
}

// AppendCodexAgentRow writes a row only while its parent is in history, in
// the parent's turn, with the kind, tool name and output payload id the
// store chooses, and writes nothing once a cut removed the parent.
func TestAppendCodexAgentRowFollowsItsParent(t *testing.T) {
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
	command := CodexAgentRow{ThreadID: "t", ParentID: "spawn", ID: "cmd", Status: "completed", Summary: "Bash: ls", CreatedAt: base, UpdatedAt: base,
		Output: &CodexAgentOutput{Data: []byte("done\n"), CreatedAt: base}}
	persisted, written, err := appendCodexAgentRowForTest(t, s, command)
	if err != nil || !written || persisted.TurnIndex != 0 || persisted.Kind != "tool_call" || persisted.ToolName != "command_execution" || persisted.PayloadID != CodexAgentOutputPayloadID("cmd") {
		t.Fatalf("command under a present parent: %+v written=%v err=%v, want a command_execution tool_call on turn 0 with its output", persisted, written, err)
	}
	if data, err := s.GetPayloadData("t", CodexAgentOutputPayloadID("cmd")); err != nil || string(data) != "done\n" {
		t.Fatalf("command output = %q, %v", data, err)
	}
	interaction, written, err := appendCodexAgentRowForTest(t, s, CodexAgentRow{ThreadID: "t", ParentID: "spawn", ID: "interacted", Interaction: true, Status: "completed", Summary: "Interacted", CreatedAt: base})
	if err != nil || !written || interaction.TurnIndex != 0 || interaction.Kind != "terminal_interaction" || interaction.ToolName != "" || interaction.PayloadID != "" {
		t.Fatalf("interaction: %+v written=%v err=%v, want a terminal_interaction on turn 0", interaction, written, err)
	}

	if _, _, err := s.DeleteConversationFromTurn("t", 0); err != nil {
		t.Fatal(err)
	}
	command.ID = "cmd-late"
	if _, written, err := appendCodexAgentRowForTest(t, s, command); err != nil || written {
		t.Fatalf("under a cut parent: written=%v err=%v, want nothing written", written, err)
	}
	if _, found, err := s.GetThreadItem("t", "cmd-late"); err != nil || found {
		t.Fatalf("row under a cut parent stored: found=%v err=%v", found, err)
	}
	if _, _, err := s.AppendCodexAgentRow(CodexAgentRow{ID: "orphan", ThreadID: "t"}); err == nil {
		t.Fatalf("a row without a parent was accepted")
	}
}

// AppendCodexAgentRow never changes stored data: it refuses, writing
// nothing, a parent that is not a tool_call, a parent whose chain holds the
// row, any id already stored wherever it sits and whatever it is, an output
// payload id already stored (another row's output), and a meta naming a
// transcript root.
func TestAppendCodexAgentRowOnlyAppends(t *testing.T) {
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
	// A completed row whose output payload carries the id the store would
	// give the output of a new row "fresh".
	if err := insertWithPayloadCarded(s, Item{ID: "done", ThreadID: "t", TurnIndex: 0, ItemIndex: 9, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "spawn", PayloadID: CodexAgentOutputPayloadID("fresh"), CreatedAt: base},
		Payload{ID: CodexAgentOutputPayloadID("fresh"), Kind: "command_output", Data: []byte("original"), CreatedAt: base}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, id, parent, meta string
		output                 bool
	}{
		{name: "a parent that is not a tool_call", id: "new-under-text", parent: "text"},
		{name: "a parent whose chain holds the row", id: "cycled", parent: "loop"},
		{name: "a stored row in another turn, its index taken in the parent's", id: "wrong-turn", parent: "spawn"},
		{name: "a stored row in another turn, its index free in the parent's", id: "wrong-turn-free", parent: "spawn"},
		{name: "a stored row under another parent", id: "under-other", parent: "spawn"},
		{name: "a stored row in the same place", id: "kept", parent: "spawn"},
		{name: "a scoped user prompt in the same place", id: "prompt", parent: "spawn"},
		{name: "a nested spawn in the same place", id: "nested-spawn", parent: "spawn"},
		{name: "an output payload id another row holds", id: "fresh", parent: "spawn", output: true},
		{name: "a meta naming a transcript root", id: "rooted", parent: "spawn", meta: `{"` + metaKeyTranscriptRootID + `":"spawn"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, existed, err := s.GetThreadItem("t", tc.id)
			if err != nil {
				t.Fatal(err)
			}
			row := CodexAgentRow{ThreadID: "t", ParentID: tc.parent, ID: tc.id, Status: "completed", Summary: "rewritten", Meta: tc.meta, CreatedAt: base, UpdatedAt: base + 1}
			if tc.output {
				row.Output = &CodexAgentOutput{Data: []byte("replaced"), CreatedAt: base + 1}
			}
			_, written, err := appendCodexAgentRowForTest(t, s, row)
			if !errors.Is(err, ErrItemNotAppended) || written {
				t.Fatalf("append %s under %s: written=%v err=%v, want ErrItemNotAppended", tc.id, tc.parent, written, err)
			}
			after, found, err := s.GetThreadItem("t", tc.id)
			if err != nil || found != existed || (found && (after.TurnIndex != before.TurnIndex || after.ItemIndex != before.ItemIndex || after.ParentID != before.ParentID || after.Kind != before.Kind || after.Summary != before.Summary || after.Status != before.Status)) {
				t.Fatalf("%s changed: %+v -> %+v (found=%v err=%v)", tc.id, before, after, found, err)
			}
		})
	}
	if data, err := s.GetPayloadData("t", CodexAgentOutputPayloadID("fresh")); err != nil || string(data) != "original" {
		t.Fatalf("done's output = %q, %v; want original", data, err)
	}
}

// In a pointer fork, an agent row whose parent is inherited from the source
// lands in the parent's turn. When that turn is the fork's last, the row is
// the fork's own, after the inherited rows. When an earlier turn holds the
// parent, the row would precede the fork's cut, which the fork's position
// guard refuses: the write returns its error and stores nothing.
func TestAppendCodexAgentRowUnderAnInheritedParent(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "source", base)
	for _, item := range []Item{
		{ID: "user-0", TurnIndex: 0, Kind: "user_text", Role: "user"},
		{ID: "spawn-0", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "collab_agent"},
		{ID: "user-1", TurnIndex: 1, Kind: "user_text", Role: "user"},
		{ID: "spawn-1", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "collab_agent"},
	} {
		item.ThreadID, item.CreatedAt = "source", base
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreatePointerFork(makeThread("fork", "codex"), "source", ForkCut{}, testInterruptedSummary, base); err != nil {
		t.Fatal(err)
	}
	persisted, written, err := appendCodexAgentRowForTest(t, s, CodexAgentRow{ThreadID: "fork", ParentID: "spawn-1", ID: "cmd-last", Status: "completed", Summary: "Bash: ls", CreatedAt: base})
	if err != nil || !written || persisted.TurnIndex != 1 || persisted.ItemIndex <= 1 {
		t.Fatalf("append under an inherited parent in the last turn: %+v written=%v err=%v, want the fork's own row after spawn-1", persisted, written, err)
	}
	if _, found, err := s.GetThreadItem("source", "cmd-last"); err != nil || found {
		t.Fatalf("the source shows the fork's row: found=%v err=%v", found, err)
	}
	_, written, err = appendCodexAgentRowForTest(t, s, CodexAgentRow{ThreadID: "fork", ParentID: "spawn-0", ID: "cmd-early", Status: "completed", Summary: "Bash: ls", CreatedAt: base})
	if err == nil || written || !strings.Contains(err.Error(), "item position precedes the fork cut") {
		t.Fatalf("append under an inherited parent in an earlier turn: written=%v err=%v, want the fork's position guard", written, err)
	}
	if _, found, err := s.GetThreadItem("fork", "cmd-early"); err != nil || found {
		t.Fatalf("row before the fork's cut stored: found=%v err=%v", found, err)
	}
}
