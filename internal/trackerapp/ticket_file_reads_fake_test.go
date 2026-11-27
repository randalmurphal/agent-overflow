package trackerapp

import (
	"maps"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A thread on several tickets the mirror does not keep reads them in one
// read: on Linear, whose read takes several issues in one query, the
// tracker gets the requests a thread on one of them costs (Jira's read
// sends each issue's requests, together), and the details come back in
// the order asked.
func TestDetailsForThreadReadsTheUnkeptTicketsTogether(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		scenario func(t *testing.T) (*fakeScenario, Connection)
		issues   []tracker.IssueID
		keys     []string
	}{
		{
			name: "linear",
			scenario: func(t *testing.T) (*fakeScenario, Connection) {
				s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
					return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
				})
				conn := s.addLinear()
				s.writeToken(conn, "lin_api_fake00001")
				return s, conn
			},
			issues: []tracker.IssueID{tracker.IssueID(lid(62)), tracker.IssueID(lid(60)), tracker.IssueID(lid(61))},
			keys:   []string{"WEB-3", "WEB-1", "WEB-2"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			read := func(issues []tracker.IssueID) (map[string]int, []string) {
				s, conn := c.scenario(t)
				var keys []string
				costs := s.requestsOf(t, func() {
					views, err := s.svc.DetailsForThread(t.Context(), conn.ID, issues)
					if err != nil {
						t.Fatal(err)
					}
					for _, v := range views {
						keys = append(keys, v.Card.Key)
					}
				})
				return costs, keys
			}
			one, _ := read(c.issues[:1])
			all, keys := read(c.issues)
			if !maps.Equal(one, all) {
				t.Fatalf("one ticket's read sent %v; three tickets' sent %v", one, all)
			}
			if len(keys) != len(c.keys) || keys[0] != c.keys[0] || keys[1] != c.keys[1] || keys[2] != c.keys[2] {
				t.Fatalf("the details came back as %v; want %v", keys, c.keys)
			}
		})
	}
}
