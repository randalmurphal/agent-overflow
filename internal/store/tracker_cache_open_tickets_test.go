package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"testing"
)

// An open ticket keeps its issue in its root's mirror (mirrorIssueKept):
// a card of an issue on no board and no link is stored only while a
// ticket keeps it, every read serves it, and the cleanup removes it once
// the last keep goes. A keep of another root keeps nothing here; an issue
// a board also keeps is untouched by the drop; a pump keeps one issue; and
// a keep reports that it shows a record again only when the record was
// held and nothing kept it.
func TestAnOpenTicketKeepsItsIssue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	other := mustOtherRoot(t, s)
	write := func(ids ...string) MirrorChanges {
		t.Helper()
		var cards []MirrorCard
		for _, id := range ids {
			cards = append(cards, webCard(id, 7))
		}
		changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), cards, MirrorCatalog{}, mirrorReadAt(1))
		if err != nil {
			t.Fatal(err)
		}
		return changes
	}
	if ch := write("20001"); !reflect.DeepEqual(ch.Unkept, []string{"20001"}) {
		t.Fatalf("a card no ticket keeps was stored: %+v", ch)
	}
	if _, err := s.KeepMirrorTicket(other, "20001", 1, 9, mirrorReadOf(other, 5)); err != nil {
		t.Fatal(err)
	}
	if ch := write("20001"); !reflect.DeepEqual(ch.Unkept, []string{"20001"}) {
		t.Fatalf("a ticket of another root kept the card: %+v", ch)
	}
	// No record of 20001 is held, and the board keeps 10001: neither keep
	// shows a record again.
	for pump, id := range map[int64]string{1: "20001", 2: "10001"} {
		if shown, err := s.KeepMirrorTicket(root, id, 1, pump, mirrorReadOf(root, 5)); err != nil || shown {
			t.Fatalf("the keep of %s: shown %v, %v; want false", id, shown, err)
		}
	}
	if ch := write("20001"); len(ch.Unkept) != 0 {
		t.Fatalf("the open ticket's card was not stored: %+v", ch)
	}
	if cards := mustMirrorCards(t, s, root, "20001"); len(cards) != 1 {
		t.Fatalf("the open ticket's card is not served: %+v", cards)
	}
	// A pump keeps one issue: a keep of another fails and keeps 20001.
	if _, err := s.KeepMirrorTicket(root, "20002", 1, 1, mirrorReadOf(root, 5)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a pump's keep of a second issue: %v", err)
	}
	if shown, err := s.KeepMirrorTicket(root, "20001", 1, 1, mirrorReadOf(root, 5)); err != nil || shown {
		t.Fatalf("the same keep again: shown %v, %v", shown, err)
	}
	if cards := mustMirrorCards(t, s, root, "20001"); len(cards) != 1 {
		t.Fatalf("the open ticket's card is not served after a refused keep: %+v", cards)
	}
	// The drop of 20001's ticket leaves it unkept; 10001's board keeps it.
	for pump, wantUnkept := range map[int64]bool{1: true, 2: false} {
		if unkept, err := s.DropMirrorTicket(1, pump); err != nil || unkept != wantUnkept {
			t.Fatalf("the drop of pump %d: unkept %v, %v; want %v", pump, unkept, err, wantUnkept)
		}
	}
	// Opened again before the cleanup, 20001's held record is shown again,
	// once, and a keep of another root shows nothing here.
	if shown, err := s.KeepMirrorTicket(other, "20001", 1, 8, mirrorReadOf(other, 5)); err != nil || shown {
		t.Fatalf("a keep of another root: shown %v, %v", shown, err)
	}
	if shown, err := s.KeepMirrorTicket(root, "20001", 1, 3, mirrorReadOf(root, 5)); err != nil || !shown {
		t.Fatalf("the reopening before the cleanup: shown %v, %v; want true", shown, err)
	}
	if cards := mustMirrorCards(t, s, root, "20001"); len(cards) != 1 {
		t.Fatalf("the reopened ticket's card is not served: %+v", cards)
	}
	if shown, err := s.KeepMirrorTicket(root, "20001", 1, 4, mirrorReadOf(root, 5)); err != nil || shown {
		t.Fatalf("a second keep of a kept issue: shown %v, %v; want false", shown, err)
	}
	for _, pump := range []int64{3, 4} {
		if unkept, err := s.DropMirrorTicket(1, pump); err != nil || unkept != (pump == 4) {
			t.Fatalf("the drop of pump %d: unkept %v, %v", pump, unkept, err)
		}
	}
	if unkept, err := s.DropMirrorTicket(1, 1); err != nil || unkept {
		t.Fatalf("a drop of no row: unkept %v, %v", unkept, err)
	}
	if cards := mustMirrorCards(t, s, root, "20001", "10001"); len(cards) != 1 || cards[0].IssueID != "10001" {
		t.Fatalf("after the drops the mirror serves %+v; want the board's 10001 alone", cards)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT issue_id FROM tracker_cache_issues WHERE root_id = %d`, root), "10001")
	if _, err := s.KeepMirrorTicket(root, "", 1, 3, mirrorReadOf(root, 5)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a keep of no issue: %v", err)
	}
}

// mustOtherRoot is the mirror root of a second Jira connection.
func mustOtherRoot(t *testing.T, s *Store) int64 {
	t.Helper()
	conn := mustCreateTrackerConnection(t, s, jiraConnection("https://other.example.com", "user1@example.com", 11))
	root, _, err := s.GetOrCreateMirrorRoot(conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The boot's drop of the rows earlier processes left deletes every row
// but the live session's and says how many.
func TestTheBootDropsTheOpenTicketsOfOtherSessions(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	for _, keep := range []struct{ session, pump int64 }{{1, 1}, {1, 2}, {2, 1}, {3, 1}} {
		if _, err := s.KeepMirrorTicket(root, fmt.Sprint(20000+keep.session*10+keep.pump), keep.session, keep.pump, mirrorReadOf(root, 5)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DropMirrorTicketsOfOtherSessions(1); err != nil || n != 2 {
		t.Fatalf("the drop deleted %d rows, %v; want the other sessions' 2", n, err)
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT issue_id FROM tracker_cache_open_tickets WHERE root_id = %d AND session = 1 AND pump >= 1 ORDER BY pump`, root), "20011,20012")
}

// Opening a ticket costs two statements, the keep rule's probe of its
// issue and the keep, and one row; the same keep again three statements
// and no row; closing two, the drop and the probe, and one row; and the
// boot's drop one statement and a row a ticket an earlier process left
// open.
func TestOpenTicketKeepCosts(t *testing.T) {
	s, _, root := newMirror(t)
	recorder := recordMirrorWrites(t, s)
	open := map[string]int{"tracker_cache_open_tickets": 1}
	for _, op := range []struct {
		name       string
		run        func() error
		statements int
		writes     map[string]int
	}{
		{"open", func() error {
			_, err := s.KeepMirrorTicket(root, "20001", 1, 1, mirrorReadOf(root, 5))
			return err
		}, 2, open},
		{"the same keep", func() error {
			_, err := s.KeepMirrorTicket(root, "20001", 1, 1, mirrorReadOf(root, 5))
			return err
		}, 3, map[string]int{}},
		{"close", func() error { _, err := s.DropMirrorTicket(1, 1); return err }, 2, open},
		{"an earlier process's row", func() error {
			_, err := s.KeepMirrorTicket(root, "20003", 2, 1, mirrorReadOf(root, 5))
			return err
		}, 2, open},
		{"the boot's drop", func() error { _, err := s.DropMirrorTicketsOfOtherSessions(1); return err }, 1, open},
	} {
		recorder.reset()
		var err error
		statements := countMirrorStatements(t, func() { err = op.run() })
		if err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		writes := recorder.rowWrites()
		t.Logf("%s: %d statements, rows written %v", op.name, statements, writes)
		if statements != op.statements || !maps.Equal(writes, op.writes) {
			t.Errorf("%s: %d statements, rows written %v; pinned %d and %v", op.name, statements, writes, op.statements, op.writes)
		}
	}
}
