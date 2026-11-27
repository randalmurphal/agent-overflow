package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// creditAtScale measures credit on board 0 of a mirror of size, as a
// project counting it sees the board: 26 weekly sprints inside a 182-day
// window (25 closed and the active one, the others moved out of it as
// future sprints), every member's status at its completion regrouped as
// done and its card last changed inside its sprint, and 520 of those
// members with evidence (every one at 2,000
// issues, an even spread at more), each with two comments and two
// worklogs by three people inside its sprint; every other member is read
// with nobody's evidence. With a support rule naming the board, first
// while every row is estimated, so no ticket is support, then with none
// estimated, so every one is. It measures the export's credit fills (VM
// steps, rows, bytes), the pace and the section totals with credit on and
// off (VM steps and time), and, as a board that plans no sprints, the
// evidence listing's weeks arm and the weeks credit fill. It restores the
// board's sprints, plan, estimates and update times.
func creditAtScale(t *testing.T, s *Store, root int64, size benchMirrorSize, rec *mirrorWriteRecorder) {
	t.Helper()
	board := benchBoardID(0)
	week := 7 * benchDay
	window := BenchNowForTest - 182*benchDay
	members, evidence := creditScaleSetup(t, s, root, size)
	t.Logf("credit: %d members in the window's 26 sprints, %d with evidence", len(members), len(evidence))
	rules := []MirrorCreditRule{{ReviewShare: 10, ReviewNeed: 2, Commented: true, LoggedTime: true, HoursPerPoint: 4}}
	off, err := NewMirrorRegroup(map[string]int{"5": MirrorGroupDone})
	if err != nil {
		t.Fatal(err)
	}
	if off, err = off.WithSupport(MirrorSupportRule{Boards: []string{board}}); err != nil {
		t.Fatal(err)
	}
	on, err := off.WithCredit(rules)
	if err != nil {
		t.Fatal(err)
	}
	reset := currentEstimateReset(s, root, board)
	for _, rows := range []string{"every row estimated", "no row estimated"} {
		resetAt := reset
		if rows == "no row estimated" {
			resetAt = reset + 1_000_000_000_000
			mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = ? WHERE root_id = ? AND board_id = ?`, resetAt, root, board)
		}
		creditExportAtScale(t, s, root, board, on, resetAt, window-26*week, window, "sprints, "+rows)
		out := map[string]scaleCost{}
		steps := map[string]int{}
		for _, c := range []struct {
			name    string
			regroup MirrorRegroup
		}{{"credit off", off}, {"credit on", on}} {
			regroup := c.regroup
			pace := func(int) error {
				_, err := mirrorReadSnapshot(s, "pace", func(tx sqlQueryer) ([]MirrorSprintFinished, error) {
					shown, _, err := readMirrorShownBoardTx(tx, root, board)
					if err != nil {
						return nil, err
					}
					return readMirrorPaceTx(tx, root, board, shown, regroup)
				})
				return err
			}
			counts := func(int) error { _, err := s.CountMirrorSections(root, board, regroup); return err }
			out["pace, "+c.name] = measureScale(t, rec, nil, pace)
			steps["pace, "+c.name] = countMirrorVMSteps(t, func() { err = pace(0) })
			out["section totals, "+c.name] = measureScale(t, rec, nil, counts)
			steps["section totals, "+c.name] = countMirrorVMSteps(t, func() { err = errors.Join(err, counts(0)) })
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(out)) {
			t.Logf("credit, %s: %-28s %9d VM steps %s", rows, name, steps[name], out[name])
		}
		for _, what := range []string{"pace", "section totals"} {
			if on, off := out[what+", credit on"].median(), out[what+", credit off"].median(); on > 2*off {
				t.Logf("credit, %s: %s with credit on takes %s, more than twice the %s with it off", rows, what, on, off)
			}
		}
	}
	mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = ? WHERE root_id = ? AND board_id = ?`, reset, root, board)
	// As a board that plans no sprints: its sprints out of every count, and
	// every done ticket of the board updated in the window (a window from
	// 0: the bench cards' times are before any real one).
	mustExec(t, s.db, `UPDATE tracker_cache_sprints SET state = ? WHERE root_id = ? AND board_id = ?`, MirrorSprintFuture, root, board)
	mustExec(t, s.db, `UPDATE tracker_cache_boards SET plans_sprints = 0 WHERE root_id = ? AND board_id = ?`, root, board)
	read := func(int) {
		mustExec(t, s.db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall)
			SELECT root_id, issue_id, src.value, updated_at, 1, ? FROM tracker_cache_issues, json_each('[1,2]') src WHERE root_id = ?`, testNow, root)
	}
	read(0)
	creditExportAtScale(t, s, root, board, on, reset, 0, 0, "weeks, every done ticket in the window")
	boards := []MirrorActsBoard{{BoardID: board, Regroup: off}}
	listed := 0
	listDue := func(int) error {
		due, _, err := s.ListMirrorActsDue(root, boards, 0, testNow)
		listed = len(due)
		return err
	}
	for _, c := range []struct {
		name    string
		prepare func(int)
	}{{"none read", func(int) { mustExec(t, s.db, `DELETE FROM tracker_cache_issue_act_reads WHERE root_id = ?`, root) }}, {"every one read", read}} {
		cost := measureScale(t, rec, c.prepare, listDue)
		c.prepare(0)
		steps := countMirrorVMSteps(t, func() { err = listDue(0) })
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("credit, weeks: acts due, %-16s %9d VM steps %s, %d listed", c.name, steps, cost, listed)
	}
	mustExec(t, s.db, `UPDATE tracker_cache_boards SET plans_sprints = 1 WHERE root_id = ? AND board_id = ?`, root, board)
	mustExec(t, s.db, `UPDATE tracker_cache_sprints SET state = iif(CAST(sprint_id AS INTEGER) % 100 = ?3, ?4, ?5), start_at = 1000, end_at = 2000,
		completed_at = iif(CAST(sprint_id AS INTEGER) % 100 = ?3, 0, 2100) WHERE root_id = ?1 AND board_id = ?2`,
		root, board, benchBoardSprints-1, MirrorSprintActive, MirrorSprintClosed)
	mustExec(t, s.db, `DELETE FROM tracker_cache_issue_acts WHERE root_id = ?`, root)
	list, err := encodeMirrorIDs(members)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET updated_at = 1000 WHERE root_id = ? AND issue_id IN (SELECT value FROM json_each(?))`, root, list)
}

// creditExportAtScale fills a copy of board's credit by regroup, its
// estimate reset reset, from and window the export's earliest instant and
// window, and logs each fill's VM steps and best time of five, the credit
// rows by kind and the credit relation's bytes.
func creditExportAtScale(t *testing.T, s *Store, root int64, board string, regroup MirrorRegroup, reset, from, window int64, name string) {
	t.Helper()
	ctx := context.Background()
	set, err := mirrorExportRelations([]string{"credit"})
	if err != nil {
		t.Fatal(err)
	}
	args := namedArgs(mirrorExportArgs(root, board, regroup, reset, true, from, window, math.MaxInt64, CardShapeForTest))
	steps, best := map[string]int{}, map[string]time.Duration{}
	var order []string
	var kinds string
	var bytes int64
	for range 5 {
		err := func() (err error) {
			conn, closeConn, err := newExportConn()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, closeConn()) }()
			for _, q := range []string{MirrorExportSchema, `ATTACH '` + mirrorExportURI(s.path) + `' AS ` + mirrorExportSchemaName} {
				if _, err := conn.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			err = runFills(ctx, conn, set, args, func(f mirrorExportFill, n int, d time.Duration) {
				if _, seen := steps[f.name]; !seen {
					order = append(order, f.name)
				}
				steps[f.name] = n
				if best[f.name] == 0 || d < best[f.name] {
					best[f.name] = d
				}
			})
			if err != nil {
				return err
			}
			kinds = strings.Join(exportRows(t, conn, `SELECT kind || ' ' || count(*) || ' (' || count(value) || ' counted)' FROM credit GROUP BY kind ORDER BY kind`), ", ")
			return conn.QueryRowContext(ctx, `SELECT coalesce(sum(pgsize), 0) FROM dbstat('main') WHERE name = 'credit'`).Scan(&bytes)
		}()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("credit export, %s: rows %s; %d bytes", name, kinds, bytes)
	for _, f := range order {
		t.Logf("credit export, %s: %-24s %10d VM steps, best %v", name, f, steps[f], best[f].Round(time.Microsecond))
	}
}

// creditScaleSetup gives board 0 of a mirror of size creditAtScale's
// sprints and evidence: the members of the window's sprints, and those of
// them with evidence.
func creditScaleSetup(t *testing.T, s *Store, root int64, size benchMirrorSize) (members, evidence []string) {
	t.Helper()
	board := benchBoardID(0)
	week := 7 * benchDay
	now := BenchNowForTest
	const first = benchBoardSprints - 26
	// The sprints: k from first on inside the window, a week each, the last
	// active; the ones before it future, which credit and the pace skip.
	mustExec(t, s.db, `UPDATE tracker_cache_sprints SET state = ?3, start_at = 0, end_at = 0, completed_at = 0
		WHERE root_id = ?1 AND board_id = ?2 AND CAST(sprint_id AS INTEGER) % 100 < ?4`, root, board, MirrorSprintFuture, first)
	for k := first; k < benchBoardSprints; k++ {
		start := now - int64(benchBoardSprints-k)*week
		state, completed := MirrorSprintClosed, start+week
		if k == benchBoardSprints-1 {
			state, completed = MirrorSprintActive, 0
		}
		mustExec(t, s.db, `UPDATE tracker_cache_sprints SET state = ?, start_at = ?, end_at = ?, completed_at = ? WHERE root_id = ? AND sprint_id = ?`,
			state, start, start+week, completed, root, fmt.Sprint(k))
	}
	stride := max(1, size.issues/2000)
	for i := range size.issues {
		if i%benchBoardSprints < first {
			continue
		}
		id := benchIssueID(0, i)
		members = append(members, id)
		start := now - int64(benchBoardSprints-i%benchBoardSprints)*week
		mustExec(t, s.db, `UPDATE tracker_cache_issues SET updated_at = ? WHERE root_id = ? AND issue_id = ?`, start+2*benchDay, root, id)
		if (i/benchBoardSprints)%stride != 0 {
			continue
		}
		evidence = append(evidence, id)
		for a, act := range []struct {
			source  int
			person  string
			seconds int
		}{{MirrorActComment, "acc-user1", 0}, {MirrorActComment, "acc-user2", 0}, {MirrorActWorklog, "acc-user1", 3600}, {MirrorActWorklog, "acc-user3", 7200}} {
			mustExec(t, s.db, `INSERT INTO tracker_cache_issue_acts(root_id, issue_id, source, act_id, person, at, seconds) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				root, id, act.source, fmt.Sprint("a", a), act.person, start+benchDay+int64(a)*3_600_000, act.seconds)
		}
	}
	list, err := encodeMirrorIDs(members)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall)
		SELECT root_id, issue_id, src.value, updated_at, 1, ? FROM tracker_cache_issues, json_each('[1,2]') src
		WHERE root_id = ? AND issue_id IN (SELECT value FROM json_each(?))`, testNow, root, list)
	return members, evidence
}
