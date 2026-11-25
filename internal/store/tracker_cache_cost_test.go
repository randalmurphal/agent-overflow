package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// mirrorStatements are the texts of every cached statement naming a
// mirror table that the package's tests have run.
var mirrorStatements sync.Map

// vmSteps holds, while countMirrorVMSteps or countStatementRuns counts,
// the VM steps the cached statements ran (of query alone when it is set)
// and the runs of query, and each statement's step counter at its last
// run, so a run's steps are the counter's growth since.
var vmSteps struct {
	mu          sync.Mutex
	counting    bool
	query       string
	total, runs int
	last        map[uintptr]int
}

func init() {
	checked := cachedStmtRan
	cachedStmtRan = func(stmt driver.Stmt, query string) {
		if strings.Contains(query, "tracker_cache_") {
			if _, own := statementCallSite(); own {
				mirrorStatements.Store(query, true)
			}
			recordVMSteps(stmt, query)
		}
		if checked != nil {
			checked(stmt, query)
		}
	}
}

// recordVMSteps adds the steps stmt ran since its last run to the count,
// while counting. A counter that went back (statementVMSteps resets it)
// counts from zero.
func recordVMSteps(stmt driver.Stmt, query string) {
	tls, pstmt, err := sqliteStmtHandles(stmt)
	if err != nil || pstmt == 0 {
		return
	}
	now := int(sqlite3.Xsqlite3_stmt_status(tls, pstmt, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 0))
	vmSteps.mu.Lock()
	defer vmSteps.mu.Unlock()
	if vmSteps.last == nil {
		vmSteps.last = map[uintptr]int{}
	}
	last := vmSteps.last[pstmt]
	if now < last {
		last = 0
	}
	if vmSteps.counting && (vmSteps.query == "" || vmSteps.query == query) {
		vmSteps.total += now - last
		vmSteps.runs++
	}
	vmSteps.last[pstmt] = now
}

// countMirrorVMSteps runs fn and returns the VM steps of the cached mirror
// statements it ran. Like countMirrorStatements its callers are not
// parallel.
func countMirrorVMSteps(t *testing.T, fn func()) int {
	t.Helper()
	vmSteps.mu.Lock()
	vmSteps.counting, vmSteps.total = true, 0
	vmSteps.mu.Unlock()
	fn()
	vmSteps.mu.Lock()
	defer vmSteps.mu.Unlock()
	vmSteps.counting = false
	return vmSteps.total
}

// countStatementRuns runs fn and returns how many times it ran the cached
// mirror statement query, and the VM steps of those runs. Like
// countMirrorVMSteps its callers are not parallel.
func countStatementRuns(t *testing.T, query string, fn func()) (runs, steps int) {
	t.Helper()
	vmSteps.mu.Lock()
	vmSteps.counting, vmSteps.query, vmSteps.total, vmSteps.runs = true, query, 0, 0
	vmSteps.mu.Unlock()
	fn()
	vmSteps.mu.Lock()
	defer vmSteps.mu.Unlock()
	vmSteps.counting, vmSteps.query = false, ""
	return vmSteps.runs, vmSteps.total
}

// countMirrorStatements runs fn and returns the statements it ran. The
// count is of every cached statement the process runs (cachedStmtRuns), so
// its callers are not parallel: Go starts the parallel tests after them.
func countMirrorStatements(t *testing.T, fn func()) int {
	t.Helper()
	before := cachedStmtRuns.Load()
	fn()
	return int(cachedStmtRuns.Load() - before)
}

// runEveryMirrorAccessor calls every mirror accessor on s, with work for
// each stage of the cleanup, so each of their statements runs.
func runEveryMirrorAccessor(t *testing.T, s *Store, jira TrackerConnection, root int64) {
	t.Helper()
	seedFullMirror(t, s, root)
	// A process's first read of the root seeds its clock.
	s.forgetMirrorReads()
	if _, err := s.BeginMirrorRead(root, 1); err != nil {
		t.Fatal(err)
	}
	linkIssue(t, s, "linked", "10001")
	for name, write := range mirrorWriters(s) {
		if err := write(root); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The other paths of a long history: staged new, appended in place,
	// shortened, identical, and a staging left for the cleanup.
	for _, h := range []MirrorHistory{statusHistory("10002", clearMirrorChunk+100), statusHistory("10002", clearMirrorChunk+150),
		statusHistory("10002", clearMirrorChunk+150), statusHistory("10002", 3)} {
		if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
			t.Fatal(err)
		}
	}
	// A row naming a sprint the board lacks, and a complete structure read
	// that marks it unexplained.
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10003", 5, "c", "98")}, ReadAt: mirrorReadAt(50)}, nil)
	complete := webStructure()
	complete.ReadAt, complete.Complete = mirrorReadAt(51), true
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, complete)); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_board_unexplained
		WHERE root_id = %d AND board_id = '10' AND kind = 3 AND ref_id = '98' AND named_at = 0`, root), "1")
	// A row only a read by ID placed, which a listing names, read again
	// identical: its card's read time moves alone.
	mustWriteRows(t, s, rowsPage(s, root, 60, nil, webRow("10004", 5, "d")), root)
	mustReconcile(t, s, root, []string{"10004"}, false, 61)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10004", 5, "d")}, ReadAt: mirrorReadAt(62)}, nil)
	assertRows(t, s.db, fmt.Sprintf(`SELECT read_at FROM tracker_cache_issues WHERE root_id = %d AND issue_id = '10004'`, root), "62")
	mustExec(t, s.db, `UPDATE tracker_cache_issues SET history_dirty = 1 WHERE issue_id = '10002'`)
	reads := []func() error{
		func() error { _, _, err := s.GetMirrorRoot(jira.ID); return err },
		func() error { _, _, err := s.GetMirrorSync(root, "10", testNow); return err },
		func() error { _, _, err := s.GetMirrorSync(root, "", testNow); return err },
		func() error { _, _, err := s.ListMirrorHistoryDue(root, testNow); return err },
		func() error { _, err := s.ListMirrorBodies(root, []string{"10001"}); return err },
		func() error {
			_, _, err := s.ListMirrorWorklogs(root, "acc-user1", "2026-01-01", "2026-01-31")
			return err
		},
		func() error { _, _, err := s.ListMirrorIssueWorklogs(root, "10001"); return err },
		func() error { _, _, err := s.GetMirrorBoard(root, "10"); return err },
		func() error { _, err := s.GetMirrorCatalog(root, []string{"acc-user1"}, []string{"1"}); return err },
		func() error { _, err := s.ListMirrorCards(root, []string{"10001"}); return err },
		func() error { _, _, err := s.ListMirrorIssueRows(root, "10001", ""); return err },
		func() error { _, _, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10"}); return err },
		func() error { _, err := s.CountMirrorSections(root, "10"); return err },
		func() error {
			_, _, err := s.ReadMirrorBoardPatch(root, "10", []MirrorSection{{Kind: MirrorSectionSprint, SprintID: "21"},
				{Kind: MirrorSectionBacklog}, {Kind: MirrorSectionColumn}}, []string{"10001", "10002"}, MirrorTotalsAt{})
			return err
		},
		func() error {
			_, err := s.ListMirrorIssueBoards(root, []string{"10001", "10002"}, []string{"10001", "10002"})
			return err
		},
		func() error {
			_, err := s.ReadMirrorRefreshTargets(root, []string{"10001", "10002"}, []string{"10001"}, []string{"10", "11"})
			return err
		},
		func() error {
			_, _, err := s.ReadMirrorBoardView(root, "10", theseSections([]MirrorSection{{Kind: MirrorSectionBacklog}}))
			return err
		},
	}
	for _, section := range []MirrorSection{{Kind: MirrorSectionSprint, SprintID: "21"}, {Kind: MirrorSectionBacklog}, {Kind: MirrorSectionColumn}} {
		reads = append(reads, func() error { _, err := s.ListMirrorSection(root, "10", section, MirrorRowKey{}); return err })
	}
	for i, read := range reads {
		if err := read(); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	// Work for every cleanup stage: an unkept issue, a retired board and a
	// detached root.
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("20001", 5, "a")})
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: mirrorReadAt(3)})); err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DetachMirror(jira.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetOrCreateMirrorRoot(jira.ID); err != nil {
		t.Fatal(err)
	}
}

var (
	// planFullScan matches a plan line that reads a whole table or index
	// (not a virtual table such as json_each, a constant row or a
	// materialized subquery).
	planFullScan = regexp.MustCompile(`^SCAN (\S+)(.*)$`)
	planNewOld   = regexp.MustCompile(`\b(NEW|OLD)\.\w+`)
	planParam    = regexp.MustCompile(`\?(\d*)`)
)

// explainMirror is the plan of query, each of its parameters bound to NULL.
func explainMirror(t *testing.T, s *Store, query string) string {
	t.Helper()
	n := 0
	for _, m := range planParam.FindAllStringSubmatch(query, -1) {
		if m[1] == "" {
			n++
		} else if i, err := strconv.Atoi(m[1]); err == nil {
			n = max(n, i)
		}
	}
	return queryPlanText(t, s.db, "EXPLAIN QUERY PLAN "+query, make([]any, n)...)
}

// mirrorPlanViolations lists the plan lines of query that scan a table,
// walk every row of a root (a key prefix of root_id alone), or sort in a
// temporary b-tree.
func mirrorPlanViolations(t *testing.T, s *Store, query string) []string {
	t.Helper()
	var violations []string
	for line := range strings.SplitSeq(strings.TrimSpace(explainMirror(t, s, query)), "\n") {
		line = strings.TrimSpace(line)
		if m := planFullScan.FindStringSubmatch(line); m != nil && !strings.Contains(m[2], "VIRTUAL TABLE") &&
			m[1] != "CONSTANT" && !strings.HasPrefix(m[1], "(subquery") {
			violations = append(violations, line)
		}
		if strings.HasPrefix(line, "SEARCH ") && strings.HasSuffix(line, "(root_id=?)") {
			violations = append(violations, line)
		}
		if strings.Contains(line, "TEMP B-TREE") {
			violations = append(violations, line)
		}
	}
	return violations
}

// mirrorPlanAllowed are the plan lines that may scan or sort, each for
// the statements containing a fragment (any line when it names none), with
// why its cost is bounded.
var mirrorPlanAllowed = []struct{ fragment, line, why string }{
	{"FROM tracker_cache_boards WHERE retired = 1", "SCAN tracker_cache_boards USING COVERING INDEX idx_tracker_cache_boards_retired",
		"the partial index holds only the retired boards, the work itself"},
	{"i.board_rows = 0 AND NOT", "SCAN i USING COVERING INDEX idx_tracker_cache_issues_boardless",
		"the partial index holds only the issues no board row keeps: those the sweep owes and those only a link keeps"},
	{"ORDER BY 1, 3, 2", "USE TEMP B-TREE FOR ORDER BY",
		"the listed issues with no row, kept in listing order, the unlisted rows read within the settle window or never listed, " +
			"and the listed rows no listing marked: a listing's misses, not the board"},
	{"ORDER BY position, sprint_id LIMIT", "USE TEMP B-TREE FOR ORDER BY", "one board's sprints, at most mirrorMaxSprints"},
	{`DELETE FROM main."tracker_cache_`, "", "a restore empties each mirror table whole"},
	{"SELECT max(0, (SELECT coalesce(max(read_at), 0) FROM tracker_cache_users WHERE root_id = ?1)", "",
		"the read clock's seed (mirrorReadSeedSQL) walks the root's rows of each table holding a read time, once per root per process, at its first read"},
	{" LIMIT 500) AND EXISTS (SELECT 1 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NULL)", "",
		"a detached root's batch reads at most clearMirrorChunk rows of the root"},
	{"ORDER BY EXISTS (SELECT 1 FROM tracker_cache_open_tickets o", "USE TEMP B-TREE FOR ORDER BY",
		"a root's bodies, at most mirrorBodyCap and the bodies one write started, ordered by their keep"},
	{"INDEXED BY idx_tracker_cache_issues_body_opened", "SEARCH b USING INDEX idx_tracker_cache_issues_body_opened (root_id=?)",
		"the partial index holds only the bodies a root holds, at most mirrorBodyCap and the bodies one write started"},
	{"DELETE FROM tracker_cache_roots WHERE id = ? AND connection_id IS NULL", "",
		"the foreign key probes of a drained root, whose tables the drain emptied"},
	{"INSERT INTO tracker_cache_roots", "", "the foreign key probes of a new root, which has no rows"},
	{"FROM tracker_cache_issues WHERE history_dirty = 1", "SCAN tracker_cache_issues USING COVERING INDEX idx_tracker_cache_issues_history_dirty",
		"the partial index holds only the issues whose history staging was cut short, the work itself"},
	{"WHERE i.history_dirty = 1", "SCAN i USING INDEX idx_tracker_cache_issues_history_dirty",
		"the partial index holds only the issues whose history staging was cut short, the work itself"},
	{"EXISTS (SELECT 1 FROM tracker_cache_issues hd WHERE hd.root_id = ?1", "SEARCH hd USING INDEX idx_tracker_cache_issues_history_due (root_id=?)",
		"every entry of the partial index is due a history read, and the probe stops at the first one kept"},
	{"FROM tracker_cache_issues i\n\t\t  WHERE i.root_id = ?1 AND (i.history_for < i.updated_at", "SEARCH i USING INDEX idx_tracker_cache_issues_history_due (root_id=?)",
		"every entry of the partial index is due a history read, and the listing stops at a page"},
	{"WITH h AS MATERIALIZED (SELECT value FROM json_each(?2))", "SCAN h",
		"a write's bound list (the ticket move's, the history times'), read once; it drives the issue lookups by primary key"},
	{"i.status_id IN (SELECT value FROM json_each(?2))", "SEARCH i USING COVERING INDEX idx_tracker_cache_issues_status (root_id=?)",
		"a status entering or leaving the done groups is rare; finding the boards whose rows have it walks the root's statuses once, " +
			"in the index the counts read"},
}

func mirrorPlanLineAllowed(query, line string) bool {
	return slices.ContainsFunc(mirrorPlanAllowed, func(a struct{ fragment, line, why string }) bool {
		return (a.line == "" || a.line == line) && strings.Contains(query, a.fragment)
	})
}

// Every statement the mirror runs, the trigger bodies included, reads by
// key or index range: no plan scans a table or sorts in a temporary
// b-tree, except a sort of a bound input list.
func TestMirrorStatementsReadByKey(t *testing.T) {
	s, jira, root := newMirror(t)
	runEveryMirrorAccessor(t, s, jira, root)

	var queries []string
	mirrorStatements.Range(func(query, _ any) bool {
		queries = append(queries, query.(string))
		return true
	})
	triggers, err := queryIDs(s.db, `SELECT sql FROM sqlite_master WHERE type = 'trigger'
		AND (tbl_name LIKE 'tracker\_cache\_%' ESCAPE '\' OR name = 'trg_project_tracker_boards_retire')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range triggers {
		body := trigger[strings.Index(trigger, "BEGIN")+len("BEGIN") : strings.LastIndex(trigger, "END")]
		for stmt := range strings.SplitSeq(body, ";") {
			if stmt = strings.TrimSpace(stmt); stmt != "" {
				queries = append(queries, planNewOld.ReplaceAllString(stmt, "?"))
			}
		}
	}
	slices.Sort(queries)
	if len(queries) < 60 {
		t.Fatalf("checked %d statements; the accessors ran too few", len(queries))
	}
	for _, query := range queries {
		violations := mirrorPlanViolations(t, s, query)
		violations = slices.DeleteFunc(violations, func(line string) bool { return mirrorPlanLineAllowed(query, line) })
		if len(violations) > 0 {
			t.Errorf("%s\nplan lines that scan or sort:\n%s", strings.TrimSpace(query), strings.Join(violations, "\n"))
		}
	}
}

// The reads the mirror's contracts name walk their indexes.
func TestMirrorReadsUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	for kind, query := range mirrorSectionSQL {
		plan := explainMirror(t, s, query)
		if !strings.Contains(plan, "idx_tracker_cache_board_issues_order (root_id=? AND board_id=? AND (order_key,issue_id)>(?,?))") ||
			strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("section %d plan:\n%s\nwant a range of idx_tracker_cache_board_issues_order and no sort", kind, plan)
		}
	}
	for name, read := range map[string]struct{ query, index string }{
		"sweep candidates": {mirrorSweepCandidatesSQL, "idx_tracker_cache_issues_boardless (root_id=? AND issue_id>?)"},
		"cleanup probe":    {mirrorCleanupPendingSQL, "idx_tracker_cache_boards_retired"},
		"listing check":    {mirrorBoardWriteSQL, "idx_project_tracker_boards_board (connection_id=? AND board_id=?)"},
		"linked issues":    {mirrorOffBoardLinkedSQL, "idx_thread_tickets_issue (kind=? AND site=? AND issue_id>?)"},
		"linked issues' walk of their rows": {mirrorOffBoardLinkedSQL,
			"SEARCH on_board USING PRIMARY KEY (root_id=? AND issue_id=?)\nLIST SUBQUERY"},
		"card read":     {mirrorCardReadSQL, "(root_id=? AND issue_id=?)\nLIST SUBQUERY"},
		"card states":   {mirrorCardStatesSQL, "SEARCH i USING INDEX sqlite_autoindex_tracker_cache_issues_1 (root_id=? AND issue_id=?)"},
		"ticket move":   {mirrorTicketMoveSQL.full, "SCAN h\nSEARCH tracker_cache_issues USING INDEX sqlite_autoindex_tracker_cache_issues_1 (root_id=? AND issue_id=?)"},
		"ticket reads":  {mirrorTicketMoveSQL.reads, "SCAN h\nSEARCH tracker_cache_issues USING INDEX sqlite_autoindex_tracker_cache_issues_1 (root_id=? AND issue_id=?)"},
		"history times": {mirrorHistoryTimesSQL, "SCAN h\nSEARCH tracker_cache_issues USING INDEX sqlite_autoindex_tracker_cache_issues_1 (root_id=? AND issue_id=?)"},
		"issue rows":    {mirrorIssueRowsSQL, "PRIMARY KEY (root_id=? AND issue_id=? AND board_id>?)"},
		"keep rule":     {`SELECT ` + mirrorIssueKept("?1", "?2"), "PRIMARY KEY (root_id=? AND issue_id=?)"},
		"backlog section": {mirrorSectionSQL[MirrorSectionBacklog],
			"SEARCH m USING PRIMARY KEY (root_id=? AND issue_id=?)\nSEARCH sp USING PRIMARY KEY (root_id=? AND board_id=? AND sprint_id=?)"},
		"sprint section": {mirrorSectionSQL[MirrorSectionSprint],
			"SEARCH m USING COVERING INDEX idx_tracker_cache_issue_sprints_sprint (root_id=? AND sprint_id=?)\nCREATE BLOOM FILTER"},
		"backlog rows by ID": {mirrorPatchSQL[MirrorSectionBacklog],
			"SEARCH m USING PRIMARY KEY (root_id=? AND issue_id=?)\nSEARCH sp USING PRIMARY KEY (root_id=? AND board_id=? AND sprint_id=?)"},
		"sprint rows by ID": {mirrorPatchSQL[MirrorSectionSprint], "SEARCH m EXISTS USING PRIMARY KEY (root_id=? AND issue_id=? AND sprint_id=?)"},
		"row count": {mirrorCountRowsSQL, "SEARCH i USING COVERING INDEX idx_tracker_cache_issues_status (root_id=? AND issue_id=?)\n" +
			"LIST SUBQUERY 1\nSEARCH sp USING PRIMARY KEY (root_id=? AND board_id=?)\n" +
			"SEARCH m USING COVERING INDEX idx_tracker_cache_issue_sprints_sprint (root_id=? AND sprint_id=?)"},
		"sprint totals": {mirrorCountSprintsSQL, "SEARCH sp USING PRIMARY KEY (root_id=? AND board_id=?)\n" +
			"SEARCH m USING COVERING INDEX idx_tracker_cache_issue_sprints_sprint (root_id=? AND sprint_id=?)\n" +
			"SEARCH b USING PRIMARY KEY (root_id=? AND issue_id=? AND board_id=?)"},
		"worklogs of a day": {mirrorWorklogSupersededDaysSQL, "PRIMARY KEY (root_id=? AND author_id=? AND day>? AND day<?)"},
	} {
		plan := explainMirror(t, s, read.query)
		if !strings.Contains(plan, read.index) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s plan:\n%s\nwant %s and no sort", name, plan, read.index)
		}
	}
	// The cap's eviction walks the bodies held alone, and sorts those
	// (mirrorPlanAllowed), not the root's issues.
	if plan := explainMirror(t, s, mirrorBodyEvictSQL); !strings.Contains(plan, "SEARCH b USING INDEX idx_tracker_cache_issues_body_opened (root_id=?)") {
		t.Errorf("body eviction plan:\n%s\nwant a walk of idx_tracker_cache_issues_body_opened", plan)
	}
	// A listing probes each listed issue's row by key, which finds the rows
	// to mark; only the arm of the unlisted rows walks the board.
	if plan := explainMirror(t, s, mirrorReconcileFoundSQL); !strings.Contains(plan,
		"SCAN l VIRTUAL TABLE INDEX 1:\nSEARCH b USING PRIMARY KEY (root_id=? AND issue_id=? AND board_id=?) LEFT-JOIN") {
		t.Errorf("listing plan:\n%s\nwant a probe of each listed issue's row by key", plan)
	}
	// A rowid table's drain deletes its batch by rowid; with root_id beside
	// the rowid list the planner walks the root's rows to match the batch.
	for _, table := range mirrorTableKeys {
		if table.keys != "rowid" {
			continue
		}
		if plan := explainMirror(t, s, mirrorRootChunkSQL[table.table]); !strings.Contains(plan, "SEARCH "+table.table+" USING INTEGER PRIMARY KEY (rowid=?)") {
			t.Errorf("%s drain plan:\n%s\nwant a delete by rowid", table.table, plan)
		}
	}
}

// An identical write writes nothing: each writer run a second time with
// the same input writes no row (triggers and foreign key actions
// included) and commits no write.
func TestIdenticalMirrorWritesWriteNothing(t *testing.T) {
	t.Parallel()
	writers := mirrorWriters(nil)
	for name := range writers {
		s, _, root := newMirror(t)
		seedFullMirror(t, s, root)
		write := mirrorWriters(s)[name]
		if err := write(root); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		conn, err := s.reader().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		counters := func() (changes, version int64) {
			t.Helper()
			if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			return changes, version
		}
		recorder := recordMirrorWrites(t, s)
		changes, version := counters()
		if err := write(root); err != nil {
			t.Fatalf("%s again: %v", name, err)
		}
		changesAfter, versionAfter := counters()
		if writes := recorder.rowWrites(); len(writes) > 0 || changesAfter != changes || versionAfter != version {
			t.Errorf("%s again wrote rows %v, total changes %d -> %d, data version %d -> %d", name, writes, changes, changesAfter, version, versionAfter)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
