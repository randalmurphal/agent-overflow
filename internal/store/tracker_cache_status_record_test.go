package store

import (
	"errors"
	"slices"
	"testing"
)

// statusRecord is a board of issue 10001 at update u in status 1, read at
// 1, for the tests of what a status result applies to.
func statusRecord(t *testing.T, u int64) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", u, "a")), nil)
	return s, root
}

// dueFor reports whether issue is due a history read at testNow.
func dueFor(t *testing.T, s *Store, root int64, issue string) bool {
	t.Helper()
	due, _, err := s.ListMirrorHistoryDue(root, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(due, func(d MirrorHistoryDue) bool { return d.IssueID == issue })
}

// Two ticket reads of one update that found the issue in different
// statuses (a status change within the tracker's millisecond): the card of
// the later read is the record's whichever lands first, and the earlier
// read's card is refused with its evidence, which gives the record no
// result. So is a refused card of the same status (it differs in another
// field, a rename at the same update): its read is not the record's.
func TestARefusedTicketCardGivesNoStatusResult(t *testing.T) {
	t.Parallel()
	const u = 1_000_000
	s, root := statusRecord(t, u)
	write := func(read int64, status, title string, at int64, final bool) {
		t.Helper()
		card := webCard("10001", u)
		card.StatusID, card.Data = status, []byte(`{"title":"`+title+`"}`)
		e := MirrorStatusEvidence{Status: status, CreatedAt: card.CreatedAt, UpdatedAt: u, LatestTo: status, LatestAt: at, Final: final, Settle: 60_000}
		if _, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: card, Body: []byte(`{}`), Status: &e}},
			MirrorCatalog{}, mirrorReadAt(read), mirrorReadAt(read)); err != nil {
			t.Fatal(err)
		}
	}
	write(20, "3", "Issue", 900_000, true)
	write(10, "1", "Issue", 500_000, true)
	c := mustMirrorCards(t, s, root, "10001")[0]
	if c.StatusID != "3" || c.StatusSince != 900_000 || !c.StatusFinal {
		t.Fatalf("the record is in %s since %d, final %v; want the later read's 3 since 900000, final", c.StatusID, c.StatusSince, c.StatusFinal)
	}
	s, root = statusRecord(t, u)
	write(20, "1", "Renamed", 900_000, true)
	write(10, "1", "Issue", 500_000, true)
	if got := statusOf(t, s, root, "10001"); got != "900000 true 1000000" {
		t.Fatalf("after a refused read of the same status: %s; want the later read's 900000, final", got)
	}
}

// Two overlapping ticket reads of one update see the same card; the later
// read saw the change to its status (inside the settle margin, so its
// time is provisional), the earlier did not. Whichever lands first, the
// record shows the later read's time: provisional evidence of an earlier
// read replaces none of a later read (mirrorStatusWriteSQL).
func TestAnOlderReadsProvisionalTimeNeverReplacesANewerOnes(t *testing.T) {
	t.Parallel()
	const u = 1_000_000
	write := func(s *Store, root, read int64, latestTo string, at int64) {
		t.Helper()
		card := webCard("10001", u)
		card.StatusID, card.Data = "1", []byte(`{"title":"Issue"}`)
		e := MirrorStatusEvidence{Status: "1", CreatedAt: card.CreatedAt, UpdatedAt: u, LatestTo: latestTo, LatestAt: at, Settle: 60_000}
		if _, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: card, Body: []byte(`{}`), Status: &e}},
			MirrorCatalog{}, mirrorReadAt(read), mirrorReadAt(read)); err != nil {
			t.Fatal(err)
		}
	}
	for name, order := range map[string][]int64{"the later read first": {20, 10}, "the earlier read first": {10, 20}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, root := statusRecord(t, u)
			for _, read := range order {
				if read == 20 {
					write(s, root, read, "1", u-1_000)
				} else {
					write(s, root, read, "", 0)
				}
			}
			if got := statusOf(t, s, root, "10001"); got != "999000 false 1000000" {
				t.Fatalf("the record's status time: %s; want the later read's 999000, provisional", got)
			}
		})
	}
}

// Evidence of a change after the version it describes is of a later
// version: it gives no result, final or not.
func TestEvidenceOfALaterChangeGivesNoResult(t *testing.T) {
	t.Parallel()
	for _, final := range []bool{true, false} {
		e := MirrorStatusEvidence{Status: "1", CreatedAt: 1, UpdatedAt: 1_000, LatestTo: "1", LatestAt: 1_001, Final: final, Settle: 60_000}
		if r, since := mirrorStatusSince(e); r != 0 || since != 0 {
			t.Errorf("final %v: result %d since %d; want none", final, r, since)
		}
	}
}

// A staged history computes its result for the status the record held
// when the history was admitted. A card of the same update in another
// status (a later read's) that lands while it stages moves the issue's
// receipt (mirrorStatusKeptBy): the switch is refused, its staged changes
// deleted, and the issue is due a history read for the status it is in.
// Were the status moved by a write that left the receipt, the switch
// itself finds no record of the status its result is for, and applies
// none (mirrorStatusWriteSQL).
func TestAStagedHistoryAppliesItsResultOnlyToTheStatusItDescribes(t *testing.T) {
	t.Parallel()
	const u, n = 10_000, 5000
	for name, tc := range map[string]struct {
		set  string
		want MirrorWrite
		due  bool
	}{
		"the card path":        {"status_id = '3', " + mirrorStatusKeptBy("'3'", "updated_at", "read_at"), MirrorWrite{Superseded: true}, true},
		"a status write alone": {"status_id = '3', status_since = 0, status_final_for = 0", MirrorWrite{Changed: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, root := statusRecord(t, u)
			// The write of a card of update u in status 3 between two chunks.
			if _, err := s.db.Exec(`CREATE TRIGGER test_status_moves AFTER INSERT ON tracker_cache_changes WHEN NEW.seq = 1000
				BEGIN UPDATE tracker_cache_issues SET ` + tc.set + ` WHERE issue_id = '10001'; END`); err != nil {
				t.Fatal(err)
			}
			mustReplaceHistory(t, s, root, longHistory("10001", n, 1), tc.want)
			if _, err := s.db.Exec(`DROP TRIGGER test_status_moves`); err != nil {
				t.Fatal(err)
			}
			c := mustMirrorCards(t, s, root, "10001")[0]
			if c.StatusID != "3" || c.StatusSince != 0 || c.StatusFinal {
				t.Fatalf("the record is in %s since %d, final %v; want 3 with no time: the history's result was for 1", c.StatusID, c.StatusSince, c.StatusFinal)
			}
			if got := dueFor(t, s, root, "10001"); got != tc.due {
				t.Fatalf("the issue is due a history read: %v; want %v", got, tc.due)
			}
		})
	}
}

// A long history that keeps the stored one in place (its changes and one
// more) is the issue's only while its receipt is the one it was admitted
// at: a card of the same update in another status that lands between the
// admission and its transaction (mirrorStatusKeptBy moved the receipt)
// supersedes it, so the issue stays due a history read.
func TestAHistoryKeptInPlaceChecksItsReceipt(t *testing.T) {
	t.Parallel()
	const u, n = 10_000, 5000
	s, root := statusRecord(t, u)
	h := longHistory("10001", n, 1)
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	more := h
	more.Changes = append(slices.Clone(h.Changes), MirrorChange{Kind: MirrorChangeStatus, At: n + 1, To: "1"})
	more.AnsweredAt++
	in, err := newMirrorHistoryInput(dueHistories(s, root, []MirrorHistory{more})[0], nextMirrorRead(s, root).rootOrder)
	if err != nil {
		t.Fatal(err)
	}
	var result MirrorHistoryResult
	st, admitted, err := s.stagedHistoryState(root, in, &result)
	if err != nil || !admitted {
		t.Fatalf("admitted %v, %v", admitted, err)
	}
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET status_id = '3', `+mirrorStatusKeptBy("'3'", "updated_at", "read_at")+` WHERE issue_id = '10001'`)
	if _, err := s.writeStagedHistory(root, in, st, "test"); !errors.Is(err, errStaleReceipt) {
		t.Fatalf("the history kept in place after its receipt moved: %v; want its receipt stale", err)
	}
	if !dueFor(t, s, root, "10001") {
		t.Fatal("the issue whose status moved after the history's admission is not due a history read")
	}
}

// A history asked for the card of update u may hold changes the tracker
// made after it (the issue moved on, to 3 and back to 1): the card's stay
// in its status began with the latest change at or before u.
func TestAHistoryGivesTheStayOfTheVersionItWasAskedFor(t *testing.T) {
	t.Parallel()
	const u = 1_000
	s, root := statusRecord(t, u)
	h := MirrorHistory{IssueID: "10001", AnsweredAt: 2_000, Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 100, To: "1"},
		{Kind: MirrorChangeStatus, At: 1_100, To: "3"},
		{Kind: MirrorChangeStatus, At: 1_200, To: "1"},
	}}
	mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
	if got := statusOf(t, s, root, "10001"); got != "100 true 1000" {
		t.Fatalf("the card of update 1000 shows %s; want 100, final: the return at 1200 is a later version's", got)
	}
}

// A card of the same update in another status, from a later read, clears
// the status time a final read decided; the issue, whose history was read
// for that update, is due a history read again. A final unknown (no change
// in a history that is not complete from the creation, Linear's folded
// opening) is decided: the issue is not due again.
func TestAStatusResultLostAtTheSameUpdateIsDueAndAFinalUnknownIsNot(t *testing.T) {
	t.Parallel()
	const u = 1_000
	s, root := statusRecord(t, u)
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10001", AnsweredAt: 2_000}, MirrorWrite{Changed: true})
	if got := statusOf(t, s, root, "10001"); got != "1 true 1000" || dueFor(t, s, root, "10001") {
		t.Fatalf("after a final read: %s, due %v; want the creation 1, final, not due", got, dueFor(t, s, root, "10001"))
	}
	moved := webCard("10001", u)
	moved.StatusID = "3"
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{moved}, MirrorCatalog{}, mirrorReadAt(5)); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, root, "10001"); got != "0 false 1000" || !dueFor(t, s, root, "10001") {
		t.Fatalf("after a card of the same update in status 3: %s, due %v; want no time, due", got, dueFor(t, s, root, "10001"))
	}
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10001", CompleteFrom: 500, AnsweredAt: 3_000}, MirrorWrite{Changed: true})
	if got := statusOf(t, s, root, "10001"); got != "0 true 1000" || dueFor(t, s, root, "10001") {
		t.Fatalf("after a final unknown: %s, due %v; want no time, final, not due", got, dueFor(t, s, root, "10001"))
	}
}

// A body's identity as the record holds it is the admitted body's: a body
// of a later version read before the held one's read replaces it with its
// own read, so a later read of that version is admitted after it; in the other
// order the earlier version is superseded and the record keeps its own.
func TestAStoredBodyIsTheAdmittedBodysIdentity(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*Store, int64, func(u, read int64, data string) MirrorTicketWrite) {
		s, root := statusRecord(t, 1)
		return s, root, func(u, read int64, data string) MirrorTicketWrite {
			t.Helper()
			w, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: webCard("10001", u), Body: []byte(data)}},
				MirrorCatalog{}, mirrorReadAt(read), mirrorReadAt(read))
			if err != nil {
				t.Fatal(err)
			}
			return w.Tickets[0]
		}
	}
	held := func(t *testing.T, s *Store, root int64) MirrorBody {
		t.Helper()
		return heldBody(t, s, root, "10001")
	}
	t.Run("a later version read earlier", func(t *testing.T) {
		t.Parallel()
		s, root, write := setup(t)
		write(100, 30, `{"v":1}`)
		if w := write(200, 10, `{"v":2}`); w.Body != MirrorBodyStored {
			t.Fatalf("the later version: %+v; want stored", w)
		}
		if b := held(t, s, root); b.UpdatedAt != 200 || b.ReadAt != 10 || b.Wall != 10 || string(b.Data) != `{"v":2}` {
			t.Fatalf("the record holds %d read %d wall %d %s; want version 200 read 10", b.UpdatedAt, b.ReadAt, b.Wall, b.Data)
		}
		if w := write(200, 20, `{"v":3}`); w.Body != MirrorBodyStored || !w.BodyChanged {
			t.Fatalf("a later read of the version: %+v; want stored", w)
		}
		if b := held(t, s, root); b.ReadAt != 20 || string(b.Data) != `{"v":3}` {
			t.Fatalf("the record holds read %d %s; want read 20's", b.ReadAt, b.Data)
		}
	})
	t.Run("an earlier version read later", func(t *testing.T) {
		t.Parallel()
		s, root, write := setup(t)
		write(200, 10, `{"v":2}`)
		if w := write(100, 30, `{"v":1}`); w.Body != MirrorBodySuperseded {
			t.Fatalf("the earlier version: %+v; want superseded", w)
		}
		if b := held(t, s, root); b.UpdatedAt != 200 || b.ReadAt != 10 || string(b.Data) != `{"v":2}` {
			t.Fatalf("the record holds %d read %d %s; want version 200 read 10", b.UpdatedAt, b.ReadAt, b.Data)
		}
	})
}
