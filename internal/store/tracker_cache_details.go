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

// MirrorDetailWrite is what a detail write did: whether the issue's detail
// is held after it (Stored), whether its document changed, whether the
// document was superseded by one from a read that began later, and the
// issues whose details it dropped to keep the mirrorDetailCap most
// recently opened, the written issue's own included when it is the oldest.
type MirrorDetailWrite struct {
	Stored, Changed, Superseded bool
	Dropped                     []string
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
	// the document read from ?4 of a kept issue, its opening, and whether
	// its document differs from ?5.
	mirrorDetailConfirmSQL = `SELECT d.rowid = ?3 AND d.read_at = ?4 AND d.data IS NOT NULL AND ` + mirrorIssueKept("?1", "?2") + `,
		d.opened_at, d.data IS NOT ?5 FROM tracker_cache_issue_details d WHERE d.root_id = ?1 AND d.issue_id = ?2`
	// mirrorDetailConfirmWriteSQL moves the opening forward to ?3 and
	// replaces the document with ?4 unless it is NULL.
	mirrorDetailConfirmWriteSQL = `UPDATE tracker_cache_issue_details SET opened_at = max(opened_at, ?3), data = coalesce(?4, data)
		WHERE root_id = ?1 AND issue_id = ?2`
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

// TouchMirrorDetail records that an issue the mirror keeps was opened at
// openedAt; the time only moves forward, and an issue with no detail yet
// gets one with no document whose read watermark is the opening, so no
// document from a read that began before it is stored, one from before
// the cap last dropped the detail included. A new detail drops those
// beyond the mirrorDetailCap most recently opened.
func (s *Store) TouchMirrorDetail(rootID int64, issueID string, openedAt int64) (result MirrorDetailWrite, err error) {
	if issueID == "" || openedAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: an opening needs its issue and its time", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: record tracker mirror %d issue %s opened", rootID, issueID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		st, err := readMirrorDetailTx(tx, rootID, issueID, nil)
		switch {
		case err != nil:
			return err
		case !st.kept:
			return nil
		case st.held && st.openedAt >= openedAt:
			result.Stored = true
			return nil
		case st.held:
			if _, err := tx.Exec(`UPDATE tracker_cache_issue_details SET opened_at = ?3 WHERE root_id = ?1 AND issue_id = ?2`,
				rootID, issueID, openedAt); err != nil {
				return fmt.Errorf("record the opening: %w", err)
			}
			result.Stored = true
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at, read_at) VALUES (?1, ?2, ?3, ?3)`,
			rootID, issueID, openedAt); err != nil {
			return fmt.Errorf("record the opening: %w", err)
		}
		result.Stored = true
		return evictMirrorDetailsTx(tx, rootID, issueID, &result)
	})
	if err != nil {
		return MirrorDetailWrite{}, err
	}
	return result, nil
}

// ConfirmMirrorDetail marks d, a document GetMirrorDetail read, opened at
// openedAt (only forward) when the mirror still holds it: the same row,
// the document of the same read, of an issue the mirror keeps, with no
// sweep since d was read. replace, when not nil, is stored in its place
// with d's read time: d's document with what it shows of other issues
// (a linked issue's card) read since. Stored reports the confirmation and
// Changed a replacement that differs from the stored document. Unconfirmed,
// nothing is written: the caller reads the detail again rather than store
// d's document, which may predate a sweep. It never adds a detail, so it
// drops none.
func (s *Store) ConfirmMirrorDetail(rootID int64, d MirrorDetail, openedAt int64, replace []byte) (result MirrorDetailWrite, err error) {
	if d.IssueID == "" || d.row <= 0 || d.sweep <= 0 || d.ReadAt <= 0 || openedAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a confirmation needs a detail GetMirrorDetail read and the time it was opened", ErrInvalidTrackerCache)
	}
	var doc any
	if replace != nil {
		doc = replace
	}
	action := fmt.Sprintf("store: confirm tracker mirror %d issue %s detail", rootID, d.IssueID)
	err = s.mirrorSweptWrite(rootID, d.sweep, action, func(tx *sql.Tx) error {
		var same, differs bool
		var opened int64
		err := tx.QueryRow(mirrorDetailConfirmSQL, rootID, d.IssueID, d.row, d.ReadAt, doc).Scan(&same, &opened, &differs)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("read detail: %w", err)
		case !same:
			return nil
		}
		result.Stored, result.Changed = true, doc != nil && differs
		if opened >= openedAt && !result.Changed {
			return nil
		}
		if _, err := tx.Exec(mirrorDetailConfirmWriteSQL, rootID, d.IssueID, openedAt, doc); err != nil {
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

// StoreMirrorDetail stores the document of an issue the mirror keeps, read
// from readAt, unless a read that began later was admitted for it
// (Superseded when that read stored another document); the same document
// from a later read moves its read time alone. openedAt is when the issue
// was opened, recorded in the same write as TouchMirrorDetail records it
// (zero: the write is no opening). Only an opening starts a detail: a
// write that is none stores nothing for an issue whose detail the mirror
// does not hold, and one that starts it stores its document only when its
// read began no earlier than the opening (Superseded otherwise), so a
// document read before its detail was dropped (the cap) or swept does not
// come back. sweep is the root's sweep captured
// before the read (ErrTrackerCacheSwept). Each outcome is one statement.
// A new detail drops those beyond the mirrorDetailCap most recently
// opened.
func (s *Store) StoreMirrorDetail(rootID, sweep int64, issueID string, openedAt, readAt int64, data []byte) (result MirrorDetailWrite, err error) {
	if issueID == "" || readAt <= 0 || openedAt < 0 || sweep <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a detail needs its issue, the time its read began and the root's sweep", ErrInvalidTrackerCache)
	}
	data = mirrorData(data)
	action := fmt.Sprintf("store: store tracker mirror %d issue %s detail", rootID, issueID)
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		st, err := readMirrorDetailTx(tx, rootID, issueID, data)
		switch {
		case err != nil:
			return err
		case !st.kept || (!st.held && openedAt == 0):
			return nil
		case st.held && (st.readAt > readAt || !st.differs):
			// A later read's document, or the same one: the opening and the
			// read time move forward, a narrow write that the opened index
			// reads only when the opening moves.
			result.Stored, result.Superseded = true, st.readAt > readAt && st.differs
			if st.readAt >= readAt && st.openedAt >= openedAt {
				return nil
			}
			if _, err := tx.Exec(mirrorDetailMoveSQL, rootID, issueID, readAt, openedAt); err != nil {
				return fmt.Errorf("record the detail's read: %w", err)
			}
			return nil
		}
		if _, err := tx.Exec(mirrorDetailStoreSQL, rootID, issueID, readAt, data, openedAt); err != nil {
			return fmt.Errorf("write detail: %w", err)
		}
		result.Stored, result.Changed = true, st.held || readAt >= openedAt
		result.Superseded = !result.Changed
		if !st.held {
			return evictMirrorDetailsTx(tx, rootID, issueID, &result)
		}
		return nil
	})
	if err != nil {
		return MirrorDetailWrite{}, err
	}
	return result, nil
}

// evictMirrorDetailsTx drops the details beyond the cap after a new one,
// reporting whether the new one itself went.
func evictMirrorDetailsTx(tx *sql.Tx, rootID int64, issueID string, result *MirrorDetailWrite) error {
	dropped, err := queryIDs(tx, mirrorDetailEvictSQL, rootID)
	if err != nil {
		return fmt.Errorf("drop details: %w", err)
	}
	slices.Sort(dropped)
	result.Dropped = dropped
	if slices.Contains(dropped, issueID) {
		result.Stored, result.Changed = false, false
	}
	return nil
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
