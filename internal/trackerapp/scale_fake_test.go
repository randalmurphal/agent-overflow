package trackerapp

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// The requests a board's sync sends at the size of a large workspace,
// through the real providers and clients against the fake trackers: the
// first sync as the pump runs it, an idle tick, a reconcile, and the ticks
// after 100 issues change. The times include the fake's own work, which
// runs in the same process. Unset, AO_TRACKER_SCALE skips it.
//
//	AO_TRACKER_SCALE=5000,20000 make go-test GO_TEST_PKGS=./internal/trackerapp \
//	  GO_TEST_FLAGS="-count=1 -timeout 60m -v -run TestFakeTrackersAtScale"
func TestFakeTrackersAtScale(t *testing.T) {
	for _, n := range scaleSizes(t) {
		t.Run(fmt.Sprint("jira/", n), func(t *testing.T) {
			s := newFakeScenario(t, largeJiraFixture(n), "example.atlassian.net")
			conn := s.addJira()
			s.writeToken(conn, "fake-jira-token-1")
			edited := make([]tracker.IssueID, 100)
			for i := range edited {
				edited[i] = tracker.IssueID(fmt.Sprint(20000 + i*(n/100)))
			}
			s.atScale(t, Scope{Connection: conn.ID, Board: "1"}, n, func() { s.editTitles(t, conn.ID, edited) })
		})
		t.Run(fmt.Sprint("linear/", n), func(t *testing.T) {
			s := newFakeScenario(t, largeLinearFixture(n))
			conn := s.addLinear()
			s.writeToken(conn, "lin_api_fake00001")
			edited := make([]tracker.IssueID, 100)
			for i := range edited {
				edited[i] = tracker.IssueID(lid(1000 + i*(n/100)))
			}
			s.atScale(t, Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}, n, func() { s.editTitles(t, conn.ID, edited) })
		})
	}
}

// atScale measures scope's board of n issues; edit changes 100 of them.
func (s *fakeScenario) atScale(t *testing.T, scope Scope, n int, edit func()) {
	t.Helper()
	s.listBoards(scope.Connection, scope.Board)
	op := func(name string, run func()) {
		t.Helper()
		start := time.Now()
		routes := s.requestsOf(t, run)
		t.Logf("%-24s %9s %5d requests: %s", name, time.Since(start).Round(time.Millisecond), total(routes), describe(routes))
	}
	var ticks int
	op("first sync", func() { ticks = pumpTicks(t, s.svc, scope) })
	t.Logf("first sync: %d ticks", ticks)
	if v := must[BoardView](t)(s.svc.Board(scope, nil)); v.All.Rows != n {
		t.Fatalf("the board holds %d rows of %d", v.All.Rows, n)
	}
	op("idle tick", func() { pumpTicks(t, s.svc, scope) })
	op("reconcile", func() { tickOK(t, s.svc, scope, BoardDue{Reconcile: true}) })
	// The edit lands after the time the providers read history as final
	// through, so the history read after it is final.
	s.clock.advance(2 * time.Hour)
	edit()
	op("delta, 100 changed", func() {
		if out := tickOK(t, s.svc, scope, BoardDue{Delta: true}); len(out.Changes().Cards) != 100 {
			t.Fatalf("the delta stored %d cards", len(out.Changes().Cards))
		}
	})
	op("history, 100 due", func() { tickOK(t, s.svc, scope, BoardDue{History: true}) })
}
