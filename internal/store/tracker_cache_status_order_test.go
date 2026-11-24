package store

import (
	"errors"
	"fmt"
	"testing"
)

// The tests of the order of status evidence (mirrorStatusWriteSQL): each
// on statusRecord's issue 10001 at update orderU in status 1, its card
// read at 1.
const orderU = 1_000_000

// orderTicket stores a ticket read of issue 10001 begun at read, with its
// body, whose evidence finds the card's status 1 entered at at (zero: no
// change held), final or not; card is the card it read.
func orderTicket(t *testing.T, s *Store, root, read int64, card MirrorCard, at int64, final bool) MirrorTicketWrite {
	t.Helper()
	e := MirrorStatusEvidence{Status: card.StatusID, CreatedAt: card.CreatedAt, UpdatedAt: card.UpdatedAt, Final: final, Settle: 60_000}
	if at > 0 {
		e.LatestTo, e.LatestAt = card.StatusID, at
	}
	w, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: card, Body: []byte(`{}`), Status: &e}},
		MirrorCatalog{}, mirrorReadOf(root, read), mirrorReadOf(root, read))
	if err != nil {
		t.Fatal(err)
	}
	return w.Tickets[0]
}

// orderHistory is a history of issue 10001 whose latest status change
// entered status 1 at at, final when wait is zero.
func orderHistory(at, wait int64) MirrorHistory {
	return MirrorHistory{IssueID: "10001", AnsweredAt: 2_000_000, Wait: wait,
		Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: at, From: "3", To: "1"}}}
}

// storeOrderHistory stores h, listed now at the issue's current receipt,
// as read by the read that began at read.
func storeOrderHistory(t *testing.T, s *Store, root, read int64, h MirrorHistory) MirrorHistoryResult {
	t.Helper()
	results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{},
		mirrorReadOf(root, read))
	if err != nil {
		t.Fatal(err)
	}
	return results[0]
}

// statusOrders is issue 10001's status_floor_at and status_read_at.
func statusOrders(t *testing.T, s *Store, root int64) [2]int64 {
	t.Helper()
	var o [2]int64
	if err := s.db.QueryRow(`SELECT status_floor_at, status_read_at FROM tracker_cache_issues WHERE root_id = ?1 AND issue_id = '10001'`,
		root).Scan(&o[0], &o[1]); err != nil {
		t.Fatal(err)
	}
	return o
}

// A provisional time from a later read is never replaced by one from an
// earlier read, whichever writer brought each and in either order: a
// ticket read against a history read, a history read against a history
// read (the store's own order; the service also serializes a connection's
// history reads), and a ticket read against one whose body the cap
// dropped in its own transaction, so no held body orders them.
func TestProvisionalStatusEvidenceIsOrderedByItsRead(t *testing.T) {
	t.Parallel()
	card := webCard("10001", orderU)
	const older, newer = int64(10), int64(20)
	const olderAt, newerAt = orderU - 2_000, orderU - 1_000
	writers := map[string]func(t *testing.T, s *Store, root, read, at int64){
		"ticket": func(t *testing.T, s *Store, root, read, at int64) { orderTicket(t, s, root, read, card, at, false) },
		"history": func(t *testing.T, s *Store, root, read, at int64) {
			storeOrderHistory(t, s, root, read, orderHistory(at, 60_000))
		},
		// The body this read stores is dropped at once, as the cap drops a
		// body in the transaction of the read that stored the 201st.
		"ticket, body dropped": func(t *testing.T, s *Store, root, read, at int64) {
			orderTicket(t, s, root, read, card, at, false)
			mustExec(t, s.db, `UPDATE tracker_cache_issues SET body_updated_at = 0, body_read_at = 0, body_wall = 0, body_opened_at = 0,
				body_digest = x'' WHERE issue_id = '10001'`)
			mustExec(t, s.db, `DELETE FROM tracker_cache_issue_bodies WHERE issue_id = '10001'`)
		},
	}
	for _, pair := range [][2]string{{"ticket", "history"}, {"history", "ticket"}, {"history", "history"}, {"ticket, body dropped", "ticket"}} {
		for _, newerFirst := range []bool{true, false} {
			name := "newer " + pair[0] + ", older " + pair[1] + map[bool]string{true: ", newer first", false: ", older first"}[newerFirst]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s, root := statusRecord(t, orderU)
				newerWrite := func() { writers[pair[0]](t, s, root, newer, newerAt) }
				olderWrite := func() { writers[pair[1]](t, s, root, older, olderAt) }
				if newerFirst {
					newerWrite()
					olderWrite()
				} else {
					olderWrite()
					newerWrite()
				}
				if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
					t.Fatalf("the record's status time: %s; want the newer read's 999000, provisional", got)
				}
				if got := statusOrders(t, s, root); got != [2]int64{1, newer} {
					t.Fatalf("the evidence orders: %v; want the card's floor 1 and the newer read %d", got, newer)
				}
			})
		}
	}
}

// A later read of the same provisional time moves the order of the
// evidence and is not told as a change, by a ticket read or a history
// read; a read between the two then gives no result.
func TestALaterReadOfTheSameProvisionalTimeMovesOnlyItsOrder(t *testing.T) {
	t.Parallel()
	card := webCard("10001", orderU)
	const at = orderU - 1_000
	t.Run("ticket", func(t *testing.T) {
		t.Parallel()
		s, root := statusRecord(t, orderU)
		if w := orderTicket(t, s, root, 10, card, at, false); !w.StatusChanged {
			t.Fatalf("the first provisional time: %+v; want its status time changed", w)
		}
		if w := orderTicket(t, s, root, 30, card, at, false); w.StatusChanged {
			t.Fatalf("the same time read again: %+v; want no status change", w)
		}
		if got := statusOrders(t, s, root); got[1] != 30 {
			t.Fatalf("the evidence orders: %v; want the later read 30", got)
		}
		orderTicket(t, s, root, 20, card, 0, false)
		if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
			t.Fatalf("after a read between them: %s; want 999000, provisional", got)
		}
	})
	t.Run("history", func(t *testing.T) {
		t.Parallel()
		s, root := statusRecord(t, orderU)
		if r := storeOrderHistory(t, s, root, 10, orderHistory(at, 60_000)); !r.Changed {
			t.Fatalf("the first provisional time: %+v; want changed", r)
		}
		later := orderHistory(at, 60_000)
		later.AnsweredAt++
		if r := storeOrderHistory(t, s, root, 30, later); r.Changed || r.Superseded {
			t.Fatalf("the same time read again: %+v; want stored, unchanged", r)
		}
		if got := statusOrders(t, s, root); got[1] != 30 {
			t.Fatalf("the evidence orders: %v; want the later read 30", got)
		}
		storeOrderHistory(t, s, root, 20, orderHistory(orderU-2_000, 60_000))
		if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
			t.Fatalf("after a read between them: %s; want 999000, provisional", got)
		}
	})
}

// A ticket read that began before a history read and lands after it gives
// no provisional time over the history's, on each path a history takes:
// the batch, a long one switched in from its staging, and a long one kept
// in place over the stored one.
func TestAnOlderTicketNeverReplacesANewerHistorysProvisionalTime(t *testing.T) {
	t.Parallel()
	card := webCard("10001", orderU)
	// A long history of status changes to 3, and when at is over zero a
	// last one back to 1 at at.
	long := func(at int64) MirrorHistory {
		h := statusHistory("10001", clearMirrorChunk+100)
		if at > 0 {
			h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: at, From: "3", To: "1"})
		}
		h.AnsweredAt, h.Wait = 2_000_000, 60_000
		return h
	}
	for name, tc := range map[string]struct {
		stored  *MirrorHistory
		history MirrorHistory
		gen     int64 // the generation the history is written to
	}{
		"batch":          {nil, orderHistory(orderU-1_000, 60_000), 0},
		"staged, switch": {nil, long(orderU - 1_000), 1},
		// The stored changes and one more: kept in place, the one added.
		"staged in place": {new(long(0)), long(orderU - 1_000), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, root := statusRecord(t, orderU)
			if tc.stored != nil {
				storeOrderHistory(t, s, root, 5, *tc.stored)
			}
			if r := storeOrderHistory(t, s, root, 20, tc.history); !r.Changed {
				t.Fatalf("the history: %+v; want changed", r)
			}
			assertRows(t, s.db, `SELECT history_gen FROM tracker_cache_issues WHERE issue_id = '10001'`, fmt.Sprint(tc.gen))
			if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
				t.Fatalf("after the history: %s; want 999000, provisional", got)
			}
			orderTicket(t, s, root, 10, card, 0, false)
			if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
				t.Fatalf("after the older ticket: %s; want the history's 999000, provisional", got)
			}
		})
	}
}

// A final time from a read older than a stored provisional one still
// decides: a history read that began first and answered after the settle
// margin, landing after a ticket read inside the margin. The history
// leaves the issue due nothing, so were it refused the issue would keep
// the provisional time for good.
func TestAFinalTimeFromAnOlderReadReplacesANewerProvisionalOne(t *testing.T) {
	t.Parallel()
	card := webCard("10001", orderU)
	s, root := statusRecord(t, orderU)
	orderTicket(t, s, root, 20, card, 0, false)
	if got := statusOf(t, s, root, "10001"); got != "0 false 1000000" {
		t.Fatalf("after the ticket: %s; want no time, provisional", got)
	}
	if r := storeOrderHistory(t, s, root, 10, orderHistory(orderU-1_000, 0)); !r.Changed {
		t.Fatalf("the final history: %+v; want changed", r)
	}
	if got := statusOf(t, s, root, "10001"); got != "999000 true 1000000" {
		t.Fatalf("after the final history: %s; want 999000, final", got)
	}
	if dueFor(t, s, root, "10001") {
		t.Fatal("the issue is due a history read after its final history")
	}
}

// A card write that moves the record's version or status is the floor of
// its status evidence: a read that began before it may have seen a stay
// the issue left (status 1, then 3 and back to 1 within one update time),
// so its result, final or provisional, does not enter, and the issue is
// due a history read, which a read after the card write answers.
func TestEvidenceFromBeforeTheCardWriteThatMovedTheRecordDoesNotEnter(t *testing.T) {
	t.Parallel()
	in := func(status string, updated int64) MirrorCard {
		c := webCard("10001", updated)
		c.StatusID = status
		return c
	}
	for name, moves := range map[string][]MirrorCard{
		"the same update, 1 to 3 and back": {in("3", orderU), in("1", orderU)},
		"a later update":                   {in("1", orderU+5_000)},
	} {
		for _, final := range []bool{true, false} {
			t.Run(name+map[bool]string{true: ", final", false: ", provisional"}[final], func(t *testing.T) {
				t.Parallel()
				s, root := statusRecord(t, orderU)
				read := int64(10)
				for _, c := range moves {
					read++
					if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{c}, MirrorCatalog{}, mirrorReadOf(root, read)); err != nil {
						t.Fatal(err)
					}
				}
				last := moves[len(moves)-1]
				if got := statusOrders(t, s, root); got != [2]int64{read, read} {
					t.Fatalf("the evidence orders after the card writes: %v; want both at the last write's read %d", got, read)
				}
				// The held read, begun at 10, read the card the record holds.
				orderTicket(t, s, root, 10, last, last.UpdatedAt-1_000, final)
				if got := mustMirrorCards(t, s, root, "10001")[0]; got.StatusSince != 0 || got.StatusFinal {
					t.Fatalf("after the held read: since %d, final %v; want no time", got.StatusSince, got.StatusFinal)
				}
				if !dueFor(t, s, root, "10001") {
					t.Fatal("the issue whose evidence was refused is not due a history read")
				}
				storeOrderHistory(t, s, root, read+1, orderHistory(last.UpdatedAt-1_000, 0))
				if got := mustMirrorCards(t, s, root, "10001")[0]; got.StatusSince != last.UpdatedAt-1_000 || !got.StatusFinal {
					t.Fatalf("after the history read since: since %d, final %v; want %d, final", got.StatusSince, got.StatusFinal, last.UpdatedAt-1_000)
				}
			})
		}
	}
}

// The card write that first stores an issue is the floor of its status
// evidence too: a ticket read begun before it gives no result.
func TestEvidenceFromBeforeTheIssueWasStoredDoesNotEnter(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	page := webPage("10", webRow("10001", orderU, "a"))
	page.ReadAt = mirrorReadOf(root, 11)
	mustWriteRowPage(t, s, root, page, nil)
	if got := statusOrders(t, s, root); got != [2]int64{11, 11} {
		t.Fatalf("the evidence orders of the new issue: %v; want its card's read 11", got)
	}
	orderTicket(t, s, root, 10, webCard("10001", orderU), orderU-1_000, true)
	if got := statusOf(t, s, root, "10001"); got != "0 false 1000000" {
		t.Fatalf("after a read begun before the issue was stored: %s; want no time", got)
	}
}

// A history write needs the read it came from, also with no catalog to
// store: its evidence is ordered by it.
func TestAHistoryWithNoReadIsRefused(t *testing.T) {
	t.Parallel()
	s, root := statusRecord(t, orderU)
	_, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{orderHistory(orderU-1_000, 0)}),
		MirrorCatalog{}, MirrorRead{})
	if !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a history with no read: %v; want ErrInvalidTrackerCache", err)
	}
	if got := statusOf(t, s, root, "10001"); got != "0 false 1000000" {
		t.Fatalf("after the refused history: %s; want nothing stored", got)
	}
}

// The order of the provisional evidence seeds the read clock: after a
// restart, a read orders after a history read whose order no other column
// holds, so its newer provisional time enters.
func TestAReadAfterARestartOrdersAfterTheStoredStatusEvidence(t *testing.T) {
	s, root := statusRecord(t, orderU)
	const stored = int64(5_000_000)
	storeOrderHistory(t, s, root, stored, orderHistory(orderU-2_000, 60_000))
	s = reopenStore(t, s)
	read := beginRead(t, s, root, 1_000)
	if read.At() <= stored {
		t.Fatalf("the first read after the restart began at %d; want after the stored evidence's %d", read.At(), stored)
	}
	orderTicket(t, s, root, read.At(), webCard("10001", orderU), orderU-1_000, false)
	if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
		t.Fatalf("after the read since the restart: %s; want its 999000, provisional", got)
	}
}
