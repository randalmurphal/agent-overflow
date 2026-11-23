package trackerapp

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// statusSinceTracker is one tracker's issue for the status time tests:
// the issue on the board, the status it is in and another it can move to,
// and the tracker's history settle margin (Capabilities.HistorySettle).
type statusSinceTracker struct {
	open          func(t *testing.T) (*fakeScenario, Connection, Scope)
	issue         tracker.IssueID
	status, other tracker.StatusID
	settle        time.Duration
}

func statusSinceTrackers() map[string]statusSinceTracker {
	return map[string]statusSinceTracker{
		"jira": {
			open: func(t *testing.T) (*fakeScenario, Connection, Scope) {
				s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
				}, "example.atlassian.net")
				conn := s.addJira()
				s.writeToken(conn, "fake-jira-token-1")
				return s, conn, Scope{Connection: conn.ID, Board: "1"}
			},
			issue: "10101", status: "10001", other: "10002", settle: time.Minute,
		},
		"linear": {
			open: func(t *testing.T) (*fakeScenario, Connection, Scope) {
				s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					// The issue entered its status two days ago: Linear's
					// history holds no creation, so the time comes from an entry.
					ws := linearSeed(at)
					ws.Issues[0].History = []trackerfake.LinearHistory{{ID: lid(80), CreatedAt: at.Add(-48 * time.Hour), Actor: lid(1), FromState: lid(20), ToState: lid(21)}}
					return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}
				})
				conn := s.addLinear()
				s.writeToken(conn, "lin_api_fake00001")
				return s, conn, Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
			},
			issue: tracker.IssueID(lid(60)), status: tracker.StatusID(lid(21)), other: tracker.StatusID(lid(23)), settle: 2 * time.Minute,
		},
	}
}

// leaveAndReturn moves tc's issue to its other status and back, a minute
// apart, through the tracker, and returns when it came back.
func (tc statusSinceTracker) leaveAndReturn(t *testing.T, s *fakeScenario, conn Connection) time.Time {
	t.Helper()
	p := s.provider(conn.ID)
	for _, to := range []tracker.StatusID{tc.other, tc.status} {
		s.clock.advance(time.Minute)
		moves := must[[]tracker.Move](t)(p.Moves(t.Context(), tc.issue))
		i := slices.IndexFunc(moves, func(m tracker.Move) bool { return m.To == to })
		if i < 0 {
			t.Fatalf("no move of %s to %s among %+v", tc.issue, to, moves)
		}
		applied(t, "move", p.Move(t.Context(), tracker.MoveInput{Issue: tc.issue, Move: moves[i].ID}))
	}
	return s.clock.Now()
}

// The Monday and Tuesday case: an issue in its status since Monday leaves
// it and comes back on Tuesday, between two reads of its card. The board's
// card keeps Monday's time until a history read stores Tuesday's changes
// (the store's rule), while a ticket read once the tracker's history is
// final for the card's update shows Tuesday's return, the latest status
// change its document holds (statusSinceOf rule 1). Read inside the
// tracker's settle margin, the document's history is not trusted, the
// mirror's Monday time contradicts the return it holds, and the ticket
// shows the time unknown rather than Monday's.
func TestATicketShowsTheLatestStayFromItsHistory(t *testing.T) {
	t.Parallel()
	for name, tc := range statusSinceTrackers() {
		for _, settled := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "/settled", false: "/inside the settle margin"}[settled], func(t *testing.T) {
				t.Parallel()
				s, conn, scope := tc.open(t)
				s.listBoards(conn.ID, scope.Board)
				s.syncBoard(t, scope)
				monday := must[IssueView](t)(s.svc.Issue(conn.ID, tc.issue)).Card.StatusSince
				if monday == 0 {
					t.Fatal("the board's card shows no status time after its history read")
				}
				tuesday := tc.leaveAndReturn(t, s, conn)
				must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
				board := must[IssueView](t)(s.svc.Issue(conn.ID, tc.issue)).Card
				if board.Updated != millis(tuesday) || board.StatusSince != monday {
					t.Fatalf("the board's card after the return: updated %d since %d; want %d since Monday's %d", board.Updated, board.StatusSince, millis(tuesday), monday)
				}
				want := int64(0)
				if settled {
					s.clock.advance(tc.settle + time.Second)
					want = millis(tuesday)
				}
				v, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tc.issue, Interactive)
				if err != nil || v.Card.Status.ID != string(tc.status) || v.Card.StatusSince != want {
					t.Fatalf("the ticket shows %s since %d, %v; want since %d (Monday was %d)", v.Card.Status.ID, v.Card.StatusSince, err, want, monday)
				}
				if c := must[IssueView](t)(s.svc.Issue(conn.ID, tc.issue)).Card; c.StatusSince != monday {
					t.Fatalf("the board's card after the ticket read shows since %d; want Monday's %d until its history is read", c.StatusSince, monday)
				}
			})
		}
	}
}

// A ticket of an issue the mirror does not keep has no mirror card to
// take a time from: it shows the latest status change its document's
// history holds, once that history is final for the card's update.
func TestAnUnkeptTicketShowsItsHistorysStatusTime(t *testing.T) {
	t.Parallel()
	for name, tc := range statusSinceTrackers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn, _ := tc.open(t)
			back := tc.leaveAndReturn(t, s, conn)
			s.clock.advance(tc.settle + time.Second)
			v, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tc.issue, Interactive)
			if err != nil || v.Card.StatusSince != millis(back) {
				t.Fatalf("the unkept ticket shows since %d, %v; want %d", v.Card.StatusSince, err, millis(back))
			}
			if _, err := s.svc.Issue(conn.ID, tc.issue); err == nil {
				t.Fatal("the mirror keeps the issue")
			}
		})
	}
}
