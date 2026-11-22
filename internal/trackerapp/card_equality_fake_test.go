package trackerapp

import (
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A detail document's card is the card a cards read and a board's rows
// read make of the same issue, column for column and in the blob a view
// decodes, on each provider: the mirror stores all three by one rule
// (store mirrorCardSupersededSQL), so a difference would make the mirror's
// card of an unchanged issue alternate as the reads land. The issues carry
// a parent, labels, a sprint or cycle, an estimate and a resolution.
func TestFakeDetailCardIsTheReadCard(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		seed   func(at time.Time) trackerfake.Fixture
		sites  []string
		open   func(s *fakeScenario) Connection
		board  tracker.BoardID
		issues []tracker.IssueID
	}{
		"jira": {
			seed: func(at time.Time) trackerfake.Fixture {
				site := jiraSeed(at)
				day := at.Add(-48 * time.Hour)
				site.Issues[0].Parent, site.Issues[0].Labels = "10104", []string{"frontend", "checkout"}
				site.Issues[1].Status, site.Issues[1].Resolution, site.Issues[1].Resolved = "10002", "Done", day.Add(time.Hour)
				site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10000",
					Summary: "The epic", Status: "10000", Created: day, Updated: day})
				return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
			},
			sites: []string{"example.atlassian.net"},
			open: func(s *fakeScenario) Connection {
				conn := s.addJira()
				s.writeToken(conn, "fake-jira-token-1")
				return conn
			},
			board:  "1",
			issues: ids("10101", "10102", "10103", "10104"),
		},
		"linear": {
			seed: func(at time.Time) trackerfake.Fixture {
				ws := linearSeed(at)
				ws.Labels = []trackerfake.LinearLabel{{ID: lid(40), Name: "frontend"}, {ID: lid(41), Name: "checkout"}}
				ws.Issues[0].Parent, ws.Issues[0].Labels = lid(61), []string{lid(40), lid(41)}
				done := at.Add(-time.Hour)
				ws.Issues[2].State, ws.Issues[2].CompletedAt, ws.Issues[2].UpdatedAt = lid(23), done, done
				return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}
			},
			open: func(s *fakeScenario) Connection {
				conn := s.addLinear()
				s.writeToken(conn, "lin_api_fake00001")
				return conn
			},
			board:  tracker.BoardID(lid(10)),
			issues: ids(lid(60), lid(61), lid(62)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newFakeScenario(t, tc.seed, tc.sites...)
			conn := tc.open(s)
			p := s.provider(conn.ID)
			details := must[tracker.Details](t)(p.Details(t.Context(), tc.issues))
			cards := must[tracker.Cards](t)(p.Cards(t.Context(), tc.issues))
			rows := must[tracker.Rows](t)(p.BoardRows(t.Context(), tc.board, tc.issues))
			if len(details.Details) != len(tc.issues) || len(cards.Cards) != len(tc.issues) {
				t.Fatalf("%d details and %d cards of %d issues", len(details.Details), len(cards.Cards), len(tc.issues))
			}
			stored := func(c tracker.Card) store.MirrorCard { return must[store.MirrorCard](t)(mirrorCard(c)) }
			read := map[tracker.IssueID]store.MirrorCard{}
			for _, c := range cards.Cards {
				read[c.ID] = stored(c)
			}
			onBoard := map[tracker.IssueID]store.MirrorCard{}
			for _, r := range rows.Rows {
				onBoard[r.Card.ID] = stored(r.Card)
			}
			var parent, labelled, sprinted, resolved bool
			for _, d := range details.Details {
				got := stored(d.Card)
				if want, ok := read[d.Card.ID]; !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("issue %s: the detail's card\n%+v\nthe cards read's\n%+v", d.Card.ID, got, want)
				}
				if want, ok := onBoard[d.Card.ID]; ok && !reflect.DeepEqual(got, want) {
					t.Errorf("issue %s: the detail's card\n%+v\nthe board read's\n%+v", d.Card.ID, got, want)
				}
				parent = parent || got.ParentID != ""
				labelled = labelled || len(d.Card.Labels) > 0
				sprinted = sprinted || len(got.Sprints) > 0
				resolved = resolved || got.ResolvedAt > 0
			}
			if len(onBoard) == 0 || !parent || !labelled || !sprinted || !resolved {
				t.Fatalf("the issues compared: %d on the board, a parent %v, labels %v, a sprint %v, a resolution %v",
					len(onBoard), parent, labelled, sprinted, resolved)
			}
		})
	}
}
