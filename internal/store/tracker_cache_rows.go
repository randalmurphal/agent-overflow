package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
)

// MirrorRow is an issue on one board: its card with that board's order
// key and estimate (nil for none).
type MirrorRow struct {
	Card     MirrorCard
	OrderKey string
	Estimate *float64
}

// MirrorRowPage is one read of a board's rows: the rows, the issues asked
// for that the board no longer lists (Missing), the users and statuses
// they name, when the read began (ReadAt), and the board's lifetime its
// caller captured before the read (MirrorSync.Lifetime).
type MirrorRowPage struct {
	BoardID  string
	Lifetime int64
	Rows     []MirrorRow
	Missing  []string
	Catalog  MirrorCatalog
	ReadAt   int64
}

func (p MirrorRowPage) check() error {
	switch {
	case p.BoardID == "" || p.ReadAt <= 0:
		return fmt.Errorf("%w: rows need their board and the time their read began", ErrInvalidTrackerCache)
	case len(p.Rows)+len(p.Missing) > mirrorMaxPageRows:
		return fmt.Errorf("%w: a page of %d rows and %d missing issues, over %d", ErrInvalidTrackerCache, len(p.Rows), len(p.Missing), mirrorMaxPageRows)
	}
	seen := make(map[string]bool, len(p.Rows)+len(p.Missing))
	for _, row := range p.Rows {
		if err := row.Card.check(); err != nil {
			return err
		}
		if seen[row.Card.IssueID] {
			return fmt.Errorf("%w: issue %s is read twice", ErrInvalidTrackerCache, row.Card.IssueID)
		}
		seen[row.Card.IssueID] = true
	}
	for _, id := range p.Missing {
		if id == "" || seen[id] {
			return fmt.Errorf("%w: missing issue %q is empty or also read", ErrInvalidTrackerCache, id)
		}
		seen[id] = true
	}
	return p.Catalog.check()
}

// WriteMirrorRowPage stores one page of a board's changed rows and the
// cursor that page returned (nil: the next walk starts over), in one
// transaction: either all of it lands or none. A card is stored unless the
// mirror holds a later version; a row's order key and estimate, and the
// cursor, unless a read that began later was admitted for them. An
// identical page from a later read moves the rows' and the cursor's read
// times alone; one from the same read writes nothing. A read with no rows
// that returned the stored cursor need not be written: recording it as
// succeeded (RecordMirrorSyncAttempt) admits the cursor as its own. The
// board must be listed in page.Lifetime (ErrTrackerCacheUnlisted).
func (s *Store) WriteMirrorRowPage(rootID int64, page MirrorRowPage, cursor []byte) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, true, cursor)
}

// WriteMirrorRows stores a read of a board's rows for a set of issues, as
// WriteMirrorRowPage does but with no cursor, and removes the rows of the
// issues the board no longer lists (page.Missing) unless a read that began
// later was admitted for them. Missing issues are recorded as the board's
// latest removal even when it holds no row of them, so no read that began
// earlier adds a row. An issue left with no row and no link becomes
// unkept.
func (s *Store) WriteMirrorRows(rootID int64, page MirrorRowPage) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, false, nil)
}

// The row statements: ?1 root, ?2 the JSON list, ?3 board, ?4 when the
// read began, ?5 whether the page may add rows.
const (
	// mirrorRowsStateSQL sorts each row of the page: superseded (its card
	// is older than the stored one, it is new and the page may not add it,
	// or a later read stored other content), to write (new and addable, or
	// other content from a read no earlier), or to advance (identical
	// content stored by an earlier read, whose read time alone moves).
	mirrorRowsStateSQL = `SELECT c.value ->> 'i',
		    (b.root_id IS NULL AND NOT ?5) OR i.updated_at > c.value ->> 'u'
		      OR (b.read_at > ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    (b.root_id IS NULL AND ?5)
		      OR (b.read_at <= ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    b.read_at < ?4 AND b.order_key IS c.value ->> 'o' AND b.estimate IS c.value ->> 'e'
		  FROM json_each(?2) c
		  LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
		  LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3`
	// mirrorRowsAdvanceSQL moves the read time of the identical rows ?2
	// (IDs): no index holds read_at and no trigger reads it.
	mirrorRowsAdvanceSQL = `UPDATE tracker_cache_board_issues SET read_at = ?4
		WHERE root_id = ?1 AND board_id = ?3 AND issue_id IN (SELECT value FROM json_each(?2)) AND read_at < ?4`
	mirrorRowsUpsertSQL = `INSERT INTO tracker_cache_board_issues(root_id, issue_id, board_id, order_key, estimate, read_at)
		SELECT ?1, c.value ->> 'i', ?3, c.value ->> 'o', c.value ->> 'e', ?4 FROM json_each(?2) c
		 WHERE ?5 OR EXISTS (SELECT 1 FROM tracker_cache_board_issues b WHERE b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3)
		ON CONFLICT(root_id, issue_id, board_id) DO UPDATE SET order_key = excluded.order_key, estimate = excluded.estimate,
		    read_at = excluded.read_at
		WHERE excluded.read_at >= read_at AND (order_key IS NOT excluded.order_key OR estimate IS NOT excluded.estimate)
		RETURNING issue_id`
	mirrorRowsRemoveSQL = `DELETE FROM tracker_cache_board_issues
		WHERE root_id = ?1 AND board_id = ?3 AND read_at <= ?4 AND issue_id IN (SELECT value FROM json_each(?2))
		RETURNING issue_id`
	mirrorRowsKeptSQL = `SELECT value FROM json_each(?2) c WHERE EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		WHERE b.root_id = ?1 AND b.issue_id = c.value AND b.board_id = ?3)`
	// mirrorRowsRemovedAtSQL records a removal; its callers run it only for
	// a read later than the board's latest removal.
	mirrorRowsRemovedAtSQL = `UPDATE tracker_cache_boards SET rows_removed_at = ?2 WHERE root_id = ?1 AND board_id = ?3`
)

func (s *Store) writeMirrorRows(rootID int64, page MirrorRowPage, withCursor bool, cursor []byte) (changes MirrorChanges, err error) {
	if err := page.check(); err != nil {
		return MirrorChanges{}, err
	}
	rows := make([]mirrorCardRow, len(page.Rows))
	for i, row := range page.Rows {
		rows[i] = newMirrorCardRow(row.Card)
		rows[i].OrderKey, rows[i].Estimate = row.OrderKey, row.Estimate
	}
	encoded, err := encodeMirrorJSON(rows)
	if err != nil {
		return MirrorChanges{}, err
	}
	action := fmt.Sprintf("store: write tracker mirror %d board %s rows", rootID, page.BoardID)
	err = s.mirrorBoardWrite(rootID, page.BoardID, page.Lifetime, action, true, func(tx *sql.Tx, board mirrorBoard) error {
		addsRows := page.ReadAt >= board.rowsRemovedAt
		var err error
		if changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, page.Catalog, page.ReadAt); err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := writeMirrorRowsTx(tx, rootID, page, encoded, addsRows, &changes); err != nil {
				return err
			}
		}
		if err := removeMirrorRowsTx(tx, rootID, page, board.rowsRemovedAt, &changes); err != nil {
			return err
		}
		if withCursor {
			return writeMirrorCursorTx(tx, rootID, page.BoardID, cursor, page.ReadAt)
		}
		return nil
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	slices.Sort(changes.Superseded)
	return changes, nil
}

// writeMirrorRowsTx stores the page's cards and rows: one statement sorts
// the rows, then the card statements, the row upsert when a row is to be
// written, and the read-time move when one is identical.
func writeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, encoded string, addsRows bool, changes *MirrorChanges) error {
	var write bool
	var advance []string
	err := scanMirrorRows(tx, mirrorRowsStateSQL, []any{rootID, encoded, page.BoardID, page.ReadAt, addsRows}, func(r rowScanner) error {
		var id string
		var superseded, writes, advances sql.NullBool
		if err := r.Scan(&id, &superseded, &writes, &advances); err != nil {
			return err
		}
		if superseded.Bool {
			changes.Superseded = append(changes.Superseded, id)
		}
		write = write || writes.Bool
		if advances.Bool {
			advance = append(advance, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sort rows: %w", err)
	}
	if changes.Cards, err = storeMirrorCardsTx(tx, mirrorRowCards, rootID, encoded, page.BoardID, addsRows); err != nil {
		return err
	}
	if write {
		if changes.Rows, err = queryIDs(tx, mirrorRowsUpsertSQL, rootID, encoded, page.BoardID, page.ReadAt, addsRows); err != nil {
			return fmt.Errorf("upsert rows: %w", err)
		}
		slices.Sort(changes.Rows)
	}
	if len(advance) > 0 {
		list, err := encodeMirrorIDs(advance)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorRowsAdvanceSQL, rootID, list, page.BoardID, page.ReadAt); err != nil {
			return fmt.Errorf("record the rows' read: %w", err)
		}
	}
	return nil
}

// removeMirrorRowsTx removes the rows of page.Missing stored by reads that
// began no later than the page, and records the page as the board's latest
// removal whether or not the board held a row of them.
func removeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, removedAt int64, changes *MirrorChanges) error {
	if len(page.Missing) == 0 {
		return nil
	}
	encoded, err := encodeMirrorIDs(page.Missing)
	if err != nil {
		return err
	}
	if changes.Removed, err = queryIDs(tx, mirrorRowsRemoveSQL, rootID, encoded, page.BoardID, page.ReadAt); err != nil {
		return fmt.Errorf("remove rows: %w", err)
	}
	slices.Sort(changes.Removed)
	if page.ReadAt > removedAt {
		if _, err := tx.Exec(mirrorRowsRemovedAtSQL, rootID, page.ReadAt, page.BoardID); err != nil {
			return fmt.Errorf("record the removal: %w", err)
		}
	}
	if len(changes.Removed) < len(page.Missing) {
		kept, err := queryIDs(tx, mirrorRowsKeptSQL, rootID, encoded, page.BoardID)
		if err != nil {
			return fmt.Errorf("find rows a later read stored: %w", err)
		}
		changes.Superseded = append(changes.Superseded, kept...)
	}
	return nil
}

// mirrorCursorSQL stores a board's cursor (NULL: the walk starts over)
// from a read that began at ?4, unless a later read was admitted for it:
// one that stored a cursor (cursor_read_at) or one recorded as succeeded
// (read_at), whose cursor is the stored one when it wrote none. The same
// cursor from a later read moves cursor_read_at alone, and from the same
// read writes nothing.
const mirrorCursorSQL = `INSERT INTO tracker_cache_sync(root_id, board_id, cursor, cursor_read_at) VALUES (?1, ?2, ?3, ?4)
	ON CONFLICT(root_id, board_id) DO UPDATE SET cursor = excluded.cursor, cursor_read_at = excluded.cursor_read_at
	WHERE excluded.cursor_read_at >= read_at
	  AND (excluded.cursor_read_at > cursor_read_at OR (excluded.cursor_read_at = cursor_read_at AND cursor IS NOT excluded.cursor))`

// writeMirrorCursorTx stores a board's cursor by mirrorCursorSQL.
func writeMirrorCursorTx(tx *sql.Tx, rootID int64, boardID string, cursor []byte, readAt int64) error {
	if _, err := tx.Exec(mirrorCursorSQL, rootID, boardID, cursor, readAt); err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	return nil
}

// MirrorReconcile is what a reconcile of a board found and did: Missing
// keeps the listing's order, Removed is sorted by issue ID.
type MirrorReconcile struct {
	// Missing are the listed issues with no row on the board: the caller
	// reads their rows and stores them (WriteMirrorRows).
	Missing []string
	// Removed are the issues whose row was removed, not being listed.
	Removed []string
}

// The reconcile statements: ?1 root, ?2 board, ?3 when the listing's read
// began, ?4 the JSON listing.
var (
	mirrorReconcileMissingSQL = `SELECT l.value FROM json_each(?4) l
		WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		                   WHERE b.root_id = ?1 AND b.issue_id = l.value AND b.board_id = ?2)
		ORDER BY l.key`
	// mirrorReconcileRemoveSQL walks the board's rows in its order index,
	// which leaves out read_at, and reads read_at by key (r) only for the
	// rows the listing leaves out.
	mirrorReconcileRemoveSQL = `DELETE FROM tracker_cache_board_issues WHERE root_id = ?1 AND board_id = ?2 AND issue_id IN (
		SELECT b.issue_id FROM tracker_cache_board_issues b
		  CROSS JOIN tracker_cache_board_issues r ON r.root_id = b.root_id AND r.issue_id = b.issue_id AND r.board_id = b.board_id
		 WHERE b.root_id = ?1 AND b.board_id = ?2 AND b.issue_id NOT IN (SELECT value FROM json_each(?4)) AND r.read_at <= ?3
		 LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)
		RETURNING issue_id`
)

// ReconcileMirrorBoard compares a listing of the issues on a board, read
// from readAt, with the board's rows. It returns the listed issues that
// have no row and, only when the listing is complete, removes the rows it
// does not list that reads begun no later than it stored, at most
// clearMirrorChunk per transaction, and records the listing as the board's
// latest removal, so no read that began earlier adds a row the listing
// leaves out. An incomplete listing removes nothing. An issue left with no
// row and no link becomes unkept. The board must be listed in lifetime
// (ErrTrackerCacheUnlisted).
func (s *Store) ReconcileMirrorBoard(rootID int64, boardID string, lifetime int64, listed []string, complete bool, readAt int64) (result MirrorReconcile, err error) {
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
	for batch := 0; ; batch++ {
		removed := 0
		err = s.mirrorBoardWrite(rootID, boardID, lifetime, action, true, func(tx *sql.Tx, board mirrorBoard) error {
			if batch == 0 {
				var err error
				if result.Missing, err = queryIDs(tx, mirrorReconcileMissingSQL, rootID, boardID, readAt, encoded); err != nil {
					return fmt.Errorf("find missing rows: %w", err)
				}
			}
			if !complete {
				return nil
			}
			ids, err := queryIDs(tx, mirrorReconcileRemoveSQL, rootID, boardID, readAt, encoded)
			if err != nil {
				return fmt.Errorf("remove unlisted rows: %w", err)
			}
			removed = len(ids)
			result.Removed = append(result.Removed, ids...)
			if readAt > board.rowsRemovedAt {
				if _, err := tx.Exec(mirrorRowsRemovedAtSQL, rootID, readAt, boardID); err != nil {
					return fmt.Errorf("record the removal: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return MirrorReconcile{}, err
		}
		if removed < clearMirrorChunk {
			break
		}
	}
	slices.Sort(result.Removed)
	return result, nil
}
