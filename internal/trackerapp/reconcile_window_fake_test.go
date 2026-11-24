package trackerapp

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// agedOutIsRemoved runs the reconciles of TestACompletedIssueThatAgesOutLosesItsRow
// on s: done is a completed issue updated an hour inside the history
// window, whose row the first sync stores; once the window's start passes
// its update, a reconcile's listing leaves it out and the read by ID that
// confirms the listing judges it by the same rule, so its row goes; the
// next reconcile costs its listing alone (listed requests).
func (s *fakeScenario) agedOutIsRemoved(t *testing.T, scope Scope, done tracker.IssueID, listed int) {
	t.Helper()
	var shift atomic.Int64
	s.svc.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	if first := s.syncBoard(t, scope); !slices.Contains(first.Rows, done) {
		t.Fatalf("the first sync stored rows %v; want the completed issue %s", first.Rows, done)
	}
	shift.Store(int64(2 * time.Hour))
	if out := tickOK(t, s.svc, scope, BoardDue{Reconcile: true}); !slices.Equal(out.Changes().Removed, []tracker.IssueID{done}) {
		t.Fatalf("the reconcile after the window passed the completed issue removed %v; want %s", out.Changes().Removed, done)
	}
	var out Outcome
	routes := s.requestsOf(t, func() { out = tickOK(t, s.svc, scope, BoardDue{Reconcile: true}) })
	if len(out.Changes().Removed) != 0 || total(routes) != listed {
		t.Fatalf("the next reconcile removed %v in %d requests (%s); want nothing, in the listing's %d", out.Changes().Removed, total(routes), describe(routes), listed)
	}
}

// A completed issue that ages out of the history window leaves the
// board's listing, and the reads by ID that confirm a listing judge by
// the rule it listed by (tracker.Listing.Rule), so its row is removed
// and no later reconcile reads it again.
func TestACompletedIssueThatAgesOutLosesItsRow(t *testing.T) {
	t.Parallel()
	t.Run("jira", func(t *testing.T) {
		t.Parallel()
		s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
			site := jiraSeed(at)
			old := at.Add(-HistoryWindow + 2*time.Hour)
			site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10002",
				Summary: "Done long ago", Status: "10002", Created: old, Updated: old})
			return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
		}, "example.atlassian.net")
		conn := s.addJira()
		s.writeToken(conn, "fake-jira-token-1")
		scope := Scope{Connection: conn.ID, Board: "1"}
		s.listBoards(conn.ID, scope.Board)
		s.agedOutIsRemoved(t, scope, "10104", 1)
	})
	t.Run("linear", func(t *testing.T) {
		t.Parallel()
		s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
			ws := linearSeed(at)
			old := at.Add(-HistoryWindow + 2*time.Hour)
			ws.Issues = append(ws.Issues, trackerfake.LinearIssue{ID: lid(67), Team: lid(10), Number: 9, Title: "Done long ago", State: lid(23),
				Creator: lid(1), SortOrder: 900, CreatedAt: old, UpdatedAt: old, CompletedAt: old})
			return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}
		})
		conn := s.addLinear()
		s.writeToken(conn, "lin_api_fake00001")
		scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
		s.listBoards(conn.ID, scope.Board)
		s.agedOutIsRemoved(t, scope, tracker.IssueID(lid(67)), 2)
	})
}
