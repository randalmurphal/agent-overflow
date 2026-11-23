package trackerapp

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A detail document's card is the card a cards read and a board's rows
// read make of the same issue, column for column and in the blob a view
// decodes, on each provider: the view shows the mirror's card in place of
// a document's at the same update (shownDetailOf), so a difference would
// show a ticket other than its document. The two reads also leave out the
// same issues: a trashed Linear issue has no card and no detail. Each
// issue the board shows is compared with its row, and the issues carry a
// parent, labels their history shows changing, a sprint or cycle, an
// estimate and a resolution.
func TestFakeDetailCardIsTheReadCard(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		seed   func(at time.Time) trackerfake.Fixture
		sites  []string
		open   func(s *fakeScenario) Connection
		board  tracker.BoardID
		issues []tracker.IssueID
		gone   []tracker.IssueID
		// offBoard are the issues compared that the board does not show.
		offBoard []tracker.IssueID
	}{
		"jira": {
			seed: func(at time.Time) trackerfake.Fixture {
				site := jiraSeed(at)
				day := at.Add(-48 * time.Hour)
				site.Issues[0].Parent, site.Issues[0].Labels = "10104", []string{"frontend", "checkout"}
				from, to := "legacy", "frontend checkout"
				site.Issues[0].Changelog = []trackerfake.JiraChange{{ID: "20001", Author: "acc-user1", Created: day.Add(time.Hour),
					Items: []trackerfake.JiraChangeItem{{FieldID: "labels", Field: "labels", FieldType: "jira", FromString: &from, ToString: &to}}}}
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
			// The epic, a parent, is on no scrum board.
			offBoard: ids("10104"),
		},
		"linear": {
			seed: func(at time.Time) trackerfake.Fixture {
				ws := linearSeed(at)
				ws.Labels = []trackerfake.LinearLabel{{ID: lid(40), Name: "frontend"}, {ID: lid(41), Name: "checkout"}, {ID: lid(42), Name: "legacy"}}
				ws.Issues[0].Parent, ws.Issues[0].Labels = lid(61), []string{lid(40), lid(41)}
				ws.Issues[0].History = []trackerfake.LinearHistory{
					{ID: lid(70), CreatedAt: at.Add(-72 * time.Hour), Actor: lid(1), AddedLabels: []string{lid(40), lid(42)}},
					{ID: lid(71), CreatedAt: at.Add(-48 * time.Hour), Actor: lid(1), AddedLabels: []string{lid(41)}, RemovedLabels: []string{lid(42)}},
				}
				ws.Issues = append(ws.Issues, trackerfake.LinearIssue{ID: lid(63), Team: lid(10), Number: 4, Title: "Old idea", State: lid(20),
					Creator: lid(1), SortOrder: 400, CreatedAt: at.Add(-96 * time.Hour), UpdatedAt: at.Add(-24 * time.Hour), Trashed: true})
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
			gone:   ids(lid(63)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newFakeScenario(t, tc.seed, tc.sites...)
			conn := tc.open(s)
			p := s.provider(conn.ID)
			asked := append(slices.Clone(tc.issues), tc.gone...)
			details := must[tracker.Details](t)(p.Details(t.Context(), asked))
			cards := must[tracker.Cards](t)(p.Cards(t.Context(), asked))
			rows := must[tracker.Rows](t)(p.BoardRows(t.Context(), tc.board, tc.issues))
			if len(details.Details) != len(tc.issues) || len(cards.Cards) != len(tc.issues) ||
				!slices.Equal(details.Missing, tc.gone) || !slices.Equal(cards.Missing, tc.gone) {
				t.Fatalf("of %d issues, %d details (missing %v) and %d cards (missing %v); want %d each, missing %v",
					len(asked), len(details.Details), details.Missing, len(cards.Cards), cards.Missing, len(tc.issues), tc.gone)
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
			var parent, labelled, sprinted, resolved, relabelled bool
			for _, d := range details.Details {
				got := stored(d.Card)
				if want, ok := read[d.Card.ID]; !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("issue %s: the detail's card\n%+v\nthe cards read's\n%+v", d.Card.ID, got, want)
				}
				if want, ok := onBoard[d.Card.ID]; ok != !slices.Contains(tc.offBoard, d.Card.ID) || ok && !reflect.DeepEqual(got, want) {
					t.Errorf("issue %s: the detail's card\n%+v\nthe board read's\n%+v", d.Card.ID, got, want)
				}
				parent = parent || got.ParentID != ""
				labelled = labelled || len(d.Card.Labels) > 0
				sprinted = sprinted || len(got.Sprints) > 0
				resolved = resolved || got.ResolvedAt > 0
				relabelled = relabelled || slices.ContainsFunc(d.History, func(e tracker.Event) bool { return strings.EqualFold(e.Field, "labels") })
			}
			if !parent || !labelled || !sprinted || !resolved || !relabelled {
				t.Fatalf("the issues compared: a parent %v, labels %v, a sprint %v, a resolution %v, a label change %v",
					parent, labelled, sprinted, resolved, relabelled)
			}
		})
	}
}
