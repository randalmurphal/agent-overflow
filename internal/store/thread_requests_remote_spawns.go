package store

import (
	"errors"
	"fmt"
	"slices"
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
		  WHERE target_computer_id = ` + textParam("?2") + ` AND target_computer_id <> '' AND nested_open=1
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
