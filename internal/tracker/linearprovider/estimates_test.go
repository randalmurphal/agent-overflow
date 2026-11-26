package linearprovider

import (
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
)

// A detail read carries the issue's one estimate, none included, as its
// team's board reads it: WEB-1's 3, the same as its row's, and WEB-2's
// none. A board with no estimate reads none from it.
func TestADetailCarriesItsEstimate(t *testing.T) {
	s := newScenario(t)
	board := tracker.Estimate{Unit: tracker.UnitPoints}
	first := s.detail(t, scIssue1)
	if v, carried := first.EstimateBy(board); !carried || v == nil || *v != 3 {
		t.Errorf("WEB-1's estimate %v, %v; want 3", v, carried)
	}
	if r := s.row(t, scIssue1); r.Estimate == nil || *r.Estimate != 3 {
		t.Errorf("WEB-1's row estimate %v; want the detail's 3", r.Estimate)
	}
	if v, carried := s.detail(t, scIssue2).EstimateBy(board); !carried || v != nil {
		t.Errorf("WEB-2's estimate %v, %v; want carried as none", v, carried)
	}
	if _, carried := first.EstimateBy(tracker.Estimate{Unit: tracker.UnitNone}); carried {
		t.Error("a board with no estimate reads one from the detail")
	}
}

// An estimate given the read of its issue's row sends with no read of the
// issue's team, judging the value on the scale the read found: a value off
// the scale is refused with no request. Given no read, it reads the team
// first, as before.
func TestAnEstimateTakesItsRowsRead(t *testing.T) {
	s := newScenario(t)
	board := tracker.BoardID(scTeam)
	rows := scenarioMust[tracker.Rows](t)(s.p.BoardRows(s.ctx, board, []tracker.IssueID{tracker.IssueID(scIssue1)}, tracker.Eligibility{}))
	read := rows.Read.RowRead(tracker.IssueID(scIssue1))
	for _, c := range []struct {
		name    string
		read    tracker.IssueRead
		value   float64
		applied bool
		ops     []string
	}{
		{"its row's read", read, 5, true, []string{"UpdateIssue"}},
		{"its row's read, off the scale", read, 4, false, nil},
		{"none", nil, 5, true, []string{"IssueTeam", "UpdateIssue"}},
	} {
		since := s.e.Requests(0).Requests
		r := s.p.SetEstimate(s.ctx, tracker.EstimateInput{Board: board, Issue: tracker.IssueID(scIssue1), Value: &c.value, Read: c.read})
		if r.AllApplied() != c.applied {
			t.Fatalf("%s: estimate %+v; want applied %v", c.name, r, c.applied)
		}
		var ops []string
		for _, req := range s.e.Requests(since[len(since)-1].Seq).Requests {
			ops = append(ops, req.Operation)
		}
		if !slices.Equal(ops, c.ops) {
			t.Errorf("%s: requests %q; want %q", c.name, ops, c.ops)
		}
	}
	if rows.Read.RowRead(tracker.IssueID(scIssue2)) != nil {
		t.Error("the rows read answers for an issue it did not return")
	}
}
