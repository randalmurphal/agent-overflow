package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// MirrorCard is an issue as a board or backlog row draws it, the same on
// every board. Sprints are the sprints the card read names. Data is the
// rest of the card.
type MirrorCard struct {
	IssueID, Key, ProjectID          string
	TypeID                           string
	TypeLevel                        int
	StatusID, AssigneeID             string
	PriorityID, ParentID             string
	CreatedAt, UpdatedAt, ResolvedAt int64
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
// for that the board no longer lists (Missing), and the users, statuses
// and projects they name.
type MirrorRowPage struct {
	BoardID string
	Rows    []MirrorRow
	Missing []string
	Catalog MirrorCatalog
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
	// Pruned are the issues that left the mirror (pruneMirrorIssuesTx).
	Pruned []string
	// Unkept are the cards UpsertMirrorCards did not store because the
	// mirror does not keep their issue.
	Unkept []string
	// Catalog reports a changed project, user or status.
	Catalog bool
}

// mirrorIssueKept is the rule for which issues a mirror keeps, in one
// place: an issue with a row on one of its root's boards, or one a thread
// this computer owns links through its ticket (the connection's kind and
// site, and the issue ID). Any other issue leaves the mirror with its
// changes, sprints and detail, which cascade from it. root and issue are
// SQL expressions naming the issue.
func mirrorIssueKept(root, issue string) string {
	return `(EXISTS (SELECT 1 FROM tracker_cache_board_issues kept_row
	          WHERE kept_row.root_id = ` + root + ` AND kept_row.issue_id = ` + issue + `)
	  OR EXISTS (SELECT 1 FROM tracker_cache_roots kept_root
	          JOIN tracker_connections kept_conn ON kept_conn.id = kept_root.connection_id
	          JOIN thread_tickets kept_ticket ON kept_ticket.kind = kept_conn.kind AND kept_ticket.site = kept_conn.site
	           AND kept_ticket.issue_id = ` + issue + `
	          JOIN owned_threads kept_thread ON kept_thread.id = kept_ticket.thread_id
	          WHERE kept_root.id = ` + root + `))`
}

var (
	pruneMirrorIssuesSQL = `DELETE FROM tracker_cache_issues
		WHERE root_id = ? AND issue_id IN (SELECT value FROM json_each(?))
		  AND NOT ` + mirrorIssueKept("tracker_cache_issues.root_id", "tracker_cache_issues.issue_id") + `
		RETURNING issue_id`
	mirrorIssueKeptSQL = `SELECT ` + mirrorIssueKept("?1", "?2")
)

// pruneMirrorIssuesTx removes those of candidates the mirror no longer
// keeps (mirrorIssueKept) and returns them, sorted.
func pruneMirrorIssuesTx(tx *sql.Tx, rootID string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	encoded, err := encodeMirrorIDs(candidates)
	if err != nil {
		return nil, err
	}
	pruned, err := queryIDs(tx, pruneMirrorIssuesSQL, rootID, encoded)
	if err != nil {
		return nil, fmt.Errorf("prune issues: %w", err)
	}
	slices.Sort(pruned)
	return pruned, nil
}

// mirrorPruneSweepPage bounds the issues one PruneUnkeptMirrorIssues call
// reads.
const mirrorPruneSweepPage = 200

// mirrorBoardlessSQL reads a page of a root's issues with no board row, by
// ID, from idx_tracker_cache_issues_boardless: an issue a board row keeps
// is never read.
var mirrorBoardlessSQL = `SELECT issue_id FROM tracker_cache_issues
	WHERE root_id = ? AND board_rows = 0 AND issue_id > ?
	ORDER BY issue_id LIMIT ` + strconv.Itoa(mirrorPruneSweepPage+1)

// PruneUnkeptMirrorIssues removes a page of the issues of rootID the
// mirror no longer keeps (mirrorIssueKept). A row leaving a board prunes
// its issue at once; a thread's link goes by an unlink, the thread's
// delete, its retire to a holder or a transfer, none of which writes the
// mirror, so the sync of the root's linked issues runs this sweep. It
// reads up to mirrorPruneSweepPage issues with no board row after after,
// by ID, removes the unkept among them and returns them, sorted, with the
// last ID it read as next; more reports that issues follow next. A sweep
// starts with after empty.
func (s *Store) PruneUnkeptMirrorIssues(rootID, after string) (pruned []string, next string, more bool, err error) {
	action := fmt.Sprintf("store: sweep tracker mirror %s for unkept issues", rootID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		page, err := queryIDs(tx, mirrorBoardlessSQL, rootID, after)
		if err != nil {
			return fmt.Errorf("read issues with no board: %w", err)
		}
		if len(page) > mirrorPruneSweepPage {
			page, more = page[:mirrorPruneSweepPage], true
		}
		if len(page) > 0 {
			next = page[len(page)-1]
		}
		pruned, err = pruneMirrorIssuesTx(tx, rootID, page)
		return err
	})
	if err != nil {
		return nil, "", false, err
	}
	return pruned, next, more, nil
}

func (c MirrorCard) check() error {
	if c.IssueID == "" || c.Key == "" || c.UpdatedAt <= 0 || c.CreatedAt < 0 || c.ResolvedAt < 0 {
		return fmt.Errorf("%w: card %q needs an ID, a key and an update time", ErrInvalidTrackerCache, c.IssueID)
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
	if p.BoardID == "" {
		return fmt.Errorf("%w: rows need their board", ErrInvalidTrackerCache)
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
// transaction: either all of it lands or none. Each card is stored unless
// the mirror holds a version with a later update time; each row on the
// board is stored whatever its card's time.
func (s *Store) WriteMirrorRowPage(rootID string, page MirrorRowPage, cursor []byte) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, true, cursor)
}

// WriteMirrorRows stores a read of a board's rows for a set of issues, as
// WriteMirrorRowPage does but with no cursor, and removes the rows of the
// issues the board no longer lists (page.Missing).
func (s *Store) WriteMirrorRows(rootID string, page MirrorRowPage) (MirrorChanges, error) {
	return s.writeMirrorRows(rootID, page, false, nil)
}

func (s *Store) writeMirrorRows(rootID string, page MirrorRowPage, withCursor bool, cursor []byte) (changes MirrorChanges, err error) {
	if err := page.check(); err != nil {
		return MirrorChanges{}, err
	}
	action := fmt.Sprintf("store: write tracker mirror %s board %s rows", rootID, page.BoardID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		if err := requireMirrorBoardTx(tx, rootID, page.BoardID); err != nil {
			return err
		}
		var err error
		if changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, page.Catalog); err != nil {
			return err
		}
		for _, row := range page.Rows {
			cardChanged, err := upsertMirrorCardTx(tx, rootID, row.Card)
			if err != nil {
				return err
			}
			if cardChanged {
				changes.Cards = append(changes.Cards, row.Card.IssueID)
			}
			rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_board_issues(root_id, board_id, issue_id, order_key, estimate)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(root_id, board_id, issue_id) DO UPDATE SET order_key = excluded.order_key, estimate = excluded.estimate
				WHERE order_key IS NOT excluded.order_key OR estimate IS NOT excluded.estimate
				RETURNING issue_id`, rootID, page.BoardID, row.Card.IssueID, row.OrderKey, row.Estimate)
			if err != nil {
				return fmt.Errorf("upsert row %s: %w", row.Card.IssueID, err)
			}
			if rowChanged {
				changes.Rows = append(changes.Rows, row.Card.IssueID)
			}
		}
		if changes.Removed, changes.Pruned, err = removeMirrorRowsTx(tx, rootID, page.BoardID, page.Missing); err != nil {
			return err
		}
		if withCursor {
			if _, err := tx.Exec(`INSERT INTO tracker_cache_sync(root_id, board_id, cursor) VALUES (?, ?, ?)
				ON CONFLICT(root_id, board_id) DO UPDATE SET cursor = excluded.cursor`, rootID, page.BoardID, cursor); err != nil {
				return fmt.Errorf("write cursor: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	slices.Sort(changes.Cards)
	slices.Sort(changes.Rows)
	return changes, nil
}

// removeMirrorRowsTx removes the rows of issues on a board and prunes the
// issues that leave the mirror with them.
func removeMirrorRowsTx(tx *sql.Tx, rootID, boardID string, issues []string) (removed, pruned []string, err error) {
	if len(issues) == 0 {
		return nil, nil, nil
	}
	encoded, err := encodeMirrorIDs(issues)
	if err != nil {
		return nil, nil, err
	}
	removed, err = queryIDs(tx, `DELETE FROM tracker_cache_board_issues
		WHERE root_id = ? AND board_id = ? AND issue_id IN (SELECT value FROM json_each(?)) RETURNING issue_id`, rootID, boardID, encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("remove rows: %w", err)
	}
	slices.Sort(removed)
	pruned, err = pruneMirrorIssuesTx(tx, rootID, removed)
	return removed, pruned, err
}

// upsertMirrorCardTx stores card and its sprints unless the mirror holds a
// version of the card with a later update time, and reports whether
// either changed.
func upsertMirrorCardTx(tx *sql.Tx, rootID string, card MirrorCard) (bool, error) {
	var storedAt int64
	err := tx.QueryRow(`SELECT updated_at FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ?`, rootID, card.IssueID).Scan(&storedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("read card %s: %w", card.IssueID, err)
	case storedAt > card.UpdatedAt:
		return false, nil
	}
	changed, err := execChangedTx(tx, `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, project_id, type_id, type_level,
	        status_id, assignee_id, priority_id, parent_id, created_at, updated_at, resolved_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, project_id = excluded.project_id,
		    type_id = excluded.type_id, type_level = excluded.type_level, status_id = excluded.status_id,
		    assignee_id = excluded.assignee_id, priority_id = excluded.priority_id, parent_id = excluded.parent_id,
		    created_at = excluded.created_at, updated_at = excluded.updated_at, resolved_at = excluded.resolved_at, data = excluded.data
		WHERE issue_key IS NOT excluded.issue_key OR project_id IS NOT excluded.project_id OR type_id IS NOT excluded.type_id
		   OR type_level IS NOT excluded.type_level OR status_id IS NOT excluded.status_id OR assignee_id IS NOT excluded.assignee_id
		   OR priority_id IS NOT excluded.priority_id OR parent_id IS NOT excluded.parent_id OR created_at IS NOT excluded.created_at
		   OR updated_at IS NOT excluded.updated_at OR resolved_at IS NOT excluded.resolved_at OR data IS NOT excluded.data
		RETURNING issue_id`, rootID, card.IssueID, card.Key, card.ProjectID, card.TypeID, card.TypeLevel,
		card.StatusID, card.AssigneeID, card.PriorityID, card.ParentID, card.CreatedAt, card.UpdatedAt, card.ResolvedAt, mirrorData(card.Data))
	if err != nil {
		return false, fmt.Errorf("upsert card %s: %w", card.IssueID, err)
	}
	sprints, err := encodeMirrorIDs(card.Sprints)
	if err != nil {
		return false, err
	}
	left, err := execChangedTx(tx, `DELETE FROM tracker_cache_issue_sprints
		WHERE root_id = ? AND issue_id = ? AND sprint_id NOT IN (SELECT value FROM json_each(?)) RETURNING sprint_id`,
		rootID, card.IssueID, sprints)
	if err != nil {
		return false, fmt.Errorf("remove card %s sprints: %w", card.IssueID, err)
	}
	joined, err := execChangedTx(tx, `INSERT INTO tracker_cache_issue_sprints(root_id, issue_id, sprint_id)
		SELECT ?, ?, value FROM json_each(?) WHERE true
		ON CONFLICT(root_id, issue_id, sprint_id) DO NOTHING RETURNING sprint_id`, rootID, card.IssueID, sprints)
	if err != nil {
		return false, fmt.Errorf("add card %s sprints: %w", card.IssueID, err)
	}
	return changed || left || joined, nil
}

// UpsertMirrorCards stores cards read with no board (issues linked to
// threads) by the same rule as a row page: a card is not replaced by one
// with an earlier update time. A card of an issue the mirror does not keep
// (mirrorIssueKept) is not stored and is returned in Unkept.
func (s *Store) UpsertMirrorCards(rootID string, cards []MirrorCard, catalog MirrorCatalog) (changes MirrorChanges, err error) {
	seen := make(map[string]bool, len(cards))
	for _, card := range cards {
		if err := card.check(); err != nil {
			return MirrorChanges{}, err
		}
		if seen[card.IssueID] {
			return MirrorChanges{}, fmt.Errorf("%w: issue %s is read twice", ErrInvalidTrackerCache, card.IssueID)
		}
		seen[card.IssueID] = true
	}
	if err := catalog.check(); err != nil {
		return MirrorChanges{}, err
	}
	action := fmt.Sprintf("store: upsert tracker mirror %s cards", rootID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		if changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, catalog); err != nil {
			return err
		}
		for _, card := range cards {
			var kept bool
			if err := tx.QueryRow(mirrorIssueKeptSQL, rootID, card.IssueID).Scan(&kept); err != nil {
				return fmt.Errorf("check card %s is kept: %w", card.IssueID, err)
			}
			if !kept {
				changes.Unkept = append(changes.Unkept, card.IssueID)
				continue
			}
			changed, err := upsertMirrorCardTx(tx, rootID, card)
			if err != nil {
				return err
			}
			if changed {
				changes.Cards = append(changes.Cards, card.IssueID)
			}
		}
		return nil
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	slices.Sort(changes.Cards)
	slices.Sort(changes.Unkept)
	return changes, nil
}

// MirrorReconcile is what a reconcile of a board found and did, each list
// sorted by issue ID except Missing, which keeps the listing's order.
type MirrorReconcile struct {
	// Missing are the listed issues with no row on the board: the caller
	// reads their rows and stores them (WriteMirrorRows).
	Missing []string
	// Removed are the issues whose row was removed, not being listed.
	Removed []string
	// Pruned are the issues that left the mirror with their row.
	Pruned []string
}

// ReconcileMirrorBoard compares a listing of the issues on a board with
// the board's rows. It returns the listed issues that have no row and,
// only when the listing is complete, removes the rows of the issues it
// does not list. An incomplete listing removes nothing.
func (s *Store) ReconcileMirrorBoard(rootID, boardID string, listed []string, complete bool) (result MirrorReconcile, err error) {
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
	action := fmt.Sprintf("store: reconcile tracker mirror %s board %s", rootID, boardID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		if err := requireMirrorBoardTx(tx, rootID, boardID); err != nil {
			return err
		}
		var err error
		result.Missing, err = queryIDs(tx, `SELECT value FROM json_each(?3) AS listed
			WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_board_issues
			                   WHERE root_id = ?1 AND board_id = ?2 AND issue_id = listed.value)
			ORDER BY listed.key`, rootID, boardID, encoded)
		if err != nil {
			return fmt.Errorf("find missing rows: %w", err)
		}
		if !complete {
			return nil
		}
		result.Removed, err = queryIDs(tx, `DELETE FROM tracker_cache_board_issues
			WHERE root_id = ? AND board_id = ? AND issue_id NOT IN (SELECT value FROM json_each(?)) RETURNING issue_id`,
			rootID, boardID, encoded)
		if err != nil {
			return fmt.Errorf("remove unlisted rows: %w", err)
		}
		slices.Sort(result.Removed)
		result.Pruned, err = pruneMirrorIssuesTx(tx, rootID, result.Removed)
		return err
	})
	if err != nil {
		return MirrorReconcile{}, err
	}
	return result, nil
}
