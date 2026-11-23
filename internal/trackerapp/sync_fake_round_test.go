package trackerapp

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// fakeRoundIssues is how many issues the round scenarios seed: more open
// tickets than one Linear details read takes, as many as the cost table
// in tracker-backend.md names.
const fakeRoundIssues = 50

// The shown tickets of a connection cost one request a round through the
// real providers and clients, whatever their number: on Jira one cards
// read (an issue bulk fetch) while none changed, on Linear one details
// read of up to linear.MaxIssueDetails issues, a further one past it. A
// ticket that changed adds its own full read on Jira (the issue and its
// comments), and on Linear the read of the card its new document is
// owed, one for all the changed tickets.
func TestFakeRoundRequests(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"jira", "linear"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var s *fakeScenario
			var conn Connection
			var scope Scope
			var issues []tracker.IssueID
			if kind == "jira" {
				s = newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					site := jiraSeed(at)
					day := at.Add(-48 * time.Hour)
					site.Issues = nil
					for i := range fakeRoundIssues {
						site.Issues = append(site.Issues, trackerfake.JiraIssue{
							ID: fmt.Sprint(20000 + i), Key: fmt.Sprintf("WEB-%d", i+1), Project: "WEB", Type: "10002",
							Summary: fmt.Sprintf("Task %d", i+1), Status: "10000", Assignee: "acc-user1", Created: day, Updated: day.Add(time.Duration(i) * time.Second),
						})
						issues = append(issues, tracker.IssueID(fmt.Sprint(20000+i)))
					}
					return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
				}, "example.atlassian.net")
				conn = s.addJira()
				s.writeToken(conn, "fake-jira-token-1")
				scope = Scope{Connection: conn.ID, Board: "1"}
			} else {
				s = newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					w := linearSeed(at)
					w.Issues = nil
					for i := range fakeRoundIssues {
						w.Issues = append(w.Issues, trackerfake.LinearIssue{
							ID: lid(1000 + i), Team: lid(10), Number: float64(i + 1), Title: fmt.Sprintf("Task %d", i+1), State: lid(21), Assignee: lid(1),
							Creator: lid(1), SortOrder: float64(i), CreatedAt: at.Add(-72 * time.Hour), UpdatedAt: at.Add(-48*time.Hour + time.Duration(i)*time.Second),
						})
						issues = append(issues, tracker.IssueID(lid(1000+i)))
					}
					return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{w}}
				})
				conn = s.addLinear()
				s.writeToken(conn, "lin_api_fake00001")
				scope = Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
			}
			s.listBoards(conn.ID, scope.Board)
			s.syncBoard(t, scope)
			s.svc.cadence.Ticket = time.Hour
			// Each count of open tickets: the rounds' requests while nothing
			// changed.
			want := map[int]int{1: 1, 3: 1, 10: 1, fakeRoundIssues: 1}
			if kind == "linear" {
				want[fakeRoundIssues] = (fakeRoundIssues + linear.MaxIssueDetails - 1) / linear.MaxIssueDetails
			}
			open := 0
			for _, n := range []int{1, 3, 10, fakeRoundIssues} {
				for ; open < n; open++ {
					if _, err := s.svc.SubscribeTicket(conn.ID, issues[open]); err != nil {
						t.Fatal(err)
					}
				}
				settled(t, s.svc)
				rd := s.svc.roundOf(conn.ID)
				s.svc.roundTick(rd)
				routes := s.requestsOf(t, func() {
					if res := s.svc.roundTick(rd); res.failed {
						t.Fatalf("round %+v", res)
					}
				})
				t.Logf("%d tickets, nothing changed: %d requests: %s", n, total(routes), describe(routes))
				if total(routes) != want[n] {
					t.Errorf("%d tickets: %d requests, pinned at %d: %s", n, total(routes), want[n], describe(routes))
				}
			}
			// One of them changes: its own full read is added, nothing else.
			s.clock.advance(time.Minute)
			s.editTitles(t, conn.ID, issues[1:2])
			rd := s.svc.roundOf(conn.ID)
			routes := s.requestsOf(t, func() { s.svc.roundTick(rd) })
			t.Logf("one changed: %d requests: %s", total(routes), describe(routes))
			// Jira: the cards read, then the issue and its comments. Linear:
			// the details reads alone, which read the changed one in full and
			// store its card with its body.
			wantChanged := want[fakeRoundIssues] + 2
			if kind == "linear" {
				wantChanged = want[fakeRoundIssues]
			}
			if total(routes) != wantChanged {
				t.Errorf("one changed: %d requests, pinned at %d: %s", total(routes), wantChanged, describe(routes))
			}
			// Ten of them change: Jira reads each in full; Linear's details
			// reads read them already.
			s.clock.advance(time.Minute)
			s.editTitles(t, conn.ID, issues[2:12])
			routes = s.requestsOf(t, func() { s.svc.roundTick(rd) })
			t.Logf("ten changed: %d requests: %s", total(routes), describe(routes))
			wantTen := want[fakeRoundIssues] + 2*10
			if kind == "linear" {
				wantTen = want[fakeRoundIssues]
			}
			if total(routes) != wantTen {
				t.Errorf("ten changed: %d requests, pinned at %d: %s", total(routes), wantTen, describe(routes))
			}
		})
	}
}

// settled waits until every ticket pump of s has settled its first read.
func settled(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.subMu.Lock()
		ok := true
		for _, tp := range s.subs.tickets {
			ok = ok && tp.settled > 0
		}
		s.subMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the open tickets' first reads did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
