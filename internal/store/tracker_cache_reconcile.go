package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// MirrorReconcile is what a reconcile of a board found and did: Missing
// keeps the listing's order, Removed is sorted by issue ID.
type MirrorReconcile struct {
	// Missing are the listed issues with no row on the board: the caller
	// reads their rows and stores them (WriteMirrorRows).
	Missing []string
	// Removed are the issues whose row was removed, not being listed.
	Removed []string
	// Unsettled are the rows the listing does not hold that it does not
	// judge, sorted: those a read began after settledAt stored, which the
	// listing may not show yet, and those no listing has named, which only
	// a read by ID placed on the board. The caller reads them by ID
	// (WriteMirrorRows removes those the board no longer holds).
	Unsettled []string
}

// The reconcile statements: ?1 root, ?2 board, ?3 when the listing's read
// began, ?4 the JSON listing, ?5 the settled bound, ?6 whether the
// listing judges the board's rows.
const (
	// mirrorReconcileFoundSQL is, from one probe per listed issue, the
	// listed issues with no row (0, in the listing's order) and those whose
	// row no listing marked (3, to mark), and, for a listing that judges
	// the rows, from one walk of the board's rows, the unlisted rows read
	// no later than the listing: unsettled (1) or to remove (2), a row no
	// listing marked being unsettled whatever its age.
	mirrorReconcileFoundSQL = `SELECT iif(b.root_id IS NULL, 0, 3), l.value, l.key FROM json_each(?4) l
		LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = l.value AND b.board_id = ?2
		WHERE b.root_id IS NULL OR b.listed = 0
		UNION ALL
		SELECT 1 + (b.listed AND b.read_at <= ?5), b.issue_id, 0 FROM tracker_cache_board_issues b
		 WHERE ?6 AND b.root_id = ?1 AND b.board_id = ?2 AND b.read_at <= ?3
		   AND b.issue_id NOT IN (SELECT value FROM json_each(?4))
		ORDER BY 1, 3, 2`
	// mirrorReconcileListedSQL records listing ?2 as the board's newest
	// complete listing and its latest removal.
	mirrorReconcileListedSQL = `UPDATE tracker_cache_boards SET listing_read_at = ?2, rows_removed_at = max(rows_removed_at, ?2)
		WHERE root_id = ?1 AND board_id = ?3`
	// mirrorReconcileMarkSQL marks listed the rows of the issues ?3 (IDs)
	// on board ?2, by key.
	mirrorReconcileMarkSQL = `UPDATE tracker_cache_board_issues SET listed = 1
		WHERE root_id = ?1 AND board_id = ?2 AND issue_id IN (SELECT value FROM json_each(?3)) AND listed = 0`
)

// ReconcileMirrorBoard compares a listing of the issues on a board, read
// from readAt, with the board's rows. It returns the listed issues that
// have no row, marks listed the rows it names that no listing marked, and,
// only when the listing judges the board's rows, removes the rows it does
// not list that a listing marked and reads begun no later than settledAt
// stored, returns the other rows it does not list that reads begun no
// later than readAt stored (Unsettled), and records the listing as the
// board's newest complete listing and latest removal, so no read that
// began earlier adds a row the listing leaves out or removes one it may
// have named. A complete listing judges the rows unless it began before
// the newest complete listing the board reconciled, which saw the rows it
// leaves out; an incomplete or older one removes nothing and returns no
// Unsettled. A listing that may show a row late (a search index) leaves
// the rows read within that lag to a read by ID; settledAt equal to readAt
// judges every marked row. One statement finds the rows to remove and to
// mark; they are removed and marked in transactions of at most
// clearMirrorChunk of each, each removing a row only while no read that
// began after settledAt stored it and no newer complete listing was
// reconciled. A reconcile with no row to remove or mark runs no statement
// for either. An issue left with no row and no link becomes unkept. The
// board must be listed in lifetime (ErrTrackerCacheUnlisted). A call that
// fails after a transaction of it committed returns, with the error, the
// rows those transactions removed and no Missing or Unsettled: the caller
// tells the removals whatever it does about the error.
func (s *Store) ReconcileMirrorBoard(rootID int64, boardID string, lifetime int64, listed []string, complete bool, readAt, settledAt int64) (result MirrorReconcile, err error) {
	switch {
	case readAt <= 0:
		return MirrorReconcile{}, fmt.Errorf("%w: a listing needs the time its read began", ErrInvalidTrackerCache)
	case settledAt > readAt:
		return MirrorReconcile{}, fmt.Errorf("%w: a listing's settled bound %d is after its read %d", ErrInvalidTrackerCache, settledAt, readAt)
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
	// stale and unmarked are the rows the first transaction found to remove
	// and to mark, those later transactions have not reached.
	var stale, unmarked []string
	for batch := 0; ; batch++ {
		// What this transaction found and removed, taken only once it commits.
		var missing, unsettled, removed []string
		var judges bool
		err = s.mirrorBoardWrite(rootID, boardID, lifetime, action, true, func(tx *sql.Tx, board mirrorBoard) error {
			judges = complete && readAt >= board.listingReadAt
			if batch == 0 {
				var err error
				if missing, unsettled, stale, unmarked, err = reconcileFoundTx(tx, rootID, boardID, readAt, encoded, settledAt, judges); err != nil {
					return fmt.Errorf("find missing and unlisted rows: %w", err)
				}
				if judges && (readAt > board.listingReadAt || readAt > board.rowsRemovedAt) {
					if _, err := tx.Exec(mirrorReconcileListedSQL, rootID, readAt, boardID); err != nil {
						return fmt.Errorf("record the listing: %w", err)
					}
				}
			}
			if len(unmarked) > 0 {
				list, err := encodeMirrorIDs(unmarked[:min(len(unmarked), clearMirrorChunk)])
				if err != nil {
					return err
				}
				if _, err := tx.Exec(mirrorReconcileMarkSQL, rootID, boardID, list); err != nil {
					return fmt.Errorf("mark listed rows: %w", err)
				}
			}
			if !judges || len(stale) == 0 {
				return nil
			}
			list, err := encodeMirrorIDs(stale[:min(len(stale), clearMirrorChunk)])
			if err != nil {
				return err
			}
			if removed, err = queryIDs(tx, mirrorRowsRemoveSQL, rootID, list, boardID, settledAt); err != nil {
				return fmt.Errorf("remove unlisted rows: %w", err)
			}
			return nil
		})
		if err != nil {
			slices.Sort(result.Removed)
			return MirrorReconcile{Removed: result.Removed}, err
		}
		if batch == 0 {
			result.Missing, result.Unsettled = missing, unsettled
		}
		result.Removed = append(result.Removed, removed...)
		unmarked = unmarked[min(len(unmarked), clearMirrorChunk):]
		if stale = stale[min(len(stale), clearMirrorChunk):]; !judges {
			stale = nil
		}
		if len(stale) == 0 && len(unmarked) == 0 {
			break
		}
	}
	slices.Sort(result.Removed)
	return result, nil
}

// reconcileFoundTx runs mirrorReconcileFoundSQL: the listed issues with no
// row, in the listing's order, the unlisted rows, unsettled and to remove,
// each sorted, and the listed rows to mark, in the listing's order.
func reconcileFoundTx(tx *sql.Tx, rootID int64, boardID string, readAt int64, listed string, settledAt int64,
	judges bool) (missing, unsettled, stale, unmarked []string, err error) {
	rows, err := tx.Query(mirrorReconcileFoundSQL, rootID, boardID, readAt, listed, settledAt, judges)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var kind, key int64
		var id string
		if err := rows.Scan(&kind, &id, &key); err != nil {
			return nil, nil, nil, nil, errors.Join(err, rows.Close())
		}
		switch kind {
		case 0:
			missing = append(missing, id)
		case 1:
			unsettled = append(unsettled, id)
		case 2:
			stale = append(stale, id)
		default:
			unmarked = append(unmarked, id)
		}
	}
	return missing, unsettled, stale, unmarked, errors.Join(rows.Err(), rows.Close())
}
