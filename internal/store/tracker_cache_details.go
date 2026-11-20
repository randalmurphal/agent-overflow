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
// last opened, when the document's read began, and the document.
type MirrorDetail struct {
	IssueID          string
	OpenedAt, ReadAt int64
	Data             []byte
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
	mirrorDetailStoreSQL = `INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at, read_at, data) VALUES (?1, ?2, ?3, ?3, ?4)
		ON CONFLICT(root_id, issue_id) DO UPDATE SET read_at = excluded.read_at, data = excluded.data`
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
// gets one with no document. A new detail drops those beyond the
// mirrorDetailCap most recently opened.
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
		if _, err := tx.Exec(`INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at) VALUES (?, ?, ?)`,
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

// StoreMirrorDetail stores the document of an issue the mirror keeps, read
// from readAt, unless the stored one came from a read that began later
// (Superseded); an identical document writes nothing. It leaves when the
// issue was opened, except that an issue with no detail yet counts as
// opened at readAt. A new detail drops those beyond the mirrorDetailCap
// most recently opened.
func (s *Store) StoreMirrorDetail(rootID int64, issueID string, readAt int64, data []byte) (result MirrorDetailWrite, err error) {
	if issueID == "" || readAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a detail needs its issue and the time its read began", ErrInvalidTrackerCache)
	}
	data = mirrorData(data)
	action := fmt.Sprintf("store: store tracker mirror %d issue %s detail", rootID, issueID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		st, err := readMirrorDetailTx(tx, rootID, issueID, data)
		switch {
		case err != nil:
			return err
		case !st.kept:
			return nil
		case st.held && !st.differs:
			result.Stored = true
			return nil
		case st.held && st.readAt > readAt:
			result.Stored, result.Superseded = true, true
			return nil
		}
		if _, err := tx.Exec(mirrorDetailStoreSQL, rootID, issueID, readAt, data); err != nil {
			return fmt.Errorf("write detail: %w", err)
		}
		result.Stored, result.Changed = true, true
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
// if it has one.
func (s *Store) GetMirrorDetail(rootID int64, issueID string) (MirrorDetail, bool, error) {
	d := MirrorDetail{IssueID: issueID}
	err := s.reader().QueryRow(`SELECT d.opened_at, d.read_at, d.data FROM tracker_cache_issue_details d
		WHERE d.root_id = ?1 AND d.issue_id = ?2 AND d.data IS NOT NULL AND `+mirrorIssueKept("?1", "?2"),
		rootID, issueID).Scan(&d.OpenedAt, &d.ReadAt, &d.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorDetail{}, false, nil
	}
	if err != nil {
		return MirrorDetail{}, false, fmt.Errorf("store: read tracker mirror %d issue %s detail: %w", rootID, issueID, err)
	}
	return d, true, nil
}
