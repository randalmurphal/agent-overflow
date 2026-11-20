package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// mirrorDetailCap is how many details a root keeps: the most recently
// opened.
const mirrorDetailCap = 200

// MirrorDetail is an issue's stored detail document: when the issue was
// last opened, when the document's read began, and the document, with the
// update time of the mirror's card of the issue as the same read found it.
type MirrorDetail struct {
	IssueID          string
	OpenedAt, ReadAt int64
	Data             []byte
	CardUpdatedAt    int64
	// row and sweep name the document for ConfirmMirrorDetail: its row,
	// and the root's sweep when it was read.
	row, sweep int64
}

// MirrorDetailWrite is what a detail write did for one issue, as its
// transaction committed: whether the issue's detail is held after it
// (Stored), whether its document changed, and whether the document was
// superseded by one from a read that began later.
type MirrorDetailWrite struct {
	Stored, Changed, Superseded bool
}

// MirrorDetailWrites is what StoreMirrorDetails did, as its transaction
// committed: each document's outcome, in the order of its documents, and
// the issues whose details it dropped to keep the mirrorDetailCap most
// recently opened, sorted. Dropped names only details held before the
// write: a new detail the cap drops in the same write was never held, and
// its outcome says so (Stored false).
type MirrorDetailWrites struct {
	Docs    []MirrorDetailWrite
	Dropped []string
}

// mirrorDetailEvictSQL drops the details of a root beyond the
// mirrorDetailCap most recently opened, walking the opened index.
var mirrorDetailEvictSQL = `DELETE FROM tracker_cache_issue_details WHERE root_id = ?1 AND issue_id IN (
	SELECT issue_id FROM tracker_cache_issue_details WHERE root_id = ?1
	 ORDER BY opened_at DESC, issue_id DESC LIMIT -1 OFFSET ` + strconv.Itoa(mirrorDetailCap) + `)
	RETURNING issue_id`

// The detail statements: ?1 root, ?2 issue.
var (
	mirrorDetailStateSQL = `SELECT ` + mirrorIssueKept("?1", "?2") + `, d.opened_at, d.read_at, d.data IS NOT ?3
		FROM (SELECT 1) LEFT JOIN tracker_cache_issue_details d ON d.root_id = ?1 AND d.issue_id = ?2`
	// mirrorDetailStoreSQL stores the document ?4 read from ?3 and moves the
	// opening forward to ?5 (zero: none). Only an opening starts a detail,
	// and the opening is the new detail's read watermark: its document is
	// stored only from a read that began no earlier. A write that is no
	// opening reaches the insert for a held detail alone, whose conflict
	// keeps its opening, so its inserted 1 only meets the CHECK, which
	// SQLite applies before the conflict.
	mirrorDetailStoreSQL = `INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at, read_at, data)
		VALUES (?1, ?2, max(?5, 1), max(?3, ?5), iif(?3 >= ?5, ?4, NULL))
		ON CONFLICT(root_id, issue_id) DO UPDATE SET read_at = ?3, data = ?4, opened_at = max(opened_at, ?5)`
	// mirrorDetailConfirmSQL is whether the detail is still row ?3 holding
	// the document read from ?4 of a kept issue, and its opening.
	mirrorDetailConfirmSQL = `SELECT d.rowid = ?3 AND d.read_at = ?4 AND d.data IS NOT NULL AND ` + mirrorIssueKept("?1", "?2") + `,
		d.opened_at FROM tracker_cache_issue_details d WHERE d.root_id = ?1 AND d.issue_id = ?2`
	// mirrorDetailOpenSQL moves the opening forward to ?3.
	mirrorDetailOpenSQL = `UPDATE tracker_cache_issue_details SET opened_at = max(opened_at, ?3) WHERE root_id = ?1 AND issue_id = ?2`
	// mirrorDetailMoveSQL moves a held detail's read time to ?3 and its
	// opening to ?4, each only forward, leaving the document.
	mirrorDetailMoveSQL = `UPDATE tracker_cache_issue_details SET read_at = max(read_at, ?3), opened_at = max(opened_at, ?4)
		WHERE root_id = ?1 AND issue_id = ?2`
)

// mirrorDetailState is an issue's detail as a write of it reads it.
type mirrorDetailState struct {
	kept, held, differs bool
	openedAt, readAt    int64
}

func readMirrorDetailTx(tx *sql.Tx, rootID int64, issueID string, data []byte) (mirrorDetailState, error) {
	var st mirrorDetailState
	var opened, read sql.NullInt64
	var differs sql.NullBool
	if err := tx.QueryRow(mirrorDetailStateSQL, rootID, issueID, data).Scan(&st.kept, &opened, &read, &differs); err != nil {
		return mirrorDetailState{}, fmt.Errorf("read detail: %w", err)
	}
	st.held, st.openedAt, st.readAt, st.differs = opened.Valid, opened.Int64, read.Int64, differs.Bool
	return st, nil
}

// ConfirmMirrorDetail marks d, a document GetMirrorDetail read, opened at
// openedAt (only forward) when the mirror still holds it: the same row,
// the document of the same read, of an issue the mirror keeps, with no
// sweep since d was read. Stored reports the confirmation. Unconfirmed,
// nothing is written: the caller reads the detail again rather than show
// d's document, which may predate a sweep. It never adds a detail, so it
// drops none, and it never changes a document: only a detail read does.
func (s *Store) ConfirmMirrorDetail(rootID int64, d MirrorDetail, openedAt int64) (result MirrorDetailWrite, err error) {
	if d.IssueID == "" || d.row <= 0 || d.sweep <= 0 || d.ReadAt <= 0 || openedAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a confirmation needs a detail GetMirrorDetail read and the time it was opened", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: confirm tracker mirror %d issue %s detail", rootID, d.IssueID)
	err = s.mirrorSweptWrite(rootID, d.sweep, action, func(tx *sql.Tx) error {
		var same bool
		var opened int64
		err := tx.QueryRow(mirrorDetailConfirmSQL, rootID, d.IssueID, d.row, d.ReadAt).Scan(&same, &opened)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("read detail: %w", err)
		case !same:
			return nil
		}
		result.Stored = true
		if opened >= openedAt {
			return nil
		}
		if _, err := tx.Exec(mirrorDetailOpenSQL, rootID, d.IssueID, openedAt); err != nil {
			return fmt.Errorf("record the opening: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, ErrTrackerCacheSwept):
		return MirrorDetailWrite{}, nil
	case err != nil:
		return MirrorDetailWrite{}, err
	}
	return result, nil
}

// MirrorDetailDoc is one issue's detail document as a read found it.
type MirrorDetailDoc struct {
	IssueID string
	Data    []byte
}

// StoreMirrorDetail is StoreMirrorDetails of one document, returning its
// outcome alone: a caller that must learn which other details the write
// dropped calls StoreMirrorDetails.
func (s *Store) StoreMirrorDetail(rootID, sweep int64, issueID string, openedAt, readAt int64, data []byte) (MirrorDetailWrite, error) {
	out, err := s.StoreMirrorDetails(rootID, sweep, []MirrorDetailDoc{{IssueID: issueID, Data: data}}, openedAt, readAt)
	if err != nil {
		return MirrorDetailWrite{}, err
	}
	return out.Docs[0], nil
}

// StoreMirrorDetails stores, in one transaction, the documents of docs
// (each issue at most once) that one read begun at readAt found, each for
// an issue the mirror keeps, unless a read that began later was admitted
// for it (Superseded when that read stored another document); the same
// document from a later read moves its read time alone. openedAt is when
// a person opened or refreshed the issues (zero: no person asked for the
// read). Only an opening starts a detail: a write that is none stores
// nothing for an issue whose detail the mirror does not hold, and one
// that starts it stores its document only when its read began no earlier
// than the opening (Superseded otherwise), so a document read before its
// detail was dropped (the cap) or swept does not come back. sweep is the
// root's sweep captured before the read (ErrTrackerCacheSwept). Each
// document costs its state read and at most one write; a write that
// started a detail then drops, in one statement after every document is
// written, those beyond the mirrorDetailCap most recently opened, and the
// outcomes are of the committed state: a document the cap dropped, by an
// opening of the same write or another, is not Stored.
func (s *Store) StoreMirrorDetails(rootID, sweep int64, docs []MirrorDetailDoc, openedAt, readAt int64) (out MirrorDetailWrites, err error) {
	if len(docs) == 0 || readAt <= 0 || openedAt < 0 || sweep <= 0 {
		return MirrorDetailWrites{}, fmt.Errorf("%w: a detail write needs documents, the time their read began and the root's sweep", ErrInvalidTrackerCache)
	}
	for i, d := range docs {
		if d.IssueID == "" || slices.ContainsFunc(docs[:i], func(o MirrorDetailDoc) bool { return o.IssueID == d.IssueID }) {
			return MirrorDetailWrites{}, fmt.Errorf("%w: a detail write names each of its issues once", ErrInvalidTrackerCache)
		}
	}
	action := fmt.Sprintf("store: store tracker mirror %d details of %d issues", rootID, len(docs))
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		out = MirrorDetailWrites{Docs: make([]MirrorDetailWrite, len(docs))}
		started := make([]bool, len(docs))
		for i, d := range docs {
			var err error
			if started[i], err = storeMirrorDetailTx(tx, rootID, d.IssueID, openedAt, readAt, mirrorData(d.Data), &out.Docs[i]); err != nil {
				return fmt.Errorf("issue %s: %w", d.IssueID, err)
			}
		}
		if !slices.Contains(started, true) {
			return nil
		}
		dropped, err := queryIDs(tx, mirrorDetailEvictSQL, rootID)
		if err != nil {
			return fmt.Errorf("drop details: %w", err)
		}
		for i, d := range docs {
			if !slices.Contains(dropped, d.IssueID) {
				continue
			}
			out.Docs[i].Stored, out.Docs[i].Changed = false, false
			if started[i] {
				dropped = slices.DeleteFunc(dropped, func(id string) bool { return id == d.IssueID })
			}
		}
		if len(dropped) > 0 {
			slices.Sort(dropped)
			out.Dropped = dropped
		}
		return nil
	})
	if err != nil {
		return MirrorDetailWrites{}, err
	}
	return out, nil
}

// storeMirrorDetailTx is StoreMirrorDetails of one document before the
// cap's eviction, reporting whether it started a detail the mirror did not
// hold.
func storeMirrorDetailTx(tx *sql.Tx, rootID int64, issueID string, openedAt, readAt int64, data []byte, result *MirrorDetailWrite) (started bool, err error) {
	st, err := readMirrorDetailTx(tx, rootID, issueID, data)
	switch {
	case err != nil:
		return false, err
	case !st.kept || (!st.held && openedAt == 0):
		return false, nil
	case st.held && (st.readAt > readAt || !st.differs):
		// A later read's document, or the same one: the opening and the
		// read time move forward, a narrow write that the opened index
		// reads only when the opening moves.
		result.Stored, result.Superseded = true, st.readAt > readAt && st.differs
		if st.readAt >= readAt && st.openedAt >= openedAt {
			return false, nil
		}
		if _, err := tx.Exec(mirrorDetailMoveSQL, rootID, issueID, readAt, openedAt); err != nil {
			return false, fmt.Errorf("record the detail's read: %w", err)
		}
		return false, nil
	}
	if _, err := tx.Exec(mirrorDetailStoreSQL, rootID, issueID, readAt, data, openedAt); err != nil {
		return false, fmt.Errorf("write detail: %w", err)
	}
	result.Stored, result.Changed = true, st.held || readAt >= openedAt
	result.Superseded = !result.Changed
	return !st.held, nil
}

// GetMirrorDetail reads the stored document of an issue the mirror keeps,
// if it has one, with the update time of the mirror's card of the issue,
// in one statement.
func (s *Store) GetMirrorDetail(rootID int64, issueID string) (MirrorDetail, bool, error) {
	d := MirrorDetail{IssueID: issueID}
	err := s.reader().QueryRow(`SELECT d.rowid, r.sweep, d.opened_at, d.read_at, d.data, i.updated_at FROM tracker_cache_issue_details d
		JOIN tracker_cache_issues i ON i.root_id = d.root_id AND i.issue_id = d.issue_id
		JOIN tracker_cache_roots r ON r.id = d.root_id
		WHERE d.root_id = ?1 AND d.issue_id = ?2 AND d.data IS NOT NULL AND `+mirrorIssueKept("?1", "?2"),
		rootID, issueID).Scan(&d.row, &d.sweep, &d.OpenedAt, &d.ReadAt, &d.Data, &d.CardUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorDetail{}, false, nil
	}
	if err != nil {
		return MirrorDetail{}, false, fmt.Errorf("store: read tracker mirror %d issue %s detail: %w", rootID, issueID, err)
	}
	return d, true, nil
}

// mirrorDetailStatesSQL is the stored detail of each issue of the JSON
// list ?2 that the mirror keeps, without its document, probing the
// details' key per issue.
var mirrorDetailStatesSQL = `SELECT d.issue_id, d.rowid, r.sweep, d.opened_at, d.read_at, i.updated_at
	FROM json_each(?2) l
	CROSS JOIN tracker_cache_issue_details d ON d.root_id = ?1 AND d.issue_id = l.value
	JOIN tracker_cache_issues i ON i.root_id = d.root_id AND i.issue_id = d.issue_id
	JOIN tracker_cache_roots r ON r.id = d.root_id
	WHERE d.data IS NOT NULL AND ` + mirrorIssueKept("d.root_id", "d.issue_id")

// ListMirrorDetailStates is GetMirrorDetail of each of issueIDs (at most
// mirrorMaxLookup) without the documents, in one statement: the details
// the mirror holds, in no order, each as ConfirmMirrorDetail names it.
func (s *Store) ListMirrorDetailStates(rootID int64, issueIDs []string) ([]MirrorDetail, error) {
	switch {
	case len(issueIDs) == 0:
		return nil, nil
	case len(issueIDs) > mirrorMaxLookup:
		return nil, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(mirrorDetailStatesSQL, rootID, list)
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d details of %d issues: %w", rootID, len(issueIDs), err)
	}
	defer rows.Close()
	var out []MirrorDetail
	for rows.Next() {
		var d MirrorDetail
		if err := rows.Scan(&d.IssueID, &d.row, &d.sweep, &d.OpenedAt, &d.ReadAt, &d.CardUpdatedAt); err != nil {
			return nil, fmt.Errorf("store: read tracker mirror %d details: %w", rootID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d details: %w", rootID, err)
	}
	return out, nil
}
