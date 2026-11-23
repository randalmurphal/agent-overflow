package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A Codex execution's walk returns only the rows in the execution's bounds,
// the agent's earlier and later rows and its rows on another turn left out
// by the result selection, so the execution tag is read for no other row.
// A nested agent's rows inside the bounds are returned, not direct; its rows
// past the bounds are not returned.
func TestCodexExecutionWalkReturnsOnlyTheExecutionsRows(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t", base)
	for _, item := range []Item{
		{ID: "user-0", TurnIndex: 0, Kind: "user_text", Role: "user"},
		{ID: "launch", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "collab_agent"},
		{ID: "user-1", TurnIndex: 1, Kind: "user_text", Role: "user"},
	} {
		item.ThreadID, item.CreatedAt = "t", base
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	tagged := func(execution string) string { return `{"` + CodexExecutionTurnMetaKey + `":"` + execution + `"}` }
	for _, item := range []Item{
		{ID: "a1", ItemIndex: 2, ParentID: "launch", Meta: tagged("A")},
		{ID: "a2", ItemIndex: 3, ParentID: "launch"},
		{ID: "nested", ItemIndex: 4, ParentID: "launch", ToolName: "collab_agent"},
		{ID: "n-in", ItemIndex: 5, ParentID: "nested"},
		{ID: "b1", ItemIndex: 6, ParentID: "launch", Meta: tagged("B")},
		{ID: "b-old", ItemIndex: 7, ParentID: "launch", Meta: tagged("A")},
		{ID: "c1", ItemIndex: 8, ParentID: "launch", Meta: tagged("C")},
		{ID: "n-out", ItemIndex: 9, ParentID: "nested"},
		{ID: "other-turn", TurnIndex: 1, ItemIndex: 5, ParentID: "launch"},
	} {
		item.ThreadID, item.Kind, item.Role, item.Status, item.CreatedAt = "t", "tool_call", "assistant", "completed", base
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	type visited struct {
		id     string
		direct bool
		tag    string
	}
	var got []visited
	if err := forEachCodexExecutionRow(s.reader(), "t", "launch", 0, 3, 7, func(row subagentAggregateRow) {
		got = append(got, visited{row.id, row.direct, row.executionTag})
	}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b visited) int {
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	want := []visited{{"b-old", true, "A"}, {"b1", true, "B"}, {"n-in", false, ""}, {"nested", true, ""}}
	if !slices.Equal(got, want) {
		t.Fatalf("execution walk returned %v, want only the rows in (3, 7] of turn 0: %v", got, want)
	}

	meta, tools, err := s.SnapshotSubagentExecutionMeta("t", "launch", "B", "{}", 0, 3, 7)
	if err != nil || tools != 2 {
		t.Fatalf("snapshot of B: tools=%d err=%v, want nested and b1", tools, err)
	}
	if want := `"subagentDescendantCount":3`; !strings.Contains(meta, want) {
		t.Fatalf("snapshot of B meta %s, want %s (nested, n-in and b1)", meta, want)
	}
}
