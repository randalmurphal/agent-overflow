package linearprovider

import (
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
