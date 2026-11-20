package store

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
)

// Bounds of one write of cards or rows. A card's sprints bound what
// replacing them deletes; a page's rows and missing issues bound the rows
// it adds and removes.
const (
	mirrorMaxCardSprints = 50
	mirrorMaxPageRows    = 500
)

// MirrorCard is an issue as a board or backlog row draws it, the same on
// every board. Sprints are the sprints the card read names. Data is the
// rest of the card. StatusSince is when the card's status began, set by
// the store from the issue's stored history (zero: unknown); writes ignore
// it.
type MirrorCard struct {
	IssueID, Key, ProjectID          string
	TypeID                           string
	TypeLevel                        int
	StatusID, AssigneeID             string
	PriorityID, ParentID             string
	CreatedAt, UpdatedAt, ResolvedAt int64
	StatusSince                      int64
	Sprints                          []string
	Data                             []byte
}

// MirrorRow is an issue on one board: its card with that board's order
// key and estimate (nil for none).
type MirrorRow struct {
	Card     MirrorCard
	OrderKey string
	Estimate *float64
}

// MirrorRowPage is one read of a board's rows: the rows, the issues asked
// for that the board no longer lists (Missing), the users and statuses
// they name, and when the read began (ReadAt).
type MirrorRowPage struct {
	BoardID string
	Rows    []MirrorRow
	Missing []string
	Catalog MirrorCatalog
	ReadAt  int64
}

// MirrorChanges is what a write of cards or rows changed, each list sorted
// by issue ID.
type MirrorChanges struct {
	// Cards are the issues whose card was stored new or changed.
	Cards []string
	// Rows are the issues whose row on the board was stored new or changed.
	Rows []string
	// Removed are the issues whose row on the board was removed.
	Removed []string
	// Superseded are the issues of which the write stored less than it
	// carried: a card older than the stored one, a row or a removal of a
	// row stored by a read that began later, or a new row from a read that
	// began before the board last lost rows.
	Superseded []string
	// Unkept are the cards UpsertMirrorCards did not store because the
	// mirror does not keep their issue.
	Unkept []string
	// Catalog reports a changed user or status.
	Catalog bool
}

// mirrorCardRow is a card as the card statements read it from JSON, with
// its row's order key and estimate when it is one.
type mirrorCardRow struct {
	ID         string   `json:"i"`
	Key        string   `json:"k"`
	Project    string   `json:"p"`
	Type       string   `json:"t"`
	TypeLevel  int      `json:"l"`
	Status     string   `json:"s"`
	Assignee   string   `json:"a"`
	Priority   string   `json:"r"`
	Parent     string   `json:"n"`
	CreatedAt  int64    `json:"c"`
	UpdatedAt  int64    `json:"u"`
	ResolvedAt int64    `json:"z"`
	Data       string   `json:"d"`
	Sprints    []string `json:"m,omitempty"`
	OrderKey   string   `json:"o"`
	Estimate   *float64 `json:"e"`
}

func newMirrorCardRow(c MirrorCard) mirrorCardRow {
	return mirrorCardRow{ID: c.IssueID, Key: c.Key, Project: c.ProjectID, Type: c.TypeID, TypeLevel: c.TypeLevel,
		Status: c.StatusID, Assignee: c.AssigneeID, Priority: c.PriorityID, Parent: c.ParentID,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, ResolvedAt: c.ResolvedAt,
		Data: hex.EncodeToString(c.Data), Sprints: c.Sprints}
}

func (c MirrorCard) check() error {
	if c.IssueID == "" || c.Key == "" || c.UpdatedAt <= 0 || c.CreatedAt < 0 || c.ResolvedAt < 0 {
		return fmt.Errorf("%w: card %q needs an ID, a key and an update time", ErrInvalidTrackerCache, c.IssueID)
	}
	if len(c.Sprints) > mirrorMaxCardSprints {
		return fmt.Errorf("%w: card %s names %d sprints, over %d", ErrInvalidTrackerCache, c.IssueID, len(c.Sprints), mirrorMaxCardSprints)
	}
	seen := make(map[string]bool, len(c.Sprints))
	for _, sprint := range c.Sprints {
		if sprint == "" || seen[sprint] {
			return fmt.Errorf("%w: card %s names sprint %q empty or twice", ErrInvalidTrackerCache, c.IssueID, sprint)
		}
		seen[sprint] = true
	}
	return nil
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

// The card statements read ?1 root and ?2 the JSON list of cards, and a
// card's filter that names the card c. A card replaces the stored one when
// its update time is not earlier and its content differs; its sprints are
// replaced by the same rule. A status change zeroes status_since.
const mirrorCardUpsertSQL = `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, project_id, type_id, type_level,
	        status_id, assignee_id, priority_id, parent_id, created_at, updated_at, resolved_at, data)
	SELECT ?1, c.value ->> 'i', c.value ->> 'k', c.value ->> 'p', c.value ->> 't', c.value ->> 'l', c.value ->> 's',
	       c.value ->> 'a', c.value ->> 'r', c.value ->> 'n', c.value ->> 'c', c.value ->> 'u', c.value ->> 'z',
	       unhex(c.value ->> 'd')
	  FROM json_each(?2) c WHERE `

const mirrorCardConflictSQL = `
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, project_id = excluded.project_id,
	    type_id = excluded.type_id, type_level = excluded.type_level, status_id = excluded.status_id,
	    assignee_id = excluded.assignee_id, priority_id = excluded.priority_id, parent_id = excluded.parent_id,
	    created_at = excluded.created_at, updated_at = excluded.updated_at, resolved_at = excluded.resolved_at,
	    data = excluded.data, status_since = CASE WHEN status_id IS excluded.status_id THEN status_since ELSE 0 END
	WHERE excluded.updated_at >= updated_at AND (issue_key IS NOT excluded.issue_key OR project_id IS NOT excluded.project_id
	   OR type_id IS NOT excluded.type_id OR type_level IS NOT excluded.type_level OR status_id IS NOT excluded.status_id
	   OR assignee_id IS NOT excluded.assignee_id OR priority_id IS NOT excluded.priority_id OR parent_id IS NOT excluded.parent_id
	   OR created_at IS NOT excluded.created_at OR updated_at IS NOT excluded.updated_at OR resolved_at IS NOT excluded.resolved_at
	   OR data IS NOT excluded.data)
	RETURNING issue_id`

// mirrorCardAdmittedSQL joins the stored issue i of card c when the card
// is not older than it. Each join from a JSON list is a CROSS JOIN, so the
// list drives it: without row counts the planner can otherwise walk the
// root's rows and probe the list for each.
const mirrorCardAdmittedSQL = ` CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
	AND i.updated_at <= c.value ->> 'u'`

const mirrorCardSprintsLeaveSQL = `DELETE FROM tracker_cache_issue_sprints WHERE root_id = ?1 AND (issue_id, sprint_id) IN (
	SELECT m.issue_id, m.sprint_id FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	  JOIN tracker_cache_issue_sprints m ON m.root_id = ?1 AND m.issue_id = i.issue_id
	 WHERE m.sprint_id NOT IN (SELECT value FROM json_each(c.value -> 'm')) AND `

const mirrorCardSprintsJoinSQL = `INSERT INTO tracker_cache_issue_sprints(root_id, issue_id, sprint_id)
	SELECT ?1, i.issue_id, sp.value FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	  JOIN json_each(c.value -> 'm') sp WHERE `

// mirrorCardStatements are the three statements that store cards, for one
// filter of the cards they take.
type mirrorCardStatements struct{ upsert, leave, join string }

func newMirrorCardStatements(filter string) mirrorCardStatements {
	return mirrorCardStatements{
		upsert: mirrorCardUpsertSQL + filter + mirrorCardConflictSQL,
		leave:  mirrorCardSprintsLeaveSQL + filter + `) RETURNING issue_id`,
		join:   mirrorCardSprintsJoinSQL + filter + ` ON CONFLICT DO NOTHING RETURNING issue_id`,
	}
}

var (
	// mirrorRowCards store a row page's cards: ?3 board, ?4 whether the
	// page may add rows; a card whose row it may not add is not stored.
	mirrorRowCards = newMirrorCardStatements(`(?4 OR EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		WHERE b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3))`)
	// mirrorLinkedCards store cards of the issues the mirror keeps.
	mirrorLinkedCards = newMirrorCardStatements(mirrorIssueKept("?1", "c.value ->> 'i'"))
)

// storeMirrorCardsTx runs statements over encoded cards and returns the
// issues whose card or sprints changed, sorted.
func storeMirrorCardsTx(tx *sql.Tx, statements mirrorCardStatements, args ...any) ([]string, error) {
	var changed []string
	for _, step := range []struct{ what, query string }{
		{"upsert cards", statements.upsert}, {"remove card sprints", statements.leave}, {"add card sprints", statements.join},
	} {
		ids, err := queryIDs(tx, step.query, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", step.what, err)
		}
		changed = append(changed, ids...)
	}
	slices.Sort(changed)
	return slices.Compact(changed), nil
}

// WriteMirrorRowPage stores one page of a board's changed rows and the
// cursor that page returned (nil: the next walk starts over), in one
// transaction: either all of it lands or none. A card is stored unless the
// mirror holds a later version; a row's order key and estimate, and the
// cursor, unless a read that began later stored them. An identical page
// writes nothing.
func (s *Store) WriteMirrorRowPage(rootID int64, page MirrorRowPage, cursor []byte) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, true, cursor)
}

// WriteMirrorRows stores a read of a board's rows for a set of issues, as
// WriteMirrorRowPage does but with no cursor, and removes the rows of the
// issues the board no longer lists (page.Missing) unless a read that began
// later stored them. An issue left with no row and no link becomes unkept.
func (s *Store) WriteMirrorRows(rootID int64, page MirrorRowPage) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, false, nil)
}

// The row statements: ?1 root, ?2 the JSON list, ?3 board, ?4 when the
// read began, ?5 whether the page may add rows.
const (
	mirrorRowsSupersededSQL = `SELECT c.value ->> 'i' FROM json_each(?2) c
		  LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
		  LEFT JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?3
		 WHERE (b.root_id IS NULL AND NOT ?5) OR i.updated_at > c.value ->> 'u'
		    OR (b.read_at > ?4 AND (b.order_key IS NOT c.value ->> 'o' OR b.estimate IS NOT c.value ->> 'e'))`
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
	mirrorRowsRemovedAtSQL = `UPDATE tracker_cache_boards SET rows_removed_at = ?2 WHERE root_id = ?1 AND board_id = ?3 AND rows_removed_at < ?2`
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
	err = s.mirrorBoardWrite(rootID, page.BoardID, action, true, func(tx *sql.Tx, board mirrorBoard) error {
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
		if err := removeMirrorRowsTx(tx, rootID, page, &changes); err != nil {
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

func writeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, encoded string, addsRows bool, changes *MirrorChanges) error {
	var err error
	if changes.Superseded, err = queryIDs(tx, mirrorRowsSupersededSQL, rootID, encoded, page.BoardID, page.ReadAt, addsRows); err != nil {
		return fmt.Errorf("find superseded rows: %w", err)
	}
	if changes.Cards, err = storeMirrorCardsTx(tx, mirrorRowCards, rootID, encoded, page.BoardID, addsRows); err != nil {
		return err
	}
	if changes.Rows, err = queryIDs(tx, mirrorRowsUpsertSQL, rootID, encoded, page.BoardID, page.ReadAt, addsRows); err != nil {
		return fmt.Errorf("upsert rows: %w", err)
	}
	slices.Sort(changes.Rows)
	return nil
}

// removeMirrorRowsTx removes the rows of page.Missing stored by reads that
// began no later than the page, and records the page as the board's latest
// removal when it removed one.
func removeMirrorRowsTx(tx *sql.Tx, rootID int64, page MirrorRowPage, changes *MirrorChanges) error {
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
	if len(changes.Removed) > 0 {
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

// writeMirrorCursorTx stores a board's cursor unless a read that began
// later stored one; an identical cursor writes nothing.
func writeMirrorCursorTx(tx *sql.Tx, rootID int64, boardID string, cursor []byte, readAt int64) error {
	query := `INSERT INTO tracker_cache_sync(root_id, board_id, cursor, cursor_read_at) VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT(root_id, board_id) DO UPDATE SET cursor = excluded.cursor, cursor_read_at = excluded.cursor_read_at
		WHERE excluded.cursor_read_at >= cursor_read_at AND cursor IS NOT excluded.cursor`
	if cursor == nil {
		query = `UPDATE tracker_cache_sync SET cursor = NULL, cursor_read_at = ?4
			WHERE root_id = ?1 AND board_id = ?2 AND cursor IS NOT ?3 AND cursor_read_at <= ?4`
	}
	if _, err := tx.Exec(query, rootID, boardID, cursor, readAt); err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	return nil
}

// UpsertMirrorCards stores cards read with no board (issues linked to
// threads) by the same rule as a row page: a card is not replaced by one
// with an earlier update time. A card of an issue the mirror does not keep
// (mirrorIssueKept) is not stored and is returned in Unkept. readAt is
// when the read began, which orders the catalog.
func (s *Store) UpsertMirrorCards(rootID int64, cards []MirrorCard, catalog MirrorCatalog, readAt int64) (changes MirrorChanges, err error) {
	if readAt <= 0 || len(cards) > mirrorMaxPageRows {
		return MirrorChanges{}, fmt.Errorf("%w: %d cards, over %d, or no read time", ErrInvalidTrackerCache, len(cards), mirrorMaxPageRows)
	}
	seen := make(map[string]bool, len(cards))
	rows := make([]mirrorCardRow, len(cards))
	for i, card := range cards {
		if err := card.check(); err != nil {
			return MirrorChanges{}, err
		}
		if seen[card.IssueID] {
			return MirrorChanges{}, fmt.Errorf("%w: issue %s is read twice", ErrInvalidTrackerCache, card.IssueID)
		}
		seen[card.IssueID] = true
		rows[i] = newMirrorCardRow(card)
	}
	if err := catalog.check(); err != nil {
		return MirrorChanges{}, err
	}
	encoded, err := encodeMirrorJSON(rows)
	if err != nil {
		return MirrorChanges{}, err
	}
	action := fmt.Sprintf("store: upsert tracker mirror %d cards", rootID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		if changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, catalog, readAt); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if changes.Unkept, err = queryIDs(tx, `SELECT c.value ->> 'i' FROM json_each(?2) c
			WHERE NOT `+mirrorIssueKept("?1", "c.value ->> 'i'"), rootID, encoded); err != nil {
			return fmt.Errorf("find unkept cards: %w", err)
		}
		if changes.Superseded, err = queryIDs(tx, `SELECT c.value ->> 'i' FROM json_each(?2) c
			CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i' AND i.updated_at > c.value ->> 'u'`,
			rootID, encoded); err != nil {
			return fmt.Errorf("find superseded cards: %w", err)
		}
		// Sorted here rather than by the statements, which would sort in a
		// temporary b-tree.
		slices.Sort(changes.Unkept)
		slices.Sort(changes.Superseded)
		changes.Cards, err = storeMirrorCardsTx(tx, mirrorLinkedCards, rootID, encoded)
		return err
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	return changes, nil
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
	mirrorReconcileRemoveSQL = `DELETE FROM tracker_cache_board_issues WHERE root_id = ?1 AND board_id = ?2 AND issue_id IN (
		SELECT b.issue_id FROM tracker_cache_board_issues b WHERE b.root_id = ?1 AND b.board_id = ?2 AND b.read_at <= ?3
		   AND b.issue_id NOT IN (SELECT value FROM json_each(?4)) LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)
		RETURNING issue_id`
)

// ReconcileMirrorBoard compares a listing of the issues on a board, read
// from readAt, with the board's rows. It returns the listed issues that
// have no row and, only when the listing is complete, removes the rows it
// does not list that reads begun no later than it stored, at most
// clearMirrorChunk per transaction. An incomplete listing removes nothing.
// An issue left with no row and no link becomes unkept.
func (s *Store) ReconcileMirrorBoard(rootID int64, boardID string, listed []string, complete bool, readAt int64) (result MirrorReconcile, err error) {
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
		err = s.mirrorBoardWrite(rootID, boardID, action, true, func(tx *sql.Tx, _ mirrorBoard) error {
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
			if removed = len(ids); removed > 0 {
				result.Removed = append(result.Removed, ids...)
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
