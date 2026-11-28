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
// off (VM steps and time; it fails when the totals with credit on take more
// than twice the steps, or the pace more than creditPaceSteps), and, as a board that plans no sprints, the
// evidence listing's weeks arm and the weeks credit fill. It measures the
// sprints' fills, pace and totals a second time with the forge history
// credit reads beside them (creditForgeScaleSetup), under a rule that
// counts approvals too, and the forge people's due listing, their match
// write and a forge read's advance of the totals. It restores the
// board's sprints, plan, estimates, update times and what its histories
// were read for, and leaves no forge source and no match.
func creditAtScale(t *testing.T, s *Store, root int64, size benchMirrorSize, rec *mirrorWriteRecorder) {
	t.Helper()
	board := benchBoardID(0)
	week := 7 * benchDay
	window := BenchNowForTest - 182*benchDay
	var histories string
	if err := s.db.QueryRow(`SELECT json_group_array(json_array(issue_id, history_for, history_wait)) FROM tracker_cache_issues WHERE root_id = ?`,
		root).Scan(&histories); err != nil {
		t.Fatal(err)
	}
	// The list, materialized, drives the restore by key (mirrorTicketMoveSQL).
	defer mustExec(t, s.db, `WITH h AS MATERIALIZED (SELECT value FROM json_each(?2))
		UPDATE tracker_cache_issues SET history_for = h.value ->> 1, history_wait = h.value ->> 2
		FROM h WHERE root_id = ?1 AND issue_id = h.value ->> 0`, root, histories)
	members, evidence := creditScaleSetup(t, s, root, size)
	t.Logf("credit: %d members in the window's 26 sprints, %d with evidence", len(members), len(evidence))
	rules := []MirrorCreditRule{{ReviewShare: 10, ReviewNeed: 2, Commented: true, LoggedTime: true, HoursPerPoint: 4}}
	// off is a project that counts no credit, which names no support
	// tickets (trackerapp applies a site's support entry only while the
	// project counts credit); on is the same project counting credit.
	off, err := NewMirrorRegroup(map[string]int{"5": MirrorGroupDone})
	if err != nil {
		t.Fatal(err)
	}
	on, err := off.WithSupport(MirrorSupportRule{Boards: []string{board}})
	if err == nil {
		on, err = on.WithCredit(rules)
	}
	if err != nil {
		t.Fatal(err)
	}
	reset := currentEstimateReset(s, root, board)
	for _, rows := range []string{"every row estimated", "no row estimated", "every row estimated, forge acts", "no row estimated, forge acts"} {
		if rows == "every row estimated, forge acts" {
			mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = ? WHERE root_id = ? AND board_id = ?`, reset, root, board)
			creditForgeScaleSetup(t, s, root, size, rec)
			rules[0].Approved = true
			if on, err = on.WithCredit(rules); err != nil {
				t.Fatal(err)
			}
		}
		resetAt := reset
		if strings.HasPrefix(rows, "no row estimated") {
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
		// The totals a project counting credit reads cost at most twice a
		// project's that counts none, in VM steps (times are reported,
		// not held: they move with the machine's load).
		if on, off := steps["section totals, credit on"], steps["section totals, credit off"]; on > 2*off {
			t.Errorf("credit, %s: the section totals with credit on take %d VM steps, more than twice the %d with it off", rows, on, off)
		}
		if most, ok := creditPaceSteps[size.String()+", "+rows]; ok && steps["pace, credit on"] > most {
			t.Errorf("credit, %s: the pace with credit on takes %d VM steps, more than the %d it took", rows, steps["pace, credit on"], most)
		}
	}
	mustExec(t, s.db, `UPDATE tracker_cache_boards SET estimate_reset_at = ? WHERE root_id = ? AND board_id = ?`, reset, root, board)
	forgeCreditAdvanceAtScale(t, s, root, size, on, rec)
	if _, _, err := s.SetForgeHistorySources(nil, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `DELETE FROM tracker_cache_forge_people WHERE root_id = ?`, root)
	// As a board that plans no sprints: its sprints out of every count, and
	// every done ticket of the board updated in the window (a window from
	// 0: the bench cards' times are before any real one).
	mustExec(t, s.db, `UPDATE tracker_cache_sprints SET state = ? WHERE root_id = ? AND board_id = ?`, MirrorSprintFuture, root, board)
	mustExec(t, s.db, `UPDATE tracker_cache_boards SET plans_sprints = 0 WHERE root_id = ? AND board_id = ?`, root, board)
	read := func(int) {
		mustExec(t, s.db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall)
			SELECT root_id, issue_id, src.value, updated_at, 1, ? FROM tracker_cache_issues, json_each('[1,2]') src WHERE root_id = ?`, testNow, root)
		mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_for = updated_at, history_wait = 0 WHERE root_id = ?`, root)
	}
	read(0)
	creditExportAtScale(t, s, root, board, on, reset, 0, 0, "weeks, every done ticket in the window")
	boards := []MirrorActsBoard{{BoardID: board, Regroup: on}}
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

// creditPaceSteps is the most VM steps the pace with credit on takes, by
// size and rows, as measured when it was last made cheaper: a guard against
// a change that makes it dearer unnoticed. A change that makes it cheaper
// lowers the number.
var creditPaceSteps = map[string]int{
	"1x2000, every row estimated":              154_259,
	"1x2000, no row estimated":                 66_118,
	"1x2000, every row estimated, forge acts":  220_157,
	"1x2000, no row estimated, forge acts":     66_196,
	"1x20000, every row estimated":             674_459,
	"1x20000, no row estimated":                274_018,
	"1x20000, every row estimated, forge acts": 1_800_557,
	"1x20000, no row estimated, forge acts":    274_096,
	"5x4000, every row estimated":              212_061,
	"5x4000, no row estimated":                 89_220,
	"5x4000, every row estimated, forge acts":  395_759,
	"5x4000, no row estimated, forge acts":     89_298,
}

// creditForgeScaleSetup gives the root's connection one repository whose
// pull request history its credit reads, read whole: one pull request
// for each issue of board 0 of a mirror of size, named by the issue's
// key and updated inside the issue's sprint, each with two approvals and
// two comments by four accounts, three of them matched to the people of
// the tracker's evidence, one of those also paired by a person, and one
// to nobody. It stores them a read of 500 at a time, as the forge's reads
// do, and logs the due listing's cost before any account is matched and
// after every one is.
func creditForgeScaleSetup(t *testing.T, s *Store, root int64, size benchMirrorSize, rec *mirrorWriteRecorder) {
	t.Helper()
	forgeCreditSources(t, s, root, []ForgeHistoryRef{testOrigin}, testOrigin)
	week := 7 * benchDay
	start := time.Now()
	for from := 0; from < size.issues; from += 500 {
		read := forgeWhole(testOrigin, nil)
		for i := from; i < min(from+500, size.issues); i++ {
			n := int64(i + 1)
			at := BenchNowForTest - int64(benchBoardSprints-i%benchBoardSprints)*week + benchDay
			key := "WEB-" + benchIssueID(0, i)
			read.PRs = append(read.PRs, testPR(n, key+" Change", "change", at+benchDay, key))
			read.Acts = append(read.Acts, approved(n, "user1", "User 1", at), approved(n, "user2", "User 2", at+1),
				commented(n, "user3", "User 3", at+2), commented(n, "ghost", "Ghost Dev", at+3))
		}
		applyForge(t, s, read)
	}
	t.Logf("credit, forge: %d pull requests with 4 acts each stored in %v", size.issues, time.Since(start).Round(time.Millisecond))
	listed := 0
	listDue := func(int) error {
		due, _, err := s.ListMirrorForgePeopleDue(root, testNow, "acc-user1")
		listed = len(due)
		return err
	}
	// One read for every match: the helper that makes one scans the mirror.
	matchRead := nextMirrorRead(s, root)
	match := func(int) error {
		_, err := s.SetMirrorForgePeople(root, matchRead, []MirrorForgeMatch{
			{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "user1", Name: "User 1"}, Person: "acc-user1"},
			{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "user2", Name: "User 2"}, Person: "acc-user2"},
			{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "user3", Name: "User 3"}, Person: "acc-user3"},
			{MirrorForgePerson: MirrorForgePerson{Host: "github.com", Login: "ghost", Name: "Ghost Dev"}},
		}, MirrorCatalog{}, testNow, []string{benchBoardID(0)})
		return err
	}
	for _, c := range []struct {
		name    string
		prepare func(int)
		want    int
	}{{"none matched", func(int) { mustExec(t, s.db, `DELETE FROM tracker_cache_forge_people WHERE root_id = ?`, root) }, 4},
		{"every one matched", func(int) {
			if err := match(0); err != nil {
				t.Fatal(err)
			}
		}, 0}} {
		cost := measureScale(t, rec, c.prepare, listDue)
		c.prepare(0)
		var err error
		steps := countMirrorVMSteps(t, func() { err = listDue(0) })
		if err != nil || listed != c.want {
			t.Fatalf("forge people due, %s: %d listed, %v; want %d", c.name, listed, err, c.want)
		}
		t.Logf("credit, forge: people due, %-18s %9d VM steps %s, %d listed", c.name, steps, cost, listed)
	}
	clear := func(int) { mustExec(t, s.db, `DELETE FROM tracker_cache_forge_people WHERE root_id = ?`, root) }
	t.Logf("credit, forge: a match of 4 accounts                    %s", measureScale(t, rec, clear, match))
	// A person paired one account, with the person its name matches: the
	// credit measured after reads a pairing for one act in four and finds
	// none for the rest.
	if _, err := s.SetTrackerForgePairing(TrackerForgePairing{ConnectionID: mirrorConnection(t, s, root), Host: "github.com", Login: "user1", Person: "acc-user1", PersonName: "User 1"}, nil); err != nil {
		t.Fatal(err)
	}
}

// forgeCreditAdvanceAtScale measures what a forge read's change costs
// the mirror of size: AdvanceMirrorForgeCredit for board 0 under regroup
// with the keys of a read's 500 pull requests, and with all.
func forgeCreditAdvanceAtScale(t *testing.T, s *Store, root int64, size benchMirrorSize, regroup MirrorRegroup, rec *mirrorWriteRecorder) {
	t.Helper()
	boards := []MirrorActsBoard{{BoardID: benchBoardID(0), Regroup: regroup}}
	keys := make([]string, 0, 500)
	for i := size.issues - 500; i < size.issues; i++ {
		keys = append(keys, "WEB-"+benchIssueID(0, i))
	}
	told := 0
	for _, c := range []struct {
		name string
		keys []string
		all  bool
	}{{"500 keys", keys, false}, {"all", nil, true}} {
		advance := func(int) error {
			issues, err := s.AdvanceMirrorForgeCredit(root, boards, c.keys, c.all)
			told = len(issues)
			return err
		}
		cost := measureScale(t, rec, nil, advance)
		var err error
		steps := countMirrorVMSteps(t, func() { err = advance(0) })
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("credit, forge: totals advanced by %-8s %9d VM steps %s, %d tickets named", c.name, steps, cost, told)
	}
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
	args := namedArgs(mirrorExportArgs(root, board, regroup, reset, true, from, window, math.MaxInt64, MirrorLoggedDays{}, CardShapeForTest))
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
// sprints and evidence: the members of the window's sprints, each with
// its history and receipts read for its card, and those of them with
// evidence.
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
	// Each member's history, read for its card as it now is, as the
	// history unit reads a changed card.
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_for = updated_at, history_wait = 0
		WHERE root_id = ? AND issue_id IN (SELECT value FROM json_each(?))`, root, list)
	return members, evidence
}

// cardsMovedAtScale measures a board read that writes every card of board
// 0 of a mirror of size again at a later update time, in pages of 100 as
// the first sync writes them: with every card's evidence receipts for its
// card before the read, so the credit card trigger
// (trg_tracker_cache_credit_card_moved) advances the board's totals for
// each; with every card's history final for it instead, which advances
// them for each member of a closed sprint; with neither, its probe alone;
// and each with the trigger dropped. It restores the trigger and leaves
// no receipts and no history read for a card.
func cardsMovedAtScale(t *testing.T, s *Store, root int64, size benchMirrorSize, rec *mirrorWriteRecorder) {
	t.Helper()
	board := benchBoardID(0)
	var trigger string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_tracker_cache_credit_card_moved'`).Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	updated := int64(2_000_000_000_000)
	write := func(int) error {
		updated++
		rows := make([]MirrorRow, size.issues)
		for i := range rows {
			rows[i] = benchRow(0, i, updated)
		}
		for page := range slices.Chunk(rows, 100) {
			if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: board, Rows: page, ReadAt: nextMirrorRead(s, root)}), []byte("cursor")); err != nil {
				return err
			}
		}
		return nil
	}
	receipts := func(int) {
		mustExec(t, s.db, `INSERT OR REPLACE INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall)
			SELECT i.root_id, i.issue_id, src.value, i.updated_at, 1, ? FROM tracker_cache_board_issues b
			CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id, json_each('[1,2]') src
			WHERE b.root_id = ? AND b.board_id = ?`, testNow, root, board)
	}
	none := func(int) {
		mustExec(t, s.db, `DELETE FROM tracker_cache_issue_act_reads WHERE root_id = ?`, root)
		mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_for = 0, history_wait = 0 WHERE root_id = ?`, root)
	}
	histories := func(int) {
		none(0)
		mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_for = updated_at WHERE root_id = ?`, root)
	}
	revision := func() (n int64) {
		if err := s.db.QueryRow(`SELECT totals_revision FROM tracker_cache_boards WHERE root_id = ? AND board_id = ?`, root, board).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		name    string
		prepare func(int)
		drop    bool
	}{{"trigger dropped, every receipt passed", receipts, true}, {"every receipt passed", receipts, false},
		{"trigger dropped, every history passed", histories, true}, {"every history passed", histories, false},
		{"trigger dropped, no receipts", none, true}, {"no receipts", none, false}} {
		mustExec(t, s.db, `DROP TRIGGER IF EXISTS trg_tracker_cache_credit_card_moved`)
		if !c.drop {
			mustExec(t, s.db, trigger)
		}
		// A write first, unmeasured: the first after the other paths
		// does work of their state that no later one does.
		c.prepare(0)
		if err := write(0); err != nil {
			t.Fatal(err)
		}
		cost := measureScale(t, rec, c.prepare, write)
		c.prepare(0)
		before := revision()
		var err error
		steps := countMirrorVMSteps(t, func() { err = write(0) })
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("credit, a board read writing %d cards, %-38s %10d VM steps %s, totals revision +%d", size.issues, c.name, steps, cost, revision()-before)
	}
	none(0)
}
