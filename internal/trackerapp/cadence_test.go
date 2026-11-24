package trackerapp

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A Jira connection's boards and tickets poll at the Jira interval and a
// Linear connection's at the Linear one, both live in one service; a
// Cadence override applies to both kinds.
func TestPollIntervalIsPerKind(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                         string
		cadence                      Cadence
		jiraBoard, jiraTicket, other time.Duration
		linearTicket                 time.Duration
	}{
		{"defaults", Cadence{}, JiraPollEvery, JiraPollEvery, LinearPollEvery, LinearPollEvery},
		{"override", Cadence{Delta: time.Hour, Ticket: 2 * time.Hour}, time.Hour, 2 * time.Hour, time.Hour, 2 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixtureWith(t, c.cadence)
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			m.p.set(func(p *scripted) {
				p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
					return tracker.Detail{Card: card(string(id), 1)}, nil
				}
			})
			// A ticket opened before its board's first read is kept for the
			// ticket, and its first document is owed its card.
			answerCardsFromDetails(m.p)
			linear := m.add(NewConnection{Kind: store.TrackerKindLinear, Site: "https://linear.app/example/"})
			m.listBoards(linear.ID, testBoard)
			for _, sc := range []struct {
				conn        string
				board, tick time.Duration
			}{{m.conn.ID, c.jiraBoard, c.jiraTicket}, {linear.ID, c.other, c.linearTicket}} {
				scope := Scope{Connection: sc.conn, Board: testBoard}
				board, err := m.svc.SubscribeBoard("boards-"+sc.conn, scope, []Section{{Kind: SectionBacklog}})
				if err != nil {
					t.Fatal(err)
				}
				ticket, err := m.svc.SubscribeTicket(sc.conn, "10001")
				if err != nil {
					t.Fatal(err)
				}
				got := fmt.Sprint(board.PollEvery, m.svc.boardPumpOf(scope).every, ticket.PollEvery, m.svc.ticketPumpOf(sc.conn, "10001").every)
				if want := fmt.Sprint(sc.board.Milliseconds(), sc.board, sc.tick.Milliseconds(), sc.tick); got != want {
					t.Errorf("connection %s polls %s; want %s", sc.conn, got, want)
				}
			}
		})
	}
}
