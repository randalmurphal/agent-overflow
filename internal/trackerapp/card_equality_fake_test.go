package trackerapp

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A detail's card is the card a cards read and a board's rows read make
// of the same issue, column for column and in the blob a view decodes, on
// each provider: every read stores its card in the one issue record
// through the same card path, so a difference would make the record
// change with the path that read it last. The two reads also take and
// leave out the same issues: a trashed Linear issue has no card and no
// detail, and an archived one has both and no board row. Each issue the
// board shows is compared with its row, and the issues carry a parent,
// labels their history shows changing, a sprint or cycle, an estimate and
// a resolution; on Jira one names a sprint that closed before the history
// window (the cutoff) and one's changelog is longer than a page, which
// the detail reads past the embedded newest, and on Linear one carries
// more labels than a read's nested page of 250 holds (the cap), which the
// reads name past the page.
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
				// A sprint that closed before the history window, which no
				// board keeps: the issue carried through it names it, and its
				// cards do not (Service.window).
				old := at.Add(-HistoryWindow - 24*time.Hour)
				site.Sprints = append(site.Sprints, trackerfake.JiraSprint{ID: 90, Board: 1, Name: "Sprint 0", State: "closed",
					Start: old.Add(-14 * 24 * time.Hour), End: old, Complete: old})
				site.Issues[0].Sprints = []int64{90, 1}
				site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: "10104", Key: "WEB-4", Project: "WEB", Type: "10000",
					Summary: "The epic", Status: "10000", Created: day, Updated: day})
				// A changelog longer than a page, read past the embedded newest.
				for i := range 120 {
					from, to := fmt.Sprint("Title ", i), fmt.Sprint("Title ", i+1)
					site.Issues[1].Changelog = append(site.Issues[1].Changelog, trackerfake.JiraChange{ID: fmt.Sprint(30000 + i), Author: "acc-user1",
						Created: day.Add(time.Duration(i) * time.Minute), Items: []trackerfake.JiraChangeItem{{FieldID: "summary", Field: "summary", FieldType: "jira", FromString: &from, ToString: &to}}})
				}
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
				many := trackerfake.LinearIssue{ID: lid(64), Team: lid(10), Number: 5, Title: "Many labels", State: lid(21),
					Creator: lid(1), SortOrder: 500, CreatedAt: at.Add(-96 * time.Hour), UpdatedAt: at.Add(-24 * time.Hour)}
				for i := range 251 {
					ws.Labels = append(ws.Labels, trackerfake.LinearLabel{ID: lid(1000 + i), Name: fmt.Sprintf("label-%03d", i)})
					many.Labels = append(many.Labels, lid(1000+i))
				}
				ws.Issues = append(ws.Issues, many)
				ws.Issues = append(ws.Issues, trackerfake.LinearIssue{ID: lid(65), Team: lid(10), Number: 6, Title: "Archived idea", State: lid(20),
					Creator: lid(1), SortOrder: 600, CreatedAt: at.Add(-96 * time.Hour), UpdatedAt: at.Add(-24 * time.Hour), ArchivedAt: at.Add(-24 * time.Hour)})
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
			issues: ids(lid(60), lid(61), lid(62), lid(64), lid(65)),
			gone:   ids(lid(63)),
			// An archived issue is on no board.
			offBoard: ids(lid(65)),
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
			rows := must[tracker.Rows](t)(p.BoardRows(t.Context(), tc.board, tc.issues, tracker.Eligibility{}))
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
			var parent, labelled, sprinted, resolved, relabelled, capped, paged bool
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
				capped = capped || len(d.Card.Labels) > 250 && !slices.ContainsFunc(d.Card.Labels, func(c tracker.Choice) bool { return c.Name == "" })
				sprinted = sprinted || len(got.Sprints) > 0
				if slices.Contains(got.Sprints, "90") {
					t.Errorf("issue %s names sprint 90, which closed before the history window", d.Card.ID)
				}
				resolved = resolved || got.ResolvedAt > 0
				relabelled = relabelled || slices.ContainsFunc(d.History, func(e tracker.Event) bool { return strings.EqualFold(e.Field, "labels") })
				paged = paged || len(d.History) > 100
			}
			if !parent || !labelled || !sprinted || !resolved || !relabelled || name == "linear" && !capped || name == "jira" && !paged {
				t.Fatalf("the issues compared: a parent %v, labels %v, a sprint %v, a resolution %v, a label change %v, past the label cap %v, a paged changelog %v",
					parent, labelled, sprinted, resolved, relabelled, capped, paged)
			}
		})
	}
}
