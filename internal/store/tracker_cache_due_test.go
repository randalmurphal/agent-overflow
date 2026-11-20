package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mirrorDue is what ListMirrorHistoryDue returns, each issue as its ID,
// boards and boards revision. It fails unless the sync state's HistoryDue
// agrees, the same rule read the other way.
func mirrorDue(t *testing.T, s *Store, root int64) string {
	t.Helper()
	due, _, err := s.ListMirrorHistoryDue(root, "", testNow)
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
	if results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}); err != nil || results[0].Changed {
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
	if results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}); err != nil || !results[0].Superseded {
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
		if due, _, err := s.ListMirrorHistoryDue(root, "", now); err != nil || len(due) != want {
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
		_, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{valid, ahead}))
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
		list, _, err := s.ListMirrorHistoryDue(root, "", now)
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

// With every issue of a root waiting, both reads of the rule walk the due
// index without a sort and agree on either side of the answer and of the
// wait's end: 4,096 issues read for their update time, answered at
// 7,000 s, not final, waiting a minute.
func TestMirrorWaitingHistoriesUseTheDueIndex(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", nil)
	mustExec(t, s.db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x + 1 FROM n WHERE x < 4096)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at, history_for, history_answered_at, history_wait, history_boards_rev)
		SELECT ?1, printf('%05d', x), 'WEB-' || x, 100000, 100000, 7000000, 60000, 1 FROM n`, root)
	mustExec(t, s.db, `INSERT INTO tracker_cache_board_issues(root_id, board_id, issue_id, read_at)
		SELECT root_id, '10', issue_id, 1 FROM tracker_cache_issues WHERE root_id = ?1`, root)
	for name, query := range map[string]string{"list": mirrorHistoryDueSQL, "sync state": mirrorSyncStateSQL} {
		plan := queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+query, root, "", 7_059_999)
		if !strings.Contains(plan, "idx_tracker_cache_issues_history_due") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatalf("the %s read with every issue waiting does not walk the due index unsorted:\n%s", name, plan)
		}
	}
	for _, now := range []int64{6_999_999, 7_000_000, 7_059_999, 7_060_000, 7_060_001} {
		list, more, err := s.ListMirrorHistoryDue(root, "", now)
		if err != nil {
			t.Fatal(err)
		}
		st, _, err := s.GetMirrorSync(root, "10", now)
		if err != nil {
			t.Fatal(err)
		}
		due := now < 7_000_000 || now >= 7_060_000
		if st.HistoryDue != due || (len(list) > 0) != due || more != due {
			t.Fatalf("at %d: listed %d (more %v), sync state due %v; want due %v", now, len(list), more, st.HistoryDue, due)
		}
	}
}
