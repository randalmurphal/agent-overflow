package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// bodyOf is issue's body bookkeeping and the body's v member, or "none".
func bodyOf(t *testing.T, s *Store, root int64, issue string) string {
	t.Helper()
	var got string
	err := s.db.QueryRow(`SELECT coalesce((SELECT i.body_updated_at || ' ' || i.body_read_at || ' ' || i.body_opened_at || ' ' ||
		coalesce((SELECT b.data ->> 'v' FROM tracker_cache_issue_bodies b WHERE b.root_id = i.root_id AND b.issue_id = i.issue_id), '-')
		FROM tracker_cache_issues i WHERE i.root_id = ?1 AND i.issue_id = ?2 AND i.body_read_at > 0), 'none')`, root, issue).Scan(&got)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// checkBodiesHeld fails unless every issue of a live root holds a body
// exactly when its bookkeeping says it does.
func checkBodiesHeld(t *testing.T, s *Store, after string) {
	t.Helper()
	var bad int
	if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_issues i
		JOIN tracker_cache_roots r ON r.id = i.root_id AND r.connection_id IS NOT NULL
		WHERE (i.body_read_at > 0) <> EXISTS (SELECT 1 FROM tracker_cache_issue_bodies b WHERE b.root_id = i.root_id AND b.issue_id = i.issue_id)
		   OR (i.body_read_at > 0) <> (i.body_updated_at > 0) OR (i.body_read_at > 0) <> (length(i.body_digest) > 0)`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad > 0 {
		t.Fatalf("after %s, %d issues hold a body without its bookkeeping or bookkeeping without its body", after, bad)
	}
}

// A ticket's body is ordered by its version and then its read: a body of
// a later update, or of the same update from a later read, replaces the
// record's; one of an earlier update, or of the same update from an
// earlier read, is superseded; the same body again moves the read alone
// and writes no body bytes, and reports a change only when it is of
// another version, which a view of it shows. Only a person's opening, or a read that began
// after an open ticket of the issue was bound, starts a body.
func TestATicketsBodyFollowsItsVersionAndRead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	recorder := recordMirrorWrites(t, s)
	sweep := currentSweep(s, root)
	for _, step := range []struct {
		name         string
		issue        string
		updated      int64
		opened, read int64
		body         string
		want         MirrorBodyOutcome
		changed      bool
		writes       map[string]int
		state        string
	}{
		{"no opening", "10001", 5, 0, 60, "1", MirrorBodyNotKept, false, map[string]int{"tracker_cache_issues": 1}, "none"},
		{"opened", "10001", 5, 80, 80, "1", MirrorBodyStored, true, map[string]int{"tracker_cache_issues": 1, "tracker_cache_issue_bodies": 1}, "5 80 80 1"},
		{"the same body read later", "10001", 5, 0, 90, "1", MirrorBodyStored, false, map[string]int{"tracker_cache_issues": 1}, "5 90 80 1"},
		{"the same update, an earlier read", "10001", 5, 0, 85, "2", MirrorBodySuperseded, false, map[string]int{}, "5 90 80 1"},
		{"the same update, a later read", "10001", 5, 0, 95, "3", MirrorBodyStored, true, map[string]int{"tracker_cache_issues": 1, "tracker_cache_issue_bodies": 1}, "5 95 80 3"},
		{"a later update", "10001", 6, 0, 99, "4", MirrorBodyStored, true, map[string]int{"tracker_cache_issues": 2, "tracker_cache_issue_bodies": 1}, "6 99 80 4"},
		{"an earlier update from a later read", "10001", 5, 0, 120, "5", MirrorBodySuperseded, false, map[string]int{}, "6 99 80 4"},
		{"opened again", "10001", 6, 200, 200, "4", MirrorBodyStored, false, map[string]int{"tracker_cache_issues": 1}, "6 200 200 4"},
		{"the same body of a later update", "10001", 7, 0, 210, "4", MirrorBodyStored, true, map[string]int{"tracker_cache_issues": 2}, "7 210 200 4"},
		{"a read before the opening", "10002", 5, 300, 250, "1", MirrorBodyNotKept, false, map[string]int{"tracker_cache_issues": 1}, "none"},
	} {
		recorder.reset()
		card := heldCard(t, s, root, step.issue)
		card.UpdatedAt = step.updated
		got, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{{Card: card, Body: []byte(`{"v":"` + step.body + `"}`)}}, MirrorCatalog{},
			mirrorReadAt(step.opened), mirrorReadAt(step.read))
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		w := got.Tickets[0]
		if w.Body != step.want || w.BodyChanged != step.changed || !reflect.DeepEqual(recorder.rowWrites(), step.writes) {
			t.Errorf("%s: body %v changed %v, rows written %v; want %v, %v, %v", step.name, w.Body, w.BodyChanged, recorder.rowWrites(),
				step.want, step.changed, step.writes)
		}
		if got := bodyOf(t, s, root, step.issue); got != step.state {
			t.Errorf("%s: the record holds %s; want %s", step.name, got, step.state)
		}
		checkBodiesHeld(t, s, step.name)
	}
}

// An open ticket's read starts and replaces a body only under the bind
// that is the issue's (KeepMirrorTicket), an automatic read included;
// a read no open ticket made starts one only as a person's opening. A
// close and a reopen make a new bind: a read of the closed lifetime starts
// no body and replaces none, however late it began, and the new
// lifetime's does.
func TestAnOpenTicketsBindAdmitsItsBody(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	first, _, err := s.KeepMirrorTicket(root, "10001", 1, 1, mirrorReadAt(100))
	if err != nil || first != 100 {
		t.Fatalf("the bind: %d, %v; want the bind's read 100", first, err)
	}
	if w, err := storeTicket(t, s, root, sweep, "10001", 0, 110, []byte(`{"v":"direct"}`)); err != nil || w.Body != MirrorBodyNotKept {
		t.Fatalf("a read no open ticket made, not an opening: %+v, %v", w, err)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 110, []byte(`{"v":"after"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read under the bind: %+v", w)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 110 100 after" {
		t.Fatalf("the record holds %s", got)
	}
	if _, err := s.DropMirrorTicket(1, 1); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.KeepMirrorTicket(root, "10001", 1, 2, mirrorReadAt(300))
	if err != nil || second != 300 {
		t.Fatalf("the bind of the reopening: %d, %v; want 300", second, err)
	}
	// Held: the closed lifetime's read, begun after the new bind, replaces
	// nothing.
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 320, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read of the closed lifetime over a held body: %+v", w)
	}
	mustExec(t, s.db, `DELETE FROM tracker_cache_issue_bodies`)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET body_updated_at = 0, body_read_at = 0, body_wall = 0, body_opened_at = 0, body_digest = x''`)
	if w := storeBoundTicket(t, s, root, sweep, "10001", first, 330, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
		t.Fatalf("a read of the closed lifetime with no body held: %+v", w)
	}
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 310, []byte(`{"v":"reopened"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read of the new lifetime: %+v", w)
	}
	if got := bodyOf(t, s, root, "10001"); got != "5 310 300 reopened" {
		t.Fatalf("the record holds %s", got)
	}
	checkBodiesHeld(t, s, "the reopen")
}

// A cards-only ticket write (a check that confirmed the held body) records
// its opening on the body, and so its place in the cap, only under the
// issue's current bind: a closed lifetime's check, an opening included,
// leaves the body's opening as it was.
func TestACardsOnlyWriteOfAClosedLifetimeRecordsNoOpening(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	sweep := currentSweep(s, root)
	first := keepOf(t, s, root, 1, 100)
	if _, err := s.DropMirrorTicket(1, 1); err != nil {
		t.Fatal(err)
	}
	second := keepOf(t, s, root, 2, 300)
	if w := storeBoundTicket(t, s, root, sweep, "10001", second, 310, []byte(`{"v":"reopened"}`)); w.Body != MirrorBodyStored {
		t.Fatalf("a read of the new lifetime: %+v", w)
	}
	check := func(bind MirrorBind, at int64) {
		t.Helper()
		ticket := heldTicket(t, s, root, "10001", nil)
		ticket.Bind = bind
		if _, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{ticket}, MirrorCatalog{}, mirrorReadOf(root, at), mirrorReadOf(root, at)); err != nil {
			t.Fatal(err)
		}
	}
	check(first, 400)
	if got := bodyOf(t, s, root, "10001"); got != "5 310 300 reopened" {
		t.Fatalf("after the closed lifetime's check the record holds %s; want the reopened body's opening", got)
	}
	check(second, 410)
	if got := bodyOf(t, s, root, "10001"); got != "5 310 410 reopened" {
		t.Fatalf("after the current bind's check the record holds %s; want its opening", got)
	}
}

// keepOf binds 10001 for pump of session 1 at a read begun at at.
func keepOf(t *testing.T, s *Store, root, pump, at int64) MirrorBind {
	t.Helper()
	bind, _, err := s.KeepMirrorTicket(root, "10001", 1, pump, mirrorReadAt(at))
	if err != nil {
		t.Fatal(err)
	}
	return bind
}

// A ticket opened again while the closing pump's keep still stands (its
// drop owed): the new bind takes the issue's row, so the closed lifetime
// never admits again, whichever row goes first and though its own drop
// comes last; a pump earlier than the bound one cannot bind the issue.
func TestAReadOfAClosedLifetimeStartsNoBodyAfterTheReopen(t *testing.T) {
	t.Parallel()
	for _, newerDroppedFirst := range []bool{false, true} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
		sweep := currentSweep(s, root)
		first, _, err := s.KeepMirrorTicket(root, "10001", 1, 1, mirrorReadAt(100))
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := s.KeepMirrorTicket(root, "10001", 1, 2, mirrorReadAt(300))
		if err != nil {
			t.Fatal(err)
		}
		assertRows(t, s.db, `SELECT pump FROM tracker_cache_open_tickets`, "2")
		if _, _, err := s.KeepMirrorTicket(root, "10001", 1, 1, mirrorReadAt(400)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Fatalf("the earlier pump's bind after the later one: %v; want refused", err)
		}
		if w := storeBoundTicket(t, s, root, sweep, "10001", first, 350, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
			t.Fatalf("a read of the closed lifetime after the reopen: %+v; want no body started", w)
		}
		if newerDroppedFirst {
			// The reopened ticket closes too, and the owed drop of the first
			// comes after: no bind stands, and the first never admits.
			for _, pump := range []int64{2, 1} {
				if _, err := s.DropMirrorTicket(1, pump); err != nil {
					t.Fatal(err)
				}
			}
			// Board 10 still keeps the issue.
			if w := storeBoundTicket(t, s, root, sweep, "10001", first, 360, []byte(`{"v":"closed"}`)); w.Body != MirrorBodyNotKept {
				t.Fatalf("a read of the first lifetime after both drops: %+v", w)
			}
			continue
		}
		if w := storeBoundTicket(t, s, root, sweep, "10001", second, 310, []byte(`{"v":"reopened"}`)); w.Body != MirrorBodyStored {
			t.Fatalf("a read of the new lifetime: %+v", w)
		}
	}
}

// The root keeps mirrorBodyCap bodies: those of issues an open ticket
// keeps first, then the most recently opened. The outcomes are of the
// committed state: a body the same write started and dropped was never
// held and is not reported dropped; one held before is.
func TestTheBodyCapKeepsTheOpenTicketsAndTheMostRecentlyOpened(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorBodyCap+3, "a"))
	sweep := currentSweep(s, root)
	// 10000 is opened first, and stays open as a ticket.
	if _, _, err := s.KeepMirrorTicket(root, "10000", 1, 1, mirrorReadAt(5)); err != nil {
		t.Fatal(err)
	}
	for i := range mirrorBodyCap + 1 {
		id := fmt.Sprint(10000 + i)
		if _, err := storeTicket(t, s, root, sweep, id, int64(10+i), int64(10+i), []byte(`{"v":"1"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Opened second, 10001 was the least recently opened with no keep row.
	if got := bodyOf(t, s, root, "10001"); got != "none" {
		t.Fatalf("the least recently opened body is held: %s", got)
	}
	if got := bodyOf(t, s, root, "10000"); got == "none" {
		t.Fatal("the open ticket's body was dropped")
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_bodies`, fmt.Sprint(mirrorBodyCap))
	// One write that starts two bodies opened before every other, one of
	// them an open ticket's, and reads a held one again: it drops the body
	// it started with no keep row and the least recently opened held one.
	if _, _, err := s.KeepMirrorTicket(root, fmt.Sprint(10000+mirrorBodyCap+2), 1, 2, mirrorReadAt(900)); err != nil {
		t.Fatal(err)
	}
	tickets := heldTickets(t, s, root, []string{fmt.Sprint(10000 + mirrorBodyCap + 1), fmt.Sprint(10000 + mirrorBodyCap + 2), "10003"}, []byte(`{"v":"1"}`))
	got, err := s.StoreMirrorTickets(root, sweep, tickets, MirrorCatalog{}, mirrorReadAt(1), mirrorReadAt(1_000))
	if err != nil {
		t.Fatal(err)
	}
	if w := got.Tickets[0]; w.Body != MirrorBodyNotKept || w.Held != (MirrorBodyState{}) || w.BodyChanged {
		t.Errorf("a body started and dropped by one write: %+v", w)
	}
	if w := got.Tickets[1]; w.Body != MirrorBodyStored || w.Held.ReadAt != 1_000 {
		t.Errorf("an open ticket's body started by the write: %+v", w)
	}
	if w := got.Tickets[2]; w.Body != MirrorBodyStored || w.Held.ReadAt != 1_000 || !reflect.DeepEqual(got.Dropped, []string{"10002"}) {
		t.Errorf("the held bodies after the write: %+v, dropped %v; want 10002 dropped", w, got.Dropped)
	}
	checkBodiesHeld(t, s, "the cap")
}

// A confirmation records a person's opening of the body GetMirrorBody read
// while the record holds it: not once a later read replaced it, the cap
// or a sweep dropped it, or its issue is no longer kept. Refused, it
// writes nothing.
func TestConfirmMirrorBody(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, 2, "a"))
	read := func(issue string) MirrorBody {
		t.Helper()
		b, ok, err := s.GetMirrorBody(root, issue)
		if err != nil || !ok {
			t.Fatalf("body of %s: %v, %v", issue, ok, err)
		}
		return b
	}
	confirm := func(what string, b MirrorBody, openedAt int64, want bool) {
		t.Helper()
		if got, err := s.ConfirmMirrorBody(root, b, mirrorReadAt(openedAt)); err != nil || got != want {
			t.Fatalf("%s: %v, %v; want %v", what, got, err, want)
		}
	}
	if _, err := storeTicket(t, s, root, currentSweep(s, root), "10000", 100, 100, []byte(`{"v":"1"}`)); err != nil {
		t.Fatal(err)
	}
	b := read("10000")
	if b.UpdatedAt != 5 || b.ReadAt != 100 || b.Wall != 100 || string(b.Data) != `{"v":"1"}` {
		t.Fatalf("the body read: %+v", b)
	}
	confirm("an opening", b, 150, true)
	confirm("an earlier opening", b, 120, true)
	if got := bodyOf(t, s, root, "10000"); got != "5 100 150 1" {
		t.Fatalf("after the openings: %s", got)
	}
	if _, err := storeTicket(t, s, root, currentSweep(s, root), "10000", 0, 200, []byte(`{"v":"3"}`)); err != nil {
		t.Fatal(err)
	}
	confirm("a replaced body", b, 300, false)
	if got := bodyOf(t, s, root, "10000"); got != "5 200 150 3" {
		t.Fatalf("after a refused confirmation: %s", got)
	}
	b = read("10000")
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10000"}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	confirm("an unkept issue", b, 400, false)
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	confirm("a body from before a sweep", b, 500, false)
	checkBodiesHeld(t, s, "the sweep")
	for name, b := range map[string]MirrorBody{"no body read": {}, "a body built by hand": {IssueID: "10001", ReadAt: 1}} {
		if _, err := s.ConfirmMirrorBody(root, b, mirrorReadAt(1)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A ticket's card goes through the card path of a card read by ID: the
// same order, catalog, sprints and marks. A key and a label renamed at an
// unchanged update time win from the later read in either arrival order,
// and the record keeps the body the ticket brought beside the card.
func TestATicketsCardIsAdmittedAsACardReadByID(t *testing.T) {
	t.Parallel()
	for _, ticketLast := range []bool{true, false} {
		t.Run(map[bool]string{true: "the ticket lands last", false: "the card read lands last"}[ticketLast], func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), nil)
			sweep := currentSweep(s, root)
			renamed := webCard("10001", 5, "21")
			renamed.Key, renamed.Data = "APP-1", []byte(`{"title":"Issue 10001","labels":[{"id":"web"}]}`)
			// The rename was read later than the old card.
			ticket := func() error {
				at := int64(20)
				card := renamed
				if !ticketLast {
					at = 30
				}
				_, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{{Card: card, Body: []byte(`{"v":"1"}`)}}, MirrorCatalog{
					Users: []MirrorUser{{ID: "acc-user9", Name: "User 9"}}}, mirrorReadAt(at), mirrorReadAt(at))
				return err
			}
			byID := func() error {
				at := int64(10)
				if !ticketLast {
					at = 20
				}
				old := webCard("10001", 5, "21")
				_, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{old}, MirrorCatalog{}, mirrorReadAt(at))
				return err
			}
			first, second := byID, ticket
			if !ticketLast {
				// The ticket's read began later but lands first.
				first, second = ticket, byID
			}
			if err := first(); err != nil {
				t.Fatal(err)
			}
			if err := second(); err != nil {
				t.Fatal(err)
			}
			card := mustMirrorCards(t, s, root, "10001")[0]
			if card.Key != "APP-1" || string(card.Data) != string(renamed.Data) || !reflect.DeepEqual(card.Sprints, []string{"21"}) {
				t.Fatalf("the record's card: %+v", card)
			}
			assertRows(t, s.db, `SELECT name FROM tracker_cache_users WHERE user_id = 'acc-user9'`, "User 9")
			if got := bodyOf(t, s, root, "10001"); got == "none" {
				t.Fatal("the record holds no body")
			}
		})
	}
}

// A card a ticket names that the mirror lacks a status of marks the
// boards showing the issue due a structure read, as a card read does.
func TestATicketsCardMarksWhatTheMirrorLacks(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	card := webCard("10001", 6)
	card.StatusID = "99"
	if _, err := s.StoreMirrorTickets(root, currentSweep(s, root), []MirrorTicket{{Card: card, Body: []byte(`{}`)}}, MirrorCatalog{},
		mirrorReadAt(20), mirrorReadAt(20)); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT board_id || ':' || ref_id FROM tracker_cache_board_unexplained WHERE kind = 1`, "10:99")
}

// The one status rule (MirrorStatusSince): a final read decides; a read
// that is not final shows a time only from a change no more than the
// settle margin before the version's update.
func TestMirrorStatusSince(t *testing.T) {
	t.Parallel()
	base := MirrorStatusEvidence{Status: "x", CreatedAt: 100, UpdatedAt: 1_000_000, Settle: 60_000}
	for _, tc := range []struct {
		name  string
		edit  func(*MirrorStatusEvidence)
		since int64
		final bool
	}{
		{"final, the latest change to the status", func(e *MirrorStatusEvidence) { e.Final, e.LatestTo, e.LatestAt = true, "x", 500 }, 500, true},
		{"final, the latest change elsewhere", func(e *MirrorStatusEvidence) { e.Final, e.LatestTo, e.LatestAt = true, "y", 500 }, 0, true},
		{"final, no change, complete", func(e *MirrorStatusEvidence) { e.Final, e.Complete = true, true }, 100, true},
		{"final, no change, folded opening", func(e *MirrorStatusEvidence) { e.Final = true }, 0, true},
		{"not final, a change within the margin", func(e *MirrorStatusEvidence) { e.LatestTo, e.LatestAt = "x", 950_000 }, 950_000, false},
		{"not final, a change at the margin", func(e *MirrorStatusEvidence) { e.LatestTo, e.LatestAt = "x", 940_000 }, 940_000, false},
		// Monday's stay: the issue may have left and come back unseen.
		{"not final, an older change", func(e *MirrorStatusEvidence) { e.LatestTo, e.LatestAt = "x", 500 }, 0, false},
		{"not final, the latest change elsewhere", func(e *MirrorStatusEvidence) { e.LatestTo, e.LatestAt = "y", 990_000 }, 0, false},
		{"not final, no change, complete", func(e *MirrorStatusEvidence) { e.Complete = true }, 0, false},
	} {
		e := base
		tc.edit(&e)
		if since, final := MirrorStatusSince(e); since != tc.since || final != tc.final {
			t.Errorf("%s: %d, final %v; want %d, %v", tc.name, since, final, tc.since, tc.final)
		}
	}
}

// statusOf is issue's status time, whether it is final for its card, and
// the card's update time.
func statusOf(t *testing.T, s *Store, root int64, issue string) string {
	t.Helper()
	c := mustMirrorCards(t, s, root, issue)[0]
	return fmt.Sprintf("%d %v %d", c.StatusSince, c.StatusFinal, c.UpdatedAt)
}

// A result is written for the version it was read for: a provisional one
// shows at once and never replaces a final one of the same version; a
// final one decides; a card of a later update keeps the time while the
// status stays but not its finality, so the history read for it decides;
// evidence of an earlier version writes nothing. The receipt reports each
// write that moved the time (StatusChanged), the same card or not.
func TestAStatusResultIsWrittenForItsVersion(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 1_000_000, "a")), nil)
	sweep := currentSweep(s, root)
	read := int64(10)
	write := func(updated int64, e MirrorStatusEvidence) MirrorTicketWrite {
		t.Helper()
		read++
		card := webCard("10001", updated)
		e.Status, e.CreatedAt, e.UpdatedAt, e.Settle = card.StatusID, card.CreatedAt, updated, 60_000
		got, err := s.StoreMirrorTickets(root, sweep, []MirrorTicket{{Card: card, Body: []byte(`{}`), Status: &e}}, MirrorCatalog{},
			mirrorReadAt(read), mirrorReadAt(read))
		if err != nil {
			t.Fatal(err)
		}
		return got.Tickets[0]
	}
	if w := write(1_000_000, MirrorStatusEvidence{LatestTo: "1", LatestAt: 990_000}); w.StatusFinal || !w.StatusChanged {
		t.Fatalf("a provisional result is final, or its time is not reported moved: %+v", w)
	}
	if got := statusOf(t, s, root, "10001"); got != "990000 false 1000000" {
		t.Fatalf("a move seen within the margin: %s; want its time at once", got)
	}
	if w := write(1_000_000, MirrorStatusEvidence{LatestTo: "1", LatestAt: 980_000, Final: true}); !w.StatusFinal || !w.StatusChanged || w.CardChanged {
		t.Fatalf("a final result of the same card is not final, or its moved time is not reported: %+v", w)
	}
	if w := write(1_000_000, MirrorStatusEvidence{LatestTo: "1", LatestAt: 980_000, Final: true}); w.StatusChanged {
		t.Fatalf("the same final result again reports a moved time: %+v", w)
	}
	if got := statusOf(t, s, root, "10001"); got != "980000 true 1000000" {
		t.Fatalf("the settle re-read: %s; want it final", got)
	}
	if w := write(1_000_000, MirrorStatusEvidence{LatestTo: "1", LatestAt: 999_000}); w.StatusChanged {
		t.Fatalf("a provisional result over a final one reports a moved time: %+v", w)
	}
	if got := statusOf(t, s, root, "10001"); got != "980000 true 1000000" {
		t.Fatalf("a provisional result over a final one: %s", got)
	}
	// A card of a later update in the same status keeps the time, not final.
	if _, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 2_000_000)}, MirrorCatalog{}, mirrorReadAt(100)); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, root, "10001"); got != "980000 false 2000000" {
		t.Fatalf("a card of a later update: %s", got)
	}
	// The history read for it: away and back, the time ends at the return.
	mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 980_000, To: "1"}, {Kind: MirrorChangeStatus, At: 1_500_000, From: "1", To: "3"},
		{Kind: MirrorChangeStatus, At: 1_900_000, From: "3", To: "1"}}}, MirrorWrite{Changed: true})
	if got := statusOf(t, s, root, "10001"); got != "1900000 true 2000000" {
		t.Fatalf("after the history read for the later update: %s; want the return", got)
	}
	// A ticket read of the earlier version, landing late, writes nothing.
	write(1_000_000, MirrorStatusEvidence{LatestTo: "1", LatestAt: 1, Final: true})
	if got := statusOf(t, s, root, "10001"); got != "1900000 true 2000000" {
		t.Fatalf("evidence of an earlier version: %s", got)
	}
}

// A history read that is not final for its version gives a provisional
// result, bounded by its wait (the settle margin): a change within it
// shows, an older one does not.
func TestAHistoryReadThatIsNotFinalIsProvisional(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 1_000_000, "a"), webRow("10002", 1_000_000, "b")), nil)
	for issue, at := range map[string]int64{"10001": 990_000, "10002": 500} {
		mustReplaceHistory(t, s, root, MirrorHistory{IssueID: issue, Wait: 60_000, Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: at, To: "1"}}}, MirrorWrite{Changed: true})
	}
	if got := statusOf(t, s, root, "10001"); got != "990000 false 1000000" {
		t.Errorf("a change within the margin: %s", got)
	}
	if got := statusOf(t, s, root, "10002"); got != "0 false 1000000" {
		t.Errorf("an older change: %s; want unknown, not Monday's", got)
	}
}

// No writer leaves an issue's body without its bookkeeping or the other
// way round: every mirror writer, the cleanup's sweep and drain, and a
// restore.
func TestEveryWriterKeepsBodiesAndTheirBookkeepingTogether(t *testing.T) {
	t.Parallel()
	for name, write := range mirrorWriters(nil) {
		_ = write
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			seedFullMirror(t, s, root)
			checkBodiesHeld(t, s, "the seed")
			if err := mirrorWriters(s)[name](root); err != nil {
				t.Fatal(err)
			}
			checkBodiesHeld(t, s, name)
			if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			checkBodiesHeld(t, s, name+" and the cleanup")
		})
	}
	t.Run("an unkept issue's sweep", func(t *testing.T) {
		t.Parallel()
		s, _, root := newMirror(t)
		seedFullMirror(t, s, root)
		if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: mirrorReadAt(2)})); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DropMirrorTicket(1, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		checkBodiesHeld(t, s, "the sweep")
		assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_bodies`, "0")
	})
	t.Run("a drain and a restore", func(t *testing.T) {
		t.Parallel()
		s, c, root := newMirror(t)
		seedFullMirror(t, s, root)
		snap := filepath.Join(t.TempDir(), "db.snapshot")
		if err := s.SnapshotTo(snap); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DetachMirror(c.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_bodies`, "0")
		if _, err := s.RestoreFrom(snap); err != nil {
			t.Fatal(err)
		}
		checkBodiesHeld(t, s, "the restore")
	})
}
