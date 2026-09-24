package store

import "testing"

func TestSubagentAggregatePreviewIsTheNewestSummary(t *testing.T) {
	later := subagentAggregateRow{id: "later", kind: "thinking", summary: "thinking prose", turnIndex: 8, itemIndex: 5}
	tool := subagentAggregateRow{id: "tool", kind: "tool_call", summary: "useful", turnIndex: 1}
	if !betterSubagentPreview(tool, later) {
		t.Fatal("a tool summary must beat a later row of a kind that never previews")
	}
	for _, blank := range []string{"", "  ", "\t", " \n\r\v\f "} {
		row := tool
		row.summary = blank
		row.turnIndex = 9
		if betterSubagentPreview(row, tool) {
			t.Fatalf("summary %q is blank and must not beat an older useful one", blank)
		}
		state := &subagentAggregateAccumulator{}
		state.add(row)
		if state.hasPick || state.aggregate.latestChildSummary != "" {
			t.Fatalf("summary %q must not become the preview: %+v", blank, state.aggregate)
		}
	}
	newer := tool
	newer.id, newer.turnIndex = "newer", 2
	if !betterSubagentPreview(newer, tool) {
		t.Fatal("the newer coordinate must win")
	}
	sameTurn := tool
	sameTurn.id, sameTurn.itemIndex = "same-turn", 1
	if !betterSubagentPreview(sameTurn, tool) {
		t.Fatal("a later item in the same turn must win")
	}
	samePosition := tool
	samePosition.id = "a"
	if !betterSubagentPreview(samePosition, tool) || betterSubagentPreview(tool, samePosition) {
		t.Fatal("the smaller id must break a coordinate tie")
	}

	// Rows arrive in walk order, not position order, and a newer blank
	// row or a newer row of another kind leaves the newest summary in
	// place. Every visible row counts.
	state := &subagentAggregateAccumulator{}
	blankNewest := tool
	blankNewest.id, blankNewest.summary, blankNewest.turnIndex = "blank-newest", "  ", 10
	for _, row := range []subagentAggregateRow{newer, later, tool, blankNewest, sameTurn} {
		state.add(row)
	}
	if state.aggregate.descendantCount != 5 || state.aggregate.latestChildSummary != "useful" || state.preview.id != "newer" {
		t.Fatalf("aggregate = %+v, pick = %q", state.aggregate, state.preview.id)
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
