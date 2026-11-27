package store

import (
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"agent-overflow/internal/testutil/fixturedir"
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

// benchMirrorSize is a bench mirror's shape: how many boards, each with
// how many issues of its own, and whether its issues have parents
// (benchParented).
type benchMirrorSize struct {
	boards, issues int
	parents        bool
}

// benchDefaultSize is the mirror the benchmarks run against.
var benchDefaultSize = benchMirrorSize{boards: benchBoards, issues: benchBoardIssues}

func (z benchMirrorSize) String() string {
	if z.parents {
		return fmt.Sprintf("%dx%d-parents", z.boards, z.issues)
	}
	return fmt.Sprintf("%dx%d", z.boards, z.issues)
}

// benchEpics is how many epics the stories of a parented board name.
const benchEpics = 50

// benchParented is row as a Jira scrum board reads it, which lists no
// epics: every fourth issue a sub-task of the story before it, and four
// of five others a story under one of benchEpics epics no board lists,
// each read naming its parent.
func benchParented(row MirrorRow, board, i int) MirrorRow {
	c := &row.Card
	switch {
	case i%4 == 3:
		story := benchIssueID(board, i-1)
		c.TypeLevel, c.ParentID = -1, story
		c.ParentName = &MirrorParentName{Key: "WEB-" + story, Title: "Story " + story}
	case i%5 != 0:
		epic := strconv.Itoa(800000 + board*1000 + (i/5)%benchEpics)
		c.ParentID = epic
		c.ParentName = &MirrorParentName{Key: "WEB-" + epic, Title: "Epic " + epic, TypeLevel: 1}
	}
	return row
}

// benchMirrorFixtures holds the fixture of each size, by size.
var benchMirrorFixtures sync.Map

// benchIssueID is issue i of board (0 to the size's boards less one). A
// board's IDs are 10,000,000 apart, so no size, and no new issue a
// benchmark adds past a board's own, meets another board's.
func benchIssueID(board, i int) string { return strconv.Itoa(100000 + board*10_000_000 + i) }

func benchBoardID(board int) string { return strconv.Itoa(10 + board) }

// CardShapeForTest is the shape of the card blobs these tests write
// (benchCard), trackerapp's version 2.
var CardShapeForTest = MirrorCardShape{Version: 2, VersionMember: "v", Type: "type_name", Priority: "priority_name",
	Labels: "labels", LabelID: "id", LabelName: "name"}

// OwnUnitForTest counts an exported board in its own estimate's unit, as
// trackerapp does for a project that counts estimates.
func OwnUnitForTest(estimateUnit int) int { return estimateUnit }

// benchCard is a card whose data blob is of the shape trackerapp writes
// (CardShapeForTest): its type, priority, two labels and its web address.
func benchCard(id string, updated int64, board, i int) MirrorCard {
	return MirrorCard{IssueID: id, Key: "WEB-" + id, ProjectID: "100", TypeID: "10001", TypeLevel: 0,
		StatusID: strconv.Itoa(1 + i%5), AssigneeID: fmt.Sprint("acc-user", i%20), PriorityID: "3", ParentID: "",
		CreatedAt: 1_700_000_000_000, UpdatedAt: updated, Sprints: []string{fmt.Sprint(board*benchBoardSprints + i%benchBoardSprints)},
		Data: []byte(`{"v":2,"type_name":"Story","priority_name":"Medium","labels":[{"id":"frontend","name":"frontend"},{"id":"auth","name":"auth"}],` +
			`"url":"https://example.atlassian.net/browse/WEB-` + id + `"}`)}
}

func benchRow(board, i int, updated int64) MirrorRow {
	estimate := float64(i % 8)
	return MirrorRow{Card: benchCard(benchIssueID(board, i), updated, board, i), OrderKey: fmt.Sprintf("0|i%05d:", i), Estimate: &estimate}
}

func benchStructure(board int) MirrorStructure {
	st := MirrorStructure{Board: MirrorBoard{ID: benchBoardID(board), ProjectID: "100", Name: "WEB board " + benchBoardID(board),
		PlansSprints: true, EstimateUnit: MirrorEstimatePoints, Data: []byte(`{"choices":[1,2,3,5,8]}`)}, ReadAt: mirrorReadAt(1)}
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

// buildBenchMirror writes a fixture of size through the store's own
// writers.
func buildBenchMirror(tb testing.TB, path string, size benchMirrorSize) error {
	if err := copyTestStoreTemplate(tb, path); err != nil {
		return err
	}
	s, err := New(path)
	if err != nil {
		return err
	}
	err = fillBenchMirror(s, size)
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fillBenchMirror(s *Store, size benchMirrorSize) error {
	jira, err := s.CreateTrackerConnection(jiraConnection(mirrorSite, "user1@example.com", 10))
	if err != nil {
		return err
	}
	root, _, err := s.GetOrCreateMirrorRoot(jira.ID)
	if err != nil {
		return err
	}
	listing := []ProjectTrackerBoard{}
	for board := range size.boards {
		listing = append(listing, ProjectTrackerBoard{ConnectionID: jira.ID, BoardID: benchBoardID(board)})
	}
	if _, _, err := replaceFromStored(s, defaultTestProjectID, listing); err != nil {
		return err
	}
	for board := range size.boards {
		if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, benchStructure(board))); err != nil {
			return err
		}
		rows := make([]MirrorRow, 0, size.issues)
		for i := range size.issues {
			row := benchRow(board, i, 1000)
			if size.parents {
				row = benchParented(row, board, i)
			}
			rows = append(rows, row)
		}
		for page := range slices.Chunk(rows, 100) {
			if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: benchBoardID(board), Rows: page, ReadAt: mirrorReadAt(1)}), []byte("cursor")); err != nil {
				return err
			}
		}
		for first := 0; first < size.issues; first += mirrorHistoryBatch {
			batch := make([]MirrorHistory, 0, mirrorHistoryBatch)
			for i := first; i < min(first+mirrorHistoryBatch, size.issues); i++ {
				batch = append(batch, benchHistory(benchIssueID(board, i), 100, benchChanges))
			}
			if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, batch), MirrorCatalog{}, nextMirrorRead(s, root)); err != nil {
				return err
			}
		}
	}
	for i := range benchLinkedOnly {
		id := strconv.Itoa(900000 + i)
		if err := s.CreateThread(makeThread("linked-"+id, "claude")); err != nil {
			return err
		}
		if _, err := s.db.Exec(`INSERT INTO thread_tickets(thread_id, kind, site, issue_id, issue_key, origin, linked_at) VALUES (?, ?, ?, ?, ?, ?, 10)`,
			"linked-"+id, TrackerKindJira, mirrorSite, id, "WEB-"+id, TicketOriginManual); err != nil {
			return err
		}
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{benchCard(id, 1000, 0, i)}, MirrorCatalog{}, mirrorReadAt(1)); err != nil {
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
		if _, err := s.ReplaceMirrorWorklogs(root, 0, "acc-user1", day, day, mirrorReadAt(1), logs); err != nil {
			return err
		}
	}
	return nil
}

// openBenchMirror opens a copy of the benchmarks' fixture, with its root.
func openBenchMirror(b *testing.B) (*Store, int64) {
	s, root, _ := openBenchMirrorFile(b)
	return s, root
}

// openBenchMirrorFile opens a copy of the benchmarks' fixture at path,
// with its root.
func openBenchMirrorFile(b *testing.B) (s *Store, root int64, path string) {
	return openSizedMirror(b, benchDefaultSize)
}

// benchMirrorFile is the closed fixture file of size, built once a
// process.
func benchMirrorFile(tb testing.TB, size benchMirrorSize) string {
	tb.Helper()
	entry, _ := benchMirrorFixtures.LoadOrStore(size, fixturedir.New("bench-mirror-"+size.String()+".sqlite", func(tb testing.TB, path string) {
		if err := buildBenchMirror(tb, path, size); err != nil {
			tb.Fatal(err)
		}
	}))
	return entry.(*fixturedir.Lazy).Path(tb)
}

// openSizedMirror opens a copy of the fixture of size at path, with its
// root.
func openSizedMirror(b testing.TB, size benchMirrorSize) (s *Store, root int64, path string) {
	b.Helper()
	path = filepath.Join(b.TempDir(), "store.sqlite")
	if err := copyStoreFile(benchMirrorFile(b, size), path); err != nil {
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

func benchConnectionID(b testing.TB, s *Store) string {
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
	return MirrorRowPage{BoardID: benchBoardID(board), Rows: rows, ReadAt: mirrorReadAt(1)}
}
