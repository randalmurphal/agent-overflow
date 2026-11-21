package store

import (
	"database/sql"
	"errors"
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
	ReadAt          int64
}

func (p MirrorRowPage) check() error {
	switch {
	case p.BoardID == "" || p.ReadAt <= 0 || p.EstimateResetAt < 0:
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
// rows are stored. An
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
	// content stored by an earlier read, whose read time alone moves),
	// whether it is a row the board gains, and whether it names a
	// reference the mirror lacks (mirrorRowLacksSQL), with its place in the
	// page.
	mirrorRowsStateSQL = `SELECT c.value ->> 'i',
		    (b.root_id IS NULL AND NOT ?5) OR i.updated_at > c.value ->> 'u'
		      OR (b.read_at > ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    (b.root_id IS NULL AND ?5)
		      OR (b.read_at <= ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e')),
		    b.read_at < ?4 AND b.order_key IS c.value ->> 'o' AND b.estimate IS c.value ->> 'e',
		    b.root_id IS NULL AND ?5,
		    ` + mirrorRowLacksSQL + `, c.key
		  FROM json_each(?2) c
		  LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
		  LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3`
	// mirrorRowsAdvanceSQL moves the read time of the identical rows ?2
	// (IDs).
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
			if err := writeMirrorRowsTx(tx, rootID, page, rows, encoded, addsRows, board, &changes); err != nil {
				return err
			}
		} else {
			changes.StructureDue = board.structureDueAt() > 0
		}
		if err := removeMirrorRowsTx(tx, rootID, page, board.rowsRemovedAt, &changes); err != nil {
			return err
		}
		switch {
		case !withCursor:
			return nil
		case page.EstimateResetAt != board.estimateResetAt || page.ReadAt <= board.estimateResetAt:
			changes.CursorRefused = true
			return nil
		}
		return writeMirrorCursorTx(tx, rootID, page.BoardID, cursor, page.ReadAt)
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	slices.Sort(changes.Superseded)
	slices.Sort(changes.Added)
	return changes, nil
}

// mirrorRowLacksSQL holds for a row c of board ?3 that names a status,
// user or sprint the mirror has no row for and has not marked unexplained.
const mirrorRowLacksSQL = `(c.value ->> 's' <> ''
	      AND NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st WHERE st.root_id = ?1 AND st.status_id = c.value ->> 's')
	      AND NOT EXISTS (SELECT 1 FROM tracker_cache_board_unexplained u
	                       WHERE u.root_id = ?1 AND u.board_id = ?3 AND u.kind = 1 AND u.ref_id = c.value ->> 's' AND u.named_at = 0))
	    OR (c.value ->> 'a' <> ''
	      AND NOT EXISTS (SELECT 1 FROM tracker_cache_users us WHERE us.root_id = ?1 AND us.user_id = c.value ->> 'a')
	      AND NOT EXISTS (SELECT 1 FROM tracker_cache_board_unexplained u
	                       WHERE u.root_id = ?1 AND u.board_id = ?3 AND u.kind = 2 AND u.ref_id = c.value ->> 'a' AND u.named_at = 0))
	    OR EXISTS (SELECT 1 FROM json_each(c.value -> 'm') m
	      WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_sprints sp WHERE sp.root_id = ?1 AND sp.board_id = ?3 AND sp.sprint_id = m.value)
	        AND NOT EXISTS (SELECT 1 FROM tracker_cache_board_unexplained u
	                         WHERE u.root_id = ?1 AND u.board_id = ?3 AND u.kind = 3 AND u.ref_id = m.value AND u.named_at = 0))`

// mirrorRowsPendingSQL marks pending each reference of the JSON list ?2
// of mirrorRefRow, named by rows of board ?3, that the mirror lacks, as
// named by the page's read ?4: a new mark, or a pending one the
// structure's read ?5 began after, which makes the board due again.
// An unexplained mark stays as it is.
const mirrorRowsPendingSQL = `INSERT INTO tracker_cache_board_unexplained(root_id, board_id, kind, ref_id, named_at)
	SELECT ?1, ?3, r.value ->> 'k', r.value ->> 'r', ?4 FROM json_each(?2) r
	 WHERE CASE r.value ->> 'k'
	   WHEN 1 THEN NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st WHERE st.root_id = ?1 AND st.status_id = r.value ->> 'r')
	   WHEN 2 THEN NOT EXISTS (SELECT 1 FROM tracker_cache_users us WHERE us.root_id = ?1 AND us.user_id = r.value ->> 'r')
	   ELSE NOT EXISTS (SELECT 1 FROM tracker_cache_sprints sp WHERE sp.root_id = ?1 AND sp.board_id = ?3 AND sp.sprint_id = r.value ->> 'r') END
	ON CONFLICT(root_id, board_id, kind, ref_id) DO UPDATE SET named_at = excluded.named_at
	WHERE named_at > 0 AND named_at < ?5 AND excluded.named_at > named_at`

// mirrorRefRow is a reference a row names, as mirrorRowsPendingSQL reads
// it: its kind (1 status, 2 user, 3 sprint) and ID.
type mirrorRefRow struct {
	Kind int    `json:"k"`
	ID   string `json:"r"`
}

// mirrorRefsOf is the distinct references rows name.
func mirrorRefsOf(rows []mirrorCardRow) []mirrorRefRow {
	seen := map[mirrorRefRow]bool{}
	var refs []mirrorRefRow
	add := func(kind int, id string) {
		if ref := (mirrorRefRow{Kind: kind, ID: id}); id != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for _, row := range rows {
		add(1, row.Status)
		add(2, row.Assignee)
		for _, sprint := range row.Sprints {
			add(3, sprint)
		}
	}
	return refs
}

// writeMirrorRowsTx stores the page's cards and rows: one statement sorts
// the rows, then the card statements, the row upsert when a row is to be
// written, and the read-time move when one is identical. A row the page
// does not supersede that names a reference the mirror lacks and has not
// marked unexplained marks it pending (mirrorRowsPendingSQL), one
// statement more, which makes the board due a structure read when the
// page began no earlier than the structure's read.
func writeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, rows []mirrorCardRow, encoded string, addsRows bool,
	board mirrorBoard, changes *MirrorChanges) error {
	var write bool
	var advance []string
	var lacking []mirrorCardRow
	err := scanMirrorRows(tx, mirrorRowsStateSQL, []any{rootID, encoded, page.BoardID, page.ReadAt, addsRows}, func(r rowScanner) error {
		var id string
		var superseded, writes, advances, adds, rowLacks sql.NullBool
		var at int
		if err := r.Scan(&id, &superseded, &writes, &advances, &adds, &rowLacks, &at); err != nil {
			return err
		}
		if superseded.Bool {
			changes.Superseded = append(changes.Superseded, id)
		}
		if adds.Bool {
			changes.Added = append(changes.Added, id)
		}
		if rowLacks.Bool && !superseded.Bool {
			lacking = append(lacking, rows[at])
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
	if len(lacking) > 0 {
		list, err := encodeMirrorJSON(mirrorRefsOf(lacking))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorRowsPendingSQL, rootID, list, page.BoardID, page.ReadAt, board.readAt); err != nil {
			return fmt.Errorf("mark lacking references: %w", err)
		}
	}
	changes.StructureDue = board.structureDueAt() > 0 || len(lacking) > 0 && page.ReadAt >= board.readAt
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
	// Unsettled are the rows the listing does not hold that a read began
	// after settledAt stored, sorted: the listing may not show them yet,
	// so it does not judge them, and the caller reads them by ID
	// (WriteMirrorRows removes those the board no longer holds).
	Unsettled []string
}

// The reconcile statements: ?1 root, ?2 board, ?3 when the listing's read
// began, ?4 the JSON listing, ?5 the settled bound, ?6 whether the
// listing is complete.
var (
	// mirrorReconcileFoundSQL is the listed issues with no row (0, in the
	// listing's order) and, for a complete listing, the unsettled rows (1).
	mirrorReconcileFoundSQL = `SELECT 0, l.value, l.key FROM json_each(?4) l
		WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		                   WHERE b.root_id = ?1 AND b.issue_id = l.value AND b.board_id = ?2)
		UNION ALL
		SELECT 1, b.issue_id, 0 FROM tracker_cache_board_issues b
		 WHERE ?6 AND b.root_id = ?1 AND b.board_id = ?2 AND b.read_at > ?5 AND b.read_at <= ?3
		   AND b.issue_id NOT IN (SELECT value FROM json_each(?4))
		ORDER BY 1, 3, 2`
	// mirrorReconcileRemoveSQL walks the board's rows in its order index.
	mirrorReconcileRemoveSQL = `DELETE FROM tracker_cache_board_issues WHERE root_id = ?1 AND board_id = ?2 AND issue_id IN (
		SELECT b.issue_id FROM tracker_cache_board_issues b
		 WHERE b.root_id = ?1 AND b.board_id = ?2 AND b.issue_id NOT IN (SELECT value FROM json_each(?4)) AND b.read_at <= ?5
		 LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)
		RETURNING issue_id`
)

// ReconcileMirrorBoard compares a listing of the issues on a board, read
// from readAt, with the board's rows. It returns the listed issues that
// have no row and, only when the listing is complete, removes the rows it
// does not list that reads begun no later than settledAt stored, at most
// clearMirrorChunk per transaction, returns those it does not list that
// reads begun after settledAt and no later than readAt stored
// (Unsettled), and records the listing as the board's latest removal, so
// no read that began earlier adds a row the listing leaves out. A
// listing that may show a row late (a search index) leaves the rows read
// within that lag to a read by ID; settledAt equal to readAt judges every
// row. An incomplete listing removes nothing and returns no Unsettled.
// An issue left with no row and no link becomes unkept. The board must
// be listed in lifetime (ErrTrackerCacheUnlisted). A call that fails
// after a transaction of it committed returns, with the error, the rows
// those transactions removed and no Missing or Unsettled: the caller
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
	for batch := 0; ; batch++ {
		// What this transaction found and removed, taken only once it commits.
		var missing, unsettled, removed []string
		err = s.mirrorBoardWrite(rootID, boardID, lifetime, action, true, func(tx *sql.Tx, board mirrorBoard) error {
			if batch == 0 {
				var err error
				if missing, unsettled, err = reconcileFoundTx(tx, rootID, boardID, readAt, encoded, settledAt, complete); err != nil {
					return fmt.Errorf("find missing and unsettled rows: %w", err)
				}
			}
			if !complete {
				return nil
			}
			var err error
			if removed, err = queryIDs(tx, mirrorReconcileRemoveSQL, rootID, boardID, readAt, encoded, settledAt); err != nil {
				return fmt.Errorf("remove unlisted rows: %w", err)
			}
			if readAt > board.rowsRemovedAt {
				if _, err := tx.Exec(mirrorRowsRemovedAtSQL, rootID, readAt, boardID); err != nil {
					return fmt.Errorf("record the removal: %w", err)
				}
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
		if len(removed) < clearMirrorChunk {
			break
		}
	}
	slices.Sort(result.Removed)
	return result, nil
}

// reconcileFoundTx runs mirrorReconcileFoundSQL: the listed issues with no
// row, in the listing's order, and the unsettled rows, sorted.
func reconcileFoundTx(tx *sql.Tx, rootID int64, boardID string, readAt int64, listed string, settledAt int64, complete bool) (missing, unsettled []string, err error) {
	rows, err := tx.Query(mirrorReconcileFoundSQL, rootID, boardID, readAt, listed, settledAt, complete)
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
