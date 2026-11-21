package linearprovider

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// Details reads its issues linear.MaxIssueDetails to a request, with the
// names their histories need; each detail is what Detail reads. An ID
// Linear does not return, or that is no issue ID, is Missing.
func TestDetails(t *testing.T) {
	s := newScenario(t)
	ws := scenarioWorkspace()
	var ids []tracker.IssueID
	for i := range linear.MaxIssueDetails + 2 {
		id := sid(100 + i)
		ws.Issues = append(ws.Issues, trackerfake.LinearIssue{
			ID: id, Team: scTeam, Number: float64(10 + i), Title: fmt.Sprint("Task ", i+1), State: scTodo, Creator: scUser1,
			SortOrder: float64(1000 + i), CreatedAt: scStart.Add(-72 * time.Hour), UpdatedAt: scStart.Add(-time.Hour),
			// Each names a user, which comes with the entry.
			History: []trackerfake.LinearHistory{{ID: sid(300 + i), CreatedAt: scStart.Add(-2 * time.Hour), Actor: scUser1, ToAssignee: scUser2}},
		})
		ids = append(ids, tracker.IssueID(id))
	}
	if err := s.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
		t.Fatal(err)
	}
	asked := append(slices.Clone(ids), "WEB-1", tracker.IssueID(sid(999)))
	// The provider's first call reads the viewer; this one is the detail
	// the rest are compared with.
	s.detail(t, scIssue1)
	since := len(s.e.Requests(0).Requests)
	got := scenarioMust[tracker.Details](t)(s.p.Details(s.ctx, asked))
	ops := map[string]int{}
	for _, r := range s.e.Requests(0).Requests[since:] {
		ops[r.Operation]++
	}
	if want := map[string]int{"IssueDetails": 2}; !reflect.DeepEqual(ops, want) {
		t.Errorf("requests %v, want %v", ops, want)
	}
	if !slices.Equal(got.Missing, []tracker.IssueID{"WEB-1", tracker.IssueID(sid(999))}) || len(got.Details) != len(ids) {
		t.Fatalf("details %d, missing %v", len(got.Details), got.Missing)
	}
	for _, d := range got.Details {
		if one := s.detail(t, string(d.Card.ID)); !reflect.DeepEqual(d, one) {
			t.Errorf("%s: Details read\n%+v\nDetail reads\n%+v", d.Card.ID, d, one)
		}
	}
	for _, bad := range [][]tracker.IssueID{{ids[0], ids[0]}, {""}} {
		if _, err := s.p.Details(s.ctx, bad); !errors.Is(err, tracker.ErrInvalidInput) {
			t.Errorf("%v: %v", bad, err)
		}
	}
}
