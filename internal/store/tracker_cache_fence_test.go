package store

import (
	"fmt"
	"reflect"
	"testing"
)

// rowsPage is a read by ID of board 10's rows begun at readAt: the rows it
// found and the issues it found gone.
func rowsPage(s *Store, root, readAt int64, missing []string, rows ...MirrorRow) MirrorRowPage {
	return currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: rows, Missing: missing, ReadAt: mirrorReadAt(readAt)})
}

func mustWriteRows(t *testing.T, s *Store, page MirrorRowPage, root int64) MirrorChanges {
	t.Helper()
	got, err := s.WriteMirrorRows(root, page)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustReconcile(t *testing.T, s *Store, root int64, listed []string, complete bool, readAt int64) MirrorReconcile {
	t.Helper()
	got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), listed, complete, mirrorReadAt(readAt))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func boardRowIDs(t *testing.T, s *Store, root int64) string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT issue_id FROM tracker_cache_board_issues WHERE root_id = `+boundInt+` AND board_id = '10' ORDER BY issue_id`, root)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(ids)
}

// A read by ID that began before the newest complete listing the board
// reconciled removes no row: the listing may have named it. Its absence is
// owed a read from after the listing (Refused), whichever commits first; a
// read that began in the listing's millisecond, as the reconcile's own do,
// removes, and the refused absence moves no removal time, so the read owed
// adds the page's new rows.
func TestAnAbsenceFromBeforeAListingIsOwedARead(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"listing commits first", "absence commits first"} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", nil)
		mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a")}, ReadAt: mirrorReadAt(5)}, nil)
		absence := rowsPage(s, root, 20, []string{"10001"}, webRow("10005", 5, "e"))
		switch order {
		case "listing commits first":
			if got := mustReconcile(t, s, root, []string{"10001"}, true, 30); !reflect.DeepEqual(got, MirrorReconcile{}) {
				t.Fatalf("%s: listing = %+v", order, got)
			}
			got := mustWriteRows(t, s, absence, root)
			want := MirrorChanges{Superseded: []string{"10001", "10005"}, Refused: []string{"10001", "10005"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: the older absence = %+v; want %+v", order, got, want)
			}
		default:
			got := mustWriteRows(t, s, absence, root)
			if !reflect.DeepEqual(got, MirrorChanges{Cards: []string{"10005"}, Rows: []string{"10005"}, Added: []string{"10005"}, Removed: []string{"10001"}}) {
				t.Fatalf("%s: the absence = %+v", order, got)
			}
			// 10005, which only the read by ID placed, is unsettled.
			want := MirrorReconcile{Missing: []string{"10001"}, Unsettled: []string{"10005"}}
			if got := mustReconcile(t, s, root, []string{"10001"}, true, 30); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: listing = %+v; want %+v", order, got, want)
			}
		}
		assertRows(t, s.db, `SELECT rows_removed_at || ' ' || listing_read_at FROM tracker_cache_boards WHERE board_id = '10'`, "30 30")
		// The read owed, from the listing's millisecond on, is admitted.
		got := mustWriteRows(t, s, rowsPage(s, root, 30, []string{"10001"}, webRow("10005", 5, "e")), root)
		if got.Refused != nil || !reflect.DeepEqual(got.Removed, map[string][]string{"listing commits first": {"10001"}}[order]) {
			t.Fatalf("%s: the read owed = %+v", order, got)
		}
		if ids := boardRowIDs(t, s, root); ids != "[10005]" {
			t.Fatalf("%s: rows %s; want 10005 alone", order, ids)
		}
	}
}

// The ordering time of a card is its latest admitted read, identical or
// not: an identical card from a later read fences a different one of the
// same update time from a read between the two, on a board and off it, for
// the card and its sprints, whichever of the later two commits first. The
// identical read reports no change and writes the card's read time alone.
func TestAnIdenticalCardReadFencesAnOlderOne(t *testing.T) {
	t.Parallel()
	renamed := webRow("10001", 5, "a", "21")
	renamed.Card.Key = "APP-10001"
	moved := webRow("10001", 5, "a", "22")
	for _, entry := range []string{"board", "off board"} {
		for _, order := range []string{"identical commits first", "older commits first"} {
			for name, older := range map[string]MirrorRow{"key": renamed, "sprints": moved} {
				s, _, root := newMirror(t)
				mustReplaceStructure(t, s, root, webStructure(), true)
				linkIssue(t, s, "linked", "10001")
				stored := webRow("10001", 5, "a", "21")
				write := func(readAt int64, row MirrorRow) MirrorChanges {
					t.Helper()
					if entry == "board" {
						return mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(readAt)}, nil)
					}
					got, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{row.Card}, MirrorCatalog{}, mirrorReadAt(readAt))
					if err != nil {
						t.Fatal(err)
					}
					return got
				}
				mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{stored}, ReadAt: mirrorReadAt(10)}, nil)
				what := fmt.Sprintf("%s, %s, %s", entry, order, name)
				identical := func() {
					t.Helper()
					recorder := recordMirrorWrites(t, s)
					revision := scalarRevision(t, s, root)
					got := write(30, stored)
					wantWrites := map[string]int{"tracker_cache_issues": 1}
					if entry == "board" {
						wantWrites["tracker_cache_board_issues"], wantWrites["tracker_cache_sync"] = 1, 1
					}
					if order == "older commits first" {
						// It replaces the older read's card.
						if !reflect.DeepEqual(got.Cards, []string{"10001"}) {
							t.Fatalf("%s: identical later read = %+v; want the card back", what, got)
						}
						return
					}
					if !reflect.DeepEqual(got, MirrorChanges{}) || !reflect.DeepEqual(recorder.rowWrites(), wantWrites) {
						t.Errorf("%s: identical later read = %+v, wrote %v; want nothing reported and %v", what, got, recorder.rowWrites(), wantWrites)
					}
					if after := scalarRevision(t, s, root); after != revision {
						t.Errorf("%s: identical later read moved the revision %d -> %d", what, revision, after)
					}
				}
				if order == "identical commits first" {
					identical()
					if got := write(20, older); !reflect.DeepEqual(got.Superseded, []string{"10001"}) || got.Cards != nil {
						t.Fatalf("%s: the older read = %+v; want it superseded", what, got)
					}
				} else {
					write(20, older)
					identical()
				}
				if cards := mustMirrorCards(t, s, root, "10001"); cards[0].Key != "WEB-10001" || !reflect.DeepEqual(cards[0].Sprints, []string{"21"}) {
					t.Fatalf("%s: card %s in %v; want the latest read's", what, cards[0].Key, cards[0].Sprints)
				}
			}
		}
	}
}

// No index of an issue holds its read time and every trigger on an issue's
// update names its columns, read_at not among them: moving the read time
// alone moves no index entry and fires nothing.
func TestACardsReadTimeIsInNoIndexOrTrigger(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	found, err := queryIDs(s.db, `SELECT name FROM sqlite_master WHERE tbl_name = 'tracker_cache_issues'
		AND (type = 'index' AND sql LIKE '%read_at%'
		  OR type = 'trigger' AND (sql LIKE '%UPDATE ON%' OR sql LIKE '%UPDATE OF%read_at%ON tracker_cache_issues%'))`)
	if err != nil || len(found) > 0 {
		t.Fatalf("indexes and triggers reading an issue's read time: %v, %v", found, err)
	}
}

// A listing's omission is no evidence a row left, whatever its age and
// whether or not a listing named it before: the listing returns it
// unsettled and a read by ID judges it.
func TestARowAListingLeavesOutIsJudgedByID(t *testing.T) {
	t.Parallel()
	const placed, listedAt = 1_000, 62_000
	for _, step := range []string{"by ID", "delta page", "changed delta page", "superseded delta page", "listing", "incomplete listing"} {
		s, _, root := newMirror(t)
		writeMirrorBoard(t, s, root, "10", nil)
		mustWriteRows(t, s, rowsPage(s, root, placed, nil, webRow("10004", 5, "d")), root)
		var final MirrorReconcile
		switch step {
		case "by ID":
			got := mustReconcile(t, s, root, nil, true, listedAt)
			if !reflect.DeepEqual(got, MirrorReconcile{Unsettled: []string{"10004"}}) {
				t.Fatalf("%s: a listing past the margin = %+v; want the row unsettled", step, got)
			}
			if got := mustWriteRows(t, s, rowsPage(s, root, listedAt, []string{"10004"}), root); !reflect.DeepEqual(got.Removed, []string{"10004"}) {
				t.Fatalf("%s: the read by ID = %+v; want the row removed", step, got)
			}
			continue
		case "delta page":
			mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10004", 5, "d")}, ReadAt: mirrorReadAt(placed + 500)}, nil)
		case "changed delta page":
			mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10004", 5, "d1")}, ReadAt: mirrorReadAt(placed + 500)}, nil)
			assertRows(t, s.db, `SELECT order_key FROM tracker_cache_board_issues`, "d1")
		case "superseded delta page":
			// A read by ID that began later stored the row first.
			mustWriteRows(t, s, rowsPage(s, root, placed+900, nil, webRow("10004", 5, "d2")), root)
			mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10004", 5, "d")}, ReadAt: mirrorReadAt(placed + 500)}, nil)
			assertRows(t, s.db, `SELECT order_key FROM tracker_cache_board_issues`, "d2")
		case "listing", "incomplete listing":
			if got := mustReconcile(t, s, root, []string{"10004"}, step == "listing", placed+500); !reflect.DeepEqual(got, MirrorReconcile{}) {
				t.Fatalf("%s: a listing naming it = %+v", step, got)
			}
		}
		final = mustReconcile(t, s, root, nil, true, listedAt+10_000)
		if !reflect.DeepEqual(final, MirrorReconcile{Unsettled: []string{"10004"}}) {
			t.Fatalf("%s: a later listing = %+v; want the row unsettled", step, final)
		}
		assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_issues`, "1")
	}
}

// A read by ID of a row no listing named, against a listing past the
// margin that leaves it out, in either commit order: one that began before
// the listing and found it gone is owed a read after the listing when the
// listing commits first, and removes it when it commits first; one that
// found it held keeps it either way.
func TestARowNoListingNamedFollowsTheReadOrder(t *testing.T) {
	t.Parallel()
	const placed, listedAt = 1_000, 62_000
	for _, found := range []bool{false, true} {
		for _, listingFirst := range []bool{true, false} {
			what := fmt.Sprintf("found %v, listing first %v", found, listingFirst)
			s, _, root := newMirror(t)
			writeMirrorBoard(t, s, root, "10", nil)
			mustWriteRows(t, s, rowsPage(s, root, placed, nil, webRow("10004", 5, "d")), root)
			read := rowsPage(s, root, listedAt-1_000, []string{"10004"})
			if found {
				read = rowsPage(s, root, listedAt-1_000, nil, webRow("10004", 5, "d"))
			}
			var byID MirrorChanges
			var listing MirrorReconcile
			if listingFirst {
				listing = mustReconcile(t, s, root, nil, true, listedAt)
				byID = mustWriteRows(t, s, read, root)
			} else {
				byID = mustWriteRows(t, s, read, root)
				listing = mustReconcile(t, s, root, nil, true, listedAt)
			}
			var wantByID MirrorChanges
			var wantListing MirrorReconcile
			wantRows := "[10004]"
			switch {
			case found && listingFirst:
				wantListing = MirrorReconcile{Unsettled: []string{"10004"}}
			case found:
				wantListing = MirrorReconcile{Unsettled: []string{"10004"}}
			case listingFirst:
				wantListing = MirrorReconcile{Unsettled: []string{"10004"}}
				wantByID = MirrorChanges{Superseded: []string{"10004"}, Refused: []string{"10004"}}
			default:
				wantByID, wantRows = MirrorChanges{Removed: []string{"10004"}}, "[]"
			}
			if !reflect.DeepEqual(byID, wantByID) || !reflect.DeepEqual(listing, wantListing) || boardRowIDs(t, s, root) != wantRows {
				t.Fatalf("%s: by ID %+v, listing %+v, rows %s; want %+v, %+v, %s", what, byID, listing, boardRowIDs(t, s, root), wantByID, wantListing, wantRows)
			}
		}
	}
}

// The statements the fences cost, pinned: a read by ID refused for a
// listing reads its rows once and removes none; a listing costs the same
// statements whatever rows it names, and one naming 1,001 rows writes
// none of them; a row only a read by ID placed, read by ID again
// identical, as a pass reads an unsettled row, moves its row's and its
// card's read time.
func TestMirrorFenceStatements(t *testing.T) {
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 2, "a"))
	lifetime := currentLifetime(s, root, "10")
	var recorder *mirrorWriteRecorder
	count := func(fn func() error) (int, map[string]int) {
		t.Helper()
		recorder = recordMirrorWrites(t, s)
		var err error
		statements := countMirrorStatements(t, func() { err = fn() })
		if err != nil {
			t.Fatal(err)
		}
		return statements, recorder.rowWrites()
	}
	reconcile := func(listed []string, complete bool, readAt int64) func() error {
		return func() error {
			_, err := s.ReconcileMirrorBoard(root, "10", lifetime, listed, complete, mirrorReadAt(readAt))
			return err
		}
	}
	write := func(page MirrorRowPage, check func(MirrorChanges)) func() error {
		return func() error {
			got, err := s.WriteMirrorRows(root, page)
			if check != nil {
				check(got)
			}
			return err
		}
	}
	statements, writes := count(reconcile([]string{"10001", "10002"}, true, 30))
	if statements != 3 || !reflect.DeepEqual(writes, map[string]int{"tracker_cache_boards": 1}) {
		t.Errorf("a listing naming its rows: %d statements, wrote %v; want 3 and the board's listing", statements, writes)
	}
	statements, writes = count(write(rowsPage(s, root, 20, []string{"10001"}), func(got MirrorChanges) {
		if !reflect.DeepEqual(got.Refused, []string{"10001"}) {
			t.Errorf("an absence from before the listing = %+v", got)
		}
	}))
	if statements != 2 || len(writes) > 0 {
		t.Errorf("a refused absence: %d statements, wrote %v; want 2 and nothing", statements, writes)
	}
	placed := mirrorRows(20000, 2*clearMirrorChunk+1, "p")
	for i := 0; i < len(placed); i += mirrorMaxPageRows {
		mustWriteRows(t, s, rowsPage(s, root, 40, nil, placed[i:min(i+mirrorMaxPageRows, len(placed))]...), root)
	}
	ids := make([]string, len(placed))
	for i, row := range placed {
		ids[i] = row.Card.IssueID
	}
	statements, writes = count(reconcile(ids, false, 50))
	if statements != 2 || len(writes) > 0 {
		t.Errorf("an incomplete listing naming %d rows: %d statements, wrote %v; want 2 and nothing", len(placed), statements, writes)
	}
	mustWriteRows(t, s, rowsPage(s, root, 60, nil, webRow("30001", 5, "u")), root)
	statements, writes = count(write(rowsPage(s, root, 70, nil, webRow("30001", 5, "u")), nil))
	if statements != 8 || !reflect.DeepEqual(writes, map[string]int{"tracker_cache_board_issues": 1, "tracker_cache_issues": 1}) {
		t.Errorf("an unsettled row read by ID again: %d statements, wrote %v; want 8, its row's and its card's read", statements, writes)
	}
}
