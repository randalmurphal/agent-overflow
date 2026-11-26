package jiraprovider

import (
	"testing"

	"agent-overflow/internal/tracker"
)

// A detail's read, which a write session keeps through an approval wait,
// holds what the first move or edit takes from it (the issue, its edit
// screen and its moves) and not the issue's changelog.
func TestADetailsReadKeepsWhatAWriteTakes(t *testing.T) {
	s := newScenario(t, false)
	d := s.detail(t, "10101")
	if len(d.History) == 0 {
		t.Fatal("the scenario's issue has no history to leave out")
	}
	cur := currentOf(d.Read, "10101")
	switch {
	case cur == nil || cur.Issue == nil || len(cur.Edit.Fields) == 0 || len(cur.Transitions) == 0:
		t.Fatalf("kept read %+v; want the issue, its edit screen and its moves", cur)
	case len(cur.Changelog.Entries) != 0 || !cur.Date.IsZero():
		t.Errorf("kept read holds %d changelog entries and date %v; want none", len(cur.Changelog.Entries), cur.Date)
	}
	if currentOf(d.Read, tracker.IssueID("10102")) != nil {
		t.Error("the read answers for another issue")
	}
}
