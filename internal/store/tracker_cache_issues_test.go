package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// linkIssue links a new thread to a WEB issue on the mirror's site.
func linkIssue(t *testing.T, s *Store, threadID, issueID string) {
	t.Helper()
	mustCreateThread(t, s, threadID)
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: threadID, Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: issueID},
		IssueKey: "WEB-" + issueID, Origin: TicketOriginManual, LinkedAt: 10}, true)
}

func mustMirrorCards(t *testing.T, s *Store, root string, ids ...string) []MirrorCard {
	t.Helper()
	cards, err := s.ListMirrorCards(root, ids)
	if err != nil {
		t.Fatal(err)
	}
	return cards
}

func mustMirrorCursor(t *testing.T, s *Store, root, boardID string) []byte {
	t.Helper()
	st, ok, err := s.GetMirrorSync(root, boardID)
	if err != nil || !ok {
		t.Fatalf("sync of board %s: %v, %v", boardID, ok, err)
	}
	return st.Cursor
}

func assertMirrorChanges(t *testing.T, what string, got, want MirrorChanges) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: changes = %+v\nwant %+v", what, got, want)
	}
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
	page := MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a", "22", "21"), second},
		Catalog: MirrorCatalog{Users: []MirrorUser{{ID: "acc-user2", Name: "User 2", Data: []byte(`{}`)}}}}
	got := mustWriteRowPage(t, s, root, page, []byte("c1"))
	assertMirrorChanges(t, "first page", got, MirrorChanges{Cards: []string{"10001", "10002"}, Rows: []string{"10001", "10002"}, Catalog: true})
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q", cursor)
	}
	first := webCard("10001", 5, "21", "22")
	if cards := mustMirrorCards(t, s, root, "10002", "10001", "10009"); !reflect.DeepEqual(cards, []MirrorCard{first, second.Card}) {
		t.Fatalf("cards = %+v", cards)
	}
	rows, err := s.ListMirrorIssueRows(root, "10002")
	if err != nil || !reflect.DeepEqual(rows, []MirrorIssueRow{{BoardID: "10", OrderKey: "b", Estimate: &three}}) {
		t.Fatalf("rows of 10002 = %+v, %v", rows, err)
	}

	assertMirrorChanges(t, "same page", mustWriteRowPage(t, s, root, page, []byte("c2")), MirrorChanges{})
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
	page.Rows[0].Card.Sprints = []string{"22", "23"}
	assertMirrorChanges(t, "sprint joined", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.Data = []byte(`{"title":"Renamed"}`)
	assertMirrorChanges(t, "card data", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
	page.Rows[0].Card.AssigneeID = ""
	assertMirrorChanges(t, "unassigned", mustWriteRowPage(t, s, root, page, nil), MirrorChanges{Cards: []string{"10001"}})
}

// A card older than the stored one is not stored, but its row on the board
// still is; a card of the same time replaces it.
func TestMirrorCardIsNeverReplacedByAnOlderOne(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	newer := webRow("10001", 10, "a", "21")
	newer.Card.StatusID = "3"
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{newer}}, nil)

	older := webRow("10001", 5, "z")
	got := mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{older}}, nil)
	assertMirrorChanges(t, "older card", got, MirrorChanges{Rows: []string{"10001"}})
	if cards := mustMirrorCards(t, s, root, "10001"); !reflect.DeepEqual(cards, []MirrorCard{newer.Card}) {
		t.Fatalf("card = %+v; want the newer one", cards)
	}
	assertRows(t, s.db, `SELECT order_key FROM tracker_cache_board_issues`, "z")
	if changes, err := s.UpsertMirrorCards(root, []MirrorCard{older.Card}, MirrorCatalog{}); err != nil || changes.Cards != nil {
		t.Fatalf("older card with no board = %+v, %v", changes, err)
	}

	same := newer
	same.Card.StatusID = "10001"
	got = mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{same}}, nil)
	assertMirrorChanges(t, "card of the same time", got, MirrorChanges{Cards: []string{"10001"}, Rows: []string{"10001"}})
}

// The page and its cursor land together or not at all.
func TestWriteMirrorRowPageIsAtomic(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)

	mustExec(t, s.db, `CREATE TRIGGER test_refuse_cursor BEFORE INSERT ON tracker_cache_sync BEGIN SELECT RAISE(ABORT, 'cursor refused'); END`)
	if _, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a", "21")}}, []byte("c1")); err == nil {
		t.Fatal("page stored with its cursor refused")
	}
	for _, table := range []string{"tracker_cache_issues", "tracker_cache_board_issues", "tracker_cache_issue_sprints", "tracker_cache_sync"} {
		assertRows(t, s.db, `SELECT count(*) FROM `+table, "0")
	}
	mustExec(t, s.db, `DROP TRIGGER test_refuse_cursor`)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a")}}, []byte("c1"))

	mustExec(t, s.db, `CREATE TRIGGER test_refuse_row BEFORE INSERT ON tracker_cache_board_issues WHEN NEW.issue_id = '10003'
		BEGIN SELECT RAISE(ABORT, 'row refused'); END`)
	page := MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10002", 6, "b"), webRow("10003", 6, "c")}}
	if _, err := s.WriteMirrorRowPage(root, page, []byte("c2")); err == nil {
		t.Fatal("cursor stored with a row refused")
	}
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q; want the last stored page's", cursor)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")
}

// A rows write with no cursor leaves the cursor and removes the rows of
// the issues the board no longer lists.
func TestWriteMirrorRowsRemovesMissingRows(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")}}, []byte("c1"))
	got, err := s.WriteMirrorRows(root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a1")}, Missing: []string{"10002", "10009"}})
	if err != nil {
		t.Fatal(err)
	}
	assertMirrorChanges(t, "rows", got, MirrorChanges{Rows: []string{"10001"}, Removed: []string{"10002"}, Pruned: []string{"10002"}})
	if cursor := mustMirrorCursor(t, s, root, "10"); string(cursor) != "c1" {
		t.Fatalf("cursor = %q", cursor)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")
}

func TestWriteMirrorRowsRefusesInvalidPages(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	if _, err := s.WriteMirrorRowPage(root, MirrorRowPage{BoardID: "11", Rows: []MirrorRow{webRow("10001", 5, "a")}}, nil); !errors.Is(err, ErrTrackerCacheNoBoard) {
		t.Fatalf("rows of a board with no structure: %v", err)
	}
	for name, page := range map[string]MirrorRowPage{
		"no board":         {Rows: []MirrorRow{webRow("10001", 5, "a")}},
		"no key":           {BoardID: "10", Rows: []MirrorRow{{Card: MirrorCard{IssueID: "10001", UpdatedAt: 5}}}},
		"no update time":   {BoardID: "10", Rows: []MirrorRow{webRow("10001", 0, "a")}},
		"issue twice":      {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a"), webRow("10001", 6, "b")}},
		"sprint twice":     {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a", "21", "21")}},
		"empty sprint":     {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a", "")}},
		"missing as read":  {BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a")}, Missing: []string{"10001"}},
		"bad catalog":      {BoardID: "10", Catalog: MirrorCatalog{Statuses: []MirrorStatus{{ID: "1"}}}},
		"no issue ID":      {BoardID: "10", Rows: []MirrorRow{{Card: MirrorCard{Key: "WEB-1", UpdatedAt: 5}}}},
		"negative create":  {BoardID: "10", Rows: []MirrorRow{{Card: MirrorCard{IssueID: "10001", Key: "WEB-1", UpdatedAt: 5, CreatedAt: -1}}}},
		"negative resolve": {BoardID: "10", Rows: []MirrorRow{{Card: MirrorCard{IssueID: "10001", Key: "WEB-1", UpdatedAt: 5, ResolvedAt: -1}}}},
		"empty missing":    {BoardID: "10", Missing: []string{""}},
		"missing twice":    {BoardID: "10", Missing: []string{"10002", "10002"}},
	} {
		if _, err := s.WriteMirrorRowPage(root, page, nil); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")
}

// A reconcile returns the listed issues with no row; a complete listing
// removes the rest, and an issue goes from the mirror with its changes,
// sprints and detail only when it leaves its last board. An incomplete
// listing removes nothing.
func TestReconcileMirrorBoard(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID = "11"
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{
		webRow("10001", 5, "a"), webRow("10002", 5, "b", "21"), webRow("10003", 5, "c")}}, nil)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "11", Rows: []MirrorRow{webRow("10003", 5, "c")}}, nil)
	if _, err := s.ReplaceMirrorHistory(root, MirrorHistory{IssueID: "10002", CompleteTo: 5, Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 2, To: "3"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StoreMirrorDetail(root, MirrorDetail{IssueID: "10002", OpenedAt: 3, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReconcileMirrorBoard(root, "10", []string{"10001", "10006", "10005"}, false)
	if err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10006", "10005"}}) {
		t.Fatalf("incomplete listing = %+v, %v", got, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues WHERE board_id = '10' ORDER BY 1`, "10001,10002,10003")

	got, err = s.ReconcileMirrorBoard(root, "10", []string{"10006", "10001"}, true)
	want := MirrorReconcile{Missing: []string{"10006"}, Removed: []string{"10002", "10003"}, Pruned: []string{"10002"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("complete listing = %+v, %v\nwant %+v", got, err, want)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10003")
	for _, table := range []string{"tracker_cache_changes", "tracker_cache_issue_sprints", "tracker_cache_issue_details"} {
		assertRows(t, s.db, `SELECT count(*) FROM `+table+` WHERE issue_id = '10002'`, "0")
	}
	if got, err := s.ReconcileMirrorBoard(root, "10", []string{"10006", "10001"}, true); err != nil || !reflect.DeepEqual(got, MirrorReconcile{Missing: []string{"10006"}}) {
		t.Fatalf("same listing again = %+v, %v", got, err)
	}

	if _, err := s.ReconcileMirrorBoard(root, "12", nil, true); !errors.Is(err, ErrTrackerCacheNoBoard) {
		t.Fatalf("board with no structure: %v", err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "10", []string{"10001", "10001"}, true); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("repeated listing: %v", err)
	}
}

// A thread this computer owns keeps the issue its ticket names, on this
// connection's site, after the issue leaves every board; a card with no
// board is stored only for a kept issue.
func TestMirrorKeepsIssuesLinkedToOwnedThreads(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{
		webRow("10001", 5, "a"), webRow("10002", 5, "b"), webRow("10003", 5, "c"), webRow("10004", 5, "d")}}, nil)
	linkIssue(t, s, "linked", "10001")
	linkIssue(t, s, "deleting", "10002")
	mustExec(t, s.db, `UPDATE threads SET deleting = 1 WHERE id = 'deleting'`)
	mustCreateThread(t, s, "other-kind")
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: "other-kind", Ticket: TicketRef{Kind: TrackerKindLinear, Site: mirrorSite, IssueID: "10004"},
		IssueKey: "APP-4", Origin: TicketOriginManual, LinkedAt: 10}, true)
	mustCreateThread(t, s, "elsewhere")
	mustSetThreadTicket(t, s, ThreadTicket{ThreadID: "elsewhere", Ticket: TicketRef{Kind: TrackerKindJira, Site: "https://other.example.com", IssueID: "10003"},
		IssueKey: "WEB-3", Origin: TicketOriginManual, LinkedAt: 10}, true)

	got, err := s.ReconcileMirrorBoard(root, "10", nil, true)
	if err != nil || !slices.Equal(got.Pruned, []string{"10002", "10003", "10004"}) {
		t.Fatalf("reconcile to nothing = %+v, %v; want all but 10001 pruned", got, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")

	changes, err := s.UpsertMirrorCards(root, []MirrorCard{webCard("10001", 7), webCard("10009", 7)}, MirrorCatalog{})
	assertMirrorChanges(t, "linked cards", changes, MirrorChanges{Cards: []string{"10001"}, Unkept: []string{"10009"}})
	if err != nil {
		t.Fatal(err)
	}
	if changes, err := s.UpsertMirrorCards(root, []MirrorCard{webCard("10001", 7)}, MirrorCatalog{}); err != nil || !reflect.DeepEqual(changes, MirrorChanges{}) {
		t.Fatalf("same card again = %+v, %v", changes, err)
	}
	if pruned, _, _, err := s.PruneUnkeptMirrorIssues(root, ""); err != nil || pruned != nil {
		t.Fatalf("sweep with the issue linked = %v, %v", pruned, err)
	}
	if _, err := s.UpsertMirrorCards(root, []MirrorCard{webCard("10001", 7), webCard("10001", 8)}, MirrorCatalog{}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("card twice: %v", err)
	}
}
