package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// The reads a client's patch of a board needs: which boards hold rows of
// changed issues (ListMirrorIssueBoards, for the service's change funnel),
// and the rows of named issues that are members of each section a client
// holds, with the board's totals (ReadMirrorBoardPatch).

// mirrorPatchRowsSQL reads the rows of board ?2 of root ?1 among the
// issues of the JSON list ?3, each probed by key, with ?5 a section's
// sprint or column and ?6 the board's estimate reset (a row read no later
// shows no estimate); a section's rule follows. The rows come in list
// order; the caller sorts them, at most mirrorMaxLookup, rather than sort
// in a temporary b-tree.
const mirrorPatchRowsSQL = `SELECT ` + mirrorCardColumns + `, b.order_key, iif(b.read_at <= ?6, NULL, b.estimate)
	  FROM json_each(?3) l
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = l.value AND b.board_id = ?2
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE `

// mirrorPatchSQL reads, for each section kind, the named rows that are
// members of a section, by the rule its pages read (mirrorSectionSQL).
var mirrorPatchSQL = map[MirrorSectionKind]string{
	MirrorSectionSprint:  mirrorPatchRowsSQL + mirrorSprintRuleSQL,
	MirrorSectionBacklog: mirrorPatchRowsSQL + mirrorBacklogSQL,
	MirrorSectionColumn:  mirrorPatchRowsSQL + mirrorColumnRuleSQL,
}

// MirrorBoardPatch is what a client holding some sections of a board needs
// to patch them for a set of issues, read in one snapshot: the board's
// lifetime and revision, its section totals, and for each section asked,
// in order, the named issues' rows that are members of it now, in board
// order, with what their cards name. A named issue with no row in a
// section is absent from it.
type MirrorBoardPatch struct {
	Lifetime, Revision int64
	Counts             MirrorSectionCounts
	Sections           [][]MirrorRow
	Names              MirrorNames
}

// ReadMirrorBoardPatch reads a board's patch for issueIDs (at most
// mirrorMaxLookup, sorted or not) in sections (at most
// mirrorMaxViewSections) in one read transaction: the board's row, the
// columns' statuses and the two count walks of CountMirrorSections, one
// statement per section and the names. ok is false for a board the mirror
// does not show.
func (s *Store) ReadMirrorBoardPatch(rootID int64, boardID string, sections []MirrorSection, issueIDs []string) (patch MirrorBoardPatch, ok bool, err error) {
	switch {
	case len(sections) > mirrorMaxViewSections:
		return MirrorBoardPatch{}, false, fmt.Errorf("%w: %d sections, over %d", ErrInvalidTrackerCache, len(sections), mirrorMaxViewSections)
	case len(issueIDs) > mirrorMaxLookup:
		return MirrorBoardPatch{}, false, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	for _, section := range sections {
		if _, ok := mirrorPatchSQL[section.Kind]; !ok {
			return MirrorBoardPatch{}, false, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
		}
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return MirrorBoardPatch{}, false, err
	}
	action := fmt.Sprintf("store: read tracker mirror %d board %s patch", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorBoardPatch{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
	if err != nil || !shown {
		return MirrorBoardPatch{}, false, joinCommit(tx, err, action)
	}
	patch.Lifetime, patch.Revision = board.lifetime, board.revision
	columns := map[string]int{}
	err = scanMirrorRows(tx, mirrorCountColumnStatusesSQL, []any{rootID, boardID}, func(r rowScanner) error {
		var status string
		var position int
		err := r.Scan(&status, &position)
		columns[status] = position
		return err
	})
	if err != nil {
		return MirrorBoardPatch{}, false, fmt.Errorf("%s: columns: %w", action, err)
	}
	if patch.Counts, err = countMirrorSectionsTx(tx, rootID, boardID, board, columns); err != nil {
		return MirrorBoardPatch{}, false, fmt.Errorf("%s: %w", action, err)
	}
	patch.Sections = make([][]MirrorRow, len(sections))
	for i, section := range sections {
		if len(issueIDs) == 0 {
			continue
		}
		if patch.Sections[i], err = readMirrorPatchSectionTx(tx, rootID, boardID, board, section, list); err != nil {
			return MirrorBoardPatch{}, false, fmt.Errorf("%s: section %d: %w", action, i, err)
		}
	}
	if patch.Names, err = readMirrorNamesTx(tx, rootID, patch.Sections, nil); err != nil {
		return MirrorBoardPatch{}, false, fmt.Errorf("%s: %w", action, err)
	}
	return patch, true, tx.Commit()
}

// joinCommit ends a read transaction that found nothing to read, keeping
// err.
func joinCommit(tx *sql.Tx, err error, action string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return tx.Commit()
}

// readMirrorPatchSectionTx reads the rows of the JSON list of issues that
// are members of a valid section of a shown board, in board order.
func readMirrorPatchSectionTx(tx *sql.Tx, rootID int64, boardID string, board mirrorShownBoard, section MirrorSection, list string) ([]MirrorRow, error) {
	args := []any{rootID, boardID, list, nil, nil, board.estimateResetAt}
	switch section.Kind {
	case MirrorSectionSprint:
		args[4] = section.SprintID
	case MirrorSectionColumn:
		args[4] = section.Column
	}
	var rows []MirrorRow
	err := scanMirrorRows(tx, mirrorPatchSQL[section.Kind], args, func(r rowScanner) error {
		var row MirrorRow
		var err error
		row.Card, err = scanMirrorCard(r, &row.OrderKey, &row.Estimate)
		rows = append(rows, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b MirrorRow) int {
		if c := strings.Compare(a.OrderKey, b.OrderKey); c != 0 {
			return c
		}
		return strings.Compare(a.Card.IssueID, b.Card.IssueID)
	})
	return rows, nil
}

// MirrorIssueBoard is one board row of an issue: the board the issue is on.
type MirrorIssueBoard struct {
	IssueID, BoardID string
}

// mirrorIssueBoardsSQL reads the board rows of the issues of the JSON list
// ?2, each probed by its key prefix.
const mirrorIssueBoardsSQL = `SELECT b.issue_id, b.board_id FROM json_each(?2) l
	CROSS JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = l.value
	UNION ALL
	SELECT b.issue_id, b.board_id FROM json_each(?2) l
	CROSS JOIN tracker_cache_issues c ON c.root_id = ?1 AND c.parent_id = l.value AND c.parent_id <> ''
	CROSS JOIN tracker_cache_board_issues b ON b.root_id = c.root_id AND b.issue_id = c.issue_id`

// ListMirrorIssueBoards reads which boards hold a row of each of issueIDs
// (at most mirrorMaxLookup), and of each issue whose parent is one of
// them, whose row shows its parent's key and title, in one statement; a
// board no project shows may be among them, and a pair may repeat. An
// issue with no row is absent.
func (s *Store) ListMirrorIssueBoards(rootID int64, issueIDs []string) ([]MirrorIssueBoard, error) {
	if len(issueIDs) > mirrorMaxLookup {
		return nil, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	if len(issueIDs) == 0 {
		return nil, nil
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	var out []MirrorIssueBoard
	err = scanMirrorRows(s.reader(), mirrorIssueBoardsSQL, []any{rootID, list}, func(r rowScanner) error {
		var ib MirrorIssueBoard
		err := r.Scan(&ib.IssueID, &ib.BoardID)
		out = append(out, ib)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d boards of %d issues: %w", rootID, len(issueIDs), err)
	}
	return out, nil
}
