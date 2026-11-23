package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A Codex execution's walk returns only its members: the rows in the
// execution's bounds, less the rows directly under the launch another
// execution owns. The agent's earlier and later rows and its rows on
// another turn are left out by the result selection, so membership is read
// for no other row. A row under a nested agent is a member through its
// ancestry: one inside the bounds is returned, not direct, even when the
// nested agent's own row predates the bounds; one past the bounds is not.
func TestCodexExecutionWalkReturnsOnlyTheExecutionsMembers(t *testing.T) {
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
		{ID: "a-nested", ItemIndex: 3, ParentID: "launch"},
		{ID: "nested", ItemIndex: 4, ParentID: "launch"},
		{ID: "n-in", ItemIndex: 5, ParentID: "nested"},
		{ID: "b1", ItemIndex: 6, ParentID: "launch", Meta: tagged("B")},
		{ID: "b-old", ItemIndex: 7, ParentID: "launch", Meta: tagged("A")},
		{ID: "a-nested-late", ItemIndex: 8, ParentID: "a-nested"},
		{ID: "c1", ItemIndex: 9, ParentID: "launch", Meta: tagged("C")},
		{ID: "n-out", ItemIndex: 10, ParentID: "nested"},
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
	}
	walk := func(execution string) []visited {
		t.Helper()
		var got []visited
		if err := forEachCodexExecutionRow(s.reader(), "t", "launch", execution, 0, 3, 8, func(row subagentAggregateRow) {
			got = append(got, visited{row.id, row.direct})
		}); err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b visited) int { return strings.Compare(a.id, b.id) })
		return got
	}
	if got, want := walk("B"), []visited{{"a-nested-late", false}, {"b1", true}, {"n-in", false}, {"nested", true}}; !slices.Equal(got, want) {
		t.Fatalf("execution B's walk returned %v, want its members in (3, 8] of turn 0: %v", got, want)
	}
	if got, want := walk(""), []visited{{"a-nested-late", false}, {"b-old", true}, {"b1", true}, {"n-in", false}, {"nested", true}}; !slices.Equal(got, want) {
		t.Fatalf("an execution without an id returned %v, want every row in (3, 8] of turn 0: %v", got, want)
	}

	meta, tools, err := s.SnapshotSubagentExecutionMeta("t", "launch", "B", "{}", 0, 3, 8)
	if err != nil || tools != 2 {
		t.Fatalf("snapshot of B: tools=%d err=%v, want nested and b1", tools, err)
	}
	if want := `"subagentDescendantCount":4`; !strings.Contains(meta, want) {
		t.Fatalf("snapshot of B meta %s, want %s (nested, n-in, b1 and a-nested-late)", meta, want)
	}
}
