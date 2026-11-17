package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// ForgetRemoteSpawnsOnComputer forgets what this computer keeps of its
// spawns to one computer no longer paired, where nothing will answer for
// them again: nested_open is cleared on every spawn to it, and the lineage
// of those past the retention floor is deleted, since it links a thread
// nothing here can reach, and so is every owner operation still owed to
// it. It returns the callers whose rows changed. A forgotten computer is
// rare, so the reads are the table's.
func (s *Store) ForgetRemoteSpawnsOnComputer(computerID string) (_ []string, err error) {
	action := fmt.Sprintf("store: forget the spawns on computer %s", computerID)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var callers []string
	for _, statement := range []string{
		`DELETE FROM thread_requests
		  WHERE target_computer_id = ` + textParam("?2") + ` AND target_computer_id <> '' AND lineage_only = 1
		  RETURNING caller_thread_id`,
		`UPDATE thread_requests SET nested_open = 0, updated_at = ?1
		  WHERE target_computer_id = ` + textParam("?2") + ` AND target_computer_id <> '' AND nested_open = 1
		  RETURNING caller_thread_id`,
	} {
		rows, err := tx.Query(statement, nowMillis(), computerID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		for rows.Next() {
			var caller string
			if err := rows.Scan(&caller); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("%s: scan: %w", action, err)
			}
			if !slices.Contains(callers, caller) {
				callers = append(callers, caller)
			}
		}
		// The write finishes when its rows close: either error is the write's.
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
	}
	if _, err := tx.Exec(forgetOwnerOpsSQL, computerID); err != nil {
		return nil, fmt.Errorf("%s: owed operations: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	return callers, nil
}

// DeleteRemoteSpawnLineage deletes the lineage of a spawn on another
// computer once that computer refused its thread as not the spawn's: the
// thread there, or its receipt, is gone, and nothing will list it again. It
// reports whether a row went; a spawn not reduced to its lineage stays.
func (s *Store) DeleteRemoteSpawnLineage(token string) (bool, error) {
	action := fmt.Sprintf("store: delete the lineage of remote spawn %s", token)
	result, err := s.db.Exec(`DELETE FROM thread_requests
		  WHERE token = ? AND lineage_only = 1 AND kind = 'spawn' AND target_computer_id <> ''`, token)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// ListNestedOpenRemoteSpawns returns the settled spawns on other computers
// whose last agent_rows answer listed open work, after the one settled at
// afterSettledAt with afterToken, in settled order: the rows the poller
// reads agent_rows for again until that work is done. The poller pages
// through them by the last row of each page, so a row that stays open
// does not keep the ones after it from being asked.
func (s *Store) ListNestedOpenRemoteSpawns(afterSettledAt int64, afterToken string, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(nestedOpenRemoteSpawnsSQL(limit), afterSettledAt, afterToken)
	if err != nil {
		return nil, fmt.Errorf("store: list nested-open remote spawns: %w", err)
	}
	return collectThreadRequests(rows, "store: list nested-open remote spawns")
}

// nestedOpenRemoteSpawnsSQL reads one page of
// idx_thread_requests_nested_open, whose predicate it states. A remote
// spawn's lineage is one of them: it is its thread's link here.
func nestedOpenRemoteSpawnsSQL(limit int) string {
	return `SELECT ` + threadRequestColumns + ` FROM thread_requests
	  WHERE nested_open = 1 AND settled_at IS NOT NULL AND kind = 'spawn'
	    AND target_computer_id <> ''
	    AND (settled_at, token) > (?, ?)
	  ORDER BY settled_at ASC, token ASC LIMIT ` + strconv.Itoa(limit)
}

// ListRestingRemoteSpawns returns the settled spawns on other computers
// whose last agent_rows answer listed no open work, after the one settled
// at afterSettledAt with afterToken, in settled order: the rows the poller
// asks about at its slower pace, so a thread there continued after it
// rested is found. A page is one range of idx_thread_requests_remote_rest.
func (s *Store) ListRestingRemoteSpawns(afterSettledAt int64, afterToken string, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: list resting remote spawns: limit %d", limit)
	}
	rows, err := s.reader().Query(restingRemoteSpawnsSQL(limit), afterSettledAt, afterToken)
	if err != nil {
		return nil, fmt.Errorf("store: list resting remote spawns: %w", err)
	}
	return collectThreadRequests(rows, "store: list resting remote spawns")
}

// restingRemoteSpawnSQL is the predicate of idx_thread_requests_remote_rest:
// a settled spawn on another computer whose last answer listed no open work,
// its lineage past the retention floor included.
const restingRemoteSpawnSQL = `nested_open = 0 AND settled_at IS NOT NULL AND kind = 'spawn'
	    AND target_computer_id <> '' AND target_thread_id <> ''`

// restingRemoteSpawnsSQL reads one page of idx_thread_requests_remote_rest.
func restingRemoteSpawnsSQL(limit int) string {
	return `SELECT ` + threadRequestColumns + ` FROM thread_requests
	  WHERE ` + restingRemoteSpawnSQL + `
	    AND (settled_at, token) > (?, ?)
	  ORDER BY settled_at ASC, token ASC LIMIT ` + strconv.Itoa(limit)
}
