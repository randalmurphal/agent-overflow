package store

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
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
// the store from the issue's stored history (mirrorStatusSinceSQL; zero:
// unknown); writes ignore it.
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
	// Added are the issues of Rows whose row is new on the board: the
	// writes that can make the mirror keep an issue again.
	Added []string
	// Removed are the issues whose row on the board was removed.
	Removed []string
	// Superseded are the issues of which the write stored less than it
	// carried: a card older than the stored one, a row or a removal of a
	// row stored by a read that began later, a new row from a read that
	// began before the board last lost rows, or a removal from a read that
	// began before the board's newest complete listing.
	Superseded []string
	// Refused are the issues of Superseded owed a read that began after
	// the board's latest removal or listing: a new row not added because
	// the board lost rows after the read began (the removal may have been
	// of another issue), and a row not removed because a complete listing
	// began after the read did (the listing may have named it).
	// WriteMirrorRowPage stores no cursor for a page that has one
	// (CursorRefused); a WriteMirrorRows caller reads them again.
	Refused []string
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
	// the walk began (MirrorRowPage.EstimateResetAt), the page's read
	// began no later than the change, or it refused a new row (Refused).
	// The caller stops the walk and starts again from the sync state.
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

// mirrorCardSupersededSQL is the rule that orders a card's versions, in
// one place: the card with update time updated, read from readAt, is
// superseded by the stored issue i when i is of a later update, or of the
// same update and admitted from a read that began later (read_at, which
// an identical card from a later read moves too). A project key rename
// changes every key and moves no update time. i, updated and readAt are
// SQL expressions.
func mirrorCardSupersededSQL(i, updated, readAt string) string {
	return `(` + i + `.updated_at > ` + updated + ` OR ` + i + `.updated_at = ` + updated + ` AND ` + i + `.read_at > ` + readAt + `)`
}

// mirrorCardAdvancesSQL holds for a card c of a JSON list, read from
// readAt, that the stored issue i admits from an earlier read: a card the
// upsert does not replace, its content being the same, has its read time
// moved by mirrorCardReadSQL.
func mirrorCardAdvancesSQL(readAt string) string {
	return `(i.read_at < ` + readAt + ` AND NOT ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", readAt) + `)`
}

// mirrorCardContent are the columns of a card's content, each with the
// key of mirrorCardRow it is read from; data is hex in the JSON.
var mirrorCardContent = []struct{ column, key string }{
	{"issue_key", "k"}, {"project_id", "p"}, {"type_id", "t"}, {"type_level", "l"}, {"status_id", "s"}, {"assignee_id", "a"},
	{"priority_id", "r"}, {"parent_id", "n"}, {"created_at", "c"}, {"updated_at", "u"}, {"resolved_at", "z"}, {"data", "d"},
}

// mirrorCardDiffersSQL holds when the stored issue i holds other content
// than a card, whose value of each column card names.
func mirrorCardDiffersSQL(i string, card func(column, key string) string) string {
	terms := make([]string, len(mirrorCardContent))
	for n, col := range mirrorCardContent {
		terms[n] = i + col.column + ` IS NOT ` + card(col.column, col.key)
	}
	return `(` + strings.Join(terms, ` OR `) + `)`
}

// mirrorJSONCard names the columns of the card c of a JSON list.
func mirrorJSONCard(column, key string) string {
	if column == "data" {
		return `unhex(c.value ->> 'd')`
	}
	return `c.value ->> '` + key + `'`
}

// mirrorCardRefusedSQL holds for a card c of a JSON list, read from
// readAt, that the stored issue i supersedes while holding other content
// or other sprints: what a write reports as Superseded. An identical card
// from an earlier read is not refused, as nothing of it is lost. The
// comparison runs only for a superseded card (CASE is lazy), so an
// admitted one costs the rule alone.
func mirrorCardRefusedSQL(readAt string) string {
	return `CASE WHEN ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", readAt) + ` THEN ` + mirrorCardDiffersSQL("i.", mirrorJSONCard) + `
	  OR EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id
	              AND m.sprint_id NOT IN (SELECT value FROM json_each(c.value -> 'm')))
	  OR EXISTS (SELECT 1 FROM json_each(c.value -> 'm') sp WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	              WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id AND m.sprint_id = sp.value)) ELSE 0 END`
}

// The card statements read ?1 root, ?2 the JSON list of cards and ?3
// when their read began, and a card's filter that names the card c. A
// card replaces the stored one when the stored one does not supersede it
// (mirrorCardSupersededSQL) and its content differs; its sprints are
// replaced by the same rule. status_since follows mirrorStatusSinceSQL
// for the new card and the stored history.
const mirrorCardUpsertSQL = `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, project_id, type_id, type_level,
	        status_id, assignee_id, priority_id, parent_id, created_at, updated_at, resolved_at, read_at, data)
	SELECT ?1, c.value ->> 'i', c.value ->> 'k', c.value ->> 'p', c.value ->> 't', c.value ->> 'l', c.value ->> 's',
	       c.value ->> 'a', c.value ->> 'r', c.value ->> 'n', c.value ->> 'c', c.value ->> 'u', c.value ->> 'z', ?3,
	       unhex(c.value ->> 'd')
	  FROM json_each(?2) c WHERE `

var mirrorCardConflictSQL = `
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, project_id = excluded.project_id,
	    type_id = excluded.type_id, type_level = excluded.type_level, status_id = excluded.status_id,
	    assignee_id = excluded.assignee_id, priority_id = excluded.priority_id, parent_id = excluded.parent_id,
	    created_at = excluded.created_at, updated_at = excluded.updated_at, resolved_at = excluded.resolved_at,
	    read_at = excluded.read_at, data = excluded.data, status_since = ` + mirrorStatusSinceSQL(mirrorStatusSinceOf{
	issue: "tracker_cache_issues.issue_id", status: "excluded.status_id", updatedAt: "excluded.updated_at",
	createdAt: "excluded.created_at", from: "history_from", readFor: "history_for", wait: "history_wait", gen: "history_gen",
	kept: mirrorStatusKeptBy("excluded.status_id")}) + `
	WHERE NOT ` + mirrorCardSupersededSQL("tracker_cache_issues", "excluded.updated_at", "excluded.read_at") + `
	  AND ` + mirrorCardDiffersSQL("", func(column, _ string) string { return "excluded." + column }) + `
	RETURNING issue_id`

// mirrorCardAdmittedSQL joins the stored issue i of card c when it does
// not supersede the card. The card statements run the upsert first, which
// moves i to the card only when the card is admitted, so the rule reads the
// same before and after it. Each join from a JSON list is a CROSS JOIN, so
// the list drives it: without row counts the planner can otherwise walk
// the root's rows and probe the list for each.
var mirrorCardAdmittedSQL = ` CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
	AND NOT ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", "?3")

var mirrorCardSprintsLeaveSQL = `DELETE FROM tracker_cache_issue_sprints WHERE root_id = ?1 AND (issue_id, sprint_id) IN (
	SELECT m.issue_id, m.sprint_id FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	  JOIN tracker_cache_issue_sprints m ON m.root_id = ?1 AND m.issue_id = i.issue_id
	 WHERE m.sprint_id NOT IN (SELECT value FROM json_each(c.value -> 'm')) AND `

var mirrorCardSprintsJoinSQL = `INSERT INTO tracker_cache_issue_sprints(root_id, issue_id, sprint_id)
	SELECT ?1, i.issue_id, sp.value FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	  JOIN json_each(c.value -> 'm') sp WHERE `

// mirrorCardReadSQL records read ?3 as the latest of the admitted cards
// ?2 (IDs) the upsert did not replace, their sprints changed or not, so
// the card and its sprints are ordered as one by their latest read. It
// writes read_at alone, which no index or trigger reads.
const mirrorCardReadSQL = `UPDATE tracker_cache_issues SET read_at = ?3
	WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2)) AND read_at < ?3`

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
	// mirrorRowCards store a row page's cards: ?4 board, ?5 whether the
	// page may add rows; a card whose row it may not add is not stored.
	mirrorRowCards = newMirrorCardStatements(`(?5 OR EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		WHERE b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?4))`)
	// mirrorLinkedCards store cards of the issues the mirror keeps.
	mirrorLinkedCards = newMirrorCardStatements(mirrorIssueKept("?1", "c.value ->> 'i'"))
	// mirrorLinkedCardsStateSQL reads, for each card of ?2 read from ?3
	// whose issue the mirror holds, whether the stored card refuses it
	// (mirrorCardRefusedSQL) and whether it admits it from an earlier read
	// (mirrorCardAdvancesSQL), listing only the cards stored from an
	// earlier read or superseded by the stored one.
	mirrorLinkedCardsStateSQL = `SELECT c.value ->> 'i', ` + mirrorCardRefusedSQL("?3") + `, ` + mirrorCardAdvancesSQL("?3") + `
		FROM json_each(?2) c CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
		WHERE i.read_at < ?3 OR ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", "?3")
)

// storeMirrorCardsTx runs statements over encoded cards read from readAt,
// with the filter's arguments, and returns the issues whose card or
// sprints changed, sorted. advancing are the stored cards the write
// admits from an earlier read (mirrorCardAdvancesSQL), sorted: those the
// upsert did not replace cost one statement more, which records the read
// as their latest, so an identical card from a later read orders the
// card's ties as a changed one does, and reports no change.
func storeMirrorCardsTx(tx *sql.Tx, statements mirrorCardStatements, rootID int64, encoded string, readAt int64, advancing []string,
	filter ...any) ([]string, error) {
	args := append([]any{rootID, encoded, readAt}, filter...)
	var cards, changed []string
	for i, step := range []struct{ what, query string }{
		{"upsert cards", statements.upsert}, {"remove card sprints", statements.leave}, {"add card sprints", statements.join},
	} {
		ids, err := queryIDs(tx, step.query, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", step.what, err)
		}
		if i == 0 {
			cards = ids
		}
		changed = append(changed, ids...)
	}
	slices.Sort(cards)
	slices.Sort(changed)
	changed = slices.Compact(changed)
	var unreplaced []string
	for _, id := range advancing {
		if _, found := slices.BinarySearch(cards, id); !found {
			unreplaced = append(unreplaced, id)
		}
	}
	if len(unreplaced) > 0 {
		list, err := encodeMirrorIDs(unreplaced)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(mirrorCardReadSQL, rootID, list, readAt); err != nil {
			return nil, fmt.Errorf("record the cards' read: %w", err)
		}
	}
	return changed, nil
}

// UpsertMirrorCards stores cards read with no board (issues linked to
// threads) by the same rule as a row page: a card is not replaced by one
// with an earlier update time, or with the same one from a read that began
// before the stored card's (Superseded). A card of an issue the mirror does not keep
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
		var advancing []string
		err = scanMirrorRows(tx, mirrorLinkedCardsStateSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
			var id string
			var refused, advances bool
			if err := r.Scan(&id, &refused, &advances); err != nil {
				return err
			}
			if refused {
				changes.Superseded = append(changes.Superseded, id)
			}
			if advances {
				advancing = append(advancing, id)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("find superseded cards: %w", err)
		}
		// Sorted here rather than by the statements, which would sort in a
		// temporary b-tree.
		slices.Sort(changes.Unkept)
		slices.Sort(changes.Superseded)
		slices.Sort(advancing)
		advancing = slices.DeleteFunc(advancing, func(id string) bool {
			_, unkept := slices.BinarySearch(changes.Unkept, id)
			return unkept
		})
		changes.Cards, err = storeMirrorCardsTx(tx, mirrorLinkedCards, rootID, encoded, readAt, advancing)
		return err
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	return changes, nil
}
