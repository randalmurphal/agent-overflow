package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ThreadImportState is the per-thread cursor into the provider session file
// an import came from (migration v50). It exists so a later refresh can tell
// what has already been written and where to resume reading, without
// re-deriving either from the timeline.
//
// LeafUUID records which Claude branch this thread was cut from — one session
// file can produce several threads.
type ThreadImportState struct {
	ThreadID        string `json:"threadId"`
	Provider        string `json:"provider"`
	SourcePath      string `json:"sourcePath"`
	SourceSessionID string `json:"sourceSessionId"`
	LeafUUID        string `json:"leafUuid"`
	// LastSourceUUID is the provenance stamp of the last event consumed —
	// a transcript uuid for Claude, `line:<byte offset>` for Codex, and
	// written by both. Only CLAUDE anchors on it: its transcript is a uuid
	// DAG, so a refresh walks the branch for events after this row.
	LastSourceUUID string `json:"lastSourceUuid"`
	// LastSourceOffset is the byte offset a Codex tail refresh reads from,
	// and Codex's own anchor. It stays 0 for Claude, whose conversation
	// position a file offset says nothing about.
	LastSourceOffset int64 `json:"lastSourceOffset"`
	// LastTurnIndex and LastItemIndex are the LAST ROW POSITION the import
	// wrote, and they are a pair on purpose: items.item_index restarts at 0
	// in every turn, so an item index alone names no position in a thread.
	// Together they are the (turn_index, item_index) ordering every timeline
	// read sorts by, which makes the divergence question exact — see
	// HasItemsAfterCursor. Both are -1 when nothing was written.
	LastTurnIndex int   `json:"lastTurnIndex"`
	LastItemIndex int   `json:"lastItemIndex"`
	ImportedAt    int64 `json:"importedAt"`
	RefreshedAt   int64 `json:"refreshedAt"`
}

// ErrInvalidImportProvider is returned for an import provider outside the
// migration v50 enum, before SQLite would report a raw CHECK failure.
var ErrInvalidImportProvider = errors.New("store: invalid import provider")

const threadImportStateColumns = `thread_id, provider, source_path, source_session_id,
    leaf_uuid, last_source_uuid, last_source_offset, last_turn_index, last_item_index,
    imported_at, refreshed_at`

// SetThreadImportState writes the thread's import cursor, replacing any
// previous one. Both the initial import and every refresh go through it —
// a refresh is the same row with the cursor advanced and RefreshedAt
// stamped, so there is one writer and one shape.
func (s *Store) SetThreadImportState(state ThreadImportState) error {
	if state.ThreadID == "" {
		return fmt.Errorf("store: set thread import state: thread id is required")
	}
	if state.Provider != "claude" && state.Provider != "codex" {
		return fmt.Errorf("%w: %q", ErrInvalidImportProvider, state.Provider)
	}
	if state.SourceSessionID == "" {
		return fmt.Errorf("store: set thread import state %s: source session id is required", state.ThreadID)
	}
	_, err := s.db.Exec(
		`INSERT INTO thread_import_state (`+threadImportStateColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
		     provider = excluded.provider,
		     source_path = excluded.source_path,
		     source_session_id = excluded.source_session_id,
		     leaf_uuid = excluded.leaf_uuid,
		     last_source_uuid = excluded.last_source_uuid,
		     last_source_offset = excluded.last_source_offset,
		     last_turn_index = excluded.last_turn_index,
		     last_item_index = excluded.last_item_index,
		     imported_at = excluded.imported_at,
		     refreshed_at = excluded.refreshed_at`,
		state.ThreadID, state.Provider, state.SourcePath, state.SourceSessionID,
		state.LeafUUID, state.LastSourceUUID, state.LastSourceOffset,
		state.LastTurnIndex, state.LastItemIndex,
		state.ImportedAt, state.RefreshedAt,
	)
	if err != nil {
		return fmt.Errorf("store: set thread import state %s: %w", state.ThreadID, err)
	}
	return nil
}

// GetThreadImportState reads one thread's import cursor. The boolean is
// false for a thread AO created itself — an expected state, not an error.
func (s *Store) GetThreadImportState(threadID string) (ThreadImportState, bool, error) {
	var state ThreadImportState
	err := s.reader().QueryRow(
		`SELECT `+threadImportStateColumns+` FROM thread_import_state WHERE thread_id = ?`,
		threadID,
	).Scan(
		&state.ThreadID, &state.Provider, &state.SourcePath, &state.SourceSessionID,
		&state.LeafUUID, &state.LastSourceUUID, &state.LastSourceOffset,
		&state.LastTurnIndex, &state.LastItemIndex,
		&state.ImportedAt, &state.RefreshedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadImportState{}, false, nil
	}
	if err != nil {
		return ThreadImportState{}, false, fmt.Errorf("store: get thread import state %s: %w", threadID, err)
	}
	return state, true, nil
}

// HasItemsAfterCursor reports whether threadID holds any timeline row ordered
// AFTER the (turnIndex, itemIndex) position an import recorded.
//
// This is the refresh divergence guard. `items.item_index` restarts at 0 in
// every turn, so "is there anything past the import" can only be asked of the
// PAIR — the same lexicographic (turn_index, item_index) ordering every
// timeline read sorts by. A true answer means the thread was resumed inside AO
// after it was imported, and appending the source's tail would interleave
// duplicate history under indices the live session already claimed.
//
// The predicate is written as the two-branch comparison rather than a tuple
// compare so idx_items_thread(thread_id, turn_index, item_index) serves it as
// a range scan.
func (s *Store) HasItemsAfterCursor(threadID string, turnIndex, itemIndex int) (bool, error) {
	if threadID == "" {
		return false, fmt.Errorf("store: has items after cursor: thread id is required")
	}
	var found int
	err := s.reader().QueryRow(
		`SELECT EXISTS(
		     SELECT 1 FROM items
		      WHERE thread_id = ?
		        AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		 )`,
		threadID, turnIndex, turnIndex, itemIndex,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: has items after cursor for %s: %w", threadID, err)
	}
	return found != 0, nil
}

// ListImportedSessionRefs maps every provider session id AO already knows to
// the thread that claims it. It is the dedup set an import scan subtracts
// from its candidates, which is what makes "Import All" safe to press twice.
//
// The union covers all three ways a thread can point at a session file:
// session_ref (a live or previously-resumed session), pending_fork_session_ref
// (a fork whose session exists on disk but has not been resumed yet — the
// case a session_ref-only check misses), and an earlier import's recorded
// source. When several threads claim one ref the lowest thread id wins; the
// caller only asks whether the ref is taken, and a stable answer keeps the
// scan reproducible.
func (s *Store) ListImportedSessionRefs() (map[string]string, error) {
	rows, err := s.reader().Query(
		`SELECT ref, thread_id FROM (
		     SELECT session_ref AS ref, id AS thread_id FROM threads
		      WHERE COALESCE(session_ref, '') <> ''
		     UNION ALL
		     SELECT pending_fork_session_ref, id FROM threads
		      WHERE COALESCE(pending_fork_session_ref, '') <> ''
		     UNION ALL
		     SELECT source_session_id, thread_id FROM thread_import_state
		      WHERE source_session_id <> ''
		 )
		 ORDER BY ref, thread_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list imported session refs: %w", err)
	}
	defer rows.Close()

	refs := make(map[string]string)
	for rows.Next() {
		var ref, threadID string
		if err := rows.Scan(&ref, &threadID); err != nil {
			return nil, fmt.Errorf("store: scan imported session ref: %w", err)
		}
		if _, seen := refs[ref]; !seen {
			refs[ref] = threadID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: imported session ref rows: %w", err)
	}
	return refs, nil
}
