package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A batch of details stores each document as StoreMirrorDetail would, in
// one transaction, its results in the batch's order: the same document
// moves its read time, a changed one replaces it, a new one needs an
// opening, an unkept issue stores nothing. A batch naming an issue twice,
// or none, is refused whole.
func TestStoreMirrorDetailsStoresEachInOneTransaction(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 4, "a"))
	sweep := currentSweep(s, root)
	got, err := s.StoreMirrorDetails(root, sweep, heldDocs(t, s, root, []MirrorDetailDoc{{IssueID: "10001", Data: []byte(`{"v":1}`)}, {IssueID: "10002", Data: []byte(`{"v":1}`)}}), mirrorReadAt(10), mirrorReadAt(10))
	if err != nil || !reflect.DeepEqual(got, MirrorDetailWrites{Docs: []MirrorDetailWrite{{Stored: true, Changed: true}, {Stored: true, Changed: true}}}) {
		t.Fatalf("opened: %+v, %v", got, err)
	}
	got, err = s.StoreMirrorDetails(root, sweep, heldDocs(t, s, root, []MirrorDetailDoc{
		{IssueID: "10001", Data: []byte(`{"v":1}`)}, {IssueID: "10002", Data: []byte(`{"v":2}`)}, {IssueID: "10003", Data: []byte(`{"v":1}`)}, {IssueID: "10009", Data: []byte(`{"v":1}`)},
	}), mirrorReadAt(0), mirrorReadAt(20))
	want := MirrorDetailWrites{Docs: []MirrorDetailWrite{{Stored: true}, {Stored: true, Changed: true}, {}, {}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("a round's write: %+v, %v; want %+v", got, err, want)
	}
	assertRows(t, s.db, `SELECT group_concat(issue_id || ' ' || opened_at || ' ' || read_at || ' ' || (data ->> 'v'), ', ') FROM
		(SELECT * FROM tracker_cache_issue_details ORDER BY issue_id)`, "10001 10 20 1, 10002 10 20 2")
	// Each refused batch is otherwise valid, so it fails for the reason
	// named.
	for name, refused := range map[string]struct {
		docs []MirrorDetailDoc
		why  string
	}{
		"none":           {nil, "needs documents"},
		"an issue twice": {heldDocs(t, s, root, []MirrorDetailDoc{{IssueID: "10001"}, {IssueID: "10003"}, {IssueID: "10001"}}), "each of its issues once"},
		"no issue":       {[]MirrorDetailDoc{{IssueID: "", Data: []byte(`{}`), CardUpdatedAt: 5}}, "each of its issues once"},
		"no card update": {[]MirrorDetailDoc{{IssueID: "10001", Data: []byte(`{}`)}}, "names no card update time"},
	} {
		if _, err := s.StoreMirrorDetails(root, sweep, refused.docs, mirrorReadAt(30), mirrorReadAt(30)); !errors.Is(err, ErrInvalidTrackerCache) || !strings.Contains(err.Error(), refused.why) {
			t.Errorf("%s: %v; want %q", name, err, refused.why)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, "2")
}

// The states of stored details are what GetMirrorDetail reads of each,
// without the documents, in one statement: none for an issue with no
// detail, no document or no longer kept. A lookup over mirrorMaxLookup
// issues is refused.
func TestListMirrorDetailStates(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 4, "a"))
	sweep := currentSweep(s, root)
	if _, err := s.StoreMirrorDetails(root, sweep, heldDocs(t, s, root, []MirrorDetailDoc{{IssueID: "10001", Data: []byte(`{"v":1}`)}, {IssueID: "10002", Data: []byte(`{"v":1}`)}}), mirrorReadAt(10), mirrorReadAt(10)); err != nil {
		t.Fatal(err)
	}
	// An opening whose read began earlier holds no document.
	if _, err := storeDetail(t, s, root, sweep, "10003", 30, 20, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: mirrorReadAt(40)})); err != nil {
		t.Fatal(err)
	}
	var got []MirrorDetail
	statements := countMirrorStatements(t, func() {
		var err error
		if got, err = s.ListMirrorDetailStates(root, []string{"10001", "10002", "10003", "10004", "10009"}); err != nil {
			t.Fatal(err)
		}
	})
	full, ok, err := s.GetMirrorDetail(root, "10001")
	if err != nil || !ok {
		t.Fatalf("detail: %v, %v", ok, err)
	}
	full.Data = nil
	if statements != 1 || !reflect.DeepEqual(got, []MirrorDetail{full}) {
		t.Fatalf("%d statements: %+v; want %+v", statements, got, full)
	}
	if confirmed, err := s.ConfirmMirrorDetail(root, got[0], mirrorReadAt(50)); err != nil || !confirmed.Stored {
		t.Fatalf("a state confirms as the detail: %+v, %v", confirmed, err)
	}
	ids := make([]string, mirrorMaxLookup+1)
	for i := range ids {
		ids[i] = fmt.Sprint(20000 + i)
	}
	if _, err := s.ListMirrorDetailStates(root, ids); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a lookup over the bound: %v", err)
	}
	if got, err := s.ListMirrorDetailStates(root, slices.Clone(ids[:mirrorMaxLookup])); err != nil || len(got) != 0 {
		t.Fatalf("a lookup at the bound: %v, %v", got, err)
	}
}

// A batch's outcomes are of its committed state: the cap is applied once,
// after every document is written, so a document a later opening of the
// same batch drops is not Stored, and Dropped names the details held
// before the batch that it dropped, a held document of the batch
// included, and never a new one, which was never held. The eviction is
// the batch's only statement beyond the root's read and each document's
// read and write.
func TestMirrorDetailBatchReportsTheCommittedState(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, mirrorDetailCap+3, "a"))
	sweep := currentSweep(s, root)
	for i := range mirrorDetailCap - 1 {
		if _, err := storeDetail(t, s, root, sweep, fmt.Sprint(10004+i), 100, 100, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	batch := func(what string, openedAt int64, want MirrorDetailWrites, issues ...string) {
		t.Helper()
		docs := make([]MirrorDetailDoc, len(issues))
		for i, issue := range issues {
			docs[i] = heldDoc(t, s, root, issue, []byte(`{}`))
		}
		var got MirrorDetailWrites
		var err error
		statements := countMirrorStatements(t, func() {
			got, err = s.StoreMirrorDetails(root, sweep, docs, mirrorReadAt(openedAt), mirrorReadAt(openedAt))
		})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %+v, %v; want %+v", what, got, err, want)
		}
		// The sweep check, the documents' states, a write each and the
		// eviction.
		if want := 2 + len(issues) + 1; statements != want {
			t.Fatalf("%s ran %d statements; want %d", what, statements, want)
		}
		for i, issue := range issues {
			if _, held, err := s.GetMirrorDetail(root, issue); err != nil || held != got.Docs[i].Stored {
				t.Fatalf("%s: issue %s held %v (%v), its outcome %+v", what, issue, held, err, got.Docs[i])
			}
		}
	}
	// 199 held at 100; two new ones opened at 50: the second drops the
	// first, which the batch itself started.
	batch("two openings older than every detail", 50,
		MirrorDetailWrites{Docs: []MirrorDetailWrite{{}, {Stored: true, Changed: true}}}, "10001", "10002")
	// 10002 opened again at 60 with 10003 new at 60: 10002, held before
	// the batch, ties with 10003 at the cap's edge and goes.
	batch("a held document at the cap's edge", 60,
		MirrorDetailWrites{Docs: []MirrorDetailWrite{{}, {Stored: true, Changed: true}}, Dropped: []string{"10002"}}, "10002", "10003")
	// Two openings later than every detail drop the two held longest.
	batch("two openings later than every detail", 1000, MirrorDetailWrites{
		Docs:    []MirrorDetailWrite{{Stored: true, Changed: true}, {Stored: true, Changed: true}},
		Dropped: []string{"10003", "10004"},
	}, "10001", "10002")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))
}

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
	got, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{heldDoc(t, s, root, issue, []byte(`{}`))}, mirrorReadAt(openedAt), mirrorReadAt(readAt))
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
	got, err := storeDetail(t, s, root, currentSweep(s, root), "10001", 100, 90, []byte(`{"v":0}`))
	mustDetailWrite(t, "first opening, its read earlier", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("a detail with no document reads: %v, %v", ok, err)
	}
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 50, 40, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier opening", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	if at := opened(); at != "100 100" {
		t.Fatalf("after an earlier opening: %s", at)
	}
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 0, 180, []byte(`{"v":1}`))
	mustDetailWrite(t, "document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":0}`))
	mustDetailWrite(t, "earlier document", got, err, MirrorDetailWrite{Stored: true, Superseded: true})
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 0, 170, []byte(`{"v":1}`))
	mustDetailWrite(t, "identical earlier document", got, err, MirrorDetailWrite{Stored: true})
	if d, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || !ok || !reflect.DeepEqual(d, MirrorDetail{IssueID: "10001", OpenedAt: 100, ReadAt: 180,
		Data: []byte(`{"v":1}`), CardUpdatedAt: 5, row: d.row, sweep: currentSweep(s, root)}) || d.row <= 0 {
		t.Fatalf("detail = %+v, %v, %v", d, ok, err)
	}
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 0, 190, []byte(`{"v":2}`))
	mustDetailWrite(t, "later document", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d, _, err := s.GetMirrorDetail(root, "10001")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ConfirmMirrorDetail(root, d, mirrorReadAt(200))
	mustDetailWrite(t, "later opening", got, err, MirrorDetailWrite{Stored: true})
	if at := opened(); at != "200 190" {
		t.Fatalf("after a later opening: %s", at)
	}

	got, err = storeDetail(t, s, root, currentSweep(s, root), "10009", 300, 300, []byte(`{}`))
	mustDetailWrite(t, "opening of an unkept issue", got, err, MirrorDetailWrite{})
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10009", 0, 300, []byte(`{}`))
	mustDetailWrite(t, "document of an unkept issue", got, err, MirrorDetailWrite{})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetMirrorDetail(root, "10001"); err != nil || ok {
		t.Fatalf("detail of an unkept issue reads: %v, %v", ok, err)
	}
	for name, err := range map[string]error{
		"no documents": func() error {
			_, err := s.StoreMirrorDetails(root, currentSweep(s, root), nil, mirrorReadAt(0), mirrorReadAt(1))
			return err
		}(),
		"an issue twice": func() error {
			_, err := s.StoreMirrorDetails(root, currentSweep(s, root), []MirrorDetailDoc{{IssueID: "10001", CardUpdatedAt: 5}, {IssueID: "10001", CardUpdatedAt: 5}}, mirrorReadAt(0), mirrorReadAt(1))
			return err
		}(),
		"document with no time": func() error {
			_, err := storeDetail(t, s, root, currentSweep(s, root), "10001", 0, 0, nil)
			return err
		}(),
		"document with no issue": func() error { _, err := storeDetail(t, s, root, currentSweep(s, root), "", 0, 1, nil); return err }(),
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
		got, err := storeDetail(t, s, root, currentSweep(s, root), fmt.Sprint(10000+i), int64(100+i), int64(100+i), []byte(`{}`))
		mustDetailWrite(t, fmt.Sprint("opening ", i), got, err, MirrorDetailWrite{Stored: true, Changed: true})
	}
	oldest, _, err := s.GetMirrorDetail(root, "10000")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConfirmMirrorDetail(root, oldest, mirrorReadAt(1000))
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
		got, err := s.ConfirmMirrorDetail(root, d, mirrorReadAt(openedAt))
		mustDetailWrite(t, what, got, err, want)
	}
	got, err := storeDetail(t, s, root, currentSweep(s, root), "10000", 100, 100, []byte(`{"v":1}`))
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
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10000", 0, 200, []byte(`{"v":3}`))
	mustDetailWrite(t, "a later read", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	confirm("a replaced document", d, 300, MirrorDetailWrite{})
	if at := state("10000"); at != "150 200 3" {
		t.Fatalf("after a refused confirmation: %s", at)
	}

	// The cap dropped the detail: refused, and no row comes back to drop
	// another.
	d = read("10000")
	for i := range mirrorDetailCap {
		if _, err := storeDetail(t, s, root, currentSweep(s, root), fmt.Sprint(10001+i), int64(1000+i), int64(1000+i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	confirm("a dropped detail", d, 5000, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details WHERE issue_id = '10000'`, "0")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issue_details`, fmt.Sprint(mirrorDetailCap))

	// An issue no longer kept: refused.
	got, err = storeDetail(t, s, root, currentSweep(s, root), "10001", 6000, 6000, []byte(`{"v":4}`))
	mustDetailWrite(t, "a read of another issue", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	d = read("10001")
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	confirm("an unkept issue", d, 7000, MirrorDetailWrite{})
	if at := state("10001"); at != "6000 6000 4" {
		t.Fatalf("after an unkept issue's confirmation: %s", at)
	}

	for name, d := range map[string]MirrorDetail{"no detail read": {}, "a detail built by hand": {IssueID: "10002", ReadAt: 1}} {
		if _, err := s.ConfirmMirrorDetail(root, d, mirrorReadAt(1)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ConfirmMirrorDetail(root, d, mirrorReadAt(0)); !errors.Is(err, ErrInvalidTrackerCache) {
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
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := storeDetail(t, s, root, currentSweep(s, root), "10001", 100, 100, []byte(`{"v":1}`)); err != nil {
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
	got, err := s.ConfirmMirrorDetail(root, before, mirrorReadAt(200))
	mustDetailWrite(t, "a document from before the sweep", got, err, MirrorDetailWrite{})
	assertRows(t, s.db, `SELECT opened_at || ' ' || (data ->> 'v') FROM tracker_cache_issue_details WHERE issue_id = '10001'`, "100 1")
	got, err = s.ConfirmMirrorDetail(root, after, mirrorReadAt(200))
	mustDetailWrite(t, "the document after it", got, err, MirrorDetailWrite{Stored: true})
}

// A document writes no card and is fenced by its card's update time: one
// of an issue the mirror holds no card of (a linked issue's first
// document), or of a later update than the mirror's card, is owed its card
// and writes nothing; once a cards read stores that card the same
// document is stored. One of an earlier update than the mirror's card is
// stale: it is not stored, and a person's opening of it is recorded, on a
// held detail or as a detail with no document that the next read fills.
// The fence compares update times alone: the owed card's read began after
// the document's, which the card rule's tie would refuse.
func TestADocumentIsFencedByItsCardsUpdate(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	sweep := currentSweep(s, root)
	linkIssue(t, s, "linked", "10001")
	doc := func(issue string, cardAt int64, v int) MirrorDetailDoc {
		return MirrorDetailDoc{IssueID: issue, Data: fmt.Appendf(nil, `{"v":%d}`, v), CardUpdatedAt: cardAt}
	}
	detail := func(issue string) string {
		t.Helper()
		var got string
		err := s.db.QueryRow(`SELECT coalesce((SELECT opened_at || ' ' || read_at || ' ' || coalesce(data ->> 'v', '-')
			FROM tracker_cache_issue_details WHERE issue_id = ?), 'none')`, issue).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got, err := s.StoreMirrorDetail(root, sweep, doc("10001", 5, 1), mirrorReadAt(10), mirrorReadAt(10))
	mustDetailWrite(t, "a linked issue's first document, no card", got, err, MirrorDetailWrite{CardOwed: true})
	if d := detail("10001"); d != "none" {
		t.Fatalf("an owed document wrote %s", d)
	}
	if _, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 5)}, MirrorCatalog{}, mirrorReadAt(11)); err != nil {
		t.Fatal(err)
	}
	got, err = s.StoreMirrorDetail(root, sweep, doc("10001", 5, 1), mirrorReadAt(10), mirrorReadAt(10))
	mustDetailWrite(t, "the same document once its card is stored", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	got, err = s.StoreMirrorDetail(root, sweep, doc("10001", 6, 2), mirrorReadAt(0), mirrorReadAt(20))
	mustDetailWrite(t, "a document ahead of the card", got, err, MirrorDetailWrite{CardOwed: true})
	if d := detail("10001"); d != "10 10 1" {
		t.Fatalf("an owed document wrote %s", d)
	}

	// A board read stores a later card while a detail read of the earlier
	// one is in flight.
	if _, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10001", 7)}, MirrorCatalog{}, mirrorReadAt(21)); err != nil {
		t.Fatal(err)
	}
	got, err = s.StoreMirrorDetail(root, sweep, doc("10001", 6, 2), mirrorReadAt(30), mirrorReadAt(20))
	mustDetailWrite(t, "a stale document, opened", got, err, MirrorDetailWrite{})
	if d := detail("10001"); d != "30 10 1" {
		t.Fatalf("a stale opened document left %s; want its opening alone", d)
	}
	got, err = s.StoreMirrorDetail(root, sweep, doc("10001", 7, 3), mirrorReadAt(0), mirrorReadAt(40))
	mustDetailWrite(t, "the card's document", got, err, MirrorDetailWrite{Stored: true, Changed: true})

	linkIssue(t, s, "linked again", "10002")
	if _, err := s.UpsertMirrorCards(root, sweep, []MirrorCard{webCard("10002", 9)}, MirrorCatalog{}, mirrorReadAt(50)); err != nil {
		t.Fatal(err)
	}
	got, err = s.StoreMirrorDetail(root, sweep, doc("10002", 8, 1), mirrorReadAt(60), mirrorReadAt(60))
	mustDetailWrite(t, "a stale first opening", got, err, MirrorDetailWrite{})
	if d := detail("10002"); d != "60 60 -" {
		t.Fatalf("a stale first opening left %s; want a detail with no document", d)
	}
	got, err = s.StoreMirrorDetail(root, sweep, doc("10002", 9, 2), mirrorReadAt(0), mirrorReadAt(70))
	mustDetailWrite(t, "the next read, no opening", got, err, MirrorDetailWrite{Stored: true, Changed: true})
	if d := detail("10002"); d != "60 70 2" {
		t.Fatalf("the next read left %s", d)
	}
}
