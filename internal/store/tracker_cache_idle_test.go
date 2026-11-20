package store

import (
	"fmt"
	"maps"
	"testing"
)

// The store calls of a pump's tick that finds nothing changed, as
// trackerapp's TestIdlePumpTickCosts and TestRedeliveredRowsTellNothing
// pin them, cost the statements and row writes pinned here. Each tick runs
// twice; the second is the steady state. Each writes only read times: a
// board's read and cursor, a redelivered row's read, an open detail's
// opening (Jira) or read (Linear).
func TestIdleTickStatements(t *testing.T) {
	s, _, root := newMirror(t)
	seedFullMirror(t, s, root)
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
	view := func() {
		call("GetMirrorCatalog", func() error {
			_, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"})
			return err
		})
		call("ListMirrorCards", func() error { _, err := s.ListMirrorCards(root, []string{"10000"}); return err })
	}
	for _, tick := range []struct {
		name       string
		run        func()
		statements int
		writes     map[string]int
	}{
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
		{"an open Jira ticket", func() {
			call("GetMirrorSync", syncState(""))
			call("GetMirrorDetail", func() error { _, _, err := s.GetMirrorDetail(root, "10001"); return err })
			call("UpsertMirrorCards", func() error {
				_, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 5, "21")}, MirrorCatalog{}, at)
				return err
			})
			call("TouchMirrorDetail", func() error { _, err := s.TouchMirrorDetail(root, "10001", at); return err })
			view()
		}, 13, map[string]int{"tracker_cache_issue_details": 1}},
		{"an open Linear ticket", func() {
			call("GetMirrorSync", syncState(""))
			call("StoreMirrorDetail", func() error {
				_, err := s.StoreMirrorDetail(root, sweep, "10001", at, at, []byte(`{}`))
				return err
			})
			view()
		}, 6, map[string]int{"tracker_cache_issue_details": 1}},
	} {
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
