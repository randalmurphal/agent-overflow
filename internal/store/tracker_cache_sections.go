package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// MirrorSectionKind is a section of a board.
type MirrorSectionKind int

const (
	// MirrorSectionSprint is the rows in one sprint.
	MirrorSectionSprint MirrorSectionKind = iota + 1
	// MirrorSectionBacklog is the rows in no future or active sprint of
	// the board whose status is not in the done or canceled group, by the
	// group tracker_cache_statuses holds; a status the mirror does not
	// know counts as not done. On a board with no sprints, every row not
	// done.
	MirrorSectionBacklog
	// MirrorSectionColumn is the rows whose status is in one column.
	MirrorSectionColumn
)

// MirrorSection names a section: a sprint by ID, the backlog, or a column
// by position.
type MirrorSection struct {
	Kind     MirrorSectionKind
	SprintID string
	Column   int
}

// MirrorRowKey is a row's place in a board's order at a revision of one
// lifetime of the board: a page continues after it. A key with no issue
// starts at the first row, at the board's current lifetime and revision.
type MirrorRowKey struct {
	OrderKey, IssueID  string
	Lifetime, Revision int64
}

// MirrorSectionPage is a page of a section's rows in order-key order, read
// at the board's Revision. More reports that rows follow Next. Names are
// what the rows' cards name, read in the same snapshot (ListMirrorSection;
// a board view names its pages together, in MirrorBoardView.Names).
type MirrorSectionPage struct {
	Rows     []MirrorRow
	Next     MirrorRowKey
	More     bool
	Revision int64
	Names    MirrorNames
}

// mirrorSectionRowsSQL reads ?1 root, ?2 board, after (?3, ?4), a
// section's ?5, and ?6 the board's estimate reset.
var mirrorSectionRowsSQL = `SELECT ` + mirrorRowColumnsSQL + `
	  FROM tracker_cache_board_issues b
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND (b.order_key, b.issue_id) > (?3, ?4) AND `

const mirrorSectionOrderSQL = ` ORDER BY b.order_key, b.issue_id LIMIT `

// mirrorRowEstimate is the estimate a board row b shows: none when the
// row was read no later than reset, the SQL expression of its board's
// estimate reset, since its number is in the board's earlier estimate
// definition. Every read of a row's estimate states it with this.
func mirrorRowEstimate(reset string) string {
	return `iif(b.read_at <= ` + reset + `, NULL, b.estimate)`
}

// mirrorRowColumnsSQL are a section row's columns, of a board row b and
// its issue i, as scanMirrorRow reads them, ?6 being the board's estimate
// reset. Every read of a section's rows, by page or by ID, projects them.
var mirrorRowColumnsSQL = mirrorCardColumns + `, b.order_key, ` + mirrorRowEstimate("?6")

// scanMirrorRow reads a row of mirrorRowColumnsSQL.
func scanMirrorRow(r rowScanner) (MirrorRow, error) {
	var row MirrorRow
	var err error
	row.Card, err = scanMirrorCard(r, &row.OrderKey, &row.Estimate)
	return row, err
}

// ruleArg is the value a section's rule names as ?5: its sprint or its
// column's position.
func (section MirrorSection) ruleArg() any {
	switch section.Kind {
	case MirrorSectionSprint:
		return section.SprintID
	case MirrorSectionColumn:
		return section.Column
	case MirrorSectionBacklog:
		// The backlog's rule names no ?5.
	}
	return nil
}

// The sprint rules hold for a row b of board ?2 of root ?1 in a future or
// active sprint of the board (InOpenSprint) or in sprint ?5 (InSprint).
// Each is stated in two forms, by how many rows a statement tests: a Probe
// reads the row's own sprints, which costs a few rows a test, for a
// statement that tests few rows (a read by ID, the backlog's page); Members
// reads the sprints' members once a statement
// (idx_tracker_cache_issue_sprints_sprint), which costs every member
// once, for a statement that walks the board for few of them (the counts,
// a sprint's page). The pages, the patch read and the counts are checked
// against one another (TestMirrorCountsMatchThePages, requirePatchParity).
const (
	mirrorInOpenSprintProbeSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
	 WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND sp.state IN (1, 2))`
	mirrorInOpenSprintMembersSQL = `b.issue_id IN (SELECT m.issue_id FROM tracker_cache_sprints sp
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = sp.root_id AND m.sprint_id = sp.sprint_id
	 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND sp.state IN (1, 2))`
	mirrorInSprintProbeSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND m.sprint_id = ?5)`
	mirrorInSprintMembersSQL = `b.issue_id IN (SELECT m.issue_id FROM tracker_cache_issue_sprints m
	  WHERE m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = ?5)`
)

// mirrorNotDoneSQL holds for a row of b whose issue i has a status the
// mirror does not place in the done or canceled group.
const mirrorNotDoneSQL = `NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st
	 WHERE st.root_id = b.root_id AND st.status_id = i.status_id AND st.status_group IN (4, 5))`

// mirrorBacklog holds for a row of b in the board's backlog, with
// inOpenSprint one form of the open-sprint rule.
func mirrorBacklog(inOpenSprint string) string {
	return `NOT ` + inOpenSprint + ` AND ` + mirrorNotDoneSQL
}

// mirrorColumnRuleSQL holds for a row b (with its issue i) of board ?2 of
// root ?1 in the column at position ?5. Every read of a section's rows, by
// page or by ID, states its rule with it, mirrorBacklog or a sprint rule.
const mirrorColumnRuleSQL = `i.status_id IN (SELECT c.status_id FROM tracker_cache_board_column_statuses c
	  WHERE c.root_id = CAST(?1 AS INTEGER) AND c.board_id = ?2 AND c.position = ?5)`

var mirrorSectionSQL = map[MirrorSectionKind]string{
	MirrorSectionSprint:  mirrorSectionRowsSQL + mirrorInSprintMembersSQL + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
	MirrorSectionBacklog: mirrorSectionRowsSQL + mirrorBacklog(mirrorInOpenSprintProbeSQL) + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
	MirrorSectionColumn:  mirrorSectionRowsSQL + mirrorColumnRuleSQL + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
}

// mirrorShownBoardSQL reads the lifetime, revisions, estimate unit and
// estimate reset of a board the mirror shows.
var mirrorShownBoardSQL = `SELECT b.lifetime, b.revision, b.totals_revision, b.estimate_unit, b.estimate_reset_at FROM tracker_cache_boards b
	WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ` + mirrorBoardShown(intParam("?1"), "?2")

// mirrorShownBoard is a shown board's state as a read of its rows needs it.
type mirrorShownBoard struct {
	lifetime, revision, totalsRevision, estimateResetAt int64
	unit                                                int
}

// readMirrorShownBoardTx reads a shown board's lifetime, revisions and
// estimate unit; ok is false for a board the mirror does not show.
func readMirrorShownBoardTx(tx sqlQueryer, rootID int64, boardID string) (board mirrorShownBoard, ok bool, err error) {
	err = tx.QueryRow(mirrorShownBoardSQL, rootID, boardID).Scan(&board.lifetime, &board.revision, &board.totalsRevision, &board.unit,
		&board.estimateResetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mirrorShownBoard{}, false, nil
	}
	if err != nil {
		return mirrorShownBoard{}, false, fmt.Errorf("read board: %w", err)
	}
	return board, true, nil
}

// ListMirrorSection reads up to mirrorSectionPageSize rows of one section
// of a board, in order-key order, after after, in one snapshot with the
// board's revision and what the rows' cards name (MirrorNames). A
// continuation (after names an issue) read at another
// lifetime or revision fails with ErrTrackerCacheStale: a change between
// the pages could have moved a row across the cursor, and a board unlisted
// and listed again holds other rows. A board the mirror does not show has
// no rows.
func (s *Store) ListMirrorSection(rootID int64, boardID string, section MirrorSection, after MirrorRowKey) (page MirrorSectionPage, err error) {
	if _, ok := mirrorSectionSQL[section.Kind]; !ok {
		return MirrorSectionPage{}, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
	}
	label := fmt.Sprintf("read tracker mirror %d board %s section", rootID, boardID)
	return mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorSectionPage, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		switch {
		case err != nil:
			return MirrorSectionPage{}, err
		case after.IssueID != "" && (!shown || board.lifetime != after.Lifetime || board.revision != after.Revision):
			return MirrorSectionPage{}, ErrTrackerCacheStale
		case !shown:
			return MirrorSectionPage{}, nil
		}
		page, err := readMirrorSectionTx(tx, rootID, boardID, board, section, after)
		if err != nil {
			return MirrorSectionPage{}, err
		}
		page.Names, err = readMirrorNamesTx(tx, rootID, [][]MirrorRow{page.Rows}, nil)
		return page, err
	})
}

// readMirrorSectionTx reads a page of a valid section of a shown board,
// after after, in tx's snapshot.
func readMirrorSectionTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, section MirrorSection,
	after MirrorRowKey) (page MirrorSectionPage, err error) {
	args := []any{rootID, boardID, after.OrderKey, after.IssueID, section.ruleArg(), board.estimateResetAt}
	page.Revision = board.revision
	err = scanMirrorRows(tx, mirrorSectionSQL[section.Kind], args, func(r rowScanner) error {
		row, err := scanMirrorRow(r)
		page.Rows = append(page.Rows, row)
		return err
	})
	if err != nil {
		return MirrorSectionPage{}, err
	}
	if len(page.Rows) > mirrorSectionPageSize {
		page.Rows, page.More = page.Rows[:mirrorSectionPageSize], true
	}
	if n := len(page.Rows); n > 0 {
		page.Next = MirrorRowKey{OrderKey: page.Rows[n-1].OrderKey, IssueID: page.Rows[n-1].Card.IssueID,
			Lifetime: board.lifetime, Revision: board.revision}
	}
	return page, nil
}

// MirrorSectionTotal is how many rows a section holds and the sum of the
// board's estimates of them; a row with no estimate, or one read no later
// than the board's estimate definition last changed (unknown), adds nothing,
// and a board with no estimate unit sums to zero.
type MirrorSectionTotal struct {
	Rows     int
	Estimate float64
}

// MirrorSectionCounts are the totals of each section of a board at its
// Revision and TotalsRevision. A sprint or column with no rows is absent.
type MirrorSectionCounts struct {
	Revision, TotalsRevision int64
	All, Backlog             MirrorSectionTotal
	Sprints                  map[string]MirrorSectionTotal
	Columns                  map[int]MirrorSectionTotal
}

// The count statements read no issue's table row and sort nothing.
// mirrorCountRowsSQL walks the board's rows once, in
// idx_tracker_cache_board_issues_order, reads each row's status from
// idx_tracker_cache_issues_status, tests it against the open sprints'
// members (read once), and returns a row per board row, which
// countMirrorSectionsTx sums: a GROUP BY would sort every row, in memory
// that grows with the board. mirrorCountSprintsSQL totals each sprint of
// the board from its members, in the sprints' key order.
var (
	mirrorCountRowsSQL = `SELECT ` + mirrorBacklog(mirrorInOpenSprintMembersSQL) + `, i.status_id, ` + mirrorRowEstimate("?3") + `
		FROM tracker_cache_board_issues b
		CROSS JOIN tracker_cache_issues i INDEXED BY idx_tracker_cache_issues_status ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`
	mirrorCountSprintsSQL = `SELECT sp.sprint_id, count(*), total(` + mirrorRowEstimate("?3") + `) FROM tracker_cache_sprints sp
		  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = sp.root_id AND m.sprint_id = sp.sprint_id
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = sp.root_id AND b.issue_id = m.issue_id AND b.board_id = sp.board_id
		 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 GROUP BY sp.sprint_id`
	mirrorCountColumnStatusesSQL = `SELECT status_id, position FROM tracker_cache_board_column_statuses
		WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 LIMIT ` + strconv.Itoa(mirrorMaxColumnStatuses)
)

// CountMirrorSections totals the rows of each section of a board the
// mirror shows, by the section rules ListMirrorSection reads, in one
// snapshot with the board's revision. A board it does not show has none.
func (s *Store) CountMirrorSections(rootID int64, boardID string) (MirrorSectionCounts, error) {
	label := fmt.Sprintf("count tracker mirror %d board %s sections", rootID, boardID)
	return mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorSectionCounts, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		if !shown {
			return MirrorSectionCounts{Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}, nil
		}
		columns, err := readMirrorColumnPlacesTx(tx, rootID, boardID)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		return countMirrorSectionsTx(tx, rootID, boardID, board, columns)
	})
}

// readMirrorColumnPlacesTx reads the column position of each status a
// board's columns hold.
func readMirrorColumnPlacesTx(tx sqlQueryer, rootID int64, boardID string) (map[string]int, error) {
	columns := map[string]int{}
	err := scanMirrorRows(tx, mirrorCountColumnStatusesSQL, []any{rootID, boardID}, func(r rowScanner) error {
		var status string
		var position int
		err := r.Scan(&status, &position)
		columns[status] = position
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	return columns, nil
}

// countMirrorSectionsTx totals a shown board's sections in tx's snapshot;
// columns places each status the board's columns hold.
func countMirrorSectionsTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, columns map[string]int) (MirrorSectionCounts, error) {
	counts := MirrorSectionCounts{Revision: board.revision, TotalsRevision: board.totalsRevision,
		Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}
	estimated := board.unit != MirrorEstimateNone
	add := func(t MirrorSectionTotal, estimate sql.NullFloat64) MirrorSectionTotal {
		t.Rows++
		if estimated {
			t.Estimate += estimate.Float64
		}
		return t
	}
	var status sql.RawBytes
	var estimate sql.NullFloat64
	err := scanMirrorRows(tx, mirrorCountRowsSQL, []any{rootID, boardID, board.estimateResetAt}, func(r rowScanner) error {
		var backlog bool
		if err := r.Scan(&backlog, &status, &estimate); err != nil {
			return err
		}
		counts.All = add(counts.All, estimate)
		if backlog {
			counts.Backlog = add(counts.Backlog, estimate)
		}
		if position, ok := columns[string(status)]; ok {
			counts.Columns[position] = add(counts.Columns[position], estimate)
		}
		return nil
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("rows: %w", err)
	}
	err = scanMirrorRows(tx, mirrorCountSprintsSQL, []any{rootID, boardID, board.estimateResetAt}, func(r rowScanner) error {
		var sprint string
		var total MirrorSectionTotal
		if err := r.Scan(&sprint, &total.Rows, &total.Estimate); err != nil {
			return err
		}
		if !estimated {
			total.Estimate = 0
		}
		counts.Sprints[sprint] = total
		return nil
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("sprints: %w", err)
	}
	return counts, nil
}
