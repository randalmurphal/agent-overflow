package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"agent-overflow/internal/diffsummary"
)

// Checkpoint is the persisted bookkeeping row for a single per-turn snapshot.
// The heavy data lives in Git (keyed by RefName); this table lets us look up
// the ref for a given (thread, turn) and clean everything up on thread delete.
type Checkpoint struct {
	ID                  string             `json:"id"`
	ThreadID            string             `json:"threadId"`
	TurnIndex           int                `json:"turnIndex"` // legacy alias for CheckpointTurnCount
	CheckpointTurnCount int                `json:"checkpointTurnCount"`
	TurnID              string             `json:"turnId,omitempty"`
	RefName             string             `json:"refName"`
	BaselineSHA         string             `json:"baselineSha,omitempty"`
	Status              string             `json:"status"`
	Files               []diffsummary.File `json:"files"`
	// ToolPaths records the workspace-relative paths the agent's
	// file-mutating tools wrote during the turn this checkpoint closes.
	// Bash side effects are intentionally NOT tracked. An empty slice
	// means the agent did no file-mutating tool calls during the turn
	// (or the row predates v32) — a conversation-and-files revert
	// targeting such a row is a no-op on the worktree.
	ToolPaths          []string `json:"toolPaths"`
	AssistantMessageID string   `json:"assistantMessageId,omitempty"`
	CompletedAt        int64    `json:"completedAt,omitempty"`
	CapturedAt         int64    `json:"capturedAt"`
	WorkspacePath      string   `json:"workspacePath"`
}

// CheckpointRef identifies the Git ref backing a checkpoint row and the
// workspace repo that owns it.
type CheckpointRef struct {
	RefName       string
	WorkspacePath string
}

const checkpointColumns = `id, thread_id, turn_index, checkpoint_turn_count, turn_id, ref_name, baseline_sha, status, files, tool_paths, assistant_message_id, completed_at, captured_at, workspace_path`

func scanCheckpoint(scanner interface{ Scan(...any) error }) (Checkpoint, error) {
	var c Checkpoint
	var filesJSON, toolPathsJSON string
	if err := scanner.Scan(
		&c.ID, &c.ThreadID, &c.TurnIndex, &c.CheckpointTurnCount, &c.TurnID, &c.RefName, &c.BaselineSHA,
		&c.Status, &filesJSON, &toolPathsJSON, &c.AssistantMessageID, &c.CompletedAt,
		&c.CapturedAt, &c.WorkspacePath,
	); err != nil {
		return Checkpoint{}, err
	}
	if c.CheckpointTurnCount == 0 && c.TurnIndex != 0 {
		c.CheckpointTurnCount = c.TurnIndex
	}
	if c.TurnIndex == 0 && c.CheckpointTurnCount != 0 {
		c.TurnIndex = c.CheckpointTurnCount
	}
	if c.Status == "" {
		c.Status = "ready"
	}
	if filesJSON != "" {
		if err := json.Unmarshal([]byte(filesJSON), &c.Files); err != nil {
			return Checkpoint{}, fmt.Errorf("store: unmarshal checkpoint files for %s: %w", c.ID, err)
		}
	}
	if toolPathsJSON != "" {
		if err := json.Unmarshal([]byte(toolPathsJSON), &c.ToolPaths); err != nil {
			return Checkpoint{}, fmt.Errorf("store: unmarshal checkpoint tool_paths for %s: %w", c.ID, err)
		}
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
	c = normalizeCheckpoint(c)
	filesJSON, err := json.Marshal(c.Files)
	if err != nil {
		return fmt.Errorf("store: marshal checkpoint files: %w", err)
	}
	toolPathsJSON, err := json.Marshal(c.ToolPaths)
	if err != nil {
		return fmt.Errorf("store: marshal checkpoint tool_paths: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO thread_checkpoints (
			id, thread_id, turn_index, checkpoint_turn_count, turn_id, ref_name,
			baseline_sha, status, files, tool_paths, assistant_message_id, completed_at,
			captured_at, workspace_path
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ThreadID, c.TurnIndex, c.CheckpointTurnCount, c.TurnID, c.RefName,
		c.BaselineSHA, c.Status, string(filesJSON), string(toolPathsJSON),
		c.AssistantMessageID, c.CompletedAt, c.CapturedAt, c.WorkspacePath,
	)
	if err != nil {
		return fmt.Errorf("store: insert checkpoint %s: %w", c.ID, err)
	}
	return nil
}

// ReplaceCheckpointByTurnCount atomically swaps the checkpoint row for
// (thread, checkpoint_turn_count). It returns the ref that was replaced so the
// caller can clean up the old git ref after the DB commit succeeds.
func (s *Store) ReplaceCheckpointByTurnCount(c Checkpoint) (CheckpointRef, error) {
	if c.ID == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: id is required")
	}
	if c.ThreadID == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: thread id is required")
	}
	if c.RefName == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: ref name is required")
	}
	c = normalizeCheckpoint(c)
	filesJSON, err := json.Marshal(c.Files)
	if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: marshal replacement checkpoint files: %w", err)
	}
	toolPathsJSON, err := json.Marshal(c.ToolPaths)
	if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: marshal replacement checkpoint tool_paths: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: begin replace checkpoint tx: %w", err)
	}
	defer tx.Rollback()

	var oldRef CheckpointRef
	err = tx.QueryRow(
		`SELECT ref_name, workspace_path FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		c.ThreadID, c.CheckpointTurnCount,
	).Scan(&oldRef.RefName, &oldRef.WorkspacePath)
	if err == sql.ErrNoRows {
		oldRef = CheckpointRef{}
	} else if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: lookup replacement checkpoint thread=%s turn=%d: %w",
			c.ThreadID, c.CheckpointTurnCount, err)
	}

	if _, err := tx.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		c.ThreadID, c.CheckpointTurnCount,
	); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: delete replaced checkpoint thread=%s turn=%d: %w",
			c.ThreadID, c.CheckpointTurnCount, err)
	}

	if _, err := tx.Exec(
		`INSERT INTO thread_checkpoints (
			id, thread_id, turn_index, checkpoint_turn_count, turn_id, ref_name,
			baseline_sha, status, files, tool_paths, assistant_message_id, completed_at,
			captured_at, workspace_path
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ThreadID, c.TurnIndex, c.CheckpointTurnCount, c.TurnID, c.RefName,
		c.BaselineSHA, c.Status, string(filesJSON), string(toolPathsJSON),
		c.AssistantMessageID, c.CompletedAt, c.CapturedAt, c.WorkspacePath,
	); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: insert replacement checkpoint %s: %w", c.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: commit replace checkpoint tx: %w", err)
	}
	return oldRef, nil
}

func normalizeCheckpoint(c Checkpoint) Checkpoint {
	if c.CheckpointTurnCount == 0 && c.TurnIndex != 0 {
		c.CheckpointTurnCount = c.TurnIndex
	}
	c.TurnIndex = c.CheckpointTurnCount
	if c.Status == "" {
		c.Status = "ready"
	}
	if c.Files == nil {
		c.Files = []diffsummary.File{}
	}
	if c.ToolPaths == nil {
		c.ToolPaths = []string{}
	}
	return c
}

// GetCheckpointByTurnCount looks up the checkpoint for (thread, turn count).
// The second return is false when no row exists, and err is nil in that case.
// The schema enforces at most one row per (thread_id, checkpoint_turn_count),
// so this is an exact lookup with no tie-breaker.
func (s *Store) GetCheckpointByTurnCount(threadID string, turnCount int) (Checkpoint, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		threadID, turnCount,
	)
	c, err := scanCheckpoint(row)
	if err == sql.ErrNoRows {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("store: get checkpoint thread=%s turn=%d: %w",
			threadID, turnCount, err)
	}
	return c, true, nil
}

// ListCheckpoints returns every checkpoint row for the thread, ordered by
// checkpoint turn count ascending.
func (s *Store) ListCheckpoints(threadID string) ([]Checkpoint, error) {
	rows, err := s.db.Query(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ?
		 ORDER BY checkpoint_turn_count ASC, captured_at ASC`,
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

// DeleteCheckpointsAfterTurn removes every checkpoint row with checkpoint turn
// count strictly greater than keepThroughTurn for the given thread. Returns
// the refs that were deleted so the caller can clean up the backing git refs.
// Order is ascending by checkpoint turn count.
//
// Used by the revert flow: after reverting to checkpoint count N, forward
// checkpoints no longer correspond to reachable conversation state and are
// torn down so future captures can re-use those counts cleanly.
func (s *Store) DeleteCheckpointsAfterTurn(threadID string, keepThroughTurn int) ([]CheckpointRef, error) {
	rows, err := s.db.Query(
		`SELECT ref_name, workspace_path FROM thread_checkpoints
		 WHERE thread_id = ? AND checkpoint_turn_count > ?
		 ORDER BY checkpoint_turn_count`,
		threadID, keepThroughTurn,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints after turn for thread %s: %w", threadID, err)
	}
	var refs []CheckpointRef
	for rows.Next() {
		var ref CheckpointRef
		if err := rows.Scan(&ref.RefName, &ref.WorkspacePath); err != nil {
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
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count > ?`,
		threadID, keepThroughTurn,
	); err != nil {
		return nil, fmt.Errorf("store: delete checkpoints after turn for thread %s: %w", threadID, err)
	}
	return refs, nil
}

// GetCumulativeToolPaths returns the deduped union of `tool_paths` across
// every checkpoint row for the thread with checkpoint_turn_count strictly
// greater than fromTurnCountExclusive. Used by RevertToCheckpoint to find
// every workspace path the agent touched after the target checkpoint, so
// the path-scoped restore can roll exactly those paths back. Order is
// stable (sorted ascending) for deterministic git invocations.
func (s *Store) GetCumulativeToolPaths(threadID string, fromTurnCountExclusive int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT tool_paths FROM thread_checkpoints
		 WHERE thread_id = ? AND checkpoint_turn_count > ?
		 ORDER BY checkpoint_turn_count`,
		threadID, fromTurnCountExclusive,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list cumulative tool paths thread=%s after=%d: %w",
			threadID, fromTurnCountExclusive, err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("store: scan cumulative tool paths: %w", err)
		}
		if raw == "" {
			continue
		}
		var paths []string
		if err := json.Unmarshal([]byte(raw), &paths); err != nil {
			return nil, fmt.Errorf("store: unmarshal cumulative tool paths thread=%s: %w", threadID, err)
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate cumulative tool paths: %w", err)
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// DeleteCheckpointByThreadTurnCount removes the checkpoint row for
// (thread, checkpoint turn count) if it exists, returning the backing ref so
// the caller can clean it up. The bool is false when no row existed.
func (s *Store) DeleteCheckpointByThreadTurnCount(threadID string, turnCount int) (CheckpointRef, bool, error) {
	var ref CheckpointRef
	err := s.db.QueryRow(
		`SELECT ref_name, workspace_path FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		threadID, turnCount,
	).Scan(&ref.RefName, &ref.WorkspacePath)
	if err == sql.ErrNoRows {
		return CheckpointRef{}, false, nil
	}
	if err != nil {
		return CheckpointRef{}, false, fmt.Errorf("store: lookup checkpoint thread=%s turn=%d: %w",
			threadID, turnCount, err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		threadID, turnCount,
	); err != nil {
		return CheckpointRef{}, false, fmt.Errorf("store: delete checkpoint thread=%s turn=%d: %w",
			threadID, turnCount, err)
	}
	return ref, true, nil
}
