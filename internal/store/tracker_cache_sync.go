package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// mirrorWorklogLimit bounds one worklog read.
const mirrorWorklogLimit = 1000

// MirrorWorklog is time logged on an issue. Day is YYYY-MM-DD in the
// author's own zone; SpentSeconds is positive.
type MirrorWorklog struct {
	ID, IssueID, AuthorID, Day string
	SpentSeconds               int64
	Description                string
	UpdatedAt                  int64
}

// MirrorWorklogChanges are the worklogs a replace stored new or changed
// and those it removed, each sorted by ID.
type MirrorWorklogChanges struct {
	Changed, Removed []string
}

func checkMirrorDay(day string) error {
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		return fmt.Errorf("%w: day %q is not YYYY-MM-DD", ErrInvalidTrackerCache, day)
	}
	return nil
}

// ReplaceMirrorWorklogs makes logs the whole of authorID's worklogs whose
// day is fromDay to toDay, both included. Every log must be the author's
// and inside the range.
func (s *Store) ReplaceMirrorWorklogs(rootID, authorID, fromDay, toDay string, logs []MirrorWorklog) (changes MirrorWorklogChanges, err error) {
	if err := errors.Join(checkMirrorDay(fromDay), checkMirrorDay(toDay)); err != nil {
		return MirrorWorklogChanges{}, err
	}
	if authorID == "" || toDay < fromDay {
		return MirrorWorklogChanges{}, fmt.Errorf("%w: worklogs need an author and days in order", ErrInvalidTrackerCache)
	}
	seen := make(map[string]bool, len(logs))
	kept := make([]string, 0, len(logs))
	for _, w := range logs {
		if w.ID == "" || seen[w.ID] || w.IssueID == "" || w.AuthorID != authorID || w.Day < fromDay || w.Day > toDay ||
			w.SpentSeconds <= 0 || w.UpdatedAt < 0 {
			return MirrorWorklogChanges{}, fmt.Errorf("%w: worklog %q is repeated, incomplete, or not %s's from %s to %s",
				ErrInvalidTrackerCache, w.ID, authorID, fromDay, toDay)
		}
		if err := checkMirrorDay(w.Day); err != nil {
			return MirrorWorklogChanges{}, err
		}
		seen[w.ID] = true
		kept = append(kept, w.ID)
	}
	encoded, err := encodeMirrorIDs(kept)
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %s worklogs of %s", rootID, authorID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		for _, w := range logs {
			changed, err := execChangedTx(tx, `INSERT INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day,
			        spent_seconds, description, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(root_id, worklog_id) DO UPDATE SET issue_id = excluded.issue_id, author_id = excluded.author_id,
				    day = excluded.day, spent_seconds = excluded.spent_seconds, description = excluded.description,
				    updated_at = excluded.updated_at
				WHERE issue_id IS NOT excluded.issue_id OR author_id IS NOT excluded.author_id OR day IS NOT excluded.day
				   OR spent_seconds IS NOT excluded.spent_seconds OR description IS NOT excluded.description
				   OR updated_at IS NOT excluded.updated_at
				RETURNING worklog_id`, rootID, w.ID, w.IssueID, w.AuthorID, w.Day, w.SpentSeconds, w.Description, w.UpdatedAt)
			if err != nil {
				return fmt.Errorf("write worklog %s: %w", w.ID, err)
			}
			if changed {
				changes.Changed = append(changes.Changed, w.ID)
			}
		}
		var err error
		changes.Removed, err = queryIDs(tx, `DELETE FROM tracker_cache_worklogs
			WHERE root_id = ? AND author_id = ? AND day BETWEEN ? AND ? AND worklog_id NOT IN (SELECT value FROM json_each(?))
			RETURNING worklog_id`, rootID, authorID, fromDay, toDay, encoded)
		if err != nil {
			return fmt.Errorf("remove worklogs: %w", err)
		}
		return nil
	})
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	slices.Sort(changes.Changed)
	slices.Sort(changes.Removed)
	return changes, nil
}

const mirrorWorklogColumns = `worklog_id, issue_id, author_id, day, spent_seconds, description, updated_at`

// ListMirrorWorklogs reads authorID's worklogs whose day is fromDay to
// toDay, by day and ID, at most mirrorWorklogLimit; more reports that the
// range holds more.
func (s *Store) ListMirrorWorklogs(rootID, authorID, fromDay, toDay string) (logs []MirrorWorklog, more bool, err error) {
	logs, more, err = queryMirrorWorklogs(s.reader(), `SELECT `+mirrorWorklogColumns+` FROM tracker_cache_worklogs
		WHERE root_id = ? AND author_id = ? AND day BETWEEN ? AND ?
		ORDER BY day, worklog_id LIMIT `+strconv.Itoa(mirrorWorklogLimit+1), rootID, authorID, fromDay, toDay)
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %s worklogs of %s: %w", rootID, authorID, err)
	}
	return logs, more, nil
}

// ListMirrorIssueWorklogs reads the worklogs of one issue, by day and ID,
// at most mirrorWorklogLimit; more reports that the issue has more.
func (s *Store) ListMirrorIssueWorklogs(rootID, issueID string) (logs []MirrorWorklog, more bool, err error) {
	logs, more, err = queryMirrorWorklogs(s.reader(), `SELECT `+mirrorWorklogColumns+` FROM tracker_cache_worklogs
		WHERE root_id = ? AND issue_id = ?
		ORDER BY day, worklog_id LIMIT `+strconv.Itoa(mirrorWorklogLimit+1), rootID, issueID)
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %s issue %s worklogs: %w", rootID, issueID, err)
	}
	return logs, more, nil
}

func queryMirrorWorklogs(q sqlQueryer, query string, args ...any) ([]MirrorWorklog, bool, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	var logs []MirrorWorklog
	for rows.Next() {
		var w MirrorWorklog
		if err := rows.Scan(&w.ID, &w.IssueID, &w.AuthorID, &w.Day, &w.SpentSeconds, &w.Description, &w.UpdatedAt); err != nil {
			return nil, false, errors.Join(err, rows.Close())
		}
		logs = append(logs, w)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(logs) > mirrorWorklogLimit {
		return logs[:mirrorWorklogLimit], true, nil
	}
	return logs, false, nil
}

// Failure kinds, aligned with tracker_cache_sync.failure_kind.
const (
	MirrorFailureTransient   = "transient"
	MirrorFailureRateLimited = "rate_limited"
	MirrorFailureSetup       = "setup"
	MirrorFailureForbidden   = "forbidden"
	MirrorFailureTracker     = "tracker"
)

// MirrorSyncFailure is a scope's last failure; the zero value is none.
type MirrorSyncFailure struct {
	At            int64
	Kind, Message string
}

// MirrorSync is a scope's sync state. A scope is a board, or with board ID
// empty the root's linked issues.
type MirrorSync struct {
	Cursor       []byte
	ReadAt       int64
	ReconciledAt int64
	Failure      MirrorSyncFailure
}

// GetMirrorSync reads a scope's sync state, if any was written.
func (s *Store) GetMirrorSync(rootID, boardID string) (MirrorSync, bool, error) {
	var st MirrorSync
	err := s.reader().QueryRow(`SELECT cursor, read_at, reconciled_at, failed_at, failure_kind, failure_message
		FROM tracker_cache_sync WHERE root_id = ? AND board_id = ?`, rootID, boardID).Scan(
		&st.Cursor, &st.ReadAt, &st.ReconciledAt, &st.Failure.At, &st.Failure.Kind, &st.Failure.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorSync{}, false, nil
	}
	if err != nil {
		return MirrorSync{}, false, fmt.Errorf("store: read tracker mirror %s sync of board %q: %w", rootID, boardID, err)
	}
	return st, true, nil
}

// SetMirrorSyncRead records when a scope was last read, leaving the rest of
// its state, and reports whether it changed.
func (s *Store) SetMirrorSyncRead(rootID, boardID string, at int64) (bool, error) {
	if at < 0 {
		return false, fmt.Errorf("%w: read time %d", ErrInvalidTrackerCache, at)
	}
	return s.setMirrorSync(rootID, boardID, "read", `INSERT INTO tracker_cache_sync(root_id, board_id, read_at) VALUES (?, ?, ?)
		ON CONFLICT(root_id, board_id) DO UPDATE SET read_at = excluded.read_at WHERE read_at IS NOT excluded.read_at
		RETURNING board_id`, at)
}

// SetMirrorSyncReconciled records when a scope was last reconciled,
// leaving the rest of its state, and reports whether it changed.
func (s *Store) SetMirrorSyncReconciled(rootID, boardID string, at int64) (bool, error) {
	if at < 0 {
		return false, fmt.Errorf("%w: reconcile time %d", ErrInvalidTrackerCache, at)
	}
	return s.setMirrorSync(rootID, boardID, "reconcile", `INSERT INTO tracker_cache_sync(root_id, board_id, reconciled_at) VALUES (?, ?, ?)
		ON CONFLICT(root_id, board_id) DO UPDATE SET reconciled_at = excluded.reconciled_at
		WHERE reconciled_at IS NOT excluded.reconciled_at
		RETURNING board_id`, at)
}

// SetMirrorSyncFailure records a scope's last failure, or with the zero
// MirrorSyncFailure clears it, leaving the rest of its state, and reports
// whether it changed.
func (s *Store) SetMirrorSyncFailure(rootID, boardID string, f MirrorSyncFailure) (bool, error) {
	switch f.Kind {
	case "":
		if f != (MirrorSyncFailure{}) {
			return false, fmt.Errorf("%w: a cleared failure has no time or message", ErrInvalidTrackerCache)
		}
	case MirrorFailureTransient, MirrorFailureRateLimited, MirrorFailureSetup, MirrorFailureForbidden, MirrorFailureTracker:
		if f.At <= 0 {
			return false, fmt.Errorf("%w: a failure needs its time", ErrInvalidTrackerCache)
		}
	default:
		return false, fmt.Errorf("%w: failure kind %q", ErrInvalidTrackerCache, f.Kind)
	}
	return s.setMirrorSync(rootID, boardID, "failure", `INSERT INTO tracker_cache_sync(root_id, board_id, failed_at, failure_kind, failure_message)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(root_id, board_id) DO UPDATE SET failed_at = excluded.failed_at, failure_kind = excluded.failure_kind,
		    failure_message = excluded.failure_message
		WHERE failed_at IS NOT excluded.failed_at OR failure_kind IS NOT excluded.failure_kind
		   OR failure_message IS NOT excluded.failure_message
		RETURNING board_id`, f.At, f.Kind, f.Message)
}

func (s *Store) setMirrorSync(rootID, boardID, what, query string, values ...any) (changed bool, err error) {
	action := fmt.Sprintf("store: write tracker mirror %s sync %s of board %q", rootID, what, boardID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		changed, err = execChangedTx(tx, query, append([]any{rootID, boardID}, values...)...)
		return err
	})
	return changed, err
}
