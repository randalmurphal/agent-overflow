package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// MirrorReconcile is what a reconcile of a board found and did: Missing
// keeps the listing's order, Removed is sorted by issue ID.
type MirrorReconcile struct {
	// Missing are the listed issues with no row on the board: the caller
	// reads their rows and stores them (WriteMirrorRows).
	Missing []string
	// Unsettled are the rows the listing does not hold, sorted, when it
	// judges the board's rows: the caller reads them by ID, and
	// WriteMirrorRows removes those the board no longer holds.
	Unsettled []string
}

// The reconcile statements: ?1 root, ?2 board, ?3 when the listing's read
// began, ?4 the JSON listing, ?5 whether the listing judges the board's
// rows.
const (
	// mirrorReconcileFoundSQL is, from one probe per listed issue, the
	// listed issues with no row (0, in the listing's order), and, for a
	// listing that judges the rows, from one walk of the board's rows, the
	// unlisted rows read no later than the listing (1).
	mirrorReconcileFoundSQL = `SELECT 0, l.value, l.key FROM json_each(?4) l
		LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = l.value AND b.board_id = ?2
		WHERE b.root_id IS NULL
		UNION ALL
		SELECT 1, b.issue_id, 0 FROM tracker_cache_board_issues b
		 WHERE ?5 AND b.root_id = ?1 AND b.board_id = ?2 AND b.read_at <= ?3
		   AND b.issue_id NOT IN (SELECT value FROM json_each(?4))
		ORDER BY 1, 3, 2`
	// mirrorReconcileListedSQL records listing ?2 as the board's newest
	// complete listing and its latest removal.
	mirrorReconcileListedSQL = `UPDATE tracker_cache_boards SET listing_read_at = ?2, rows_removed_at = max(rows_removed_at, ?2)
		WHERE root_id = ?1 AND board_id = ?3`
)

// ReconcileMirrorBoard compares a listing of the issues on a board, read
// from read, with the board's rows; readAt below is read's order time. It
// returns the listed issues that have no row and, only when the listing
// judges the board's rows, returns the rows it does not list that reads
// begun no later than readAt stored (Unsettled) and records the listing
// as the board's newest complete listing and latest removal, so no read
// that began earlier adds a row the listing leaves out. It removes no row: a
// listing may show a row late (a search index), and how late is no
// interval an order time measures, so every row it leaves out is read by
// ID, whose answer removes the rows the board no longer holds
// (WriteMirrorRows). A complete listing judges the rows unless it began
// before the newest complete listing the board reconciled, which saw the
// rows it leaves out; an incomplete or older one returns no Unsettled.
// One transaction: one statement finds the missing and the unlisted
// rows, and one records a judging listing. The board must be listed in
// lifetime (ErrTrackerCacheUnlisted).
func (s *Store) ReconcileMirrorBoard(rootID int64, boardID string, lifetime int64, listed []string, complete bool, read MirrorRead) (result MirrorReconcile, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return MirrorReconcile{}, err
	}
	readAt := times.at
	if readAt <= 0 {
		return MirrorReconcile{}, fmt.Errorf("%w: a listing needs the time its read began", ErrInvalidTrackerCache)
	}
	seen := make(map[string]bool, len(listed))
	for _, id := range listed {
		if id == "" || seen[id] {
			return MirrorReconcile{}, fmt.Errorf("%w: listed issue %q is empty or repeated", ErrInvalidTrackerCache, id)
		}
		seen[id] = true
	}
	encoded, err := encodeMirrorIDs(listed)
	if err != nil {
		return MirrorReconcile{}, err
	}
	action := fmt.Sprintf("store: reconcile tracker mirror %d board %s", rootID, boardID)
	err = s.mirrorBoardWrite(rootID, boardID, lifetime, action, true, func(tx *sql.Tx, board mirrorBoard) error {
		judges := complete && readAt >= board.listingReadAt
		var found MirrorReconcile
		var err error
		if found.Missing, found.Unsettled, err = reconcileFoundTx(tx, rootID, boardID, readAt, encoded, judges); err != nil {
			return fmt.Errorf("find missing and unlisted rows: %w", err)
		}
		if judges && (readAt > board.listingReadAt || readAt > board.rowsRemovedAt) {
			if _, err := tx.Exec(mirrorReconcileListedSQL, rootID, readAt, boardID); err != nil {
				return fmt.Errorf("record the listing: %w", err)
			}
		}
		result = found
		return nil
	})
	if err != nil {
		return MirrorReconcile{}, err
	}
	return result, nil
}

// reconcileFoundTx runs mirrorReconcileFoundSQL: the listed issues with no
// row, in the listing's order, and the unlisted rows, sorted.
func reconcileFoundTx(tx *sql.Tx, rootID int64, boardID string, readAt int64, listed string,
	judges bool) (missing, unsettled []string, err error) {
	rows, err := tx.Query(mirrorReconcileFoundSQL, rootID, boardID, readAt, listed, judges)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var kind, key int64
		var id string
		if err := rows.Scan(&kind, &id, &key); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		if kind == 0 {
			missing = append(missing, id)
		} else {
			unsettled = append(unsettled, id)
		}
	}
	return missing, unsettled, errors.Join(rows.Err(), rows.Close())
}
