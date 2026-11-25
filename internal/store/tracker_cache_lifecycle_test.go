package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// mirrorRows are n rows of board, IDs from first, read at 1.
func mirrorRows(first, n int, order string) []MirrorRow {
	rows := make([]MirrorRow, 0, n)
	for i := range n {
		rows = append(rows, webRow(fmt.Sprint(first+i), 5, fmt.Sprintf("%s%05d", order, i)))
	}
	return rows
}

// writeMirrorBoard stores board's structure and rows in pages.
func writeMirrorBoard(t *testing.T, s *Store, root int64, board string, rows []MirrorRow) {
	t.Helper()
	st := webStructure()
	st.Board.ID = board
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
		t.Fatal(err)
	}
	for page := range slices.Chunk(rows, mirrorMaxPageRows) {
		mustWriteRowPage(t, s, root, webPage(board, page...), nil)
	}
}

// statusHistory is n status changes of issue, answered at n+1.
func statusHistory(issue string, n int) MirrorHistory {
	h := MirrorHistory{IssueID: issue, AnsweredAt: int64(n + 1)}
	for i := range n {
		h.Changes = append(h.Changes, MirrorChange{Kind: MirrorChangeStatus, At: int64(i + 1), To: "3"})
	}
	return h
}

// Removing a connection deletes no mirror row: the schema detaches its
// root, so writes fail at once; the cleanup drains it later. The Tempo
// connection goes with its Jira one.
func TestDeleteTrackerConnectionDetachesItsMirror(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 11})
	seedFullMirror(t, s, root)
	before := countMirrorRows(t, s.db)
	deleted, err := s.DeleteTrackerConnection(jira.ID)
	if err != nil || !slices.Equal(deleted, []string{jira.ID, tempo.ID}) {
		t.Fatalf("delete = %v, %v; want %s and its Tempo connection %s", deleted, err, jira.ID, tempo.ID)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_connections`, "0")
	for _, table := range []string{"tracker_cache_issues", "tracker_cache_board_issues", "tracker_cache_changes", "tracker_cache_users"} {
		if after := countMirrorRows(t, s.db); after[table] != before[table] {
			t.Fatalf("the removal deleted %s rows: %v, was %v", table, after, before)
		}
	}
	if _, _, err := s.RecordMirrorSyncAttempt(root, "", currentAttempt(s, root, "", MirrorSyncAttempt{ReadAt: mirrorReadAt(1)})); !errors.Is(err, ErrTrackerCacheGone) {
		t.Fatalf("write after the removal: %v", err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("after the cleanup: %v", counts)
	}
	if deleted, err := s.DeleteTrackerConnection(jira.ID); err != nil || deleted != nil {
		t.Fatalf("second delete = %v, %v", deleted, err)
	}
}

// A cleanup a failure stops leaves its item pending, carries on with the
// others, and the next call finishes it. The lock is held through every
// pause, so calls never interleave.
func TestFinishMirrorCleanupResumesAfterAFailure(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	seedFullMirror(t, s, root)
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	other, _, err := s.GetOrCreateMirrorRoot(linear.ID)
	if err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, mustCreateTrackerProject(t, s, "p2"), linear.ID, "10")
	mustReplaceStructure(t, s, other, webStructure(), true)
	for _, id := range []string{jira.ID, linear.ID} {
		if _, err := s.DetachMirror(id); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, s.db, fmt.Sprintf(`CREATE TRIGGER test_refuse_drain BEFORE DELETE ON tracker_cache_users WHEN OLD.root_id = %d
		BEGIN SELECT RAISE(ABORT, 'drain refused'); END`, root))
	pause := func() {
		if s.mirrorCleanup.TryLock() {
			s.mirrorCleanup.Unlock()
			t.Error("the cleanup lock is free during a pause")
		}
	}
	if err := s.FinishMirrorCleanup(context.Background(), pause); err == nil || !strings.Contains(err.Error(), "drain refused") {
		t.Fatalf("cleanup with a drain refused: %v", err)
	}
	assertRows(t, s.db, `SELECT id FROM tracker_cache_roots`, fmt.Sprint(root))
	mustExec(t, s.db, `DROP TRIGGER test_refuse_drain`)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = s.FinishMirrorCleanup(context.Background(), pause) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("after the second cleanup: %v", counts)
	}
}

// The delete of a board's last listing retires the board in one statement
// that deletes only its sync state: the board and its rows are absent to
// every read and write at once, and the cleanup drains them in batches.
// A board another project still lists stays.
func TestUnlistedBoardIsRetiredThenDrained(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(20000, 2*mirrorMaxPageRows+1, "a"))
	writeMirrorBoard(t, s, root, "11", mirrorRows(20000, 1, "a"))
	listMirrorBoards(t, s, mustCreateTrackerProject(t, s, "p2"), jira.ID, "11")
	for _, board := range []string{"10", "11"} {
		if _, _, err := s.RecordMirrorSyncAttempt(root, board, currentAttempt(s, root, board, MirrorSyncAttempt{ReadAt: mirrorReadAt(5)})); err != nil {
			t.Fatal(err)
		}
	}
	// A new order that keeps a board keeps its listing, so nothing retires.
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "12", "11", "10")
	assertRows(t, s.db, `SELECT board_id || ':' || retired FROM tracker_cache_boards ORDER BY 1`, "10:0,11:0,12:0")
	assertRows(t, s.db, `SELECT board_id FROM tracker_cache_sync ORDER BY 1`, "10,11")
	before := countMirrorRows(t, s.db)
	recorder := recordMirrorWrites(t, s)
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "12")
	if writes := recorder.rowWrites(); writes["tracker_cache_board_issues"] != 0 || writes["tracker_cache_sync"] != 1 ||
		writes["tracker_cache_boards"] != 1 {
		t.Fatalf("unlisting wrote %v; want the board's flag and its sync state only", writes)
	}
	assertRows(t, s.db, `SELECT board_id || ':' || retired FROM tracker_cache_boards ORDER BY 1`, "10:1,11:0,12:0")
	if after := countMirrorRows(t, s.db); after["tracker_cache_board_issues"] != before["tracker_cache_board_issues"] {
		t.Fatalf("unlisting deleted rows: %v", after)
	}

	if _, ok, err := s.GetMirrorBoard(root, "10"); err != nil || ok {
		t.Errorf("retired board reads: %v, %v", ok, err)
	}
	if page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{}); err != nil || len(page.Rows) != 0 {
		t.Errorf("retired board's backlog = %d rows, %v", len(page.Rows), err)
	}
	if counts, err := s.CountMirrorSections(root, "10", MirrorRegroup{}); err != nil || counts.All.Rows != 0 {
		t.Errorf("retired board's counts = %+v, %v", counts, err)
	}
	if _, ok, err := s.GetMirrorSync(root, "10", testNow); err != nil || ok {
		t.Errorf("retired board's sync state: %v, %v", ok, err)
	}
	if cards, err := s.ListMirrorCards(root, []string{"20000", "20001"}); err != nil || len(cards) != 1 || cards[0].IssueID != "20000" {
		t.Errorf("cards = %+v, %v; want only 20000, which board 11 keeps", cards, err)
	}
	if rows, _, err := s.ListMirrorIssueRows(root, "20000", ""); err != nil || len(rows) != 1 || rows[0].BoardID != "11" {
		t.Errorf("rows of 20000 = %+v, %v; want board 11's", rows, err)
	}
	for name, write := range map[string]func() error{
		"structure": func() error {
			_, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, webStructure()))
			return err
		},
		"rows": func() error {
			_, err := s.WriteMirrorRows(root, currentPage(s, root, webPage("10", webRow("20001", 9, "b"))))
			return err
		},
		"reconcile": func() error {
			_, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, mirrorReadAt(9))
			return err
		},
		"sync": func() error {
			_, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", MirrorSyncAttempt{ReadAt: mirrorReadAt(9)}))
			return err
		},
	} {
		if err := write(); !errors.Is(err, ErrTrackerCacheUnlisted) {
			t.Errorf("%s of a retired board: %v", name, err)
		}
	}

	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if most, _ := recorder.mostDeletedInATransaction(); most > clearMirrorChunk {
		t.Fatalf("a drain transaction deleted %d rows", most)
	}
	assertRows(t, s.db, `SELECT board_id FROM tracker_cache_boards ORDER BY 1`, "11,12")
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "20000")
	assertRows(t, s.db, `SELECT DISTINCT board_id FROM tracker_cache_sprints`, "11")

	// A project's delete unlists its boards the same way.
	mustExec(t, s.db, `DELETE FROM projects WHERE id = 'p2'`)
	assertRows(t, s.db, `SELECT board_id || ':' || retired FROM tracker_cache_boards ORDER BY 1`, "11:1,12:0")
}

// A structure read captured before its board was unlisted cannot bring the
// board back, before or after the cleanup removed it; once the board is
// listed again it is the same board, synced afresh.
func TestLateStructureCannotRestoreAnUnlistedBoard(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	captured := webStructure()
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 1, "a"))
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "11")
	for _, stage := range []string{"retired", "drained"} {
		if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, captured)); !errors.Is(err, ErrTrackerCacheUnlisted) {
			t.Fatalf("late structure, board %s: %v", stage, err)
		}
		if _, err := s.WriteMirrorRows(root, currentPage(s, root, webPage("10", webRow("10001", 5, "a")))); !errors.Is(err, ErrTrackerCacheUnlisted) {
			t.Fatalf("late rows, board %s: %v", stage, err)
		}
		if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_boards WHERE board_id = '10'`, "0")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_issues`, "0")

	// Listed again while its drain is pending, the board stays refused
	// until the cleanup has removed it.
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 1, "a"))
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "11")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, captured)); !errors.Is(err, ErrTrackerCacheUnlisted) {
		t.Fatalf("structure of a board listed again before its drain: %v", err)
	}
	if _, ok, err := s.GetMirrorBoard(root, "10"); err != nil || ok {
		t.Fatalf("board listed again before its drain reads: %v, %v", ok, err)
	}
	if cards, err := s.ListMirrorCards(root, []string{"10001"}); err != nil || len(cards) != 0 {
		t.Fatalf("cards of a board listed again before its drain = %+v, %v", cards, err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	mustReplaceStructure(t, s, root, captured, true)
}

// Each way a thread's link goes (an unlink, the thread's delete, its
// retire to a holder, a move to another computer) makes its issue unkept:
// absent to reads at once, removed by the cleanup. An issue a link still
// keeps, and one a board row keeps, stay.
func TestCleanupSweepsIssuesWhoseLinksGo(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	linkIssue(t, s, "unlinked", "10001")
	linkIssue(t, s, "deleted", "10002")
	seedLinearSource(t, s, "retired", 1)
	// Forked before the link, so the fork has none of its own.
	mustPointerFork(t, s, "retired", "retired-fork", ForkCut{})
	seedThreadTicket(t, s, ThreadTicket{ThreadID: "retired", Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: "10003"},
		IssueKey: "WEB-3", Origin: TicketOriginManual, LinkedAt: 10})
	linkIssue(t, s, "moved", "10004")
	linkIssue(t, s, "kept", "10005")
	mustWriteRowPage(t, s, root, webPage("10", webRow("10006", 5, "a")), nil)
	cards := []MirrorCard{webCard("10001", 5), webCard("10002", 5), webCard("10003", 5), webCard("10004", 5), webCard("10005", 5)}
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), cards, MirrorCatalog{}, mirrorReadAt(1)); err != nil || len(changes.Cards) != len(cards) {
		t.Fatalf("linked cards = %+v, %v", changes, err)
	}
	if linked, more, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10"}); err != nil || more || strings.Join(linked, ",") != "10001,10002,10003,10004,10005" {
		t.Fatalf("linked issues = %v, %v, %v", linked, more, err)
	}

	if _, _, err := s.ClearThreadTicket("unlinked"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("deleted"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("retired"); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT mode FROM threads WHERE id = 'retired'`, "holder")
	seedTransfer(t, s, "moved", "outgoing", "move", "complete", 0)
	if got := mustMirrorCards(t, s, root, "10001", "10002", "10003", "10004", "10005", "10006"); len(got) != 2 {
		t.Fatalf("cards after the links went = %d; want 10005 and 10006", len(got))
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10002,10003,10004,10005,10006")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10005,10006")
}

// The sweep deletes leaf first in transactions of at most a chunk: an
// issue whose changes do not fit has a batch of them deleted with its
// history times and the orders of its status evidence reset, so an issue
// kept again midway has its history read again; it is never deleted.
func TestCleanupSweepIsLeafFirstAndBounded(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	for _, id := range []string{"10001", "10002", "10003"} {
		linkIssue(t, s, "link-"+id, id)
	}
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 5), webCard("10002", 5), webCard("10003", 5)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
		t.Fatal(err)
	}
	for id, n := range map[string]int{"10001": clearMirrorChunk, "10002": 400, "10003": 10} {
		// Answered just now and not final: a reset history is due at once,
		// not after the wait.
		h := statusHistory(id, n)
		h.AnsweredAt, h.Wait = testNow, 60_000
		if _, err := replaceHistory(s, root, h); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ClearThreadTicket("link-" + id); err != nil {
			t.Fatal(err)
		}
	}
	recorder := recordMirrorWrites(t, s)
	pauses := 0
	err := s.FinishMirrorCleanup(context.Background(), func() {
		if pauses++; pauses != 2 {
			return
		}
		assertRows(t, s.db, `SELECT count(*) || ':' || (SELECT history_from || history_for || history_answered_at || history_wait || status_since || status_floor_at || status_read_at
			FROM tracker_cache_issues WHERE issue_id = '10001') FROM tracker_cache_changes WHERE issue_id = '10001'`, "0:0000000")
		linkIssue(t, s, "relinked", "10001")
	})
	if err != nil {
		t.Fatal(err)
	}
	if most, total := recorder.mostDeletedInATransaction(); most > clearMirrorChunk || total != clearMirrorChunk+400+10+2 {
		t.Fatalf("sweep deleted %d rows, at most %d in one transaction", total, most)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, "10001")
	// The reset moved the receipt past the history's write: a history
	// listed before the reset is superseded, one listed since is stored.
	if due, _, err := s.ListMirrorHistoryDue(root, testNow); err != nil || !reflect.DeepEqual(due, []MirrorHistoryDue{{IssueID: "10001", UpdatedAt: 5, Receipt: 2}}) {
		t.Fatalf("history due = %v, %v; want the issue kept again", due, err)
	}
	stale := listedAt(s, root, statusHistory("10001", 3), 1)
	if results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{stale}, MirrorCatalog{}, nextMirrorRead(s, root)); err != nil || !results[0].Superseded {
		t.Fatalf("a history listed before the reset = %+v, %v; want superseded", results, err)
	}
	mustReplaceHistory(t, s, root, statusHistory("10001", 3), MirrorWrite{Changed: true})
}

// A sweep reads a page of unkept issues at a time and its pages together
// remove every one; a kept issue between them stays.
func TestCleanupSweepPages(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	n := 2*mirrorSweepPage + 9
	mustExec(t, s.db, `WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM i WHERE n < ?)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at)
		SELECT ?, printf('1%04d', n), printf('WEB-%d', n), 5 FROM i`, n, root)
	linked := fmt.Sprintf("1%04d", mirrorSweepPage)
	linkIssue(t, s, "linked", linked)
	pauses := 0
	if err := s.FinishMirrorCleanup(context.Background(), func() { pauses++ }); err != nil {
		t.Fatal(err)
	}
	if pauses != 3 {
		t.Fatalf("%d sweep transactions; want 3 pages", pauses)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues`, linked)
}

// No mirror transaction deletes more than a chunk, through cascades
// included, over the largest writes the bounds allow: a reconcile and a
// retire of a board of over two chunks, a history and a structure replaced
// by empty ones, a page of missing rows, a range of worklogs emptied, the
// sweep of issues with full histories, and a detached root's drain.
func TestEveryMirrorDeleteIsBounded(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	recorder := recordMirrorWrites(t, s)
	check := func(what string) {
		t.Helper()
		if most, _ := recorder.mostDeletedInATransaction(); most > clearMirrorChunk {
			t.Fatalf("%s: a transaction deleted %d mirror rows; the bound is %d", what, most, clearMirrorChunk)
		}
		recorder.reset()
	}
	large := mirrorRows(30000, 2*mirrorMaxPageRows+100, "a")
	writeMirrorBoard(t, s, root, "10", large)
	writeMirrorBoard(t, s, root, "11", large)
	for _, row := range large[:3] {
		if _, err := replaceHistory(s, root, statusHistory(row.Card.IssueID, clearMirrorChunk)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{large[0].Card.IssueID}, true, mirrorReadAt(2)); err != nil {
		t.Fatal(err)
	}
	check("reconcile")
	if _, err := replaceHistory(s, root, MirrorHistory{IssueID: large[0].Card.IssueID}); err != nil {
		t.Fatal(err)
	}
	check("history replace")
	full := webStructure()
	full.Board.ID, full.Columns, full.Sprints, full.ReadAt = "12", nil, nil, mirrorReadAt(1)
	for i := range mirrorMaxColumns {
		full.Columns = append(full.Columns, MirrorColumn{Name: fmt.Sprint(i)})
	}
	for i := range mirrorMaxColumnStatuses {
		full.Columns[i%mirrorMaxColumns].Statuses = append(full.Columns[i%mirrorMaxColumns].Statuses, fmt.Sprintf("s%d", i))
	}
	for i := range mirrorMaxSprints {
		full.Sprints = append(full.Sprints, MirrorSprint{ID: fmt.Sprintf("p%d", i), State: MirrorSprintClosed})
	}
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, full)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, MirrorStructure{Board: MirrorBoard{ID: "12"}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	check("structure replace")
	missing := make([]string, 0, mirrorMaxPageRows)
	for _, row := range large[len(large)-mirrorMaxPageRows:] {
		missing = append(missing, row.Card.IssueID)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "11", Missing: missing, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	check("missing rows")
	logs := make([]MirrorWorklog, 0, clearMirrorChunk)
	for i := range clearMirrorChunk {
		logs = append(logs, MirrorWorklog{ID: fmt.Sprintf("w%d", i), IssueID: "1", AuthorID: "acc-user1", Day: "2026-01-05", SpentSeconds: 60})
	}
	for i, l := range [][]MirrorWorklog{logs, nil} {
		if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", "2026-01-05", "2026-01-05", mirrorReadAt(int64(i+1)), l); err != nil {
			t.Fatal(err)
		}
	}
	check("worklog replace")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID)
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	check("board drains and sweep")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10")
	writeMirrorBoard(t, s, root, "10", large)
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	check("root drain")
	if counts := countMirrorRows(t, s.db); mirrorRowTotal(counts) != 0 {
		t.Fatalf("left %v", counts)
	}
}

func TestDeleteTrackerConnectionOfNoMirror(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	if deleted, err := s.DeleteTrackerConnection(linear.ID); err != nil || !slices.Equal(deleted, []string{linear.ID}) {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if _, err := s.GetTrackerConnection(linear.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("connection after the delete: %v", err)
	}
}
