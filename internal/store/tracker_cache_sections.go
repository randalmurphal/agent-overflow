package store

import (
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

// MirrorRowKey is a row's place in a board's order: a page continues after
// it. The zero key starts at the first row.
type MirrorRowKey struct {
	OrderKey, IssueID string
}

// MirrorSectionPage is a page of a section's rows in order-key order. More
// reports that rows follow Next.
type MirrorSectionPage struct {
	Rows []MirrorRow
	Next MirrorRowKey
	More bool
}

const mirrorSectionRowsSQL = `SELECT ` + mirrorCardColumns + `, b.order_key, b.estimate
	  FROM tracker_cache_board_issues b
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = ?1 AND b.board_id = ?2 AND (b.order_key, b.issue_id) > (?3, ?4) AND `

const mirrorSectionOrderSQL = ` ORDER BY b.order_key, b.issue_id LIMIT `

// mirrorInOpenSprintSQL holds for a row of b in a future or active sprint
// of its board.
const mirrorInOpenSprintSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
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

// ListMirrorSection reads up to mirrorSectionPageSize rows of one section
// of a board, in order-key order, after after.
func (s *Store) ListMirrorSection(rootID, boardID string, section MirrorSection, after MirrorRowKey) (MirrorSectionPage, error) {
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
	var page MirrorSectionPage
	err := scanMirrorRows(s.reader(), query, args, func(r rowScanner) error {
		var row MirrorRow
		var err error
		row.Card, err = scanMirrorCard(r, &row.OrderKey, &row.Estimate)
		page.Rows = append(page.Rows, row)
		return err
	})
	if err != nil {
		return MirrorSectionPage{}, fmt.Errorf("store: read tracker mirror %s board %s section: %w", rootID, boardID, err)
	}
	if len(page.Rows) > mirrorSectionPageSize {
		page.Rows, page.More = page.Rows[:mirrorSectionPageSize], true
	}
	if n := len(page.Rows); n > 0 {
		page.Next = MirrorRowKey{OrderKey: page.Rows[n-1].OrderKey, IssueID: page.Rows[n-1].Card.IssueID}
	}
	return page, nil
}

// MirrorSectionCounts are how many rows each section of a board holds.
// A sprint or column with none is absent.
type MirrorSectionCounts struct {
	Rows    int
	Backlog int
	Sprints map[string]int
	Columns map[int]int
}

// CountMirrorSections counts the rows of each section of a board in one
// snapshot.
func (s *Store) CountMirrorSections(rootID, boardID string) (counts MirrorSectionCounts, err error) {
	action := fmt.Sprintf("store: count tracker mirror %s board %s sections", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := tx.QueryRow(`SELECT count(*), coalesce(sum(`+mirrorBacklogSQL+`), 0)
		FROM tracker_cache_board_issues b JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		WHERE b.root_id = ? AND b.board_id = ?`, rootID, boardID).Scan(&counts.Rows, &counts.Backlog); err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: rows: %w", action, err)
	}
	counts.Sprints = map[string]int{}
	err = scanMirrorRows(tx, `SELECT sp.sprint_id, count(*) FROM tracker_cache_sprints sp
		  JOIN tracker_cache_issue_sprints m ON m.root_id = sp.root_id AND m.sprint_id = sp.sprint_id
		  JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = sp.board_id AND b.issue_id = m.issue_id
		 WHERE sp.root_id = ? AND sp.board_id = ? GROUP BY sp.sprint_id LIMIT `+strconv.Itoa(mirrorMaxSprints),
		[]any{rootID, boardID}, func(r rowScanner) error {
			var sprint string
			var n int
			err := r.Scan(&sprint, &n)
			counts.Sprints[sprint] = n
			return err
		})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: sprints: %w", action, err)
	}
	counts.Columns = map[int]int{}
	err = scanMirrorRows(tx, `SELECT c.position, count(*) FROM tracker_cache_board_issues b
		  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		  JOIN tracker_cache_board_column_statuses c ON c.root_id = b.root_id AND c.board_id = b.board_id AND c.status_id = i.status_id
		 WHERE b.root_id = ? AND b.board_id = ? GROUP BY c.position LIMIT `+strconv.Itoa(mirrorMaxColumns),
		[]any{rootID, boardID}, func(r rowScanner) error {
			var position, n int
			err := r.Scan(&position, &n)
			counts.Columns[position] = n
			return err
		})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("%s: columns: %w", action, err)
	}
	return counts, tx.Commit()
}
