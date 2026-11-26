package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// A parent's name: tracker_cache_parents holds one row per parent a stored
// issue names, whether or not the mirror keeps the parent, and it is the
// source of a parent's name for every view (a board's cards, a ticket's
// parent, the dashboard export); a ticket's view whose parent has no row
// shows the name its own read gave (trackerapp detailViews).
//
// What must hold:
//   - One writer: the card write (storeMirrorCardsTx), in the cards'
//     transaction, after their upsert. It takes every card the write was
//     given, whether or not the mirror keeps or admits it: each card is an
//     observation of its own issue's name (its key, title and type level),
//     and each card whose read named its parent is an observation of the
//     parent's (MirrorCard.ParentName). A card that names its parent by ID
//     alone observes nothing of the parent.
//   - One observation an issue a write: of two in one write, the issue's
//     own card wins over a child's naming of it, and of two children's
//     namings the first in the write's order.
//   - An observation is written only for an issue some stored issue names
//     as its parent; for any other it writes nothing.
//   - The newest read wins: a row changes only for an observation from a
//     read that began at or after the row's and names other content, so a
//     later write of the same read corrects it and a read that names what
//     the row holds writes nothing. A write returns the issues whose row it
//     inserted or changed (MirrorChanges.Parents).
//   - A kept issue's own card and its name are written by the same read in
//     one transaction. They can differ when a child's read names it later
//     than its own card's read, until its own card is read again.
//   - A row no stored issue names is deleted by FinishMirrorCleanup, a
//     chunk a transaction (mirrorParentsSweepSQL), and a detached root's
//     with its drain (mirrorTableKeys).
//   - A root v172 upgraded owes the names its stored issues' parents had
//     before any read kept them (tracker_cache_roots.names_after, NULL on
//     every root made since): ListMirrorUnnamedParents lists them in ID
//     order after the cursor, the caller reads their cards through the
//     card write, PassMirrorUnnamedParents moves the cursor past each
//     batch and EndMirrorUnnamedParents ends the worklist.

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

// mirrorNameObservation is one issue's name as the name write reads it
// (mirrorParentsWriteSQL).
type mirrorNameObservation struct {
	ID    string `json:"i"`
	Key   string `json:"k"`
	Title string `json:"x"`
	Level int    `json:"l"`
}

// mirrorNameObservations are the names cards observe, one an issue,
// sorted by issue ID: each card's own, which wins over a child's naming
// of it, and each named parent's, the first card's naming winning.
func mirrorNameObservations(cards []mirrorCardRow) []mirrorNameObservation {
	byID := make(map[string]mirrorNameObservation, len(cards))
	for _, c := range cards {
		byID[c.ID] = mirrorNameObservation{ID: c.ID, Key: c.Key, Title: c.Title, Level: c.TypeLevel}
	}
	for _, c := range cards {
		if _, named := byID[c.Parent]; c.ParentName != nil && !named {
			p := c.ParentName
			byID[c.Parent] = mirrorNameObservation{ID: c.Parent, Key: p.Key, Title: p.Title, Level: p.TypeLevel}
		}
	}
	out := make([]mirrorNameObservation, 0, len(byID))
	for _, o := range byID {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b mirrorNameObservation) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// mirrorParentsWriteSQL writes the names of the JSON list ?2 of
// observations, read from ?3, of the issues some stored issue names as
// parent, probing idx_tracker_cache_issues_parent each, and returns the
// issues whose row it inserted or changed.
const mirrorParentsWriteSQL = `INSERT INTO tracker_cache_parents(root_id, issue_id, issue_key, title, type_level, read_at)
	SELECT CAST(?1 AS INTEGER), o.value ->> 'i', o.value ->> 'k', o.value ->> 'x', o.value ->> 'l', ?3 FROM json_each(?2) o
	 WHERE EXISTS (SELECT 1 FROM tracker_cache_issues c WHERE c.root_id = CAST(?1 AS INTEGER) AND c.parent_id = o.value ->> 'i' AND c.parent_id <> '')
	ON CONFLICT(root_id, issue_id) DO UPDATE SET issue_key = excluded.issue_key, title = excluded.title,
	    type_level = excluded.type_level, read_at = excluded.read_at
	WHERE excluded.read_at >= read_at AND (issue_key, title, type_level) IS NOT (excluded.issue_key, excluded.title, excluded.type_level)
	RETURNING issue_id`

// writeMirrorNamesTx writes the names cards, read from readAt, observe
// (mirrorParentsWriteSQL) and returns the issues whose name changed,
// sorted.
func writeMirrorNamesTx(tx sqlQueryer, rootID int64, cards []mirrorCardRow, readAt int64) ([]string, error) {
	observed := mirrorNameObservations(cards)
	if len(observed) == 0 {
		return nil, nil
	}
	list, err := encodeMirrorJSON(observed)
	if err != nil {
		return nil, err
	}
	changed, err := queryIDs(tx, mirrorParentsWriteSQL, rootID, list, readAt)
	if err != nil {
		return nil, err
	}
	slices.Sort(changed)
	return changed, nil
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

// mirrorUnnamedParentsBatch is the most unnamed parents one
// ListMirrorUnnamedParents answers.
const mirrorUnnamedParentsBatch = 100

// mirrorUnnamedParentsSQL reads, of root ?1 while it owes names, the next
// parents after its worklist's cursor, in ID order, that a stored issue
// names and no name row names: a walk of idx_tracker_cache_issues_parent
// from the cursor, probing each parent's name by key.
var mirrorUnnamedParentsSQL = `SELECT DISTINCT c.parent_id FROM tracker_cache_roots r
	CROSS JOIN tracker_cache_issues c ON c.root_id = r.id AND c.parent_id > r.names_after AND c.parent_id <> ''
	WHERE r.id = CAST(?1 AS INTEGER) AND r.names_after IS NOT NULL
	  AND NOT EXISTS (SELECT 1 FROM tracker_cache_parents n WHERE n.root_id = c.root_id AND n.issue_id = c.parent_id)
	ORDER BY c.parent_id LIMIT ` + strconv.Itoa(mirrorUnnamedParentsBatch)

// MirrorUnnamedParents is a batch of a root's worklist of unnamed parents:
// Owed reports a root whose worklist is open, Parents the next parents
// after its cursor, and More a full batch, which may have more after it.
type MirrorUnnamedParents struct {
	Parents    []string
	Owed, More bool
}

// ListMirrorUnnamedParents reads the next parents of root's worklist (v172:
// the parents an upgraded mirror's stored issues name that no read named
// yet), at most mirrorUnnamedParentsBatch, in ID order: none when the root
// owes no names, one probe of the root, or none is left after the cursor.
// The walk reads in a history read slot.
func (s *Store) ListMirrorUnnamedParents(rootID int64) (MirrorUnnamedParents, error) {
	return mirrorReadSnapshot(s, "read tracker mirror unnamed parents", func(q sqlQueryer) (MirrorUnnamedParents, error) {
		var out MirrorUnnamedParents
		if err := q.QueryRow(`SELECT names_after IS NOT NULL FROM tracker_cache_roots WHERE id = CAST(?1 AS INTEGER)`, rootID).Scan(&out.Owed); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if !out.Owed {
			return out, nil
		}
		var err error
		out.Parents, err = queryIDs(q, mirrorUnnamedParentsSQL, rootID)
		out.More = len(out.Parents) == mirrorUnnamedParentsBatch
		return out, err
	})
}

// PassMirrorUnnamedParents moves root's worklist past through, the last
// parent a read asked for, and EndMirrorUnnamedParents ends it: a root
// whose worklist ended owes no names. Each is one statement in a mirror
// write, and repeating either changes nothing.
func (s *Store) PassMirrorUnnamedParents(rootID int64, through string) error {
	if through == "" {
		return fmt.Errorf("%w: no parent to pass", ErrInvalidTrackerCache)
	}
	return s.mirrorWrite(rootID, fmt.Sprintf("store: pass tracker mirror %d unnamed parents through %s", rootID, through), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tracker_cache_roots SET names_after = ?2 WHERE id = CAST(?1 AS INTEGER) AND names_after < ?2`, rootID, through)
		return err
	})
}

// EndMirrorUnnamedParents ends root's worklist of unnamed parents.
func (s *Store) EndMirrorUnnamedParents(rootID int64) error {
	return s.mirrorWrite(rootID, fmt.Sprintf("store: end tracker mirror %d unnamed parents", rootID), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tracker_cache_roots SET names_after = NULL WHERE id = CAST(?1 AS INTEGER) AND names_after IS NOT NULL`, rootID)
		return err
	})
}
