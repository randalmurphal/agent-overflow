package store

import (
	"fmt"
	"maps"
	"testing"
)

// The store calls of a pump's tick and of a connection's ticket round
// that find nothing changed, as trackerapp's TestIdlePumpTickCosts,
// TestRedeliveredRowsTellNothing and TestIdleRoundCosts pin them, cost the
// statements and row writes pinned here. Each tick runs twice; the second
// is the steady state. Each writes only read times: a board's read and
// cursor, a redelivered row's read, an open detail's read (Linear). A
// round's statements do not grow with its tickets; on Linear its row
// writes are each ticket's read time.
func TestIdleTickStatements(t *testing.T) {
	s, _, root := newMirror(t)
	seedFullMirror(t, s, root)
	// Ten open tickets, each with its stored detail.
	tickets := []string{"10001"}
	for i := range 9 {
		tickets = append(tickets, fmt.Sprint(20001+i))
	}
	mustWriteRowPage(t, s, root, webPage("10", mirrorRows(20001, 9, "z")...), nil)
	docs := func(n int) []MirrorDetailDoc {
		var out []MirrorDetailDoc
		for _, id := range tickets[:n] {
			out = append(out, MirrorDetailDoc{IssueID: id, Data: []byte(`{}`)})
		}
		return out
	}
	if _, err := s.StoreMirrorDetails(root, currentSweep(s, root), docs(10)[1:], 7, 7); err != nil {
		t.Fatal(err)
	}
	recorder := recordMirrorWrites(t, s)
	at := int64(100)
	sweep := currentSweep(s, root)
	page := currentPage(s, root, webPage("10", webRow("10001", 5, "a", "21")))
	attempt := currentAttempt(s, root, "10", MirrorSyncAttempt{})
	var each map[string]int
	call := func(name string, fn func() error) {
		var err error
		each[name] += countMirrorStatements(t, func() { err = fn() })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	syncState := func(board string) func() error {
		return func() error { _, _, err := s.GetMirrorSync(root, board); return err }
	}
	record := func() error {
		a := attempt
		a.ReadAt = at
		_, err := s.RecordMirrorSyncAttempt(root, "10", a)
		return err
	}
	type idleTick struct {
		name       string
		run        func()
		statements int
		writes     map[string]int
	}
	var rounds []idleTick
	for _, n := range []int{1, 3, 10} {
		cards := make([]MirrorCard, n)
		for i, id := range tickets[:n] {
			cards[i] = webCard(id, 5)
		}
		cards[0] = webCard("10001", 5, "21")
		rounds = append(rounds, idleTick{fmt.Sprintf("a Jira round of %d tickets", n), func() {
			call("GetMirrorSync", syncState(""))
			call("ListMirrorDetailStates", func() error { _, err := s.ListMirrorDetailStates(root, tickets[:n]); return err })
			call("UpsertMirrorCards", func() error {
				_, err := s.UpsertMirrorCards(root, sweep, cards, MirrorCatalog{}, at)
				return err
			})
		}, 8, map[string]int{}}, idleTick{fmt.Sprintf("a Linear round of %d tickets", n), func() {
			call("GetMirrorSync", syncState(""))
			call("StoreMirrorDetails", func() error {
				_, err := s.StoreMirrorDetails(root, sweep, docs(n), 0, at)
				return err
			})
		}, 2 + 2*n, map[string]int{"tracker_cache_issue_details": n}})
	}
	for _, tick := range append([]idleTick{
		{"a shown board", func() {
			call("GetMirrorSync", syncState("10"))
			call("RecordMirrorSyncAttempt", record)
		}, 3, map[string]int{"tracker_cache_sync": 1}},
		{"a shown board redelivered an identical row", func() {
			call("GetMirrorSync", syncState("10"))
			call("WriteMirrorRowPage", func() error {
				p := page
				p.ReadAt = at
				_, err := s.WriteMirrorRowPage(root, p, fmt.Appendf(nil, "c%d", at))
				return err
			})
			call("RecordMirrorSyncAttempt", record)
		}, 10, map[string]int{"tracker_cache_board_issues": 1, "tracker_cache_sync": 2}},
	}, rounds...) {
		var statements int
		for range 2 {
			at++
			each = map[string]int{}
			recorder.reset()
			statements = countMirrorStatements(t, tick.run)
		}
		writes := recorder.rowWrites()
		t.Logf("idle tick of %s: %d statements (%v), rows written %v", tick.name, statements, each, writes)
		if statements != tick.statements || !maps.Equal(writes, tick.writes) {
			t.Errorf("idle tick of %s: %d statements, rows written %v; pinned %d and %v", tick.name, statements, writes, tick.statements, tick.writes)
		}
	}
}
