package store

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
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
	// StructureDue reports that the board is due a structure read after a
	// row page write (MirrorSync.StructureDueAt).
	StructureDue bool
	// CursorRefused reports that WriteMirrorRowPage stored the page's rows
	// but not its cursor: the board's estimate definition changed since
	// the walk began (MirrorRowPage.EstimateResetAt), or the page's read
	// began no later than the change. The caller stops the walk and starts
	// again from the sync state.
	CursorRefused bool
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

// The card statements read ?1 root and ?2 the JSON list of cards, and a
// card's filter that names the card c. A card replaces the stored one when
// its update time is not earlier and its content differs; its sprints are
// replaced by the same rule. status_since is derived again from the new
// card and the stored history.
const mirrorCardUpsertSQL = `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, project_id, type_id, type_level,
	        status_id, assignee_id, priority_id, parent_id, created_at, updated_at, resolved_at, data)
	SELECT ?1, c.value ->> 'i', c.value ->> 'k', c.value ->> 'p', c.value ->> 't', c.value ->> 'l', c.value ->> 's',
	       c.value ->> 'a', c.value ->> 'r', c.value ->> 'n', c.value ->> 'c', c.value ->> 'u', c.value ->> 'z',
	       unhex(c.value ->> 'd')
	  FROM json_each(?2) c WHERE `

var mirrorCardConflictSQL = `
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, project_id = excluded.project_id,
	    type_id = excluded.type_id, type_level = excluded.type_level, status_id = excluded.status_id,
	    assignee_id = excluded.assignee_id, priority_id = excluded.priority_id, parent_id = excluded.parent_id,
	    created_at = excluded.created_at, updated_at = excluded.updated_at, resolved_at = excluded.resolved_at,
	    data = excluded.data, status_since = ` + mirrorStatusSinceSQL(mirrorStatusSinceOf{
	issue: "tracker_cache_issues.issue_id", status: "excluded.status_id", updatedAt: "excluded.updated_at",
	createdAt: "excluded.created_at", from: "history_from", to: "history_to", gen: "history_gen"}) + `
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

// UpsertMirrorCards stores cards read with no board (issues linked to
// threads) by the same rule as a row page: a card is not replaced by one
// with an earlier update time. A card of an issue the mirror does not keep
// (mirrorIssueKept) is not stored and is returned in Unkept. readAt is
// when the read began, which orders the catalog, and sweep the root's sweep
// captured before it (ErrTrackerCacheSwept).
func (s *Store) UpsertMirrorCards(rootID, sweep int64, cards []MirrorCard, catalog MirrorCatalog, readAt int64) (changes MirrorChanges, err error) {
	if readAt <= 0 || sweep <= 0 || len(cards) > mirrorMaxPageRows {
		return MirrorChanges{}, fmt.Errorf("%w: %d cards, over %d, or no read time or sweep", ErrInvalidTrackerCache, len(cards), mirrorMaxPageRows)
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
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
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
