package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func mustDetailWrite(t *testing.T, what string, got MirrorDetailWrite, err error, want MirrorDetailWrite) {
	t.Helper()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %+v, %v; want %+v", what, got, err, want)
	}
}

// mustDetailStore stores one document of issue (StoreMirrorDetails) and
// requires its outcome and the details the write dropped.
func mustDetailStore(t *testing.T, s *Store, root int64, what, issue string, openedAt, readAt int64, want MirrorDetailWrite, dropped ...string) {
	t.Helper()
	got, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{IssueID: issue, Data: []byte(`{}`)}}, openedAt, readAt)
	if wantAll := (MirrorDetailWrites{Docs: []MirrorDetailWrite{want}, Dropped: dropped}); err != nil || !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("%s = %+v, %v; want %+v", what, got, err, wantAll)
	}
}

// An opening only moves the open time forward, and the first is the
// detail's read watermark: an opening whose read began earlier stores no
// document. A document is stored with the time its read began, unless the
// stored one came from a read that began later, and the time it was opened
// stays. A detail with no document reads as absent, as does that of an
// issue the mirror stops keeping.
func TestMirrorDetailFollowsItsReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	opened := func() string {
		var at string
		if err := s.db.QueryRow(`SELECT opened_at || ' ' || read_at FROM tracker_cache_issue_details`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	got, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 100, 90, []byte(`{"v":0}`))
	mustDetailWrite(t, "first opening, its read earlier", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("a detail with no document reads: %v, %v", ok, err)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 50, 40, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier opening", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if at := opened(); at != "100 100" {
		t.Fatalf("after an earlier opening: %s", at)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 180, []byte(`{"v":1}`))
	mustDetailWrite(t, "document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier document", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":1}`))
	mustDetailWrite(t, "identical earlier document", got, err, MirrorDetailWrite{Stored: true})
	if d, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || !ok || !reflect.DeepEqual(d, MirrorDetail{IssueID: "10001", OpenedAt: 100, ReadAt: 180,
		Data: []byte(`{"v":1}`), CardUpdatedAt: 5, row: d.row, sweep: currentSweep(s, root)}) || d.row <= 0 {
		t.Fatalf("detail = %+v, %v, %v", d, ok, err)
	}
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 190, []byte(`{"v":2}`))
	mustDetailWrite(t, "later document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d, _, err := s.GetMirrorDetail(root, "10001")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ConfirmMirrorDetail(root, d, 200)
	mustDetailWrite(t, "later opening", got, err, MirrorDetailWrite{Stored: true})
	if at := opened(); at != "200 190" {
		t.Fatalf("after a later opening: %s", at)
	}

	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10009", 300, 300, []byte(`{}`))
	mustDetailWrite(t, "opening of an unkept issue", got, err, MirrorDetailWrite{})
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10009", 0, 300, []byte(`{}`))
	mustDetailWrite(t, "document of an unkept issue", got, err, MirrorDetailWrite{})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("detail of an unkept issue reads: %v, %v", ok, err)
	}
	for name, err := range map[string]error{
		"no documents": func() error { _, err := s.StoreMirrorDetails(root, currentSweep(s, root), nil, 0, 1); return err }(),
		"an issue twice": func() error {
			_, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{IssueID: "10001"}, {IssueID: "10001"}}, 0, 1)
			return err
		}(),
		"document with no time": func() error {
			_, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 0, 0, nil)
			return err
		}(),
		"document with no issue": func() error { _, err := s.StoreMirrorDetail(root, currentSweep(s, root), "", 0, 1, nil); return err }(),
	} {
		if !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The root keeps the mirrorDetailCap most recently opened details; a new
// one that is itself the least recently opened is dropped at once and
// reported as not stored, and not as dropped: it was never held.
func TestMirrorDetailKeepsTheMostRecentlyOpened(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorDetailCap+2, "a"))
	for i := range mirrorDetailCap {
		got, err := s.StoreMirrorDetail(root, currentSweep(s, root), fmt.Sprint(10000+i), int64(100+i), int64(100+i), []byte(`{}`))
		mustDetailWrite(t, fmt.Sprint("opening ", i), got, err, MirrorDetailWrite{Stored: true, Changed: true})
	}
	oldest, _, err := s.GetMirrorDetail(root, "10000")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmMirrorDetail(root, oldest, 1000)
	mustDetailWrite(t, "reopening the oldest", got, err, MirrorDetailWrite{Stored: true})
	mustDetailStore(t, s, root, "one over the cap", "10200", 1001, 1001, MirrorDetailWrite{Stored: true, Changed: true}, "10001")
	mustDetailStore(t, s, root, "a new detail older than all", "10201", 50, 50, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id IN ('10001', '10201')`, "0")
}

// A confirmation marks a detail opened, and changes no document, only
// while the mirror holds the document read: not once a later read
// replaced it, the cap dropped it, a sweep ran or its issue is no longer
// kept. Refused, it writes nothing, so it neither adds a detail with no
// document nor drops another.
func TestConfirmMirrorDetail(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10000, mirrorDetailCap+2, "a"))
	state := func(issue string) string {
		t.Helper()
		var at string
		if err := s.db.QueryRow(`SELECT opened_at || ' ' || read_at || ' ' || coalesce(data ->> 'v', '-') FROM tracker_cache_issue_details
			WHERE root_id = ? AND issue_id = ?`, root, issue).Scan(&at); err != nil {
			return err.Error()
		}
		return at
	}
	read := func(issue string) MirrorDetail {
		t.Helper()
		d, ok, err := s.GetMirrorDetail(root, issue)
		if err != nil || !ok {
			t.Fatalf("detail of %s: %v, %v", issue, ok, err)
		}
		return d
	}
	confirm := func(what string, d MirrorDetail, openedAt int64, want MirrorDetailWrite) {
		t.Helper()
		got, err := s.ConfirmMirrorDetail(root, d, openedAt)
		mustDetailWrite(t, what, got, err, want)
	}
	got, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10000", 100, 100, []byte(`{"v":1}`))
	mustDetailWrite(t, "first read", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d := read("10000")
	if d.CardUpdatedAt != 5 {
		t.Fatalf("the card's update time read with the detail: %d", d.CardUpdatedAt)
	}
	confirm("an opening", d, 150, MirrorDetailWrite{Stored: true})
	confirm("an earlier opening", d, 120, MirrorDetailWrite{Stored: true})
	if at := state("10000"); at != "150 100 1" {
		t.Fatalf("after the openings: %s", at)
	}

	// A later read replaced the document: refused, nothing written.
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10000", 0, 200, []byte(`{"v":3}`))
	mustDetailWrite(t, "a later read", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	confirm("a replaced document", d, 300, MirrorDetailWrite{})
	if at := state("10000"); at != "150 200 3" {
		t.Fatalf("after a refused confirmation: %s", at)
	}

	// The cap dropped the detail: refused, and no row comes back to drop
	// another.
	d = read("10000")
	for i := range mirrorDetailCap {
		if _, err := s.StoreMirrorDetail(root, currentSweep(s, root), fmt.Sprint(10001+i), int64(1000+i), int64(1000+i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	confirm("a dropped detail", d, 5000, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))

	// An issue no longer kept: refused.
	got, err = s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 6000, 6000, []byte(`{"v":4}`))
	mustDetailWrite(t, "a read of another issue", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d = read("10001")
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	confirm("an unkept issue", d, 7000, MirrorDetailWrite{})
	if at := state("10001"); at != "6000 6000 4" {
		t.Fatalf("after an unkept issue's confirmation: %s", at)
	}

	for name, d := range map[string]MirrorDetail{"no detail read": {}, "a detail built by hand": {IssueID: "10002", ReadAt: 1}} {
		if _, err := s.ConfirmMirrorDetail(root, d, 1); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ConfirmMirrorDetail(root, d, 0); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Errorf("no opening time: %v", err)
	}
}

// A detail swept and stored again in its issue's next lifetime, in the
// same row with a read of the same time, is not the document read before
// the sweep: its confirmation is refused.
func TestConfirmMirrorDetailRefusesADocumentFromBeforeASweep(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	keep := func(thread string) {
		t.Helper()
		linkIssue(t, s, thread, "10001")
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StoreMirrorDetail(root, currentSweep(s, root), "10001", 100, 100, []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	keep("linked")
	before, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok {
		t.Fatalf("detail: %v, %v", ok, err)
	}
	if _, err := s.ClearThreadTicket("linked"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	keep("relinked")
	after, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok || after.row != before.row || after.ReadAt != before.ReadAt || after.sweep == before.sweep {
		t.Fatalf("the detail stored again: row %d (was %d), read %d, sweep %d (was %d), %v, %v",
			after.row, before.row, after.ReadAt, after.sweep, before.sweep, ok, err)
	}
	got, err := s.ConfirmMirrorDetail(root, before, 200)
	mustDetailWrite(t, "a document from before the sweep", got, err, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT opened_at || ' ' || (data ->> 'v') FROM tracker_cache_issue_details WHERE issue_id = '10001'`, "100 1")
	got, err = s.ConfirmMirrorDetail(root, after, 200)
	mustDetailWrite(t, "the document after it", got, err, MirrorDetailWrite{Stored: true})
}
