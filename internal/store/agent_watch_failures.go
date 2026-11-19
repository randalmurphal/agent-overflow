package store

import "fmt"

// A watch's failure notice mark (agent_wakes.failure_noticed_at, migration
// v155): set when the app raises the notification that a watch keeps
// failing, and cleared when it withdraws it. The failure episode itself is
// the app's in-memory state; the mark outlives a restart because the
// notification does, so the run that ends the episode after a restart still
// withdraws it.

// MarkAgentWatchFailureNoticed marks a watch whose failure notice was raised
// at at.
func (s *Store) MarkAgentWatchFailureNoticed(id string, at int64) error {
	if _, err := s.db.Exec(`UPDATE agent_wakes SET failure_noticed_at = ? WHERE id = ?`, at, id); err != nil {
		return fmt.Errorf("store: mark the failure notice of watch %s: %w", id, err)
	}
	return nil
}

// ClearAgentWatchFailureNoticed removes a watch's mark. A watch with none is
// left as it is.
func (s *Store) ClearAgentWatchFailureNoticed(id string) error {
	if _, err := s.db.Exec(`UPDATE agent_wakes SET failure_noticed_at = NULL
		WHERE id = ? AND failure_noticed_at IS NOT NULL`, id); err != nil {
		return fmt.Errorf("store: clear the failure notice of watch %s: %w", id, err)
	}
	return nil
}

// AgentWatchFailureNotices returns every live watch whose failure notice is
// out, with when it was raised.
func (s *Store) AgentWatchFailureNotices() (_ map[string]int64, err error) {
	const action = "store: list watch failure notices"
	rows, err := s.reader().Query(`SELECT id, failure_noticed_at FROM agent_wakes
		WHERE failure_noticed_at IS NOT NULL AND state <> 'cancelled'`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	marks := make(map[string]int64)
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		marks[id] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return marks, nil
}
