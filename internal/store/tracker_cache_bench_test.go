package store

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The mirror's benchmarks run against a store file holding a realistic
// mirror: one root, five boards of 1,000 issues each with 100 sprints a
// board, ten changes an issue, 2,000 worklogs, and 50 issues only a
// thread's link keeps. Each reports the statements one call runs
// (stmts/op).
//
//	make go-test GO_TEST_PKGS=./internal/store GO_TEST_FLAGS="-run '^$' -bench Mirror -benchmem"

const (
	benchBoards       = 5
	benchBoardIssues  = 1000
	benchBoardSprints = 100
	benchChanges      = 10
	benchWorklogDays  = 20
	benchDayWorklogs  = 100
	benchLinkedOnly   = 50
)

var benchMirrorFixture struct {
	once sync.Once
	path string
	err  error
}

// benchIssueID is issue i of board (0 to benchBoards-1).
func benchIssueID(board, i int) string { return strconv.Itoa(100000 + board*benchBoardIssues + i) }

func benchBoardID(board int) string { return strconv.Itoa(10 + board) }

// benchCard is a card the size of a real one: its data holds a title,
// labels and the fields the pane shows that no query reads.
func benchCard(id string, updated int64, board, i int) MirrorCard {
	return MirrorCard{IssueID: id, Key: "WEB-" + id, ProjectID: "100", TypeID: "10001", TypeLevel: 0,
		StatusID: strconv.Itoa(1 + i%5), AssigneeID: fmt.Sprint("acc-user", i%20), PriorityID: "3", ParentID: "",
		CreatedAt: 1_700_000_000_000, UpdatedAt: updated, Sprints: []string{fmt.Sprint(board*benchBoardSprints + i%benchBoardSprints)},
		Data: []byte(`{"summary":"Make the login page remember the last account used ` + id + `","labels":["frontend","auth"],` +
			`"components":["web"],"fixVersions":["2026.11"],"flagged":false,"commentCount":3,"attachmentCount":1}`)}
}

func benchRow(board, i int, updated int64) MirrorRow {
	estimate := float64(i % 8)
	return MirrorRow{Card: benchCard(benchIssueID(board, i), updated, board, i), OrderKey: fmt.Sprintf("0|i%05d:", i), Estimate: &estimate}
}

func benchStructure(board int) MirrorStructure {
	st := MirrorStructure{Board: MirrorBoard{ID: benchBoardID(board), ProjectID: "100", Name: "WEB board " + benchBoardID(board),
		PlansSprints: true, EstimateUnit: MirrorEstimatePoints, Data: []byte(`{"choices":[1,2,3,5,8]}`)}, ReadAt: 1}
	for c := range 5 {
		st.Columns = append(st.Columns, MirrorColumn{Name: fmt.Sprint("Column ", c), Statuses: []string{strconv.Itoa(1 + c)}})
		st.Catalog.Statuses = append(st.Catalog.Statuses, MirrorStatus{ID: strconv.Itoa(1 + c), Name: fmt.Sprint("Status ", c), Group: 1 + c})
	}
	for i := range benchBoardSprints {
		state := MirrorSprintClosed
		if i == benchBoardSprints-1 {
			state = MirrorSprintActive
		}
		st.Sprints = append(st.Sprints, MirrorSprint{ID: strconv.Itoa(board*benchBoardSprints + i), OriginBoardID: benchBoardID(board),
			Name: fmt.Sprint("WEB Sprint ", i), State: state, StartAt: 1000, EndAt: 2000, CompletedAt: 2100})
	}
	for u := range 20 {
		st.Catalog.Users = append(st.Catalog.Users, MirrorUser{ID: fmt.Sprint("acc-user", u), Name: fmt.Sprint("User ", u), Data: []byte(`{"active":true}`)})
	}
	return st
}

func benchHistory(id string, at int64, n int) MirrorHistory {
	h := MirrorHistory{IssueID: id, AnsweredAt: at + int64(n)}
	for c := range n {
		kind := MirrorChangeStatus
		if c%2 == 1 {
			kind = MirrorChangeAssignee
		}
		h.Changes = append(h.Changes, MirrorChange{Kind: kind, At: at + int64(c), AuthorID: "acc-user1", From: strconv.Itoa(c%5 + 1), To: strconv.Itoa((c+1)%5 + 1)})
	}
	return answeredHistory(h)
}

// buildBenchMirror writes the fixture through the store's own writers.
func buildBenchMirror(path string) error {
	if err := copyTestStoreTemplate(path); err != nil {
		return err
	}
	s, err := New(path)
	if err != nil {
		return err
	}
	err = fillBenchMirror(s)
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fillBenchMirror(s *Store) error {
	jira, err := s.CreateTrackerConnection(jiraConnection(mirrorSite, "user1@example.com", 10))
	if err != nil {
		return err
	}
	root, _, err := s.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		return err
	}
	listing := []ProjectTrackerBoard{}
	for board := range benchBoards {
		listing = append(listing, ProjectTrackerBoard{ConnectionID: jira.ID, BoardID: benchBoardID(board)})
	}
	if _, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, listing); err != nil {
		return err
	}
	for board := range benchBoards {
		if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, benchStructure(board))); err != nil {
			return err
		}
		rows := make([]MirrorRow, 0, benchBoardIssues)
		for i := range benchBoardIssues {
			rows = append(rows, benchRow(board, i, 1000))
		}
		for page := range slices.Chunk(rows, 100) {
			if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: benchBoardID(board), Rows: page, ReadAt: 1}), []byte("cursor")); err != nil {
				return err
			}
		}
		for first := 0; first < benchBoardIssues; first += mirrorHistoryBatch {
			batch := make([]MirrorHistory, 0, mirrorHistoryBatch)
			for i := first; i < min(first+mirrorHistoryBatch, benchBoardIssues); i++ {
				batch = append(batch, benchHistory(benchIssueID(board, i), 100, benchChanges))
			}
			if _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, batch)); err != nil {
				return err
			}
		}
	}
	for i := range benchLinkedOnly {
		id := strconv.Itoa(900000 + i)
		if err := s.CreateThread(makeThread("linked-"+id, "claude")); err != nil {
			return err
		}
		if _, err := s.SetThreadTicket(ThreadTicket{ThreadID: "linked-" + id, Ticket: TicketRef{Kind: TrackerKindJira, Site: mirrorSite, IssueID: id},
			IssueKey: "WEB-" + id, Origin: TicketOriginManual, LinkedAt: 10}); err != nil {
			return err
		}
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{benchCard(id, 1000, 0, i)}, MirrorCatalog{}, 1); err != nil {
			return err
		}
	}
	for d := range benchWorklogDays {
		day := fmt.Sprintf("2026-01-%02d", d+1)
		logs := make([]MirrorWorklog, 0, benchDayWorklogs)
		for w := range benchDayWorklogs {
			logs = append(logs, MirrorWorklog{ID: fmt.Sprintf("w%02d%03d", d, w), IssueID: benchIssueID(0, w), AuthorID: "acc-user1", Day: day,
				SpentSeconds: 1800, Description: "Review and fix the login flow", UpdatedAt: 5})
		}
		if _, err := s.ReplaceMirrorWorklogs(root, "acc-user1", day, day, 1, logs); err != nil {
			return err
		}
	}
	return nil
}

// openBenchMirror opens a copy of the fixture, with its root.
func openBenchMirror(b *testing.B) (*Store, int64) {
	s, root, _ := openBenchMirrorFile(b)
	return s, root
}

// openBenchMirrorFile opens a copy of the fixture at path, with its root.
func openBenchMirrorFile(b *testing.B) (s *Store, root int64, path string) {
	b.Helper()
	f := &benchMirrorFixture
	f.once.Do(func() {
		f.path = filepath.Join(filepath.Dir(testStoreTemplatePath), "bench-mirror.sqlite")
		f.err = buildBenchMirror(f.path)
	})
	if f.err != nil {
		b.Fatal(f.err)
	}
	path = filepath.Join(b.TempDir(), "store.sqlite")
	if err := copyStoreFile(f.path, path); err != nil {
		b.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	root, ok, err := s.GetMirrorRoot(benchConnectionID(b, s))
	if err != nil || !ok {
		b.Fatalf("bench root: %v, %v", ok, err)
	}
	return s, root, path
}

func benchConnectionID(b *testing.B, s *Store) string {
	b.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM tracker_connections WHERE kind = 'jira'`).Scan(&id); err != nil {
		b.Fatal(err)
	}
	return id
}

// measureMirror runs op b.N times with the package's per-statement test
// check off, and reports the statements each run ran.
func measureMirror(b *testing.B, op func(i int) error) {
	b.Helper()
	var runs int64
	checked := cachedStmtRan
	cachedStmtRan = func(driver.Stmt, string) { runs++ }
	defer func() { cachedStmtRan = checked }()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := op(i); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(runs)/float64(b.N), "stmts/op")
}

// benchApplied fails a row write that stored other than cards cards and
// rows rows, or superseded or refused anything: a benchmark of a write
// that silently did not apply measures nothing.
func benchApplied(got MirrorChanges, err error, cards, rows int) error {
	switch {
	case err != nil:
		return err
	case len(got.Cards) != cards || len(got.Rows) != rows || len(got.Superseded) > 0 || got.CursorRefused:
		return fmt.Errorf("the write applied %d cards and %d rows, superseded %d, cursor refused %v; want %d cards and %d rows",
			len(got.Cards), len(got.Rows), len(got.Superseded), got.CursorRefused, cards, rows)
	}
	return nil
}

func benchPage(board int, rows []MirrorRow) MirrorRowPage {
	return MirrorRowPage{BoardID: benchBoardID(board), Rows: rows, ReadAt: 1}
}

// A 100-row page: all new, identical to what is stored from the same read
// or from a later one (each row's read time moves), or with 10 rows changed.
func BenchmarkMirrorWritePage(b *testing.B) {
	b.Run("new", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(i int) error {
			rows := make([]MirrorRow, 100)
			for r := range rows {
				rows[r] = benchRow(0, benchBoardIssues*benchBoards+i*100+r, 1000)
			}
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 100, 100)
		})
	})
	b.Run("identical", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(int) error {
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 0, 0)
		})
	})
	b.Run("identical-later", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			page := benchPage(0, rows)
			page.ReadAt = int64(2 + i)
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), []byte("cursor"))
			return benchApplied(got, err, 0, 0)
		})
	})
	b.Run("10-changed", func(b *testing.B) {
		s, root := openBenchMirror(b)
		rows := make([]MirrorRow, 100)
		for r := range rows {
			rows[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			for r := range 10 {
				rows[r*10].Card.UpdatedAt = int64(2000 + i)
			}
			got, err := s.WriteMirrorRowPage(root, currentPage(s, root, benchPage(0, rows)), []byte("cursor"))
			return benchApplied(got, err, 10, 0)
		})
	})
}

// A complete reconcile of a 1,000-issue board: nothing to do, and 50 rows
// removed (put back between runs, untimed).
func BenchmarkMirrorReconcile(b *testing.B) {
	listing := make([]string, benchBoardIssues)
	for i := range listing {
		listing[i] = benchIssueID(0, i)
	}
	b.Run("nothing", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(int) error {
			_, err := s.ReconcileMirrorBoard(root, benchBoardID(0), currentLifetime(s, root, benchBoardID(0)), listing, true, 1)
			return err
		})
	})
	b.Run("50-removed", func(b *testing.B) {
		s, root := openBenchMirror(b)
		back := make([]MirrorRow, 50)
		for r := range back {
			back[r] = benchRow(0, r, 1000)
		}
		measureMirror(b, func(i int) error {
			got, err := s.ReconcileMirrorBoard(root, benchBoardID(0), currentLifetime(s, root, benchBoardID(0)), listing[50:], true, int64(10+2*i))
			if err != nil || len(got.Removed) != 50 {
				return fmt.Errorf("removed %d, %v", len(got.Removed), err)
			}
			b.StopTimer()
			page := benchPage(0, back)
			page.ReadAt = int64(11 + 2*i)
			put, err := s.WriteMirrorRows(root, currentPage(s, root, page))
			b.StartTimer()
			return benchApplied(put, err, 0, 50)
		})
	})
}

// Histories of 20 changes replacing ones of 20 with other times: one
// issue, and a read batch of mirrorHistoryBatch.
func BenchmarkMirrorReplaceHistory(b *testing.B) {
	for _, n := range []int{1, mirrorHistoryBatch} {
		b.Run(fmt.Sprint(n, "-issues"), func(b *testing.B) {
			s, root := openBenchMirror(b)
			batch, sweep := make([]MirrorHistory, n), currentSweep(s, root)
			for k := range batch {
				batch[k].IssueID = benchIssueID(0, k)
			}
			revs := make([]int64, n)
			for k, h := range dueHistories(s, root, batch) {
				revs[k] = h.BoardsRev
			}
			measureMirror(b, func(i int) error {
				for k := range batch {
					batch[k] = benchHistory(benchIssueID(0, k), int64(2000+i), 20)
					batch[k].BoardsRev = revs[k]
				}
				results, err := s.ReplaceMirrorHistories(root, sweep, batch)
				if err != nil {
					return err
				}
				for _, r := range results {
					if !r.Changed || r.Superseded || r.Unkept {
						return fmt.Errorf("history of %s not applied: %+v", r.IssueID, r)
					}
				}
				return nil
			})
		})
	}
}

// A page of the issues due a history read, each with its boards, with
// every issue of the fixture due.
func BenchmarkMirrorHistoryDue(b *testing.B) {
	s, root := openBenchMirror(b)
	if _, err := s.db.Exec(`UPDATE tracker_cache_issues SET history_for = 0`); err != nil {
		b.Fatal(err)
	}
	measureMirror(b, func(int) error {
		_, _, err := s.ListMirrorHistoryDue(root, "", testNow)
		return err
	})
}

// The first page of 100 rows of each section kind of a 1,000-row board.
func BenchmarkMirrorSectionPage(b *testing.B) {
	for name, section := range map[string]MirrorSection{
		"backlog": {Kind: MirrorSectionBacklog},
		"sprint":  {Kind: MirrorSectionSprint, SprintID: strconv.Itoa(benchBoardSprints - 1)},
		"column":  {Kind: MirrorSectionColumn, Column: 0},
	} {
		b.Run(name, func(b *testing.B) {
			s, root := openBenchMirror(b)
			measureMirror(b, func(int) error {
				_, err := s.ListMirrorSection(root, benchBoardID(0), section, MirrorRowKey{})
				return err
			})
		})
	}
}

// The section totals with estimate sums of a 1,000-row board.
func BenchmarkMirrorCountSections(b *testing.B) {
	s, root := openBenchMirror(b)
	measureMirror(b, func(int) error {
		_, err := s.CountMirrorSections(root, benchBoardID(0))
		return err
	})
}

// The cleanup with nothing pending, the fixture's 50 linked-only issues
// included, and one root's sweep with nothing unkept.
func BenchmarkMirrorCleanupIdle(b *testing.B) {
	b.Run("finish", func(b *testing.B) {
		s, _ := openBenchMirror(b)
		measureMirror(b, func(int) error { return s.FinishMirrorCleanup(nil) })
	})
	b.Run("sweep", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(int) error { return s.sweepMirrorRoot(root, func() {}) })
	})
}

// The listing check every board write runs, alone and as a whole empty
// board write.
func BenchmarkMirrorListingCheck(b *testing.B) {
	b.Run("statement", func(b *testing.B) {
		s, root := openBenchMirror(b)
		lifetime := currentLifetime(s, root, benchBoardID(0))
		measureMirror(b, func(int) error {
			var live, listed, retired, marked bool
			var readAt, removedAt, resetAt, pendingAt int64
			return s.db.QueryRow(mirrorBoardWriteSQL, root, benchBoardID(0), lifetime).Scan(&live, &listed, &retired, &readAt, &removedAt,
				&resetAt, &pendingAt, &marked)
		})
	})
	b.Run("write", func(b *testing.B) {
		s, root := openBenchMirror(b)
		measureMirror(b, func(i int) error {
			changed, err := s.RecordMirrorSyncAttempt(root, benchBoardID(0), currentAttempt(s, root, benchBoardID(0), MirrorSyncAttempt{ReadAt: int64(10 + i)}))
			if err == nil && !changed {
				err = errors.New("the attempt was not recorded")
			}
			return err
		})
	})
}

// A read of the catalog's statuses with a later read time: identical (each
// status's read time moves), and with status 3 moved into the done group
// and back, which advances the revision of the five boards with a row in
// it after a walk of the root's 5,050 issues.
func BenchmarkMirrorStatusRead(b *testing.B) {
	catalog := func(group int) MirrorCatalog {
		st := benchStructure(0).Catalog
		st.Users = nil
		st.Statuses[2].Group = group
		return st
	}
	for name, groups := range map[string][]int{"identical": {MirrorGroupInProgress}, "done-flip": {MirrorGroupDone, MirrorGroupInProgress}} {
		b.Run(name, func(b *testing.B) {
			s, root := openBenchMirror(b)
			sweep := currentSweep(s, root)
			measureMirror(b, func(i int) error {
				got, err := s.UpsertMirrorCards(root, sweep, nil, catalog(groups[i%len(groups)]), int64(10+i))
				if err == nil && got.Catalog != (len(groups) > 1) {
					err = fmt.Errorf("catalog changed %v", got.Catalog)
				}
				return err
			})
		})
	}
}

// A board's lifetime: listed (its row's insert, whose foreign key probe
// walks the root's board rows), unlisted, and drained with no rows.
func BenchmarkMirrorBoardLifetime(b *testing.B) {
	s, _ := openBenchMirror(b)
	jira := benchConnectionID(b, s)
	listing := []ProjectTrackerBoard{}
	for board := range benchBoards {
		listing = append(listing, ProjectTrackerBoard{ConnectionID: jira, BoardID: benchBoardID(board)})
	}
	measureMirror(b, func(int) error {
		if _, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, append(slices.Clone(listing), ProjectTrackerBoard{ConnectionID: jira, BoardID: "99"})); err != nil {
			return err
		}
		if _, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, listing); err != nil {
			return err
		}
		return s.FinishMirrorCleanup(nil)
	})
}

// The fixture's storage: bytes per card and per change, table and index
// sizes (dbstat), and the file. Run with -benchtime=1x.
func BenchmarkMirrorStorage(b *testing.B) {
	s, _, path := openBenchMirrorFile(b)
	sizes := map[string]float64{}
	err := scanMirrorRows(s.db, `SELECT name, sum(pgsize) FROM dbstat WHERE name LIKE '%tracker_cache%' GROUP BY name`, nil, func(r rowScanner) error {
		var name string
		var size float64
		err := r.Scan(&name, &size)
		sizes[name] = size
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	counts := countMirrorRows(b, s.db)
	for range b.N {
	}
	issues := sizes["sqlite_autoindex_tracker_cache_issues_1"] + sizes["tracker_cache_issues"]
	b.ReportMetric(sizes["tracker_cache_issues"]/float64(counts["tracker_cache_issues"]), "B/card")
	b.ReportMetric(sizes["tracker_cache_changes"]/float64(counts["tracker_cache_changes"]), "B/change")
	b.ReportMetric(sizes["tracker_cache_board_issues"]/float64(counts["tracker_cache_board_issues"]), "B/row")
	var names []string
	for name := range sizes {
		names = append(names, name)
	}
	slices.Sort(names)
	var total float64
	var report strings.Builder
	for _, name := range names {
		total += sizes[name]
		fmt.Fprintf(&report, "%s %.0f bytes, %d rows\n", name, sizes[name], counts[name])
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(total, "mirror-bytes")
	b.ReportMetric(float64(info.Size()), "file-bytes")
	b.Logf("issues with their key index: %.0f bytes\n%s", issues, report.String())
}
