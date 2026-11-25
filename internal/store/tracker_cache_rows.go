package store

import (
	"database/sql"
	"fmt"
	"slices"
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
// caller captured before the read (MirrorSync.Lifetime). A page of a
// cursor walk names the board's estimate reset captured with the cursor
// the walk began from (MirrorSync.EstimateResetAt).
type MirrorRowPage struct {
	BoardID         string
	Lifetime        int64
	EstimateResetAt int64
	Rows            []MirrorRow
	Missing         []string
	Catalog         MirrorCatalog
	ReadAt          MirrorRead
}

func (p MirrorRowPage) check(readAt int64) error {
	switch {
	case p.BoardID == "" || readAt <= 0 || p.EstimateResetAt < 0:
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
// cursor, unless a read that began later was admitted for them. The cursor
// is stored only when page.EstimateResetAt is the board's current estimate
// reset and the page's read began after it: a walk begun under an earlier
// estimate definition passed rows whose numbers are in that definition,
// and a page read no later than the reset holds rows reads show as
// unknown, so its cursor is refused (MirrorChanges.CursorRefused) and the
// rows are stored. A page whose new row the board's latest removal
// refused (MirrorChanges.Refused) stores no cursor either, so the walk
// reads the page again from a read that began after the removal. An
// identical page from a later read moves the rows', their cards' and the
// cursor's read times alone; one from the same read writes nothing. A
// read with no rows that returned the stored cursor need not be written:
// recording it as succeeded (RecordMirrorSyncAttempt) admits the cursor as
// its own. The board must be listed in page.Lifetime
// (ErrTrackerCacheUnlisted).
func (s *Store) WriteMirrorRowPage(rootID int64, page MirrorRowPage, cursor []byte) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, true, cursor)
}

// WriteMirrorRows stores a read of a board's rows for a set of issues, as
// WriteMirrorRowPage does but with no cursor, and removes the rows of the
// issues the board no longer lists (page.Missing) unless a read that
// began later was admitted for them.
// Missing issues are recorded as the board's latest removal even when it
// holds no row of them, so no read that began earlier adds a row. A read
// that began before the board's newest complete listing removes no row,
// the listing being a later read that may have named it. The caller reads
// the rows it refused for either (MirrorChanges.Refused) again. An issue
// left with no row and no link becomes unkept.
func (s *Store) WriteMirrorRows(rootID int64, page MirrorRowPage) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, false, nil)
}

// The row statements: ?1 root, ?2 the JSON list, ?3 board, ?4 when the
// read began, ?5 whether the page may add rows.
var (
	// mirrorRowsStateSQL sorts each row of the page: superseded (the
	// stored card supersedes its card, it is new and the page may not add
	// it, or a later read stored other content), to write (new and
	// addable, or other content from a read no earlier), or to advance
	// (identical content stored by an earlier read, whose read time alone
	// moves), whether the stored card admits its card from
	// an earlier read (mirrorCardAdvancesSQL), whether it is new on
	// the board (gained when the page may add rows, refused when not),
	// and whether the card names other sprints (mirrorCardSprintsMovedSQL).
	mirrorRowsStateSQL = `SELECT c.value ->> 'i',
		    (b.root_id IS NULL AND NOT ?5) OR ` + mirrorCardRefusedSQL("?4") + `
		      OR (b.read_at > ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    (b.root_id IS NULL AND ?5)
		      OR (b.read_at <= ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    b.read_at < ?4 AND b.order_key IS c.value ->> 'o' AND b.estimate IS c.value ->> 'e',
		    (b.root_id IS NOT NULL OR ?5) AND ` + mirrorCardAdvancesSQL("?4") + `,
		    b.root_id IS NULL, ` + mirrorCardSprintsMovedSQL + `
		  FROM json_each(?2) c
		  LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
		  LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3`
	// mirrorRowsAdvanceSQL moves forward the read time of the rows ?2 (IDs).
	mirrorRowsAdvanceSQL = `UPDATE tracker_cache_board_issues SET read_at = ?4
		WHERE root_id = ?1 AND board_id = ?3 AND issue_id IN (SELECT value FROM json_each(?2)) AND read_at < ?4`
	mirrorRowsUpsertSQL = `INSERT INTO tracker_cache_board_issues(root_id, issue_id, board_id, order_key, estimate, read_at)
		SELECT ?1, c.value ->> 'i', ?3, c.value ->> 'o', c.value ->> 'e', ?4 FROM json_each(?2) c
		 WHERE ?5 OR EXISTS (SELECT 1 FROM tracker_cache_board_issues b WHERE b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3)
		ON CONFLICT(root_id, issue_id, board_id) DO UPDATE SET order_key = excluded.order_key, estimate = excluded.estimate,
		    read_at = excluded.read_at
		WHERE excluded.read_at >= read_at AND (order_key IS NOT excluded.order_key OR estimate IS NOT excluded.estimate)
		RETURNING issue_id`
	// mirrorRowsRemoveSQL removes the rows of the issues ?2 (IDs) that
	// reads begun no later than ?4 stored, by key: a read by ID's absences
	// and a reconcile's unlisted rows.
	mirrorRowsRemoveSQL = `DELETE FROM tracker_cache_board_issues
		WHERE root_id = ?1 AND board_id = ?3 AND read_at <= ?4 AND issue_id IN (SELECT value FROM json_each(?2))
		RETURNING issue_id`
	// mirrorRowsKeptSQL reads the rows of the issues ?2 (IDs) the board
	// holds, and whether a read begun no later than ?4 stored each.
	mirrorRowsKeptSQL = `SELECT b.issue_id, b.read_at <= ?4 FROM json_each(?2) c
		CROSS JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = c.value AND b.board_id = ?3`
	// mirrorRowsRemovedAtSQL records a removal; its callers run it only for
	// a read later than the board's latest removal.
	mirrorRowsRemovedAtSQL = `UPDATE tracker_cache_boards SET rows_removed_at = ?2 WHERE root_id = ?1 AND board_id = ?3`
)

func (s *Store) writeMirrorRows(rootID int64, page MirrorRowPage, withCursor bool, cursor []byte) (changes MirrorChanges, err error) {
	read, err := page.ReadAt.in(rootID)
	if err != nil {
		return MirrorChanges{}, err
	}
	readAt := read.at
	if err := page.check(readAt); err != nil {
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
		addsRows := readAt >= board.rowsRemovedAt
		catalog, err := upsertMirrorCatalogTx(tx, rootID, page.Catalog, readAt)
		if err != nil {
			return err
		}
		changes.Catalog, changes.Threads = catalog.Changed, catalog.Threads
		if len(rows) > 0 {
			if err := writeMirrorRowsTx(tx, rootID, page, readAt, rows, encoded, addsRows, board, &changes); err != nil {
				return err
			}
		} else {
			changes.StructureDue = board.structureDueAt() > 0
		}
		if err := removeMirrorRowsTx(tx, rootID, page, readAt, board, &changes); err != nil {
			return err
		}
		switch {
		case !withCursor:
			return nil
		case page.EstimateResetAt != board.estimateResetAt || readAt <= board.estimateResetAt || len(changes.Refused) > 0:
			changes.CursorRefused = true
			return nil
		}
		return writeMirrorCursorTx(tx, rootID, page.BoardID, cursor, readAt)
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	slices.Sort(changes.Superseded)
	slices.Sort(changes.Added)
	slices.Sort(changes.Refused)
	return changes, nil
}

// writeMirrorRowsTx stores the page's cards and rows: one statement sorts
// the rows, then the card statements (storeMirrorCardsTx), the row upsert
// when a row is to be written, and the read-time move when one is
// identical. A card the page stores that names a reference the mirror
// lacks and has not marked unexplained marks it pending on the page's
// board (mirrorCardMarkSQL),
// which makes the board due a structure read when the page began no
// earlier than the structure's read.
func writeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, readAt int64, rows []mirrorCardRow, encoded string, addsRows bool,
	board mirrorBoard, changes *MirrorChanges) error {
	var write bool
	var advance, advancing, moved []string
	err := scanMirrorRows(tx, mirrorRowsStateSQL, []any{rootID, encoded, page.BoardID, readAt, addsRows}, func(r rowScanner) error {
		var id string
		var superseded, writes, advances, cardAdvances, fresh sql.NullBool
		var sprintsMoved bool
		if err := r.Scan(&id, &superseded, &writes, &advances, &cardAdvances, &fresh, &sprintsMoved); err != nil {
			return err
		}
		if cardAdvances.Bool {
			advancing = append(advancing, id)
		}
		if sprintsMoved {
			moved = append(moved, id)
		}
		if superseded.Bool {
			changes.Superseded = append(changes.Superseded, id)
		}
		switch {
		case fresh.Bool && addsRows:
			changes.Added = append(changes.Added, id)
		case fresh.Bool:
			changes.Refused = append(changes.Refused, id)
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
	slices.Sort(advancing)
	slices.Sort(moved)
	stored, err := storeMirrorCardsTx(tx, mirrorRowCards, rootID, rows, encoded, readAt, moved, advancing,
		mirrorCardReadMove(tx, rootID, readAt, advancing), page.BoardID, addsRows)
	if err != nil {
		return err
	}
	changes.Cards, changes.Threads = stored.changed, unionSorted(changes.Threads, stored.threads)
	if write {
		if changes.Rows, err = queryIDs(tx, mirrorRowsUpsertSQL, rootID, encoded, page.BoardID, readAt, addsRows); err != nil {
			return fmt.Errorf("upsert rows: %w", err)
		}
		slices.Sort(changes.Rows)
	}
	if len(advance) > 0 {
		list, err := encodeMirrorIDs(advance)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorRowsAdvanceSQL, rootID, list, page.BoardID, readAt); err != nil {
			return fmt.Errorf("record the rows' read: %w", err)
		}
	}
	_, marked := slices.BinarySearch(stored.marked, page.BoardID)
	changes.StructureDue = board.structureDueAt() > 0 || marked && readAt >= board.readAt
	return nil
}

// removeMirrorRowsTx removes the rows of page.Missing stored by reads that
// began no later than the page, and records the page as the board's latest
// removal whether or not the board held a row of them. A page that began
// before the newest complete listing the board reconciled removes none:
// the listing may have named the rows, so each it would have removed is
// owed a read from after the listing (Refused). Such a page records no
// removal, the listing's being later.
func removeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, readAt int64, board mirrorBoard, changes *MirrorChanges) error {
	if len(page.Missing) == 0 {
		return nil
	}
	encoded, err := encodeMirrorIDs(page.Missing)
	if err != nil {
		return err
	}
	refused := readAt < board.listingReadAt
	if !refused {
		if changes.Removed, err = queryIDs(tx, mirrorRowsRemoveSQL, rootID, encoded, page.BoardID, readAt); err != nil {
			return fmt.Errorf("remove rows: %w", err)
		}
		slices.Sort(changes.Removed)
		if readAt > board.rowsRemovedAt {
			if _, err := tx.Exec(mirrorRowsRemovedAtSQL, rootID, readAt, page.BoardID); err != nil {
				return fmt.Errorf("record the removal: %w", err)
			}
		}
	}
	if !refused && len(changes.Removed) == len(page.Missing) {
		return nil
	}
	err = scanMirrorRows(tx, mirrorRowsKeptSQL, []any{rootID, encoded, page.BoardID, readAt}, func(r rowScanner) error {
		var id string
		var owed bool
		if err := r.Scan(&id, &owed); err != nil {
			return err
		}
		changes.Superseded = append(changes.Superseded, id)
		if owed {
			changes.Refused = append(changes.Refused, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("find rows the page left: %w", err)
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
