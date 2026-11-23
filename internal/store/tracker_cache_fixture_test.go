package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
)

const mirrorSite = "https://example.atlassian.net"

// newMirror is a store with a Jira connection, its mirror root, and the
// default project listing boards 10, 11 and 12 through it.
func newMirror(t *testing.T) (*Store, TrackerConnection, int64) {
	t.Helper()
	s := newTestStore(t)
	c := mustCreateTrackerConnection(t, s, jiraConnection(mirrorSite, "user1@example.com", 10))
	root, created, err := s.GetOrCreateMirrorRoot(c.ID)
	if err != nil || !created {
		t.Fatalf("create mirror root: %v, %v", created, err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, c.ID, "10", "11", "12")
	return s, c, root
}

// listMirrorBoards makes boards the whole list projectID lists through
// connectionID.
func listMirrorBoards(t *testing.T, s *Store, projectID, connectionID string, boards ...string) {
	t.Helper()
	list := make([]ProjectTrackerBoard, 0, len(boards))
	for _, b := range boards {
		list = append(list, ProjectTrackerBoard{ConnectionID: connectionID, BoardID: b})
	}
	if _, err := s.ReplaceProjectTrackerBoards(projectID, list); err != nil {
		t.Fatalf("list boards %v: %v", boards, err)
	}
}

// webStructure is board 10: three columns, an active, a future and a
// closed sprint, read at 1.
func webStructure() MirrorStructure {
	return MirrorStructure{
		Board: MirrorBoard{ID: "10", ProjectID: "100", Name: "WEB board", PlansSprints: true, EstimateUnit: MirrorEstimatePoints, Data: []byte(`{"choices":[1,2,3]}`)},
		Columns: []MirrorColumn{
			{Name: "To do", Statuses: []string{"1"}},
			{Name: "In progress", Statuses: []string{"3"}},
			{Name: "Done", Statuses: []string{"10001", "6"}},
		},
		Sprints: []MirrorSprint{
			{ID: "21", OriginBoardID: "10", Name: "WEB Sprint 2", State: MirrorSprintActive, StartAt: 1000, EndAt: 2000},
			{ID: "22", OriginBoardID: "10", Name: "WEB Sprint 3", State: MirrorSprintFuture},
			{ID: "20", OriginBoardID: "10", Name: "WEB Sprint 1", State: MirrorSprintClosed, StartAt: 100, EndAt: 900, CompletedAt: 950},
		},
		Catalog: MirrorCatalog{
			Users: []MirrorUser{{ID: "acc-user1", Name: "User 1", Data: []byte(`{"active":true}`)}},
			Statuses: []MirrorStatus{
				{ID: "1", Name: "To Do", Group: MirrorGroupToDo},
				{ID: "3", Name: "In Progress", Group: MirrorGroupInProgress},
				{ID: "10001", Name: "Done", Group: MirrorGroupDone},
				{ID: "6", Name: "Won't do", Group: MirrorGroupCanceled},
			},
		},
		ReadAt: 1,
	}
}

func webCard(id string, updated int64, sprints ...string) MirrorCard {
	return MirrorCard{IssueID: id, Key: "WEB-" + id, ProjectID: "100", TypeID: "10001", StatusID: "1", AssigneeID: "acc-user1",
		PriorityID: "3", CreatedAt: 1, UpdatedAt: updated, Sprints: sprints, Data: []byte(`{"title":"Issue ` + id + `"}`)}
}

func webRow(id string, updated int64, order string, sprints ...string) MirrorRow {
	return MirrorRow{Card: webCard(id, updated, sprints...), OrderKey: order}
}

// heldDoc is issue's detail document with data, of the update of the card
// the mirror keeps for issue, or of update 1 when it keeps none (a
// document then owed its card), so a test of the documents passes the
// card fence. It reads the mirror: a test that counts a write's
// statements builds its documents before it counts.
func heldDoc(t *testing.T, s *Store, root int64, issue string, data []byte) MirrorDetailDoc {
	t.Helper()
	cards, err := s.ListMirrorCards(root, []string{issue})
	if err != nil {
		t.Fatal(err)
	}
	doc := MirrorDetailDoc{IssueID: issue, Data: data, CardUpdatedAt: 1}
	if len(cards) == 1 {
		doc.CardUpdatedAt = cards[0].UpdatedAt
	}
	return doc
}

// heldDocs is heldDoc of each of docs' issues and data.
func heldDocs(t *testing.T, s *Store, root int64, docs []MirrorDetailDoc) []MirrorDetailDoc {
	t.Helper()
	out := make([]MirrorDetailDoc, len(docs))
	for i, d := range docs {
		out[i] = heldDoc(t, s, root, d.IssueID, d.Data)
	}
	return out
}

// storeDetail stores issue's detail document with data and the card the
// mirror keeps (heldDoc).
func storeDetail(t *testing.T, s *Store, root, sweep int64, issue string, openedAt, readAt int64, data []byte) (MirrorDetailWrite, error) {
	t.Helper()
	return s.StoreMirrorDetail(root, sweep, heldDoc(t, s, root, issue, data), openedAt, readAt)
}

// webPage is a page of board's rows read at 1.
func webPage(board string, rows ...MirrorRow) MirrorRowPage {
	return MirrorRowPage{BoardID: board, Rows: rows, ReadAt: 1}
}

// noMirrorLifetime is a lifetime no board has, for a board with none.
const noMirrorLifetime = 1 << 62

// currentLifetime is board's lifetime in root as a caller would capture it
// now: the live one, else the retired one awaiting its drain, else one no
// board has.
func currentLifetime(s *Store, root int64, board string) int64 {
	var lifetime int64
	err := s.db.QueryRow(`SELECT lifetime FROM tracker_cache_boards WHERE root_id = ? AND board_id = ? ORDER BY retired LIMIT 1`,
		root, board).Scan(&lifetime)
	if err != nil {
		return noMirrorLifetime
	}
	return lifetime
}

// currentEstimateReset is board's estimate reset in root as a caller
// would capture it now, zero for a board with none.
func currentEstimateReset(s *Store, root int64, board string) int64 {
	var reset int64
	err := s.db.QueryRow(`SELECT estimate_reset_at FROM tracker_cache_boards WHERE root_id = ? AND board_id = ? ORDER BY retired LIMIT 1`,
		root, board).Scan(&reset)
	if err != nil {
		return 0
	}
	return reset
}

// currentSweep is root's sweep as a caller would capture it now, or the
// first one for a root with no row, whose every write is refused as gone.
func currentSweep(s *Store, root int64) int64 {
	var sweep int64
	if err := s.db.QueryRow(`SELECT sweep FROM tracker_cache_roots WHERE id = ?`, root).Scan(&sweep); err != nil {
		return 1
	}
	return sweep
}

// testNow is the caller's steady clock in the tests' due reads, past
// every answer the tests record (answeredHistory).
const testNow = int64(1_000_000)

// answeredHistory is h with no For read for update time 1 and with no
// AnsweredAt answered at 1; with no Wait it is final.
func answeredHistory(h MirrorHistory) MirrorHistory {
	if h.For == 0 {
		h.For = 1
	}
	if h.AnsweredAt == 0 {
		h.AnsweredAt = 1
	}
	return h
}

// dueHistories completes each history as a caller does from
// ListMirrorHistoryDue: each names its issue's receipt now, one with no
// For is read for its issue's update time now and one with no BoardsRev
// names its issue's boards revision now (an issue the mirror lacks has
// none of them); the rest as answeredHistory.
func dueHistories(s *Store, root int64, hs []MirrorHistory) []MirrorHistory {
	out := slices.Clone(hs)
	for i := range out {
		var updatedAt, boardsRev int64
		err := s.db.QueryRow(`SELECT updated_at, boards_rev, history_receipt FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ?`,
			root, out[i].IssueID).Scan(&updatedAt, &boardsRev, &out[i].Receipt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			panic(err)
		}
		if out[i].For == 0 {
			out[i].For = updatedAt
		}
		if out[i].BoardsRev == 0 {
			out[i].BoardsRev = boardsRev
		}
		out[i] = answeredHistory(out[i])
	}
	return out
}

// listedAt is h completed as dueHistories does, but naming receipt: a
// history read listed at an earlier state of its issue.
func listedAt(s *Store, root int64, h MirrorHistory, receipt int64) MirrorHistory {
	out := dueHistories(s, root, []MirrorHistory{h})[0]
	out.Receipt = receipt
	return out
}

// receiptOf is issue's history receipt now.
func receiptOf(t *testing.T, s *Store, root int64, issue string) int64 {
	t.Helper()
	var receipt int64
	if err := s.db.QueryRow(`SELECT history_receipt FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ?`, root, issue).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// currentStructure, currentPage and currentAttempt name the board's
// current lifetime in a write that names none; currentPage then names the
// board's current estimate reset too, as a walk captured now would.
func currentStructure(s *Store, root int64, st MirrorStructure) MirrorStructure {
	if st.Lifetime == 0 {
		st.Lifetime = currentLifetime(s, root, st.Board.ID)
	}
	return st
}

func currentPage(s *Store, root int64, p MirrorRowPage) MirrorRowPage {
	if p.Lifetime == 0 {
		p.Lifetime = currentLifetime(s, root, p.BoardID)
		p.EstimateResetAt = currentEstimateReset(s, root, p.BoardID)
	}
	return p
}

func currentAttempt(s *Store, root int64, board string, a MirrorSyncAttempt) MirrorSyncAttempt {
	if a.Lifetime == 0 && board != "" {
		a.Lifetime = currentLifetime(s, root, board)
	}
	return a
}

func mustReplaceStructure(t *testing.T, s *Store, root int64, st MirrorStructure, want bool) {
	t.Helper()
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil || got.MirrorWrite != (MirrorWrite{Changed: want}) {
		t.Fatalf("replace board %s structure: %+v, %v; want changed %v", st.Board.ID, got, err, want)
	}
}

func mustWriteRowPage(t *testing.T, s *Store, root int64, page MirrorRowPage, cursor []byte) MirrorChanges {
	t.Helper()
	changes, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), cursor)
	if err != nil {
		t.Fatalf("write board %s rows: %v", page.BoardID, err)
	}
	return changes
}

// countMirrorRows counts the rows of every tracker_cache_* table.
func countMirrorRows(t testing.TB, db sqlQueryer) map[string]int {
	t.Helper()
	tables, err := queryIDs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'tracker\_cache\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

func mirrorRowTotal(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// seedFullMirror writes a row into every mirror table under root.
func seedFullMirror(t *testing.T, s *Store, root int64) {
	t.Helper()
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
	mustExec(t, s.db, `INSERT INTO tracker_cache_board_unexplained(root_id, board_id, kind, ref_id, named_at) VALUES (?, '10', 3, '99', 0)`, root)
	if _, err := replaceHistory(s, root, MirrorHistory{IssueID: "10001", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 2, From: "1", To: "3"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := storeDetail(t, s, root, currentSweep(s, root), "10001", 7, 7, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", 1, []MirrorWorklog{
		{ID: "w1", IssueID: "10001", AuthorID: "acc-user1", Day: "2026-01-05", SpentSeconds: 3600}}); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepMirrorTicket(root, "10001", 1, 1); err != nil {
		t.Fatal(err)
	}
	for table, n := range countMirrorRows(t, s.db) {
		if n == 0 && table != "tracker_cache_roots" {
			t.Fatalf("seed left %s empty", table)
		}
	}
}
