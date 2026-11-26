package store

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// A parent's name: tracker_cache_parents holds one row per parent a stored
// card names, whether or not the mirror keeps the parent, and it is the
// only source of a parent's name for every view (a board's cards, a
// ticket's parent, the dashboard export).
//
// What must hold:
//   - One writer: the card write (storeMirrorCardsTx), over the cards its
//     filter admits. A card whose read named its parent writes the
//     parent's row; a card of an issue that has a row writes its own name
//     there. The newest read wins, whoever made it: a row changes only for
//     a read that began later and names other content, so a read that
//     names what the row holds writes nothing.
//   - A card that names its parent by ID alone writes no row and leaves
//     the row it has.
//   - A kept parent's card and its name change in the transaction of the
//     card write that read it.
//   - A row no stored issue names is deleted by FinishMirrorCleanup, a
//     chunk a transaction (mirrorParentsSweepSQL), and a detached root's
//     with its drain (mirrorTableKeys).

// MirrorParentName is a parent's name as a card's read gave it: its key,
// title and type level (MirrorCard.TypeLevel).
type MirrorParentName struct {
	Key, Title string
	TypeLevel  int
}

// MirrorParent is a parent's name as the mirror holds it.
type MirrorParent struct {
	IssueID string
	MirrorParentName
}

// mirrorParentRow is a parent's name as the card statements read it from a
// card's JSON (mirrorCardRow.ParentName).
type mirrorParentRow struct {
	Key   string `json:"k"`
	Title string `json:"x"`
	Level int    `json:"l"`
}

func newMirrorParentRow(p *MirrorParentName) *mirrorParentRow {
	if p == nil {
		return nil
	}
	return &mirrorParentRow{Key: p.Key, Title: p.Title, Level: p.TypeLevel}
}

// mirrorParentsWriteSQL writes the parents' names the cards of the JSON
// list ?2, read from ?3, give, for the card filter filter, over the cards
// the stored issues admit (mirrorCardAdmittedSQL), after the upsert: each
// card whose read named its parent writes the parent's row, and each card
// of an issue that has a row writes its own name there (its stored card,
// which the upsert made the card's). A row changes only for a later read
// with other content. It returns the parent of each row it inserted or
// changed.
func mirrorParentsWriteSQL(filter string) string {
	return `INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at)
	SELECT ?1, c.value ->> 'n', c.value ->> '$.q.k', c.value ->> '$.q.x', c.value ->> '$.q.l', ?3 FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	 WHERE c.value ->> '$.q.k' IS NOT NULL AND ` + filter + `
	UNION ALL
	SELECT ?1, i.issue_id, i.issue_key, i.title, i.type_level, ?3 FROM json_each(?2) c` + mirrorCardAdmittedSQL + `
	 WHERE EXISTS (SELECT 1 FROM tracker_cache_parents n WHERE n.root_id = CAST(?1 AS INTEGER) AND n.issue_id = i.issue_id) AND ` + filter + `
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, title = excluded.title,
	    type_level = excluded.type_level, read_at = excluded.read_at
	WHERE excluded.read_at > read_at AND (issue_key, title, type_level) IS NOT (excluded.issue_key, excluded.title, excluded.type_level)
	RETURNING issue_id`
}

// mirrorParentsSQL reads the names of the parents among a JSON list of
// IDs, probing each by key.
const mirrorParentsSQL = `SELECT n.issue_id, n.issue_key, n.title, n.type_level FROM json_each(?2) l
	CROSS JOIN tracker_cache_parents n ON n.root_id = CAST(?1 AS INTEGER) AND n.issue_id = l.value`

// ListMirrorParents reads the names of parents, at most mirrorMaxLookup,
// sorted by issue ID; a parent the mirror holds no name of is left out.
func (s *Store) ListMirrorParents(rootID int64, parents []string) ([]MirrorParent, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	if len(parents) > mirrorMaxLookup {
		return nil, fmt.Errorf("%w: %d IDs, over %d", ErrInvalidTrackerCache, len(parents), mirrorMaxLookup)
	}
	out, err := readMirrorParentsTx(s.reader(), rootID, parents)
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d parents: %w", rootID, err)
	}
	return out, nil
}

// readMirrorParentsTx reads the names of parents in one statement, sorted
// by issue ID.
func readMirrorParentsTx(q sqlQueryer, rootID int64, parents []string) ([]MirrorParent, error) {
	list, err := encodeMirrorIDs(parents)
	if err != nil {
		return nil, err
	}
	var out []MirrorParent
	err = scanMirrorRows(q, mirrorParentsSQL, []any{rootID, list}, func(r rowScanner) error {
		var p MirrorParent
		err := r.Scan(&p.IssueID, &p.Key, &p.Title, &p.TypeLevel)
		out = append(out, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b MirrorParent) int { return strings.Compare(a.IssueID, b.IssueID) })
	return out, nil
}

// mirrorOrphanParentsSQL reads the names of live roots no stored issue
// names, a probe of idx_tracker_cache_issues_parent each, and
// mirrorParentsSweepSQL deletes them, at most clearMirrorChunk a
// statement: a walk of every name. A detached root's go with its drain.
var (
	mirrorOrphanParentsSQL = `SELECT n.root_id, n.issue_id FROM tracker_cache_parents n
	  JOIN tracker_cache_roots r ON r.id = n.root_id AND r.connection_id IS NOT NULL
	 WHERE NOT EXISTS (SELECT 1 FROM tracker_cache_issues c WHERE c.root_id = n.root_id AND c.parent_id = n.issue_id AND c.parent_id <> '')`
	mirrorParentsSweepSQL = `DELETE FROM tracker_cache_parents WHERE (root_id, issue_id) IN (` + mirrorOrphanParentsSQL +
		` LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)`
)

// sweepMirrorParents deletes the names no stored issue names, a chunk a
// transaction, when there is one. It runs after the sweep of unkept
// issues, so a name only an unkept issue named goes in the same cleanup.
// The probe walks every name, so it reads in a history read slot.
func (s *Store) sweepMirrorParents(between func() error) error {
	any, err := mirrorReadSnapshot(s, "probe tracker mirror parents no issue names", func(q sqlQueryer) (bool, error) {
		var any bool
		err := q.QueryRow(`SELECT EXISTS (` + mirrorOrphanParentsSQL + `)`).Scan(&any)
		return any, err
	})
	if err != nil {
		return err
	}
	if !any {
		return nil
	}
	return s.drainMirrorChunks(mirrorParentsSweepSQL, between)
}
