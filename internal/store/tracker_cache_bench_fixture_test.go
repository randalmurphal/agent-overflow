package store

import (
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

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
