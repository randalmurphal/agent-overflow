package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Bounds of one write of cards or rows. A page's rows and missing issues
// bound the rows it adds and removes. A card names any number of sprints:
// an issue's sprints are compared as one digest (sprint_set,
// mirrorSprintSet), and only the stored cards whose digest moved read and
// write memberships (mirrorCardSprintsMovedSQL, replaceMirrorCardSprintsTx),
// each statement of at most mirrorSprintChunk memberships whatever one
// card names.
const (
	mirrorSprintChunk = 1000
	mirrorMaxPageRows = 500
)

// mirrorSprintSetPartial is the sprint_set of an issue whose memberships
// the sweep removed in part. No set's digest is one byte, so the next
// write that admits the issue's card replaces its sprints.
const mirrorSprintSetPartial = "00"

// mirrorSprintSet is the hex digest of a card's sprints that sprint_set
// stores: empty for none, else the first 16 bytes of SHA-256 over the
// sorted IDs, each prefixed by its length. Two cards name the same
// sprints exactly when their digests are equal.
func mirrorSprintSet(sprints []string) string {
	if len(sprints) == 0 {
		return ""
	}
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, id := range slices.Sorted(slices.Values(sprints)) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(id)))])
		_, _ = io.WriteString(h, id) // a hash's Write never fails
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// MirrorCard is an issue as a board or backlog row draws it, the same on
// every board. Sprints are the sprints the card read names. Data is the
// rest of the card. StatusSince is when the card's status began, set by
// the store from the reads that gave evidence of it (MirrorStatusSince;
// zero: unknown), and StatusFinal whether a final read decided it for the
// card's update time; writes ignore both.
type MirrorCard struct {
	IssueID, Key, ProjectID          string
	TypeID                           string
	TypeLevel                        int
	StatusID, AssigneeID             string
	PriorityID, ParentID             string
	CreatedAt, UpdatedAt, ResolvedAt int64
	StatusSince                      int64
	StatusFinal                      bool
	Sprints                          []string
	Data                             []byte
	// Title is the issue's title, which the mirror keeps only inside
	// Data. A write copies it to the links that name the issue
	// (thread_tickets, storeMirrorCardsTx); a read leaves it empty.
	Title string
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
	// Threads are the threads whose link the write moved: what the link
	// shows of its ticket, or its state (storeMirrorCardsTx). Sorted.
	Threads []string
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
	Title      string   `json:"x"`
	SprintSet  string   `json:"h"`
	Sprints    []string `json:"-"`
	OrderKey   string   `json:"o"`
	Estimate   *float64 `json:"e"`
}

func newMirrorCardRow(c MirrorCard) mirrorCardRow {
	return mirrorCardRow{ID: c.IssueID, Key: c.Key, Project: c.ProjectID, Type: c.TypeID, TypeLevel: c.TypeLevel,
		Status: c.StatusID, Assignee: c.AssigneeID, Priority: c.PriorityID, Parent: c.ParentID,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, ResolvedAt: c.ResolvedAt,
		Data: hex.EncodeToString(c.Data), Title: c.Title, SprintSet: mirrorSprintSet(c.Sprints), Sprints: c.Sprints}
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

// mirrorCardSupersededSQL is the one rule that orders any two writes of a
// card: the card with update time updated, read from readAt, is
// superseded by the stored issue i when i is of a later update, or of the
// same update and admitted from a read that began later (read_at, which
// an identical card from a later read moves too). A project key rename
// changes every key and moves no update time. A card has two writers, a
// board's rows (WriteMirrorRows and its kin) and cards read with no board
// (UpsertMirrorCards, and a ticket's read through StoreMirrorTickets);
// both store through storeMirrorCardsTx, whose statements apply this rule.
// i, updated and readAt are SQL expressions.
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
// key of mirrorCardRow it is read from; data and sprint_set are hex in the
// JSON. sprint_set makes a card's sprints part of its content, so the one
// comparison that finds a card changed finds its sprints changed.
var mirrorCardContent = []struct{ column, key string }{
	{"issue_key", "k"}, {"project_id", "p"}, {"type_id", "t"}, {"type_level", "l"}, {"status_id", "s"}, {"assignee_id", "a"},
	{"priority_id", "r"}, {"parent_id", "n"}, {"created_at", "c"}, {"updated_at", "u"}, {"resolved_at", "z"}, {"data", "d"},
	{"sprint_set", "h"},
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
	if column == "data" || column == "sprint_set" {
		return `unhex(c.value ->> '` + key + `')`
	}
	return `c.value ->> '` + key + `'`
}

// mirrorCardSprintsMovedSQL holds for a card c of a JSON list whose
// sprints' digest is not the stored issue i's, a new issue's being none
// (its memberships are none): every card writer's classification reads
// it before the upsert, and of the cards the upsert stores those alone
// have their memberships read and replaced (storeMirrorCardsTx), so a
// card whose other content changed reads no membership.
var mirrorCardSprintsMovedSQL = `coalesce(i.sprint_set, x'') IS NOT ` + mirrorJSONCard("sprint_set", "h")

// mirrorCardRefusedSQL holds for a card c of a JSON list, read from
// readAt, that the stored issue i supersedes while holding other content
// or other sprints: what a write reports as Superseded. An identical card
// from an earlier read is not refused, as nothing of it is lost. The
// comparison runs only for a superseded card (CASE is lazy), so an
// admitted one costs the rule alone.
func mirrorCardRefusedSQL(readAt string) string {
	return `CASE WHEN ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", readAt) + ` THEN ` + mirrorCardDiffersSQL("i.", mirrorJSONCard) + ` ELSE 0 END`
}

// The card statements read ?1 root, ?2 the JSON list of cards and ?3
// when their read began, and a card's filter that names the card c. A
// card replaces the stored one when the stored one does not supersede it
// (mirrorCardSupersededSQL) and its content, its sprints' digest
// included, differs; it returns the issue of each card it stores, whose
// sprints replaceMirrorCardSprintsTx then replaces. The status time is
// kept while the status stays (mirrorStatusKeptBy). The mark
// statement reads, as ?2, the references the cards name instead
// (mirrorCardRef), each with its card's issue and update time, so the
// same filter and rule name them.
const mirrorCardUpsertSQL = `INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, project_id, type_id, type_level,
	        status_id, assignee_id, priority_id, parent_id, created_at, updated_at, resolved_at, read_at, data, sprint_set,
	        status_floor_at, status_read_at)
	SELECT ?1, c.value ->> 'i', c.value ->> 'k', c.value ->> 'p', c.value ->> 't', c.value ->> 'l', c.value ->> 's',
	       c.value ->> 'a', c.value ->> 'r', c.value ->> 'n', c.value ->> 'c', c.value ->> 'u', c.value ->> 'z', ?3,
	       unhex(c.value ->> 'd'), unhex(c.value ->> 'h'), ?3, ?3
	  FROM json_each(?2) c WHERE `

var mirrorCardConflictSQL = `
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, project_id = excluded.project_id,
	    type_id = excluded.type_id, type_level = excluded.type_level, status_id = excluded.status_id,
	    assignee_id = excluded.assignee_id, priority_id = excluded.priority_id, parent_id = excluded.parent_id,
	    created_at = excluded.created_at, updated_at = excluded.updated_at, resolved_at = excluded.resolved_at,
	    read_at = excluded.read_at, data = excluded.data, sprint_set = excluded.sprint_set, ` + mirrorStatusKeptBy("excluded.status_id", "excluded.updated_at", "excluded.read_at") + `
	WHERE NOT ` + mirrorCardSupersededSQL("tracker_cache_issues", "excluded.updated_at", "excluded.read_at") + `
	  AND ` + mirrorCardDiffersSQL("", func(column, _ string) string { return "excluded." + column }) + `
	RETURNING ` + mirrorRecordAfterColumns

// mirrorRecordAfterColumns are what a write of an issue row returns of it
// as the write left it (mirrorRecordAfter), in that order.
const mirrorRecordAfterColumns = `issue_id, updated_at, status_since, status_final_for >= updated_at, body_updated_at, body_read_at, body_wall`

// mirrorRecordAfter is an issue row as a write left it: its card's update
// time, its status time and whether a final read decided it for that
// update, and the body it holds (its update time, read and wall time;
// zero for none).
type mirrorRecordAfter struct {
	updatedAt, statusSince            int64
	statusFinal                       bool
	bodyUpdatedAt, bodyRead, bodyWall int64
}

// scanMirrorRecordsAfter reads the rows a write returned
// (mirrorRecordAfterColumns) into after, by issue, and returns their
// issues in the order returned.
func scanMirrorRecordsAfter(tx *sql.Tx, query string, after map[string]mirrorRecordAfter, args ...any) ([]string, error) {
	var ids []string
	err := scanMirrorRows(tx, query, args, func(r rowScanner) error {
		var id string
		var a mirrorRecordAfter
		if err := r.Scan(&id, &a.updatedAt, &a.statusSince, &a.statusFinal, &a.bodyUpdatedAt, &a.bodyRead, &a.bodyWall); err != nil {
			return err
		}
		ids, after[id] = append(ids, id), a
		return nil
	})
	return ids, err
}

// mirrorCardAdmittedSQL joins the stored issue i of card c when it does
// not supersede the card. The card statements run the upsert first, which
// moves i to the card only when the card is admitted, so the rule reads the
// same before and after it. Each join from a JSON list is a CROSS JOIN, so
// the list drives it: without row counts the planner can otherwise walk
// the root's rows and probe the list for each.
var mirrorCardAdmittedSQL = ` CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = c.value ->> 'i'
	AND NOT ` + mirrorCardSupersededSQL("i", "c.value ->> 'u'", "?3")

// The sprint statements of replaceMirrorCardSprintsTx read ?1 root and ?2
// a JSON list. mirrorCardSprintsOfSQL reads, of the issues ?2 (IDs,
// sorted), a chunk of the memberships after membership (?3, ?4): the list
// drives the join (CROSS JOIN) and each issue's memberships are a range of
// the primary key, so they come in key order, which the caller checks.
// The leave and join statements take ?2 a list of [issue, sprint] pairs.
var (
	mirrorCardSprintsOfSQL = `SELECT m.issue_id, m.sprint_id FROM json_each(?2) s
		CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = ?1 AND m.issue_id = s.value
		WHERE s.value >= ?3 AND (s.value > ?3 OR m.sprint_id > ?4) LIMIT ` + strconv.Itoa(clearMirrorChunk)
	mirrorCardSprintsLeaveSQL = `DELETE FROM tracker_cache_issue_sprints WHERE root_id = ?1
		AND (issue_id, sprint_id) IN (SELECT p.value ->> 0, p.value ->> 1 FROM json_each(?2) p)`
	mirrorCardSprintsJoinSQL = `INSERT INTO tracker_cache_issue_sprints(root_id, issue_id, sprint_id)
		SELECT ?1, p.value ->> 0, p.value ->> 1 FROM json_each(?2) p`
)

// replaceMirrorCardSprintsTx makes the memberships of each issue of
// stored, sorted, the sprints its card in cards names. It reads the
// issues' memberships a chunk a statement and deletes those of each chunk
// that the card no longer names, then adds those the card names that no
// chunk held, mirrorSprintChunk a statement: each membership is read once
// and written only when it changes, and no statement touches more than a
// chunk.
func replaceMirrorCardSprintsTx(tx *sql.Tx, rootID int64, cards []mirrorCardRow, stored []string) error {
	if len(stored) == 0 {
		return nil
	}
	named := make(map[string]map[string]bool, len(stored))
	for _, c := range cards {
		if _, found := slices.BinarySearch(stored, c.ID); found {
			set := make(map[string]bool, len(c.Sprints))
			for _, sprint := range c.Sprints {
				set[sprint] = true
			}
			named[c.ID] = set
		}
	}
	ids, err := encodeMirrorIDs(stored)
	if err != nil {
		return err
	}
	pairs := func(what, query string, list [][2]string) error {
		for chunk := range slices.Chunk(list, mirrorSprintChunk) {
			encoded, err := encodeMirrorJSON(chunk)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(query, rootID, encoded); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
		}
		return nil
	}
	for after := [2]string{"", ""}; ; {
		var held, leaving [][2]string
		err := scanMirrorRows(tx, mirrorCardSprintsOfSQL, []any{rootID, ids, after[0], after[1]}, func(r rowScanner) error {
			var m [2]string
			if err := r.Scan(&m[0], &m[1]); err != nil {
				return err
			}
			if m[0] < after[0] || m[0] == after[0] && m[1] <= after[1] {
				return fmt.Errorf("membership %v read after %v: out of key order", m, after)
			}
			after = m
			held = append(held, m)
			return nil
		})
		if err != nil {
			return fmt.Errorf("read card sprints: %w", err)
		}
		for _, m := range held {
			if named[m[0]][m[1]] {
				delete(named[m[0]], m[1])
			} else {
				leaving = append(leaving, m)
			}
		}
		if err := pairs("remove card sprints", mirrorCardSprintsLeaveSQL, leaving); err != nil {
			return err
		}
		if len(held) < clearMirrorChunk {
			break
		}
	}
	var joining [][2]string
	for _, id := range stored {
		for _, sprint := range slices.Sorted(maps.Keys(named[id])) {
			joining = append(joining, [2]string{id, sprint})
		}
	}
	return pairs("add card sprints", mirrorCardSprintsJoinSQL, joining)
}

// mirrorCardMarkSQL marks pending each reference c of the JSON list ?2
// (mirrorCardRef) that the mirror lacks, named by a card the write
// admitted, on each live board the issue has a row on and on the boards
// the boards select adds (a row page's own, whose new rows the write adds
// after its cards), each probed by key: a status or user the root has no row of, or a
// sprint the board has no row of, not already marked unexplained. A new
// mark, or a pending one named before the board's latest structure read,
// is named by this read ?3, which makes the board due a structure read
// (mirrorBoard.structureDueAt); an unexplained mark stays as it is. It
// returns the board of each mark it wrote. The cards are stored first, so
// the rule admits exactly the cards the write stored or found the same.
func mirrorCardMarkSQL(filter, boards string) string {
	return `INSERT INTO tracker_cache_board_unexplained(root_id, board_id, kind, ref_id, named_at)
	SELECT ?1, bd.board_id, c.value ->> 'k', c.value ->> 'r', ?3 FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	  CROSS JOIN tracker_cache_boards bd ON bd.root_id = ?1 AND bd.retired = 0 AND bd.board_id IN (
	    SELECT b.board_id FROM tracker_cache_board_issues b WHERE b.root_id = ?1 AND b.issue_id = i.issue_id` + boards + `)
	 WHERE CASE c.value ->> 'k'
	   WHEN 1 THEN NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st WHERE st.root_id = ?1 AND st.status_id = c.value ->> 'r')
	   WHEN 2 THEN NOT EXISTS (SELECT 1 FROM tracker_cache_users us WHERE us.root_id = ?1 AND us.user_id = c.value ->> 'r')
	   ELSE NOT EXISTS (SELECT 1 FROM tracker_cache_sprints sp WHERE sp.root_id = ?1 AND sp.board_id = bd.board_id AND sp.sprint_id = c.value ->> 'r') END
	   AND ` + filter + `
	ON CONFLICT(root_id, board_id, kind, ref_id) DO UPDATE SET named_at = excluded.named_at
	WHERE named_at > 0 AND excluded.named_at > named_at AND named_at < (SELECT bl.read_at FROM tracker_cache_boards bl
	   WHERE bl.root_id = excluded.root_id AND bl.board_id = excluded.board_id)
	RETURNING board_id`
}

// mirrorCardRef is a reference a card names, as mirrorCardMarkSQL reads
// it: its kind (1 status, 2 user, 3 sprint) and ID, with the card's issue
// and update time.
type mirrorCardRef struct {
	IssueID   string `json:"i"`
	UpdatedAt int64  `json:"u"`
	Kind      int    `json:"k"`
	ID        string `json:"r"`
}

// mirrorCardRefsOf is the references cards name.
func mirrorCardRefsOf(cards []mirrorCardRow) []mirrorCardRef {
	var refs []mirrorCardRef
	add := func(c mirrorCardRow, kind int, id string) {
		if id != "" {
			refs = append(refs, mirrorCardRef{IssueID: c.ID, UpdatedAt: c.UpdatedAt, Kind: kind, ID: id})
		}
	}
	for _, c := range cards {
		add(c, 1, c.Status)
		add(c, 2, c.Assignee)
		for _, sprint := range c.Sprints {
			add(c, 3, sprint)
		}
	}
	return refs
}

// mirrorCardReadSQL records read ?3 as the latest of the admitted cards
// ?2 (IDs) the upsert did not replace, their sprints changed or not, so
// the card and its sprints are ordered as one by their latest read. It
// writes read_at alone, which no index or trigger reads.
const mirrorCardReadSQL = `UPDATE tracker_cache_issues SET read_at = ?3
	WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2)) AND read_at < ?3`

// mirrorCardStatements are the statements that store cards, for one
// filter of the cards they take: the card, and the marks of the
// references it names that the mirror lacks (mirrorCardMarkSQL), on the
// boards the issue has rows on and those the boards select adds (a
// compound SELECT arm, or nothing). The card's sprints follow the card
// (replaceMirrorCardSprintsTx).
type mirrorCardStatements struct{ upsert, mark string }

func newMirrorCardStatements(filter, boards string) mirrorCardStatements {
	return mirrorCardStatements{
		upsert: mirrorCardUpsertSQL + filter + mirrorCardConflictSQL,
		mark:   mirrorCardMarkSQL(filter, boards),
	}
}

var (
	// mirrorRowCards store a row page's cards: ?4 board, ?5 whether the
	// page may add rows; a card whose row it may not add is not stored.
	// Its references are marked on the page's board too, where its row is
	// written after it.
	mirrorRowCards = newMirrorCardStatements(`(?5 OR EXISTS (SELECT 1 FROM tracker_cache_board_issues b
		WHERE b.root_id = ?1 AND b.issue_id = c.value ->> 'i' AND b.board_id = ?4))`, ` UNION SELECT ?4`)
	// mirrorLinkedCards store cards of the issues the mirror keeps.
	mirrorLinkedCards = newMirrorCardStatements(mirrorIssueKept("?1", "c.value ->> 'i'"), ``)
)

// mirrorCardsStored is what storeMirrorCardsTx did: the issues whose card
// changed, each as the upsert left it (after), and the boards
// on which it marked a reference the mirror lacks (mirrorCardMarkSQL),
// each sorted.
type mirrorCardsStored struct {
	changed, marked []string
	after           map[string]mirrorRecordAfter
	// threads are the threads whose link the write moved
	// (mirrorLinkCopySQL), sorted.
	threads []string
}

// storeMirrorCardsTx runs statements over cards, encoded, read from readAt,
// with the filter's arguments. The upsert stores each admitted card whose
// content changed, and of those the cards of moved, sorted, which the
// writer's classification found naming other sprints than stored
// (mirrorCardSprintsMovedSQL), alone have their sprints replaced
// (replaceMirrorCardSprintsTx): a card whose sprints stay costs the
// digests' comparison and no membership read or write. Then move runs,
// given the cards the upsert stored: the card writers' move
// (mirrorCardReadMove) records the read as the latest of the stored cards
// it admits from an earlier read and did not replace, so an identical
// card from a later read orders the card's ties as a changed one does,
// and reports no change; a ticket write moves its bodies and status
// evidence in the same statement (StoreMirrorTickets). Then the mark
// statement, a chunk of references at a time, marks the references the
// admitted cards name that the mirror lacks, on each board the issue has
// a row on: every card writer calls this, so a card that moves an issue
// to a status, user or sprint the mirror has not read makes the boards
// showing it due a structure read, whichever read brought it. Between
// the move and the marks, the links that name a card's issue take the
// card the mirror now holds (mirrorLinkCopySQL), so a link's copy changes
// in the transaction that changes the card.
func storeMirrorCardsTx(tx *sql.Tx, statements mirrorCardStatements, rootID int64, cards []mirrorCardRow, encoded string, readAt int64,
	moved []string, move func(stored []string) error, filter ...any) (out mirrorCardsStored, err error) {
	args := append([]any{rootID, encoded, readAt}, filter...)
	out.after = map[string]mirrorRecordAfter{}
	stored, err := scanMirrorRecordsAfter(tx, statements.upsert, out.after, args...)
	if err != nil {
		return mirrorCardsStored{}, fmt.Errorf("upsert cards: %w", err)
	}
	slices.Sort(stored)
	out.changed = stored
	var sprinted []string
	for _, id := range stored {
		if _, found := slices.BinarySearch(moved, id); found {
			sprinted = append(sprinted, id)
		}
	}
	if err := replaceMirrorCardSprintsTx(tx, rootID, cards, sprinted); err != nil {
		return mirrorCardsStored{}, err
	}
	if err := move(stored); err != nil {
		return mirrorCardsStored{}, err
	}
	if out.threads, err = queryIDs(tx, mirrorLinkCopySQL, rootID, encoded); err != nil {
		return mirrorCardsStored{}, fmt.Errorf("copy the cards to their links: %w", err)
	}
	slices.Sort(out.threads)
	for refs := range slices.Chunk(mirrorCardRefsOf(cards), mirrorSprintChunk) {
		if args[1], err = encodeMirrorJSON(refs); err != nil {
			return mirrorCardsStored{}, err
		}
		marked, err := queryIDs(tx, statements.mark, args...)
		if err != nil {
			return mirrorCardsStored{}, fmt.Errorf("mark lacking references: %w", err)
		}
		out.marked = append(out.marked, marked...)
	}
	slices.Sort(out.marked)
	out.marked = slices.Compact(out.marked)
	return out, nil
}

// mirrorCardReadMove is the card writers' move of storeMirrorCardsTx:
// advancing, sorted, are the stored cards the write admits from an
// earlier read (mirrorCardAdvancesSQL); those the upsert did not replace
// cost one statement, which records the read as their latest. It writes
// read_at alone, which no index or trigger reads.
func mirrorCardReadMove(tx *sql.Tx, rootID, readAt int64, advancing []string) func(stored []string) error {
	return func(stored []string) error {
		var unreplaced []string
		for _, id := range advancing {
			if _, found := slices.BinarySearch(stored, id); !found {
				unreplaced = append(unreplaced, id)
			}
		}
		if len(unreplaced) == 0 {
			return nil
		}
		list, err := encodeMirrorIDs(unreplaced)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorCardReadSQL, rootID, list, readAt); err != nil {
			return fmt.Errorf("record the cards' read: %w", err)
		}
		return nil
	}
}

// UpsertMirrorCards stores cards read with no board (issues linked to
// threads) by the same rule as a row page: a card is not replaced by one
// with an earlier update time, or with the same one from a read that began
// before the stored card's (Superseded). A card of an issue the mirror does not keep
// (mirrorIssueKept) is not stored and is returned in Unkept. read is
// when the read began, which orders the cards and the catalog, and sweep
// the root's sweep captured before it (ErrTrackerCacheSwept).
func (s *Store) UpsertMirrorCards(rootID, sweep int64, cards []MirrorCard, catalog MirrorCatalog, read MirrorRead) (changes MirrorChanges, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return MirrorChanges{}, err
	}
	readAt := times.at
	if readAt <= 0 || sweep <= 0 {
		return MirrorChanges{}, fmt.Errorf("%w: cards stored with no read time or sweep", ErrInvalidTrackerCache)
	}
	rows, encoded, err := prepareMirrorCards(cards, catalog)
	if err != nil {
		return MirrorChanges{}, err
	}
	action := fmt.Sprintf("store: upsert tracker mirror %d cards", rootID)
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		var err error
		changes, err = upsertMirrorCardsTx(tx, rootID, rows, encoded, catalog, readAt)
		return err
	})
	if err != nil {
		return MirrorChanges{}, err
	}
	return changes, nil
}

// prepareMirrorCards checks cards read with no board and their catalog
// and encodes the cards for the card statements: at most a page, each
// issue once.
func prepareMirrorCards(cards []MirrorCard, catalog MirrorCatalog) ([]mirrorCardRow, string, error) {
	if len(cards) > mirrorMaxPageRows {
		return nil, "", fmt.Errorf("%w: %d cards, over %d", ErrInvalidTrackerCache, len(cards), mirrorMaxPageRows)
	}
	seen := make(map[string]bool, len(cards))
	rows := make([]mirrorCardRow, len(cards))
	for i, card := range cards {
		if err := card.check(); err != nil {
			return nil, "", err
		}
		if seen[card.IssueID] {
			return nil, "", fmt.Errorf("%w: issue %s is read twice", ErrInvalidTrackerCache, card.IssueID)
		}
		seen[card.IssueID] = true
		rows[i] = newMirrorCardRow(card)
	}
	if err := catalog.check(); err != nil {
		return nil, "", err
	}
	encoded, err := encodeMirrorJSON(rows)
	if err != nil {
		return nil, "", err
	}
	return rows, encoded, nil
}

// upsertMirrorCardsTx is UpsertMirrorCards' write, in tx: the catalog,
// then each card of an issue the mirror keeps by the card rule.
func upsertMirrorCardsTx(tx *sql.Tx, rootID int64, rows []mirrorCardRow, encoded string, catalog MirrorCatalog, readAt int64) (changes MirrorChanges, err error) {
	if changes.Catalog, err = upsertMirrorCatalogTx(tx, rootID, catalog, readAt); err != nil {
		return MirrorChanges{}, err
	}
	if len(rows) == 0 {
		return changes, nil
	}
	states, err := readMirrorCardStatesTx(tx, rootID, encoded, len(rows), readAt)
	if err != nil {
		return MirrorChanges{}, err
	}
	var advancing, moved []string
	for i, st := range states {
		id := rows[i].ID
		switch {
		case !st.kept:
			changes.Unkept = append(changes.Unkept, id)
		case st.refused:
			changes.Superseded = append(changes.Superseded, id)
		}
		if st.kept && st.advances {
			advancing = append(advancing, id)
		}
		if st.sprintsMoved {
			moved = append(moved, id)
		}
	}
	slices.Sort(changes.Unkept)
	slices.Sort(changes.Superseded)
	slices.Sort(advancing)
	slices.Sort(moved)
	stored, err := storeMirrorCardsTx(tx, mirrorLinkedCards, rootID, rows, encoded, readAt, moved, mirrorCardReadMove(tx, rootID, readAt, advancing))
	if err != nil {
		return MirrorChanges{}, err
	}
	changes.Cards, changes.Threads = stored.changed, stored.threads
	return changes, nil
}
