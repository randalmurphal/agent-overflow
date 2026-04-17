package store

import (
	"database/sql"
	"fmt"
)

// Checkpoint is the persisted bookkeeping row for a single per-turn snapshot.
// The heavy data lives in Git (keyed by RefName); this table lets us look up
// the ref for a given (thread, turn) and clean everything up on thread delete.
type Checkpoint struct {
	ID            string `json:"id"`
	ThreadID      string `json:"threadId"`
	TurnIndex     int    `json:"turnIndex"`
	RefName       string `json:"refName"`
	BaselineSHA   string `json:"baselineSha,omitempty"`
	CapturedAt    int64  `json:"capturedAt"`
	WorkspacePath string `json:"workspacePath"`
}

const checkpointColumns = `id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path`

func scanCheckpoint(scanner interface{ Scan(...any) error }) (Checkpoint, error) {
	var c Checkpoint
	if err := scanner.Scan(
		&c.ID, &c.ThreadID, &c.TurnIndex, &c.RefName, &c.BaselineSHA,
		&c.CapturedAt, &c.WorkspacePath,
	); err != nil {
		return Checkpoint{}, err
	}
	return c, nil
}

// SaveCheckpoint inserts a new checkpoint row. The ref must already exist in
// Git when this is called — we don't validate that here; the caller owns the
// ordering.
func (s *Store) SaveCheckpoint(c Checkpoint) error {
	if c.ID == "" {
		return fmt.Errorf("store: save checkpoint: id is required")
	}
	if c.ThreadID == "" {
		return fmt.Errorf("store: save checkpoint: thread id is required")
	}
	if c.RefName == "" {
		return fmt.Errorf("store: save checkpoint: ref name is required")
	}
	_, err := s.db.Exec(
		`INSERT INTO thread_checkpoints (id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ThreadID, c.TurnIndex, c.RefName, c.BaselineSHA, c.CapturedAt, c.WorkspacePath,
	)
	if err != nil {
		return fmt.Errorf("store: insert checkpoint %s: %w", c.ID, err)
	}
	return nil
}

// GetCheckpoint looks up the checkpoint for (thread, turn). The second return
// is false when no row exists, and err is nil in that case. The schema
// enforces at most one row per (thread_id, turn_index) via a UNIQUE
// constraint (migration v8), so this is an exact lookup with no tie-breaker.
func (s *Store) GetCheckpoint(threadID string, turnIndex int) (Checkpoint, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	)
	c, err := scanCheckpoint(row)
	if err == sql.ErrNoRows {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("store: get checkpoint thread=%s turn=%d: %w",
			threadID, turnIndex, err)
	}
	return c, true, nil
}

// ListCheckpoints returns every checkpoint row for the thread, ordered by
// turn_index ascending.
func (s *Store) ListCheckpoints(threadID string) ([]Checkpoint, error) {
	rows, err := s.db.Query(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ?
		 ORDER BY turn_index ASC, captured_at ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints for %s: %w", threadID, err)
	}
	defer rows.Close()

	var out []Checkpoint
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan checkpoint: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate checkpoints for %s: %w", threadID, err)
	}
	return out, nil
}

// DeleteCheckpointsForThread removes every checkpoint row for the thread.
// The FK CASCADE would handle this when the thread itself is deleted, but
// callers use this directly when cleaning up checkpoints for a thread that
// still exists (e.g. manual "clear all checkpoints" actions).
func (s *Store) DeleteCheckpointsForThread(threadID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ?`, threadID,
	); err != nil {
		return fmt.Errorf("store: delete checkpoints for %s: %w", threadID, err)
	}
	return nil
}

// DeleteCheckpointsAfterTurn removes every checkpoint row with turn_index
// strictly greater than keepThroughTurn for the given thread. Returns the
// list of ref_name values that were deleted so the caller can clean up the
// backing git refs. Order is ascending by turn_index.
//
// Used by the revert flow: after reverting to turn N, forward checkpoints
// (turn N+1 and beyond) no longer correspond to a reachable state and are
// torn down so future captures can re-use those turn indices cleanly.
func (s *Store) DeleteCheckpointsAfterTurn(threadID string, keepThroughTurn int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT ref_name FROM thread_checkpoints
		 WHERE thread_id = ? AND turn_index > ?
		 ORDER BY turn_index`,
		threadID, keepThroughTurn,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints after turn for thread %s: %w", threadID, err)
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan checkpoint ref after turn: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate checkpoints after turn: %w", err)
	}
	rows.Close()

	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND turn_index > ?`,
		threadID, keepThroughTurn,
	); err != nil {
		return nil, fmt.Errorf("store: delete checkpoints after turn for thread %s: %w", threadID, err)
	}
	return refs, nil
}

// DeleteCheckpointByThreadTurn removes the checkpoint row for (thread, turn)
// if it exists, returning the ref_name that was deleted so the caller can
// clean up the backing git ref. The bool is false when no row existed — not
// an error. Used to make capture idempotent: before a new capture writes a
// fresh git ref, any stale DB+ref pair for the same (thread, turn) is torn
// down so neither side leaks.
func (s *Store) DeleteCheckpointByThreadTurn(threadID string, turnIndex int) (string, bool, error) {
	var ref string
	err := s.db.QueryRow(
		`SELECT ref_name FROM thread_checkpoints WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&ref)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: lookup checkpoint thread=%s turn=%d: %w",
			threadID, turnIndex, err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	); err != nil {
		return "", false, fmt.Errorf("store: delete checkpoint thread=%s turn=%d: %w",
			threadID, turnIndex, err)
	}
	return ref, true, nil
}
