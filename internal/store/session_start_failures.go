package store

import "fmt"

// A thread's session start failure mark (migration v143): set when a session
// the app started for a message it queued fails and raises the start-failure
// notification, and cleared by the first start that succeeds, which
// withdraws that notification. It outlives a restart because the
// notification does.

// MarkSessionStartFailed marks a thread whose start failed at at. A thread
// already marked keeps its first mark.
func (s *Store) MarkSessionStartFailed(threadID string, at int64) error {
	if _, err := s.db.Exec(`INSERT INTO session_start_failures (thread_id, failed_at) VALUES (?, ?)
		ON CONFLICT(thread_id) DO NOTHING`, threadID, at); err != nil {
		return fmt.Errorf("store: mark the failed start of %s: %w", threadID, err)
	}
	return nil
}

// ClearSessionStartFailed removes a thread's mark and reports whether it had
// one.
func (s *Store) ClearSessionStartFailed(threadID string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM session_start_failures WHERE thread_id = ?`, threadID)
	if err != nil {
		return false, fmt.Errorf("store: clear the failed start of %s: %w", threadID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: clear the failed start of %s: rows affected: %w", threadID, err)
	}
	return n > 0, nil
}

// SessionStartFailures returns every marked thread with when its start
// failed.
func (s *Store) SessionStartFailures() (_ map[string]int64, err error) {
	const action = "store: list failed starts"
	rows, err := s.reader().Query(`SELECT thread_id, failed_at FROM session_start_failures`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	marks := make(map[string]int64)
	for rows.Next() {
		var threadID string
		var at int64
		if err := rows.Scan(&threadID, &at); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		marks[threadID] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return marks, nil
}
