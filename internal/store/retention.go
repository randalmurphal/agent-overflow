package store

import "fmt"

// ThreadIDsOlderThan returns thread IDs whose updated_at strictly
// precedes cutoffMs (Unix milliseconds), oldest first. SELECT only —
// deletion routes through app_thread_delete.go::deleteThreadTreeLocked,
// which owns side effects (attachment dirs, design workdirs, replay
// logs, checkpoint git refs in the user's repos) that a row-level
// DELETE in this package would silently skip.
//
// Excludes nothing: archived, pinned, mid-turn, and draft threads all
// match if their updated_at qualifies. The retention policy is
// intentionally uniform; mid-turn is naturally protected because
// MarkThreadActivity bumps updated_at on every persisted event, so an
// in-flight turn can't drift past the cutoff.
//
// Uses idx_threads_updated for the range scan + ORDER BY.
func (s *Store) ThreadIDsOlderThan(cutoffMs int64) ([]string, error) {
	rows, err := s.reader().Query(
		`SELECT id FROM threads WHERE updated_at < ? ORDER BY updated_at ASC`,
		cutoffMs,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads older than %d: %w", cutoffMs, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan thread id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate threads older than %d: %w", cutoffMs, err)
	}
	return ids, nil
}
