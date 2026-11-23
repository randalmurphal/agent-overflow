package store

import (
	"fmt"
	"maps"
	"testing"
)

// The store calls of a ticket round of 50 tickets that finds 10 changed,
// as trackerapp's TestChangedRoundCosts pins them, cost the statements
// and row writes pinned here. Jira: the check's card write moves each
// unchanged card's read time and writes the changed cards, and one write
// stores their documents. Linear: one write moves the read time of the
// unchanged documents and finds the changed ones owed their cards, the
// cards are written, and one more write stores the owed documents. Both
// then read the changed tickets' cards and the catalog they name for
// their views. Every input is built before the round is counted.
func TestChangedRoundStatements(t *testing.T) {
	type cost struct {
		statements int
		writes     map[string]int
	}
	pinned := map[bool]cost{
		true:  {24, map[string]int{"tracker_cache_issue_details": 10, "tracker_cache_issues": 50}},
		false: {25, map[string]int{"tracker_cache_issue_details": 50, "tracker_cache_issues": 10}},
	}
	for _, jira := range []bool{true, false} {
		name := map[bool]string{true: "a Jira round", false: "a Linear round"}[jira]
		t.Run(name, func(t *testing.T) {
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			mustWriteRowPage(t, s, root, webPage("10", mirrorRows(20001, 50, "z")...), nil)
			var tickets, changed []string
			for i := range 50 {
				tickets = append(tickets, fmt.Sprint(20001+i))
			}
			changed = tickets[:10]
			sweep := currentSweep(s, root)
			before := heldDocs(t, s, root, func() []MirrorDetailDoc {
				out := make([]MirrorDetailDoc, len(tickets))
				for i, id := range tickets {
					out[i] = MirrorDetailDoc{IssueID: id, Data: []byte(`{"v":1}`)}
				}
				return out
			}())
			if _, err := s.StoreMirrorDetails(root, sweep, before, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
				t.Fatal(err)
			}
			// What the tracker answers: each changed card a minute newer with
			// a new title, and its new document.
			cards := make([]MirrorCard, len(tickets))
			docs := make([]MirrorDetailDoc, len(tickets))
			for i, id := range tickets {
				cards[i], docs[i] = webCard(id, 5), before[i]
				if i < len(changed) {
					cards[i] = webCard(id, 60_005)
					cards[i].Data = []byte(`{"title":"Renamed ` + id + `"}`)
					docs[i] = MirrorDetailDoc{IssueID: id, Data: []byte(`{"v":2}`), CardUpdatedAt: 60_005}
				}
			}
			read := mirrorReadAt(100)
			recorder := recordMirrorWrites(t, s)
			each := map[string]int{}
			call := func(name string, fn func() error) {
				var err error
				each[name] += countMirrorStatements(t, func() { err = fn() })
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			statements := countMirrorStatements(t, func() {
				call("GetMirrorSync", func() error { _, _, err := s.GetMirrorSync(root, "", testNow); return err })
				if jira {
					call("ListMirrorDetailStates", func() error { _, err := s.ListMirrorDetailStates(root, tickets); return err })
					call("UpsertMirrorCards", func() error { _, err := s.UpsertMirrorCards(root, sweep, cards, MirrorCatalog{}, read); return err })
					call("StoreMirrorDetails", func() error {
						w, err := s.StoreMirrorDetails(root, sweep, docs[:len(changed)], MirrorRead{}, read)
						if err == nil && countStored(w) != len(changed) {
							err = fmt.Errorf("stored %d of %d documents", countStored(w), len(changed))
						}
						return err
					})
				} else {
					call("StoreMirrorDetails", func() error {
						w, err := s.StoreMirrorDetails(root, sweep, docs, MirrorRead{}, read)
						if err == nil && countOwed(w) != len(changed) {
							err = fmt.Errorf("%d documents owed their card; want %d", countOwed(w), len(changed))
						}
						return err
					})
					call("UpsertMirrorCards", func() error {
						_, err := s.UpsertMirrorCards(root, sweep, cards[:len(changed)], MirrorCatalog{}, read)
						return err
					})
					call("StoreMirrorDetails", func() error {
						w, err := s.StoreMirrorDetails(root, sweep, docs[:len(changed)], MirrorRead{}, read)
						if err == nil && countStored(w) != len(changed) {
							err = fmt.Errorf("stored %d of %d owed documents", countStored(w), len(changed))
						}
						return err
					})
				}
				call("ListMirrorCards", func() error { _, err := s.ListMirrorCards(root, changed); return err })
				call("GetMirrorCatalog", func() error {
					_, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"})
					return err
				})
			})
			writes := recorder.rowWrites()
			t.Logf("%s, 10 of 50 changed: %d statements (%v), rows written %v", name, statements, each, writes)
			if want := pinned[jira]; statements != want.statements || !maps.Equal(writes, want.writes) {
				t.Errorf("%s, 10 of 50 changed: %d statements, rows written %v; pinned %d and %v", name, statements, writes, want.statements, want.writes)
			}
		})
	}
}

// countStored is how many of w's documents were stored.
func countStored(w MirrorDetailWrites) int {
	n := 0
	for _, d := range w.Docs {
		if d.Stored {
			n++
		}
	}
	return n
}

// countOwed is how many of w's documents are owed their card.
func countOwed(w MirrorDetailWrites) int {
	n := 0
	for _, d := range w.Docs {
		if d.CardOwed {
			n++
		}
	}
	return n
}
