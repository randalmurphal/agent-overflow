package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// Change kinds, aligned with tracker_cache_changes.kind.
const (
	MirrorChangeStatus   = "status"
	MirrorChangeSprint   = "sprint"
	MirrorChangeEstimate = "estimate"
	MirrorChangeAssignee = "assignee"
)

// mirrorHistoryDueLimit bounds one ListMirrorHistoryDue read.
const mirrorHistoryDueLimit = 100

// mirrorDetailCap is how many details a root keeps: the most recently
// opened.
const mirrorDetailCap = 200

// MirrorChange is one change of an issue: when, by whom (empty for the
// tracker itself), and its values, by kind:
//   - status, assignee: From and To are IDs, empty for none;
//   - sprint: FromSprints and ToSprints are the whole sets;
//   - estimate: BoardID is the board whose estimate changed, FromNumber
//     and ToNumber its values, nil for none.
type MirrorChange struct {
	Kind                   string
	At                     int64
	AuthorID               string
	From, To               string
	FromSprints, ToSprints []string
	BoardID                string
	FromNumber, ToNumber   *float64
}

// MirrorHistory is one issue's changes, each kind oldest first. They are
// complete from CompleteFrom (zero: from the issue's creation) and final
// through CompleteTo, which is always set.
type MirrorHistory struct {
	IssueID                  string
	CompleteFrom, CompleteTo int64
	Changes                  []MirrorChange
}

func (c MirrorChange) check() error {
	if c.At <= 0 {
		return fmt.Errorf("%w: a %s change needs its time", ErrInvalidTrackerCache, c.Kind)
	}
	ids := c.From != "" || c.To != ""
	sprints := c.FromSprints != nil || c.ToSprints != nil
	estimate := c.BoardID != "" || c.FromNumber != nil || c.ToNumber != nil
	var ok bool
	switch c.Kind {
	case MirrorChangeStatus, MirrorChangeAssignee:
		ok = !sprints && !estimate
	case MirrorChangeSprint:
		ok = !ids && !estimate
	case MirrorChangeEstimate:
		ok = !ids && !sprints && c.BoardID != ""
	}
	if !ok {
		return fmt.Errorf("%w: a %q change with values of another kind", ErrInvalidTrackerCache, c.Kind)
	}
	return nil
}

// values are the change's from_value and to_value columns.
func (c MirrorChange) values() (from, to string, err error) {
	if c.Kind != MirrorChangeSprint {
		return c.From, c.To, nil
	}
	encode := func(set []string) (string, error) {
		sorted := slices.Sorted(slices.Values(set))
		if sorted == nil {
			sorted = []string{}
		}
		encoded, err := json.Marshal(sorted)
		return string(encoded), err
	}
	if from, err = encode(c.FromSprints); err != nil {
		return "", "", fmt.Errorf("encode sprints: %w", err)
	}
	if to, err = encode(c.ToSprints); err != nil {
		return "", "", fmt.Errorf("encode sprints: %w", err)
	}
	return from, to, nil
}

// ReplaceMirrorHistory makes h the whole stored history of its issue and
// reports whether anything changed. An issue not in the mirror (pruned
// since its history was read) stores nothing and reports no change.
func (s *Store) ReplaceMirrorHistory(rootID string, h MirrorHistory) (changed bool, err error) {
	if h.IssueID == "" || h.CompleteTo <= 0 || h.CompleteFrom < 0 {
		return false, fmt.Errorf("%w: a history needs its issue and the time it is final through", ErrInvalidTrackerCache)
	}
	for _, c := range h.Changes {
		if err := c.check(); err != nil {
			return false, err
		}
	}
	action := fmt.Sprintf("store: replace tracker mirror %s issue %s history", rootID, h.IssueID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		changed, err = replaceMirrorHistoryTx(tx, rootID, h)
		return err
	})
	return changed, err
}

func replaceMirrorHistoryTx(tx *sql.Tx, rootID string, h MirrorHistory) (bool, error) {
	var present int
	err := tx.QueryRow(`SELECT 1 FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ?`, rootID, h.IssueID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read issue: %w", err)
	}
	changed, err := execChangedTx(tx, `UPDATE tracker_cache_issues SET history_from = ?3, history_to = ?4
		WHERE root_id = ?1 AND issue_id = ?2 AND (history_from IS NOT ?3 OR history_to IS NOT ?4) RETURNING issue_id`,
		rootID, h.IssueID, h.CompleteFrom, h.CompleteTo)
	if err != nil {
		return false, fmt.Errorf("write history times: %w", err)
	}
	counts := map[string]int{}
	for _, c := range h.Changes {
		seq := counts[c.Kind]
		counts[c.Kind]++
		from, to, err := c.values()
		if err != nil {
			return false, err
		}
		rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_changes(root_id, issue_id, kind, seq, at, author_id, board_id,
		        from_value, to_value, from_number, to_number)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(root_id, issue_id, kind, seq) DO UPDATE SET at = excluded.at, author_id = excluded.author_id,
			    board_id = excluded.board_id, from_value = excluded.from_value, to_value = excluded.to_value,
			    from_number = excluded.from_number, to_number = excluded.to_number
			WHERE at IS NOT excluded.at OR author_id IS NOT excluded.author_id OR board_id IS NOT excluded.board_id
			   OR from_value IS NOT excluded.from_value OR to_value IS NOT excluded.to_value
			   OR from_number IS NOT excluded.from_number OR to_number IS NOT excluded.to_number
			RETURNING seq`, rootID, h.IssueID, c.Kind, seq, c.At, c.AuthorID, c.BoardID, from, to, c.FromNumber, c.ToNumber)
		if err != nil {
			return false, fmt.Errorf("write %s change %d: %w", c.Kind, seq, err)
		}
		changed = changed || rowChanged
	}
	removed, err := execChangedTx(tx, `DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2
		AND seq >= CASE kind WHEN 'status' THEN ?3 WHEN 'sprint' THEN ?4 WHEN 'estimate' THEN ?5 ELSE ?6 END
		RETURNING seq`, rootID, h.IssueID, counts[MirrorChangeStatus], counts[MirrorChangeSprint],
		counts[MirrorChangeEstimate], counts[MirrorChangeAssignee])
	if err != nil {
		return false, fmt.Errorf("remove changes: %w", err)
	}
	return changed || removed, nil
}

// mirrorHistoryDueSQL states the predicate of
// idx_tracker_cache_issues_history_due, so the read probes that index.
var mirrorHistoryDueSQL = `SELECT issue_id FROM tracker_cache_issues
	WHERE root_id = ? AND history_to < updated_at AND issue_id > ?
	ORDER BY issue_id LIMIT ` + strconv.Itoa(mirrorHistoryDueLimit)

// ListMirrorHistoryDue lists, by issue ID after after, up to
// mirrorHistoryDueLimit issues of rootID whose update time is later than
// the time their stored history is final through, or that have none: the
// issues whose history sync reads next.
func (s *Store) ListMirrorHistoryDue(rootID, after string) ([]string, error) {
	ids, err := queryIDs(s.reader(), mirrorHistoryDueSQL, rootID, after)
	if err != nil {
		return nil, fmt.Errorf("store: list tracker mirror %s history due: %w", rootID, err)
	}
	return ids, nil
}

// MirrorDetail is an issue's detail document and when it was last opened.
type MirrorDetail struct {
	IssueID  string
	OpenedAt int64
	Data     []byte
}

// MirrorDetailWrite is what StoreMirrorDetail did: whether it stored the
// detail, whether the document changed, and the issues whose details it
// dropped to keep the newest mirrorDetailCap.
type MirrorDetailWrite struct {
	Stored, Changed bool
	Dropped         []string
}

// StoreMirrorDetail stores an issue's detail with the time it was opened
// and drops the details beyond the mirrorDetailCap most recently opened,
// in one transaction. The detail of an issue the mirror does not keep is
// not stored.
func (s *Store) StoreMirrorDetail(rootID string, d MirrorDetail) (result MirrorDetailWrite, err error) {
	if d.IssueID == "" || d.OpenedAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a detail needs its issue and the time it was opened", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: store tracker mirror %s issue %s detail", rootID, d.IssueID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var present int
		err := tx.QueryRow(`SELECT 1 FROM tracker_cache_issues WHERE root_id = ? AND issue_id = ?`, rootID, d.IssueID).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read issue: %w", err)
		}
		var stored []byte
		err = tx.QueryRow(`SELECT data FROM tracker_cache_issue_details WHERE root_id = ? AND issue_id = ?`, rootID, d.IssueID).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			result.Changed = true
		case err != nil:
			return fmt.Errorf("read detail: %w", err)
		default:
			result.Changed = !bytes.Equal(stored, mirrorData(d.Data))
		}
		if _, err := tx.Exec(`INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at, data) VALUES (?, ?, ?, ?)
			ON CONFLICT(root_id, issue_id) DO UPDATE SET opened_at = excluded.opened_at, data = excluded.data`,
			rootID, d.IssueID, d.OpenedAt, mirrorData(d.Data)); err != nil {
			return fmt.Errorf("write detail: %w", err)
		}
		result.Stored = true
		result.Dropped, err = queryIDs(tx, `DELETE FROM tracker_cache_issue_details WHERE root_id = ?1 AND issue_id IN (
			SELECT issue_id FROM tracker_cache_issue_details WHERE root_id = ?1
			 ORDER BY opened_at DESC, issue_id DESC LIMIT -1 OFFSET `+strconv.Itoa(mirrorDetailCap)+`)
			RETURNING issue_id`, rootID)
		if err != nil {
			return fmt.Errorf("drop details: %w", err)
		}
		slices.Sort(result.Dropped)
		return nil
	})
	if err != nil {
		return MirrorDetailWrite{}, err
	}
	return result, nil
}

// GetMirrorDetail reads an issue's stored detail, if the mirror has it.
func (s *Store) GetMirrorDetail(rootID, issueID string) (MirrorDetail, bool, error) {
	d := MirrorDetail{IssueID: issueID}
	err := s.reader().QueryRow(`SELECT opened_at, data FROM tracker_cache_issue_details WHERE root_id = ? AND issue_id = ?`,
		rootID, issueID).Scan(&d.OpenedAt, &d.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorDetail{}, false, nil
	}
	if err != nil {
		return MirrorDetail{}, false, fmt.Errorf("store: read tracker mirror %s issue %s detail: %w", rootID, issueID, err)
	}
	return d, true, nil
}
