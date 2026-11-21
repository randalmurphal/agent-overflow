package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// mirrorDue is what ListMirrorHistoryDue returns, each issue as its ID,
// boards and boards revision. It fails unless the sync state's HistoryDue
// agrees, the same rule read the other way.
func mirrorDue(t *testing.T, s *Store, root int64) string {
	t.Helper()
	due, _, err := s.ListMirrorHistoryDue(root, testNow)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := s.GetMirrorSync(root, "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.HistoryDue != (len(due) > 0) {
		t.Fatalf("the due list %+v and the sync state's HistoryDue %v disagree", due, st.HistoryDue)
	}
	out := make([]string, len(due))
	for i, d := range due {
		out[i] = fmt.Sprintf("{%s %v %d}", d.IssueID, d.Boards, d.BoardsRev)
	}
	return fmt.Sprint(out)
}

// An issue whose history is final through its update time is due again
// when it gains a row on another shown board: estimate changes are per
// board, and the stored history was read for the boards it had.
func TestMirrorIssueGainingABoardIsHistoryDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the history = %s; want none", due)
	}
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after the issue gained board 11 = %s", due)
	}
	// A history read for board 10 alone, listed before the issue gained
	// board 11, lands after: it changes nothing while the stored history
	// is the one it was listed against, is superseded once a later read
	// stored one, and the issue stays due either way.
	early := webHistory("10001")
	early.BoardsRev = 1
	early = listedAt(s, root, early, receiptOf(t, s, root, "10001"))
	if results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}, MirrorCatalog{}, 0); err != nil || results[0].Changed {
		t.Fatalf("a history read for the earlier boards = %+v, %v", results, err)
	}
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after a history read for the earlier boards = %s", due)
	}
	full := webHistory("10001")
	full.BoardsRev = 2
	mustReplaceHistory(t, s, root, full, MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the history for both boards = %s", due)
	}
	if results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}, MirrorCatalog{}, 0); err != nil || !results[0].Superseded {
		t.Fatalf("a history listed before the stored one = %+v, %v; want superseded", results, err)
	}
	// Losing a board does not make it due.
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "11", Missing: []string{"10001"}, ReadAt: 5})); err != nil {
		t.Fatal(err)
	}
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the issue lost board 11 = %s", due)
	}
}

// A staged (long) history records the boards revision it was read for and
// its wait as the batch does, and an identical one read for a later
// revision records it with no change written.
func TestMirrorLongHistoryRecordsItsBoards(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	long := statusHistory("10001", clearMirrorChunk+100)
	mustReplaceHistory(t, s, root, long, MirrorWrite{Changed: true})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after the issue gained board 11 = %s", due)
	}
	long.BoardsRev = 2
	mustReplaceHistory(t, s, root, long, MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the same long history read for both boards = %s", due)
	}
	// One that is not final records its wait, whether only its wait
	// differs from the stored one or it is staged whole: each issue is due
	// once its wait is over.
	long.Wait = 60_000
	mustReplaceHistory(t, s, root, long, MirrorWrite{Changed: true})
	mustWriteRowPage(t, s, root, webPage("10", webRow("10002", 5, "b")), nil)
	staged := statusHistory("10002", clearMirrorChunk+100)
	staged.Wait = 60_000
	mustReplaceHistory(t, s, root, staged, MirrorWrite{Changed: true})
	for now, want := range map[int64]int{long.AnsweredAt + 59_999: 0, long.AnsweredAt + 60_000: 2} {
		if due, _, err := s.ListMirrorHistoryDue(root, now); err != nil || len(due) != want {
			t.Fatalf("due at %d after long histories that are not final = %+v, %v; want %d", now, due, err, want)
		}
	}
}

// A history naming a boards revision its issue has not reached is invalid:
// it would record boards the issue does not have yet as read. The call is
// refused before it stores anything, a short history in a batch and a
// long one the call would stage alike.
func TestMirrorHistoryAheadOfItsBoardsIsRefused(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	for name, ahead := range map[string]MirrorHistory{"short": webHistory("10001"), "long": statusHistory("10001", clearMirrorChunk+100)} {
		ahead.BoardsRev = 2
		valid := webHistory("10002")
		valid.BoardsRev = 1
		_, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{valid, ahead}), MirrorCatalog{}, 0)
		if !errors.Is(err, ErrInvalidTrackerCache) {
			t.Fatalf("%s: a history ahead of its issue's boards: %v; want invalid", name, err)
		}
		assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_changes`, "0")
		if due := mirrorDue(t, s, root); due != "[{10001 [10] 1} {10002 [10] 1}]" {
			t.Fatalf("%s: due after the refusal = %s", name, due)
		}
	}
}

// An issue is due at once when its update time is later than the one its
// stored history was read for (versions the tracker wrote, compared as
// such) or when it gained a board. A read that was not final leaves it
// due once its wait has elapsed since the answer arrived, both times the
// caller's steady clock; an answer after now, which only a later process
// whose wall clock stepped back gives, makes it due at once. A final read
// is never due again, whatever now is. The list and the sync state's
// HistoryDue apply one rule.
func TestMirrorHistoryWaitsUntilAReadCanBeFinal(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	// Tracker times near 100 s; the steady clock near 7,000 s.
	const answered, wait = int64(7_000_000), int64(60_000)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 100_000, "a")})
	due := func(now int64) string {
		t.Helper()
		list, _, err := s.ListMirrorHistoryDue(root, now)
		if err != nil {
			t.Fatal(err)
		}
		st, _, err := s.GetMirrorSync(root, "10", now)
		if err != nil {
			t.Fatal(err)
		}
		if st.HistoryDue != (len(list) > 0) {
			t.Fatalf("at %d the list %v and the sync state's HistoryDue %v disagree", now, list, st.HistoryDue)
		}
		out := make([]string, len(list))
		for i, d := range list {
			out[i] = fmt.Sprintf("%s at %d read for %d answered %d wait %d", d.IssueID, d.UpdatedAt, d.For, d.AnsweredAt, d.Wait)
		}
		return fmt.Sprint(out)
	}
	expect := func(what string, now int64, want string) {
		t.Helper()
		if got := due(now); got != want {
			t.Fatalf("%s: due at %d = %s; want %s", what, now, got, want)
		}
	}
	read := func(at, wait int64) {
		t.Helper()
		mustReplaceHistory(t, s, root, MirrorHistory{IssueID: "10001", AnsweredAt: at, Wait: wait}, MirrorWrite{Changed: true})
	}
	expect("never read", 1, "[10001 at 100000 read for 0 answered 0 wait 0]")
	read(answered, wait)
	expect("just answered", answered, "[]")
	expect("inside the wait", answered+wait-1, "[]")
	expect("the wait over", answered+wait, "[10001 at 100000 read for 100000 answered 7000000 wait 60000]")
	expect("the clock stepped back", answered-1, "[10001 at 100000 read for 100000 answered 7000000 wait 60000]")
	// A board gained in the wait is due at once: the estimate changes of
	// the new board were never read.
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10001", 100_000, "a")})
	expect("a board gained in the wait", answered+1, "[10001 at 100000 read for 100000 answered 7000000 wait 60000]")
	read(answered+1, wait)
	expect("the board's history read", answered+1, "[]")
	// Updated after the answer, within the same second of the tracker's
	// clock: a later version, due at once, and the read of it waits for
	// its own answer's wait.
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 100_500, "a")), nil)
	expect("updated in the wait", answered+2, "[10001 at 100500 read for 100000 answered 7000001 wait 60000]")
	read(answered+2, wait)
	expect("inside the new wait", answered+2+wait-1, "[]")
	expect("the new wait over", answered+2+wait, "[10001 at 100500 read for 100500 answered 7000002 wait 60000]")
	// Final: never due again, whatever the clock says.
	read(answered+70_000, 0)
	expect("final", answered+70_000, "[]")
	expect("final, the clock stepped back", 1, "[]")
	expect("final, much later", answered+1_000_000_000, "[]")
	// A board gained is due at once.
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 100_500, "a")})
	expect("a board gained", answered+70_001, "[10001 at 100500 read for 100500 answered 7070000 wait 0]")
}

// The due fixtures' clock: w issues read for their update time, answered
// at 7,000 s, not final, waiting a minute, and d due for a new version.
const (
	dueAnswered = int64(7_000_000)
	dueWait     = int64(60_000)
)

// seedHistoryDue is a root on board 10 with w issues waiting and d due for
// a new version, every one kept.
func seedHistoryDue(t *testing.T, w, d int) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", nil)
	mustExec(t, s.db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x + 1 FROM n WHERE x < ?2)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, history_for, history_answered_at, history_wait, history_boards_rev)
		SELECT ?1, printf('w%05d', x), 'WEB-' || x, 100000, 100000, ?3, ?4, 1 FROM n WHERE ?2 > 0`, root, w, dueAnswered, dueWait)
	mustExec(t, s.db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x + 1 FROM n WHERE x < ?2)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, history_for, history_boards_rev)
		SELECT ?1, printf('d%05d', x), 'WEB-' || x, 100000, 90000, 1 FROM n WHERE ?2 > 0`, root, d)
	mustExec(t, s.db, `INSERT INTO tracker_cache_board_issues(root_id, board_id, issue_id, read_at)
		SELECT root_id, '10', issue_id, 1 FROM tracker_cache_issues WHERE root_id = ?1`, root)
	return s, root
}

// historyDueCosts are the VM steps of the sync state's due probe and of
// the due listing at now on a root seedHistoryDue seeded.
func historyDueCosts(t *testing.T, w, d int, now int64) (probe, list int) {
	t.Helper()
	s, root := seedHistoryDue(t, w, d)
	return statementVMSteps(t, s, mirrorSyncStateSQL, root, "10", now), statementVMSteps(t, s, mirrorHistoryDueSQL, root, now)
}

// Neither read of the due rule grows with the issues waiting out their
// wait, and neither with the issues due: the probe stops at the first, the
// listing at a page. Each count is pinned against the same read over a
// different number of issues, so a read that visits one issue more per
// issue waiting or due fails.
func TestMirrorHistoryDueReadsCostAPage(t *testing.T) {
	t.Parallel()
	inWait, over, steppedBack := dueAnswered+dueWait-1, dueAnswered+dueWait, dueAnswered-1
	type cost struct{ probe, list int }
	measure := func(w, d int, now int64) cost {
		probe, list := historyDueCosts(t, w, d, now)
		return cost{probe, list}
	}
	for _, d := range []int{0, 1, 4096} {
		one, many := measure(1, d, inWait), measure(4096, d, inWait)
		t.Logf("%d due, 1 and 4,096 waiting: %+v, %+v", d, one, many)
		if one != many {
			t.Errorf("%d due: 4,096 issues inside their wait cost %+v; one costs %+v", d, many, one)
		}
	}
	page := 2 * mirrorHistoryDuePage
	for _, c := range []struct {
		what string
		now  int64
	}{
		{"due for a new version", inWait},
		{"past their wait", over},
		{"answered after now", steppedBack},
	} {
		at := func(n int) cost {
			if c.what == "due for a new version" {
				return measure(0, n, c.now)
			}
			return measure(n, 0, c.now)
		}
		one, two, many := at(1), at(page), at(4096)
		t.Logf("1, %d and 4,096 %s: %+v, %+v, %+v", page, c.what, one, two, many)
		if one.probe != many.probe || two.list != many.list {
			t.Errorf("4,096 issues %s cost %+v; one costs the probe %d and two pages the listing %d", c.what, many, one.probe, two.list)
		}
	}
	s, _ := seedHistoryDue(t, 1, 1)
	for name, read := range map[string]struct {
		query   string
		indexes []string
	}{
		"listing": {mirrorHistoryDueSQL, []string{"SEARCH i USING INDEX idx_tracker_cache_issues_history_waiting (root_id=? AND <expr><?)",
			"SEARCH i USING INDEX idx_tracker_cache_issues_history_answered (root_id=? AND history_answered_at>?)",
			"SEARCH i USING INDEX idx_tracker_cache_issues_history_due (root_id=?)"}},
		"probe": {mirrorSyncStateSQL, []string{"SEARCH hd USING INDEX idx_tracker_cache_issues_history_waiting (root_id=? AND <expr><?)",
			"SEARCH hd USING INDEX idx_tracker_cache_issues_history_answered (root_id=? AND history_answered_at>?)",
			"SEARCH hd USING INDEX idx_tracker_cache_issues_history_due (root_id=?)"}},
	} {
		plan := explainMirror(t, s, read.query)
		for _, index := range read.indexes {
			if !strings.Contains(plan, index) || strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("%s plan:\n%s\nwant %s and no sort", name, plan, index)
			}
		}
	}
}

// The listing lists the issues past their wait first, longest past first,
// then those answered after now, then those of a new version by ID, each
// once though it is due by two arms; it and the sync state agree on either
// side of the answer and of the wait's end; and the next listing after the
// listed issues' reads are stored reaches the issues behind them.
func TestMirrorHistoryDueListsThePageInArmOrder(t *testing.T) {
	t.Parallel()
	s, root := seedHistoryDue(t, mirrorHistoryDuePage, 2)
	for _, now := range []int64{dueAnswered - 1, dueAnswered, dueAnswered + dueWait - 1, dueAnswered + dueWait, dueAnswered + dueWait + 1} {
		list, more, err := s.ListMirrorHistoryDue(root, now)
		if err != nil {
			t.Fatal(err)
		}
		st, _, err := s.GetMirrorSync(root, "10", now)
		if err != nil {
			t.Fatal(err)
		}
		waits := now < dueAnswered || now >= dueAnswered+dueWait
		if want := map[bool]int{false: 2, true: mirrorHistoryDuePage}[waits]; !st.HistoryDue || len(list) != want || more != waits {
			t.Fatalf("at %d: listed %d (more %v), sync state due %v; want %d listed, more %v", now, len(list), more, st.HistoryDue, want, waits)
		}
	}
	// w00001 answered later than the rest, w00002 stepped back, w00003 due
	// for a new version too.
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_answered_at = history_answered_at + 1 WHERE issue_id = 'w00001'`)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_answered_at = ?1 WHERE issue_id = 'w00002'`, dueAnswered+dueWait*2)
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET updated_at = 100001 WHERE issue_id = 'w00003'`)
	mustExec(t, s.db, `DELETE FROM tracker_cache_issues WHERE issue_id > 'w00004' AND issue_id LIKE 'w%'`)
	now := dueAnswered + dueWait + 1
	list, more, err := s.ListMirrorHistoryDue(root, now)
	ids := make([]string, len(list))
	for i, d := range list {
		ids[i] = d.IssueID
	}
	if want := []string{"w00003", "w00004", "w00001", "w00002", "d00001", "d00002"}; err != nil || more || !slices.Equal(ids, want) {
		t.Fatalf("listed %v, more %v, %v; want %v", ids, more, err, want)
	}
	histories := make([]MirrorHistory, len(list))
	for i, d := range list {
		histories[i] = MirrorHistory{IssueID: d.IssueID, For: d.UpdatedAt, AnsweredAt: now, BoardsRev: d.BoardsRev, Receipt: d.Receipt}
	}
	// Another read of the first issue lands first: this read's history of
	// it is superseded, and the other's cleared it.
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), histories[:1], MirrorCatalog{}, 0); err != nil {
		t.Fatal(err)
	}
	if results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), histories, MirrorCatalog{}, 0); err != nil || !results[0].Superseded {
		t.Fatalf("the listed issues' reads: %+v, %v; want the first superseded", results, err)
	}
	if list, more, err := s.ListMirrorHistoryDue(root, now); err != nil || more || len(list) != 0 {
		t.Fatalf("after the listed issues' reads: %+v, %v, %v; want none", list, more, err)
	}
}

// The issues due a history read are the kept ones with none and those
// updated after the update theirs was read for, by ID, a page at most: the
// next listing after the page's histories are stored lists the rest.
func TestListMirrorHistoryDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	rows := make([]MirrorRow, 0, mirrorHistoryDuePage+3)
	for i := range mirrorHistoryDuePage + 3 {
		rows = append(rows, webRow(fmt.Sprintf("1%04d", i), 9, "a"))
	}
	writeMirrorBoard(t, s, root, "10", rows)
	for id, readFor := range map[string]int64{"10000": 9, "10001": 8, "10002": 9} {
		if _, err := replaceHistory(s, root, MirrorHistory{IssueID: id, For: readFor}); err != nil {
			t.Fatal(err)
		}
	}
	first, more, err := s.ListMirrorHistoryDue(root, testNow)
	if err != nil || !more || len(first) != mirrorHistoryDuePage || first[0].IssueID != "10001" || first[1].IssueID != "10003" {
		t.Fatalf("first page = %d IDs from %v, more %v, %v; want %d from 10001, 10003", len(first), first[:2], more, err, mirrorHistoryDuePage)
	}
	for batch := range slices.Chunk(first, mirrorHistoryBatch) {
		histories := make([]MirrorHistory, len(batch))
		for i, d := range batch {
			histories[i] = MirrorHistory{IssueID: d.IssueID, For: d.UpdatedAt, AnsweredAt: testNow, BoardsRev: d.BoardsRev, Receipt: d.Receipt}
		}
		if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), histories, MirrorCatalog{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	rest, more, err := s.ListMirrorHistoryDue(root, testNow)
	if err != nil || more || !reflect.DeepEqual(rest, []MirrorHistoryDue{{IssueID: "10202", Boards: []string{"10"}, BoardsRev: 1, UpdatedAt: 9}}) {
		t.Fatalf("second page = %v, %v, %v", rest, more, err)
	}
	// An unkept issue is not due; an update after the history makes one due.
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10202"}, ReadAt: 2})); err != nil {
		t.Fatal(err)
	}
	if due, more, err := s.ListMirrorHistoryDue(root, testNow); err != nil || more || len(due) != 0 {
		t.Fatalf("after 10202 became unkept = %v, %v, %v", due, more, err)
	}
	mustWriteRowPage(t, s, root, webPage("10", webRow("10000", 10, "a")), nil)
	if due, _, err := s.ListMirrorHistoryDue(root, testNow); err != nil || len(due) != 1 || due[0].IssueID != "10000" {
		t.Fatalf("after an update = %v, %v", due, err)
	}
}

// A due issue carries the boards the mirror shows it on; one only a link
// keeps has none.
func TestMirrorHistoryDueCarriesItsBoards(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	for _, board := range []string{"10", "11", "12"} {
		writeMirrorBoard(t, s, root, board, []MirrorRow{webRow("10001", 9, "a")})
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "12")
	linkIssue(t, s, "linked", "10005")
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10005", 9)}, MirrorCatalog{}, 1); err != nil {
		t.Fatal(err)
	}
	due, more, err := s.ListMirrorHistoryDue(root, testNow)
	if want := []MirrorHistoryDue{{IssueID: "10001", Boards: []string{"10", "12"}, BoardsRev: 3, UpdatedAt: 9}, {IssueID: "10005", UpdatedAt: 9}}; err != nil || more || !reflect.DeepEqual(due, want) {
		t.Fatalf("due = %+v, %v, %v\nwant %+v", due, more, err, want)
	}
}
