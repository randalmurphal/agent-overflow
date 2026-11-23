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
// cursor, a redelivered row's and its card's read, each ticket's card read
// (a Jira round) and with it its body's read (Linear), writing no body
// bytes: a body read again is compared by its digest. Every card write checks the
// references its cards name (mirrorCardMarkSQL), one statement that finds
// nothing to mark when nothing changed. A round's statements do not
// grow with its tickets; its row writes are each ticket's read time. A board tick while an issue's
// history waits until a read can be final for it is an idle tick: the sync
// state reports no history due, so the tick runs no history unit.
func TestIdleTickStatements(t *testing.T) {
	s, _, root := newMirror(t)
	seedFullMirror(t, s, root)
	// Ten open tickets, each with its stored detail.
	tickets := []string{"10001"}
	for i := range 9 {
		tickets = append(tickets, fmt.Sprint(20001+i))
	}
	mustWriteRowPage(t, s, root, webPage("10", mirrorRows(20001, 9, "z")...), nil)
	// The tickets are of their cards' updates, read before any round is
	// counted; each brings the evidence of its status time a ticket's read
	// does.
	held := heldTickets(t, s, root, tickets, []byte(`{}`))
	for i := range held {
		c := held[i].Card
		held[i].Status = &MirrorStatusEvidence{Status: c.StatusID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Complete: true, Final: true}
	}
	if _, err := s.StoreMirrorTickets(root, currentSweep(s, root), held[1:], MirrorCatalog{}, mirrorReadAt(7), mirrorReadAt(7)); err != nil {
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
		return func() error { _, _, err := s.GetMirrorSync(root, board, testNow); return err }
	}
	record := func() error {
		a := attempt
		a.ReadAt = mirrorReadAt(at)
		_, err := s.RecordMirrorSyncAttempt(root, "10", a)
		return err
	}
	type idleTick struct {
		name       string
		run        func()
		statements int
		writes     map[string]int
		before     func()
	}
	var rounds []idleTick
	for _, n := range []int{1, 3, 10} {
		cards := make([]MirrorCard, n)
		for i, id := range tickets[:n] {
			cards[i] = webCard(id, 5)
		}
		cards[0] = webCard("10001", 5, "21")
		checks := make([]MirrorTicket, n)
		for i, c := range cards {
			checks[i] = MirrorTicket{Card: c}
		}
		rounds = append(rounds, idleTick{fmt.Sprintf("a Jira round of %d tickets", n), func() {
			call("GetMirrorSync", syncState(""))
			call("StoreMirrorTickets", func() error {
				_, err := s.StoreMirrorTickets(root, sweep, checks, MirrorCatalog{}, MirrorRead{}, mirrorReadAt(at))
				return err
			})
		}, 6, map[string]int{"tracker_cache_issues": n}, nil}, idleTick{fmt.Sprintf("a Linear round of %d tickets", n), func() {
			call("GetMirrorSync", syncState(""))
			call("StoreMirrorTickets", func() error {
				_, err := s.StoreMirrorTickets(root, sweep, held[:n], MirrorCatalog{}, MirrorRead{}, mirrorReadAt(at))
				return err
			})
		}, 6, map[string]int{"tracker_cache_issues": n}, nil})
	}
	// The tick whose history waits runs last: it moves issue 10001's update
	// time.
	rounds = append(rounds, idleTick{"a shown board with an issue's history waiting", func() {
		call("GetMirrorSync", syncState("10"))
		call("RecordMirrorSyncAttempt", record)
	}, 3, map[string]int{"tracker_cache_sync": 1}, func() {
		// The other open tickets' histories are read and final.
		for _, id := range tickets[1:] {
			if _, err := replaceHistory(s, root, MirrorHistory{IssueID: id, AnsweredAt: testNow}); err != nil {
				t.Fatal(err)
			}
		}
		// Updated at 6, its history read for that update answered at
		// testNow and not final: due again a minute later.
		mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 6, "a", "21")), nil)
		mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10001", AnsweredAt: testNow, Wait: 60_000}, MirrorWrite{Changed: true})
		st, _, err := s.GetMirrorSync(root, "10", testNow)
		if err != nil || st.HistoryDue {
			t.Fatalf("an issue inside its wait: sync state %+v, %v; want none due", st, err)
		}
		if st, _, err := s.GetMirrorSync(root, "10", testNow+60_000); err != nil || !st.HistoryDue {
			t.Fatalf("an issue past its wait: sync state %+v, %v; want one due", st, err)
		}
	}})
	for _, tick := range append([]idleTick{
		{"a shown board", func() {
			call("GetMirrorSync", syncState("10"))
			call("RecordMirrorSyncAttempt", record)
		}, 3, map[string]int{"tracker_cache_sync": 1}, nil},
		{"a shown board redelivered an identical row", func() {
			call("GetMirrorSync", syncState("10"))
			call("WriteMirrorRowPage", func() error {
				p := page
				p.ReadAt = mirrorReadAt(at)
				_, err := s.WriteMirrorRowPage(root, p, fmt.Appendf(nil, "c%d", at))
				return err
			})
			call("RecordMirrorSyncAttempt", record)
		}, 10, map[string]int{"tracker_cache_board_issues": 1, "tracker_cache_issues": 1, "tracker_cache_sync": 2}, nil},
	}, rounds...) {
		if tick.before != nil {
			tick.before()
		}
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
