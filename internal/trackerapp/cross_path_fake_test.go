package trackerapp

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// crossPathTracker is one fake tracker for the cross-path tests: a board
// whose issue has a comment by a user no card names (User 3), and whether
// the tracker's detail names the comment's author (Jira's does; Linear's
// reads the author's ID alone, its users coming with a structure read).
type crossPathTracker struct {
	open           func(t *testing.T) (*fakeScenario, Connection, Scope)
	issue          tracker.IssueID
	commenter      string
	namesCommenter bool
}

func crossPathTrackers() map[string]crossPathTracker {
	return map[string]crossPathTracker{
		"jira": {
			open: func(t *testing.T) (*fakeScenario, Connection, Scope) {
				s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					site := jiraSeed(at)
					site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-user3", DisplayName: "User 3", Email: "user3@example.com", TimeZone: "UTC"})
					site.Issues[0].Comments = []trackerfake.JiraComment{{ID: "30001", Author: "acc-user3", Body: jiraDoc("a comment"),
						Created: at.Add(-47 * time.Hour), Updated: at.Add(-47 * time.Hour)}}
					return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
				}, "example.atlassian.net")
				conn := s.addJira()
				s.writeToken(conn, "fake-jira-token-1")
				return s, conn, Scope{Connection: conn.ID, Board: "1"}
			},
			issue: "10101", commenter: "acc-user3", namesCommenter: true,
		},
		"linear": {
			open: func(t *testing.T) (*fakeScenario, Connection, Scope) {
				s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					ws := linearSeed(at)
					ws.Users = append(ws.Users, trackerfake.LinearUser{ID: lid(3), Name: "User 3", Email: "user3@example.com"})
					ws.Comments = []trackerfake.LinearComment{{ID: lid(90), Issue: lid(60), User: lid(3), Body: "a comment",
						CreatedAt: at.Add(-47 * time.Hour), UpdatedAt: at.Add(-47 * time.Hour)}}
					return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}
				})
				conn := s.addLinear()
				s.writeToken(conn, "lin_api_fake00001")
				return s, conn, Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
			},
			issue: tracker.IssueID(lid(60)), commenter: lid(3),
		},
	}
}

// A ticket's read through each real provider sends the shared catalog
// only what its card names: the comment's author, whom no card names,
// stays with the body, and the ticket shows the author's name the detail
// read (Jira), or the author's ID when the detail names no author
// (Linear). No board is read, whose structure may name every user
// (Linear's does).
func TestFakeTicketReadNamesOnlyItsCardInTheSharedCatalog(t *testing.T) {
	t.Parallel()
	for name, tc := range crossPathTrackers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn, _ := tc.open(t)
			must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, tc.issue))
			settled(t, s.svc)
			v, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tc.issue, Interactive)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.namesCommenter {
				want = "User 3"
			}
			if !slices.ContainsFunc(v.Comments, func(c CommentView) bool { return c.Author.ID == tc.commenter && c.Author.Name == want }) {
				t.Fatalf("the ticket's comments %+v; want one by %s named %q", v.Comments, tc.commenter, want)
			}
			root := must[int64](t)(rootOf(s, conn))
			catalog := must[store.MirrorCatalog](t)(s.st.GetMirrorCatalog(root, []string{tc.commenter, string(v.Card.Assignee.ID)}, nil))
			// Jira's ticket read names its card's assignee there; Linear's
			// names no user, its users coming with a structure read.
			named := slices.ContainsFunc(catalog.Users, func(u store.MirrorUser) bool { return u.ID == v.Card.Assignee.ID })
			if slices.ContainsFunc(catalog.Users, func(u store.MirrorUser) bool { return u.ID == tc.commenter }) || named != tc.namesCommenter {
				t.Fatalf("the shared catalog names %+v; want the card's assignee %v and never the comment's author", catalog.Users, tc.namesCommenter)
			}
		})
	}
}

// A Jira project key rename moves no issue's update time. Whichever read
// lands last, the ticket's full read or the round's check (a cards read),
// the record takes the new key, and the board and the ticket show it.
func TestFakeJiraKeyRenameAtAnUnchangedUpdateTime(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"detail then card", "card then detail"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			tc := crossPathTrackers()["jira"]
			s, conn, scope := tc.open(t)
			s.listBoards(conn.ID, scope.Board)
			s.syncBoard(t, scope)
			must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, tc.issue))
			settled(t, s.svc)
			before := must[IssueView](t)(s.svc.Issue(conn.ID, tc.issue)).Card
			f := s.e.Snapshot()
			site := f.Jira[0]
			site.Token, site.Projects[0].Key, site.Boards[0].Project = "fake-jira-token-1", "APP", "APP"
			for i := range site.Issues {
				site.Issues[i].Project = "APP"
			}
			for i := range site.Issues {
				site.Issues[i].Key = strings.Replace(site.Issues[i].Key, "WEB-", "APP-", 1)
			}
			if err := s.e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
				t.Fatal(err)
			}
			detail := func() {
				if _, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tc.issue, Interactive); err != nil {
					t.Fatal(err)
				}
			}
			check := func() {
				if res := s.svc.roundTick(s.svc.roundOf(conn.ID)); res.failed {
					t.Fatalf("round %+v", res)
				}
			}
			if order == "detail then card" {
				detail()
				check()
			} else {
				check()
				detail()
			}
			quiet(t, s.svc)
			board := must[IssueView](t)(s.svc.Issue(conn.ID, tc.issue)).Card
			if board.Key != "APP-1" || board.Updated != before.Updated {
				t.Fatalf("the board shows %s updated %d; want APP-1 at the unchanged %d", board.Key, board.Updated, before.Updated)
			}
			v, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tc.issue, Interactive)
			if err != nil || v.Card.Key != "APP-1" {
				t.Fatalf("the ticket shows %s, %v; want APP-1", v.Card.Key, err)
			}
		})
	}
}
