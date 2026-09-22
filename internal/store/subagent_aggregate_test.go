package store

import "testing"

func TestSubagentAggregatePreviewRank(t *testing.T) {
	base := subagentAggregateRow{id: "later", kind: "thinking", status: "running", summary: "thinking prose", turnIndex: 8, itemIndex: 5}
	qualified := subagentAggregateRow{id: "earlier", kind: "tool_call", status: "completed", summary: "useful", turnIndex: 1}
	if !betterSubagentPreview(qualified, base) {
		t.Fatal("a useful tool summary must beat later active thinking")
	}
	blank := qualified
	blank.summary = "  "
	if betterSubagentPreview(blank, base) {
		t.Fatal("whitespace is not a useful summary")
	}
	tab := qualified
	tab.summary = "\t"
	if !betterSubagentPreview(tab, base) {
		t.Fatal("the rank must match SQLite TRIM's ASCII-space rule")
	}
	onlyTab := &subagentAggregateState{}
	onlyTab.add(tab)
	if onlyTab.aggregate.latestChildSummary != "" {
		t.Fatal("the selected preview must still normalize blank output")
	}
	active := qualified
	active.status = "streaming"
	active.turnIndex = 0
	if !betterSubagentPreview(active, qualified) {
		t.Fatal("active summary must beat a newer terminal summary")
	}
	newer := qualified
	newer.turnIndex = 2
	if !betterSubagentPreview(newer, qualified) {
		t.Fatal("newer coordinate must win within the same rank")
	}
	samePosition := qualified
	samePosition.id = "a"
	if !betterSubagentPreview(samePosition, qualified) {
		t.Fatal("id must break coordinate ties deterministically")
	}
	state := &subagentAggregateState{}
	for _, row := range []subagentAggregateRow{base, qualified, blank, active} {
		state.add(row)
	}
	if state.aggregate.descendantCount != 4 || state.aggregate.latestChildSummary != "useful" {
		t.Fatalf("aggregate = %+v", state.aggregate)
	}
}

func TestSubagentRoundBoundsAreHalfOpen(t *testing.T) {
	bound := subagentRoundBounds{
		lo: &TimelineCursor{TurnIndex: 2, ItemIndex: 3},
		hi: &TimelineCursor{TurnIndex: 2, ItemIndex: 6},
	}
	for _, tc := range []struct {
		position int
		want     bool
	}{{2, false}, {3, true}, {5, true}, {6, false}} {
		if got := subagentBoundContains(bound, subagentAggregateRow{turnIndex: 2, itemIndex: tc.position}); got != tc.want {
			t.Fatalf("position %d: got %t, want %t", tc.position, got, tc.want)
		}
	}
}
