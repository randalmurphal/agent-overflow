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

// MirrorRowKey is a row's place in a board's order at a revision of the
// board: a page continues after it. A key with no issue starts at the
// first row, at the board's current revision.
type MirrorRowKey struct {
	OrderKey, IssueID string
	Revision          int64
}

// MirrorSectionPage is a page of a section's rows in order-key order, read
// at the board's Revision. More reports that rows follow Next.
type MirrorSectionPage struct {
	Rows     []MirrorRow
	Next     MirrorRowKey
	More     bool
	Revision int64
}

const mirrorSectionRowsSQL = `SELECT ` + mirrorCardColumns + `, b.order_key, b.estimate
	  FROM tracker_cache_board_issues b
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = ?1 AND b.board_id = ?2 AND (b.order_key, b.issue_id) > (?3, ?4) AND `

const mirrorSectionOrderSQL = ` ORDER BY b.order_key, b.issue_id LIMIT `

// mirrorInOpenSprintSQL holds for a row of b in a future or active sprint
// of its board. The issue's sprints drive it (CROSS JOIN): from the
// board's sprints it would walk all of them for every row.
const mirrorInOpenSprintSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
	 WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND sp.state IN (1, 2))`

// mirrorNotDoneSQL holds for a row of b whose issue i has a status the
// mirror does not place in the done or canceled group.
const mirrorNotDoneSQL = `NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st
	 WHERE st.root_id = b.root_id AND st.status_id = i.status_id AND st.status_group IN (4, 5))`

// mirrorBacklogSQL holds for a row of b in the board's backlog.
const mirrorBacklogSQL = `NOT ` + mirrorInOpenSprintSQL + ` AND ` + mirrorNotDoneSQL

var mirrorSectionSQL = map[MirrorSectionKind]string{
	MirrorSectionSprint: mirrorSectionRowsSQL + `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND m.sprint_id = ?5)` +
		mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
	MirrorSectionBacklog: mirrorSectionRowsSQL + mirrorBacklogSQL +
		mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
	MirrorSectionColumn: mirrorSectionRowsSQL + `i.status_id IN (SELECT c.status_id FROM tracker_cache_board_column_statuses c
	  WHERE c.root_id = ?1 AND c.board_id = ?2 AND c.position = ?5)` +
		mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
}

// mirrorShownBoardSQL reads the revision and estimate unit of a board the
// mirror shows.
var mirrorShownBoardSQL = `SELECT b.revision, b.estimate_unit FROM tracker_cache_boards b
	WHERE b.root_id = ?1 AND b.board_id = ?2 AND ` + mirrorBoardShown("?1", "?2")

// readMirrorShownBoardTx reads a shown board's revision and estimate unit;
// ok is false for a board the mirror does not show.
func readMirrorShownBoardTx(tx *sql.Tx, rootID int64, boardID string) (revision int64, unit int, ok bool, err error) {
	err = tx.QueryRow(mirrorShownBoardSQL, rootID, boardID).Scan(&revision, &unit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("read board: %w", err)
	}
	return revision, unit, true, nil
}

// ListMirrorSection reads up to mirrorSectionPageSize rows of one section
// of a board, in order-key order, after after, in one snapshot with the
// board's revision. A continuation (after names an issue) read at another
// revision fails with ErrTrackerCacheStale: a change between the pages
// could have moved a row across the cursor. A board the mirror does not
// show has no rows.
func (s *Store) ListMirrorSection(rootID int64, boardID string, section MirrorSection, after MirrorRowKey) (page MirrorSectionPage, err error) {
	query, ok := mirrorSectionSQL[section.Kind]
	if !ok {
		return MirrorSectionPage{}, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
	}
	args := []any{rootID, boardID, after.OrderKey, after.IssueID}
	switch section.Kind {
	case MirrorSectionSprint:
		args = append(args, section.SprintID)
	case MirrorSectionColumn:
		args = append(args, section.Column)
	}
	action := fmt.Sprintf("store: read tracker mirror %d board %s section", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorSectionPage{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	revision, _, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
	switch {
	case err != nil:
		return MirrorSectionPage{}, fmt.Errorf("%s: %w", action, err)
	case after.IssueID != "" && (!shown || revision != after.Revision):
		return MirrorSectionPage{}, fmt.Errorf("%s: %w", action, ErrTrackerCacheStale)
	case !shown:
		return MirrorSectionPage{}, tx.Commit()
	}
	page.Revision = revision
	err = scanMirrorRows(tx, query, args, func(r rowScanner) error {
		var row MirrorRow
		var err error
		row.Card, err = scanMirrorCard(r, &row.OrderKey, &row.Estimate)
		page.Rows = append(page.Rows, row)
		return err
	})
	if err != nil {
		return MirrorSectionPage{}, fmt.Errorf("%s: %w", action, err)
	}
	if len(page.Rows) > mirrorSectionPageSize {
		page.Rows, page.More = page.Rows[:mirrorSectionPageSize], true
	}
	if n := len(page.Rows); n > 0 {
		page.Next = MirrorRowKey{OrderKey: page.Rows[n-1].OrderKey, IssueID: page.Rows[n-1].Card.IssueID, Revision: revision}
	}
	return page, tx.Commit()
}

// MirrorSectionTotal is how many rows a section holds and the sum of the
// board's estimates of them; a row with no estimate counts as zero, and a
// board with no estimate unit sums to zero.
type MirrorSectionTotal struct {
	Rows     int
	Estimate float64
}

// MirrorSectionCounts are the totals of each section of a board at its
// Revision. A sprint or column with no rows is absent.
type MirrorSectionCounts struct {
	Revision     int64
	All, Backlog MirrorSectionTotal
	Sprints      map[string]MirrorSectionTotal
	Columns      map[int]MirrorSectionTotal
}

// The count statements each walk the board's rows once, in
// idx_tracker_cache_board_issues_order, and return a row per board row or
// sprint membership, which CountMirrorSections sums: a GROUP BY would sort
// them in a temporary b-tree and evaluate the backlog rule twice a row.
var (
	mirrorCountRowsSQL = `SELECT ` + mirrorBacklogSQL + `, i.status_id, b.estimate
		FROM tracker_cache_board_issues b CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		WHERE b.root_id = ?1 AND b.board_id = ?2`
	mirrorCountSprintsSQL = `SELECT m.sprint_id, b.estimate FROM tracker_cache_board_issues b
		  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = b.root_id AND m.issue_id = b.issue_id
		  CROSS JOIN tracker_cache_sprints sp ON sp.root_id = b.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
		 WHERE b.root_id = ?1 AND b.board_id = ?2`
	mirrorCountColumnStatusesSQL = `SELECT status_id, position FROM tracker_cache_board_column_statuses
		WHERE root_id = ?1 AND board_id = ?2 LIMIT ` + strconv.Itoa(mirrorMaxColumnStatuses)
)

// CountMirrorSections totals the rows of each section of a board the
// mirror shows, by the section rules ListMirrorSection reads, in one
// snapshot with the board's revision. A board it does not show has none.
func (s *Store) CountMirrorSections(rootID int64, boardID string) (counts MirrorSectionCounts, err error) {
	action := fmt.Sprintf("store: count tracker mirror %d board %s sections", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	counts.Sprints, counts.Columns = map[string]MirrorSectionTotal{}, map[int]MirrorSectionTotal{}
	revision, unit, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
	if err != nil || !shown {
		return counts, errors.Join(err, tx.Commit())
	}
	counts.Revision = revision
	add := func(t MirrorSectionTotal, estimate sql.NullFloat64) MirrorSectionTotal {
		t.Rows++
		if unit != MirrorEstimateNone {
			t.Estimate += estimate.Float64
		}
		return t
	}
	columns := map[string]int{}
	err = scanMirrorRows(tx, mirrorCountColumnStatusesSQL, []any{rootID, boardID}, func(r rowScanner) error {
		var status string
		var position int
		err := r.Scan(&status, &position)
		columns[status] = position
		return err
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: columns: %w", action, err)
	}
	var status sql.RawBytes
	var estimate sql.NullFloat64
	err = scanMirrorRows(tx, mirrorCountRowsSQL, []any{rootID, boardID}, func(r rowScanner) error {
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
		return MirrorSectionCounts{}, fmt.Errorf("%s: rows: %w", action, err)
	}
	err = scanMirrorRows(tx, mirrorCountSprintsSQL, []any{rootID, boardID}, func(r rowScanner) error {
		var sprint sql.RawBytes
		if err := r.Scan(&sprint, &estimate); err != nil {
			return err
		}
		counts.Sprints[string(sprint)] = add(counts.Sprints[string(sprint)], estimate)
		return nil
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: sprints: %w", action, err)
	}
	return counts, tx.Commit()
}
