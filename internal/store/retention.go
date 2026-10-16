package store

import (
	"fmt"

	"agent-overflow/internal/threadmode"
)

// ThreadIDsOlderThan returns thread IDs whose updated_at strictly
// precedes cutoffMs (Unix milliseconds), oldest first. SELECT only —
// deletion routes through app_thread_runtime.go::deleteThreadLocked,
// which owns side effects (attachment dirs, replay
// logs, checkpoint git refs in the user's repos) that a row-level
// DELETE in this package would silently skip.
//
// Excludes pending edited-message recovery, holders, which go when no
// fork reads them (ListReleasedHolders), and an assistant's current and
// past threads, which go only when the assistant is deleted
// (docs/specs/standing-agents.md#retention). Archived, pinned, mid-turn and draft threads
// match if their updated_at qualifies. The retention policy is
// intentionally uniform; mid-turn is naturally protected because
// MarkThreadActivity bumps updated_at on every persisted event, so an
// in-flight turn can't drift past the cutoff.
//
// Uses idx_threads_updated for the range scan + ORDER BY.
func (s *Store) ThreadIDsOlderThan(cutoffMs int64) ([]string, error) {
	rows, err := s.reader().Query(
		`SELECT id FROM threads WHERE updated_at < ? AND mode <> ?
			AND NOT EXISTS(SELECT 1 FROM thread_draft_recoveries r WHERE r.thread_id = threads.id)
			AND NOT EXISTS(SELECT 1 FROM agents a WHERE a.thread_id = threads.id)
			AND NOT EXISTS(SELECT 1 FROM agent_past_threads p WHERE p.thread_id = threads.id)
			ORDER BY updated_at ASC`,
		cutoffMs, threadmode.ModeHolder,
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
