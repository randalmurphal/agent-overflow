package store

import (
	"errors"
	"reflect"
	"testing"
)

func mustMirrorCursor(t *testing.T, s *Store, root int64, boardID string) []byte {
	t.Helper()
	st, ok, err := s.GetMirrorSync(root, boardID, testNow)
	if err != nil || !ok {
		t.Fatalf("sync of board %s: %v, %v", boardID, ok, err)
	}
	return st.Cursor
}

func mustOrderKey(t *testing.T, s *Store, board, issue string) string {
	t.Helper()
	var key string
	if err := s.db.QueryRow(`SELECT order_key FROM tracker_cache_board_issues WHERE board_id = ? AND issue_id = ?`, board, issue).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

// A page stores its cards, rows, sprints, catalog and cursor and reports
// what changed; the same page again reports nothing, and each part that
// differs reports on its own.
func TestWriteMirrorRowPageReportsChanges(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	three := 3.0
	second := webRow("10002", 5, "b")
	second.Estimate = &three
	page := webPage("10", webRow("10001", 5, "a", "21", "22"), second)
	page.Catalog = MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: "User 2", Data: []byte(`{}`)}}}
	got := mustWriteRowPage(t, s, root, page, []byte("c1"))
	assertMirrorChanges(t, "first page", got, MirrorChanges{Cards: []string{"10001", "10002"}, Rows: []string{"10001", "10002"},
		Added: []string{"10001", "10002"}, Catalog: true})
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q", cursor)
	}
	first := webCard("10001", 5, "21", "22")
	if cards := mustMirrorCards(t, s, root, "10002", "10001", "10009"); !reflect.DeepEqual(cards, []MirrorCard{first, second.Card}) {
		t.Fatalf("cards = %+v", cards)
	}
	rows, more, err := s.ListMirrorIssueRows(root, "10002", "")
	if err != nil || more || !reflect.DeepEqual(rows, []MirrorIssueRow{{BoardID: "10", OrderKey: "b", Estimate: &three}}) {
		t.Fatalf("rows of 10002 = %+v, %v, %v", rows, more, err)
	}

	assertMirrorChanges(t, "same page", mustWriteRowPage(t, s, root, page, []byte("c1")), MirrorChanges{})
	assertMirrorChanges(t, "new cursor", mustWriteRowPage(t, s, root, page, []byte("c2")), MirrorChanges{})
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c2" {
		t.Fatalf("cursor = %q", cursor)
	}
	page.Rows[1].OrderKey = "a0"
	assertMirrorChanges(t, "order key", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Rows: []string{"10002"}})
	if cursor := mustMirrorCursor(t, s, root, "10"); cursor != nil {
		t.Fatalf("nil cursor stored as %q", cursor)
	}
	page.Rows[1].Estimate = nil
	assertMirrorChanges(t, "estimate", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Rows: []string{"10002"}})
	page.Rows[0].Card.Sprints = []string{"22"}
	assertMirrorChanges(t, "sprint left", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.Sprints = []string{"22", "20"}
	assertMirrorChanges(t, "sprint joined", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.Data = []byte(`{"title":"Renamed"}`)
	assertMirrorChanges(t, "card data", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.AssigneeID = ""
	assertMirrorChanges(t, "unassigned", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.Data = nil
	assertMirrorChanges(t, "no data", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	if cards := mustMirrorCards(t, s, root, "10001"); len(cards[0].Data) != 0 {
		t.Fatalf("empty data reads as %v", cards[0].Data)
	}
}

// Two reads of one board row land in either order: the one that began
// later wins. A removal fences the reads that began before it: they add no
// row, so a row it removed does not come back; a read from after it adds.
func TestMirrorRowsFollowTheirReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	at := func(readAt int64, rows ...MirrorRow) MirrorRowPage {
		p := webPage("10", rows...)
		p.ReadAt = readAt
		return p
	}
	mustWriteRowPage(t, s, root, at(20, webRow("10001", 5, "late"), webRow("10002", 5, "b")), nil)
	got := mustWriteRowPage(t, s, root, at(10, webRow("10001", 5, "early")), nil)
	assertMirrorChanges(t, "earlier read after a later one", got, MirrorChanges{Superseded: []string{"10001"}})
	if key := mustOrderKey(t, s, "10", "10001"); key != "late" {
		t.Fatalf("order key = %s; want the later read's", key)
	}
	assertMirrorChanges(t, "earlier read of the same row", mustWriteRowPage(t, s, root, at(10, webRow("10001", 5, "late")), nil), MirrorChanges{})

	// A removal by a read that began before the row was stored leaves it.
	removal, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001", "10009"}, ReadAt: 15}))
	if err != nil || !reflect.DeepEqual(removal, MirrorChanges{Superseded: []string{"10001"}}) {
		t.Fatalf("early removal = %+v, %v", removal, err)
	}
	removal, err = s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: 30}))
	if err != nil || !reflect.DeepEqual(removal, MirrorChanges{Removed: []string{"10002"}}) {
		t.Fatalf("removal = %+v, %v", removal, err)
	}
	got = mustWriteRowPage(t, s, root, at(25, webRow("10002", 6, "b"), webRow("10003", 6, "c"), webRow("10001", 5, "later")), nil)
	assertMirrorChanges(t, "read from before the removal", got, MirrorChanges{Rows: []string{"10001"}, Superseded: []string{"10002", "10003"},
		Refused: []string{"10002", "10003"}, CursorRefused: true})
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10002")
	got = mustWriteRowPage(t, s, root, at(30, webRow("10002", 6, "b"), webRow("10003", 6, "c")), nil)
	assertMirrorChanges(t, "read from the removal on", got, MirrorChanges{Cards: []string{"10002", "10003"}, Rows: []string{"10002", "10003"},
		Added: []string{"10002", "10003"}})

	// A removal by a read that began before the latest removal keeps the
	// latest removal's time.
	removal, err = s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10001"}, ReadAt: 28}))
	if err != nil || !reflect.DeepEqual(removal, MirrorChanges{Removed: []string{"10001"}}) {
		t.Fatalf("earlier removal = %+v, %v", removal, err)
	}
	got = mustWriteRowPage(t, s, root, at(29, webRow("10005", 6, "e")), nil)
	assertMirrorChanges(t, "read from between the removals", got, MirrorChanges{Superseded: []string{"10005"}, Refused: []string{"10005"}, CursorRefused: true})
}

// A page's cursor is stored unless a page whose read began later stored
// one.
func TestMirrorCursorFollowsItsReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	page := webPage("10")
	page.ReadAt = 20
	mustWriteRowPage(t, s, root, page, []byte("c2"))
	page.ReadAt = 10
	mustWriteRowPage(t, s, root, page, []byte("c1"))
	mustWriteRowPage(t, s, root, page, nil)
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c2" {
		t.Fatalf("cursor = %q; want the later read's", cursor)
	}

	// The same cursor from a later read is that read's: a read that began
	// between the two is superseded. So is a read that began before one
	// recorded as succeeded, which wrote no page.
	cursor := func(readAt int64, c string, want string) {
		t.Helper()
		page.ReadAt = readAt
		mustWriteRowPage(t, s, root, page, []byte(c))
		if got := mustMirrorCursor(t, s, root, "10"); string(got) != want {
			t.Fatalf("cursor %s read at %d: stored %q; want %q", c, readAt, got, want)
		}
	}
	cursor(30, "c2", "c2")
	assertRows(t, s.db, `SELECT cursor_read_at FROM tracker_cache_sync WHERE board_id = '10'`, "30")
	cursor(25, "c3", "c2")
	if _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", MirrorSyncAttempt{ReadAt: 40})); err != nil {
		t.Fatal(err)
	}
	cursor(35, "c4", "c2")
	cursor(40, "c5", "c5")
}

// The page and its cursor land together or not at all.
func TestWriteMirrorRowPageIsAtomic(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)

	mustExec(t, s.db, `CREATE TRIGGER test_refuse_cursor BEFORE INSERT ON tracker_cache_sync BEGIN SELECT RAISE(ABORT, 'cursor refused'); END`)
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("10", webRow("10001", 5, "a", "21"))), []byte("c1")); err == nil {
		t.Fatal("page stored with its cursor refused")
	}
	for _, table := range []string{"tracker_cache_issues", "tracker_cache_board_issues", "tracker_cache_issue_sprints", "tracker_cache_sync"} {
		assertRows(t, s.db, `SELECT count(*) FROM `+table, "0")
	}
	mustExec(t, s.db, `DROP TRIGGER test_refuse_cursor`)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), []byte("c1"))

	mustExec(t, s.db, `CREATE TRIGGER test_refuse_row BEFORE INSERT ON tracker_cache_board_issues WHEN NEW.issue_id = '10003'
		BEGIN SELECT RAISE(ABORT, 'row refused'); END`)
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("10", webRow("10002", 6, "b"), webRow("10003", 6, "c"))), []byte("c2")); err == nil {
		t.Fatal("cursor stored with a row refused")
	}
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q; want the last stored page's", cursor)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")
}

// A rows write with no cursor leaves the cursor and removes the rows of
// the issues the board no longer lists; an issue it leaves with no row is
// unkept, absent to reads, and stays for the cleanup.
func TestWriteMirrorRowsRemovesMissingRows(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a"), webRow("10002", 5, "b")), []byte("c1"))
	page := webPage("10", webRow("10001", 5, "a1"))
	page.Missing = []string{"10002", "10009"}
	got, err := s.WriteMirrorRows(root, currentPage(s, root, page))
	if err != nil {
		t.Fatal(err)
	}
	assertMirrorChanges(t, "rows", got, MirrorChanges{Rows: []string{"10001"}, Removed: []string{"10002"}})
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q", cursor)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10002")
	if cards := mustMirrorCards(t, s, root, "10001", "10002"); len(cards) != 1 {
		t.Fatalf("cards = %+v; want 10002 absent", cards)
	}
}

func TestWriteMirrorRowsRefusesInvalidPages(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("11", webRow("10001", 5, "a"))), nil); !errors.Is(err, ErrTrackerCacheNoBoard) {
		t.Fatalf("rows of a board with no structure: %v", err)
	}
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("13", webRow("10001", 5, "a"))), nil); !errors.Is(err, ErrTrackerCacheUnlisted) {
		t.Fatalf("rows of an unlisted board: %v", err)
	}
	for name, page := range map[string]MirrorRowPage{
		"no board":         {Rows: []MirrorRow{webRow("10001", 5, "a")}, ReadAt: 1},
		"no read time":     {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a")}},
		"too many rows":    webPage("10", mirrorRows(20000, mirrorMaxPageRows+1, "a")...),
		"no key":           webPage("10", MirrorRow{Card: MirrorCard{IssueID: "10001", UpdatedAt: 5}}),
		"no update time":   webPage("10", webRow("10001", 0, "a")),
		"issue twice":      webPage("10", webRow("10001", 5, "a"), webRow("10001", 6, "b")),
		"sprint twice":     webPage("10", webRow("10001", 5, "a", "21", "21")),
		"empty sprint":     webPage("10", webRow("10001", 5, "a", "")),
		"missing as read":  {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a")}, Missing: []string{"10001"}, ReadAt: 1},
		"bad catalog":      {BoardID: "10", Catalog: MirrorCatalog{Statuses: []MirrorStatus{{ID: "1"}}}, ReadAt: 1},
		"no issue ID":      webPage("10", MirrorRow{Card: MirrorCard{Key: "WEB-1", UpdatedAt: 5}}),
		"negative create":  webPage("10", MirrorRow{Card: MirrorCard{IssueID: "10001", Key: "WEB-1", UpdatedAt: 5, CreatedAt: -1}}),
		"negative resolve": webPage("10", MirrorRow{Card: MirrorCard{IssueID: "10001", Key: "WEB-1", UpdatedAt: 5, ResolvedAt: -1}}),
		"empty missing":    {BoardID: "10", Missing: []string{""}, ReadAt: 1},
		"missing twice":    {BoardID: "10", Missing: []string{"10002", "10002"}, ReadAt: 1},
	} {
		if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), nil); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{}, 0); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Errorf("cards with no read time: %v", err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")
}

// A reconcile returns the listed issues with no row; a complete listing
// removes, a chunk per transaction, the unlisted rows stored by reads that
// began no later than it, and the issues it leaves with no row become
// unkept. An incomplete listing removes nothing.
func TestReconcileMirrorBoard(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(20000, clearMirrorChunk+3, "a"))
	late := webPage("10", webRow("10001", 5, "a"))
	late.ReadAt = 9
	mustWriteRowPage(t, s, root, late, nil)

	got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"20000", "10006", "10005"}, false, 5, 5)
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10006", "10005"}}) {
		t.Fatalf("incomplete listing = %+v, %v", got, err)
	}
	recorder := recordMirrorWrites(t, s)
	got, err = s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10006", "20000"}, true, 5, 5)
	if err != nil || !reflect.DeepEqual(got.Missing, []string{"10006"}) || len(got.Removed) != clearMirrorChunk+2 {
		t.Fatalf("complete listing = %d missing %v and %d removed, %v", len(got.Missing), got.Missing, len(got.Removed), err)
	}
	if most, _ := recorder.mostDeletedInATransaction(); most > clearMirrorChunk {
		t.Fatalf("a reconcile transaction deleted %d rows", most)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues ORDER BY 1`, "10001,20000")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "504")
	if got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10006", "20000"}, true, 5, 5); err != nil ||
		!reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10006"}}) {
		t.Fatalf("same listing again = %+v, %v", got, err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "11", currentLifetime(s, root, "11"), nil, true, 5, 5); !errors.Is(err, ErrTrackerCacheNoBoard) {
		t.Fatalf("board with no structure: %v", err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"20000", "20000"}, true, 5, 5); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("repeated listing: %v", err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, 0, 0); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("listing with no read time: %v", err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, 5, 6); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a settled bound after the read: %v", err)
	}
}

// A complete listing judges only the rows read at or before its settled
// bound: an unlisted row read after it and no later than the listing is
// kept and returned as unsettled, one read after the listing is neither,
// and an incomplete listing returns none.
func TestReconcileMirrorBoardUnsettled(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", nil)
	for id, at := range map[string]int64{"10001": 2, "10002": 4, "10003": 6, "10004": 9} {
		page := webPage("10", webRow(id, 5, "a"+id))
		page.ReadAt = at
		mustWriteRowPage(t, s, root, page, nil)
	}
	lifetime := currentLifetime(s, root, "10")
	got, err := s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10005"}, false, 8, 3)
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10005"}}) {
		t.Fatalf("incomplete listing = %+v, %v", got, err)
	}
	got, err = s.ReconcileMirrorBoard(root, "10", lifetime, []string{"10005"}, true, 8, 3)
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10005"}, Removed: []string{"10001"}, Unsettled: []string{"10002", "10003"}}) {
		t.Fatalf("complete listing = %+v, %v", got, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues ORDER BY 1`, "10002,10003,10004")
	// A read by ID after the listing settles them: the one it finds gone
	// is removed.
	gone := webPage("10", webRow("10002", 5, "a10002"))
	gone.Missing, gone.ReadAt, gone.Lifetime = []string{"10003"}, 10, lifetime
	if ch, err := s.WriteMirrorRows(root, gone); err != nil || !reflect.DeepEqual(ch.Removed, []string{"10003"}) {
		t.Fatalf("read by ID = %+v, %v", ch, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues ORDER BY 1`, "10002,10004")
}

// A new row refused because the board lost a row after its read began
// may be of another issue than the one the board lost: the page's cursor
// is not stored, so the walk reads the page again, from a read that began
// after the removal, which adds the row.
func TestAFencedNewRowHoldsItsPagesCursor(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	at := func(readAt int64, rows ...MirrorRow) MirrorRowPage {
		p := webPage("10", rows...)
		p.ReadAt = readAt
		return p
	}
	mustWriteRowPage(t, s, root, at(10, webRow("10001", 5, "a"), webRow("10002", 5, "b")), []byte("c1"))
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: 30})); err != nil {
		t.Fatal(err)
	}
	page := at(20, webRow("10001", 6, "a"), webRow("10003", 6, "c"))
	got := mustWriteRowPage(t, s, root, page, []byte("c2"))
	if !got.CursorRefused || !reflect.DeepEqual(got.Superseded, []string{"10003"}) || !reflect.DeepEqual(got.Refused, []string{"10003"}) {
		t.Fatalf("a page read before the removal = %+v; want 10003 refused and the cursor with it", got)
	}
	if rows, err := s.WriteMirrorRows(root, currentPage(s, root, page)); err != nil || !reflect.DeepEqual(rows.Refused, []string{"10003"}) {
		t.Fatalf("the same rows read by ID = %+v, %v; want 10003 refused", rows, err)
	}
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q; want the page's start", cursor)
	}
	page.ReadAt = 31
	got = mustWriteRowPage(t, s, root, page, []byte("c2"))
	if got.CursorRefused || !reflect.DeepEqual(got.Added, []string{"10003"}) || string(mustMirrorCursor(t, s, root, "10")) != "c2" {
		t.Fatalf("the page read again = %+v, cursor %q; want 10003 added and c2 stored", got, mustMirrorCursor(t, s, root, "10"))
	}
}
