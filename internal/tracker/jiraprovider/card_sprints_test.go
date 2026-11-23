package jiraprovider

import (
	"slices"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A card names the sprints a board keeps of those its issue is in: the
// active and future ones and the closed ones that closed inside the
// window (Structure). An issue carried through all 40 closed sprints of
// the cost fixture, 30 of them closed before the window, and in the
// active and both future ones names 13, the same on every card read, and each
// is a sprint of the board's structure.
func TestACardNamesTheSprintsABoardKeeps(t *testing.T) {
	carried := costIssueID(0)
	r := newCostRunOf(t, false, func(site *trackerfake.JiraSite) {
		var all []int64
		for _, sp := range site.Sprints {
			all = append(all, sp.ID)
		}
		for i := range site.Issues {
			if site.Issues[i].ID == carried {
				site.Issues[i].Sprints = all
			}
		}
	})
	ctx := t.Context()
	window := r.clock.Now().Add(-scenarioWindow)
	st, err := r.p.Structure(ctx, costBoard, window)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[tracker.SprintID]bool{}
	for _, sp := range st.Sprints {
		kept[sp.ID] = true
	}
	cards, err := r.p.Cards(ctx, []tracker.IssueID{tracker.IssueID(carried)})
	if err != nil || len(cards.Cards) != 1 {
		t.Fatalf("cards %+v, %v", cards, err)
	}
	rows, err := r.p.BoardRows(ctx, costBoard, []tracker.IssueID{tracker.IssueID(carried)}, tracker.Eligibility{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	detail, err := r.p.Detail(ctx, tracker.IssueID(carried))
	if err != nil {
		t.Fatal(err)
	}
	got := cards.Cards[0].Sprints
	if len(got) != 13 {
		t.Fatalf("the card names %d sprints: %v; want the 10 closed inside the window, the active and the 2 future ones", len(got), got)
	}
	for _, sp := range got {
		if !kept[sp] {
			t.Errorf("the card names sprint %s, which the board does not keep", sp)
		}
	}
	if !slices.Equal(rows.Rows[0].Card.Sprints, got) || !slices.Equal(detail.Card.Sprints, got) {
		t.Fatalf("the row names %v and the detail %v; the cards read %v", rows.Rows[0].Card.Sprints, detail.Card.Sprints, got)
	}
}
