package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"agent-overflow/internal/diffsummary"
)

// Checkpoint is the persisted row for the workspace snapshot captured
// immediately before a real user message is submitted. Reverting "to" a user
// message means restoring this snapshot and deleting that message plus all
// later conversation rows.
type Checkpoint struct {
	ID                    string             `json:"id"`
	ThreadID              string             `json:"threadId"`
	UserItemID            string             `json:"userItemId"`
	TurnIndex             int                `json:"turnIndex"`
	ProviderUserMessageID string             `json:"providerUserMessageId,omitempty"`
	ProviderParentUUID    string             `json:"providerParentUuid,omitempty"`
	RefName               string             `json:"refName"`
	BaselineSHA           string             `json:"baselineSha,omitempty"`
	Status                string             `json:"status"`
	Files                 []diffsummary.File `json:"files"`
	CapturedAt            int64              `json:"capturedAt"`
	WorkspacePath         string             `json:"workspacePath"`
}

// CheckpointRef identifies the Git ref backing a checkpoint row and the
// workspace repo that owns it.
type CheckpointRef struct {
	RefName       string
	WorkspacePath string
}

const checkpointColumns = `id, thread_id, user_item_id, turn_index,
    provider_user_message_id, provider_parent_uuid, ref_name, baseline_sha,
    status, files, captured_at, workspace_path`

func scanCheckpoint(scanner interface{ Scan(...any) error }) (Checkpoint, error) {
	var c Checkpoint
	var filesJSON string
	if err := scanner.Scan(
		&c.ID, &c.ThreadID, &c.UserItemID, &c.TurnIndex,
		&c.ProviderUserMessageID, &c.ProviderParentUUID, &c.RefName, &c.BaselineSHA,
		&c.Status, &filesJSON, &c.CapturedAt, &c.WorkspacePath,
	); err != nil {
		return Checkpoint{}, err
	}
	if c.Status == "" {
		c.Status = "ready"
	}
	if filesJSON != "" {
		if err := json.Unmarshal([]byte(filesJSON), &c.Files); err != nil {
			return Checkpoint{}, fmt.Errorf("store: unmarshal checkpoint files for %s: %w", c.ID, err)
		}
	}
	if c.Files == nil {
		c.Files = []diffsummary.File{}
	}
	return c, nil
}

func (s *Store) SaveCheckpoint(c Checkpoint) error {
	if c.ID == "" {
		return fmt.Errorf("store: save checkpoint: id is required")
	}
	if c.ThreadID == "" {
		return fmt.Errorf("store: save checkpoint: thread id is required")
	}
	if c.UserItemID == "" {
		return fmt.Errorf("store: save checkpoint: user item id is required")
	}
	if c.RefName == "" {
		return fmt.Errorf("store: save checkpoint: ref name is required")
	}
	c = normalizeCheckpoint(c)
	filesJSON, err := json.Marshal(c.Files)
	if err != nil {
		return fmt.Errorf("store: marshal checkpoint files: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO thread_checkpoints (
			id, thread_id, user_item_id, turn_index,
			provider_user_message_id, provider_parent_uuid, ref_name, baseline_sha,
			status, files, captured_at, workspace_path
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ThreadID, c.UserItemID, c.TurnIndex,
		c.ProviderUserMessageID, c.ProviderParentUUID, c.RefName, c.BaselineSHA,
		c.Status, string(filesJSON), c.CapturedAt, c.WorkspacePath,
	)
	if err != nil {
		return fmt.Errorf("store: insert checkpoint %s: %w", c.ID, err)
	}
	return nil
}

// ReplaceCheckpointByUserItemID atomically swaps the checkpoint row for a user
// message. It returns the ref that was replaced so callers can remove the old
// git ref after the DB commit succeeds.
func (s *Store) ReplaceCheckpointByUserItemID(c Checkpoint) (CheckpointRef, error) {
	if c.ID == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: id is required")
	}
	if c.ThreadID == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: thread id is required")
	}
	if c.UserItemID == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: user item id is required")
	}
	if c.RefName == "" {
		return CheckpointRef{}, fmt.Errorf("store: replace checkpoint: ref name is required")
	}
	c = normalizeCheckpoint(c)
	filesJSON, err := json.Marshal(c.Files)
	if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: marshal replacement checkpoint files: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: begin replace checkpoint tx: %w", err)
	}
	defer tx.Rollback()

	var oldRef CheckpointRef
	err = tx.QueryRow(
		`SELECT ref_name, workspace_path FROM thread_checkpoints WHERE thread_id = ? AND user_item_id = ?`,
		c.ThreadID, c.UserItemID,
	).Scan(&oldRef.RefName, &oldRef.WorkspacePath)
	if err == sql.ErrNoRows {
		oldRef = CheckpointRef{}
	} else if err != nil {
		return CheckpointRef{}, fmt.Errorf("store: lookup replacement checkpoint thread=%s user_item=%s: %w",
			c.ThreadID, c.UserItemID, err)
	}

	if _, err := tx.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND user_item_id = ?`,
		c.ThreadID, c.UserItemID,
	); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: delete replaced checkpoint thread=%s user_item=%s: %w",
			c.ThreadID, c.UserItemID, err)
	}

	if _, err := tx.Exec(
		`INSERT INTO thread_checkpoints (
			id, thread_id, user_item_id, turn_index,
			provider_user_message_id, provider_parent_uuid, ref_name, baseline_sha,
			status, files, captured_at, workspace_path
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ThreadID, c.UserItemID, c.TurnIndex,
		c.ProviderUserMessageID, c.ProviderParentUUID, c.RefName, c.BaselineSHA,
		c.Status, string(filesJSON), c.CapturedAt, c.WorkspacePath,
	); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: insert replacement checkpoint %s: %w", c.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return CheckpointRef{}, fmt.Errorf("store: commit replace checkpoint tx: %w", err)
	}
	return oldRef, nil
}

func normalizeCheckpoint(c Checkpoint) Checkpoint {
	if c.Status == "" {
		c.Status = "ready"
	}
	if c.Files == nil {
		c.Files = []diffsummary.File{}
	}
	return c
}

func (s *Store) GetCheckpointByUserItemID(threadID, userItemID string) (Checkpoint, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ? AND user_item_id = ?`,
		threadID, userItemID,
	)
	c, err := scanCheckpoint(row)
	if err == sql.ErrNoRows {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("store: get checkpoint thread=%s user_item=%s: %w",
			threadID, userItemID, err)
	}
	return c, true, nil
}

func (s *Store) GetPreviousCheckpoint(threadID string, beforeTurnIndex int) (Checkpoint, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+checkpointColumns+` FROM thread_checkpoints
		 WHERE thread_id = ? AND turn_index < ?
		 ORDER BY turn_index DESC, captured_at DESC
		 LIMIT 1`,
		threadID, beforeTurnIndex,
	)
	c, err := scanCheckpoint(row)
	if err == sql.ErrNoRows {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("store: get previous checkpoint thread=%s before_turn=%d: %w",
			threadID, beforeTurnIndex, err)
	}
	return c, true, nil
}

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

func (s *Store) DeleteCheckpointsForThread(threadID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ?`, threadID,
	); err != nil {
		return fmt.Errorf("store: delete checkpoints for %s: %w", threadID, err)
	}
	return nil
}

// DeleteCheckpointsFromTurn removes every message checkpoint whose user
// message is deleted by a rewind starting at fromTurnIndex.
func (s *Store) DeleteCheckpointsFromTurn(threadID string, fromTurnIndex int) ([]CheckpointRef, error) {
	rows, err := s.db.Query(
		`SELECT ref_name, workspace_path FROM thread_checkpoints
		 WHERE thread_id = ? AND turn_index >= ?
		 ORDER BY turn_index`,
		threadID, fromTurnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints from turn for thread %s: %w", threadID, err)
	}
	var refs []CheckpointRef
	for rows.Next() {
		var ref CheckpointRef
		if err := rows.Scan(&ref.RefName, &ref.WorkspacePath); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan checkpoint ref from turn: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate checkpoints from turn: %w", err)
	}
	rows.Close()

	if _, err := s.db.Exec(
		`DELETE FROM thread_checkpoints WHERE thread_id = ? AND turn_index >= ?`,
		threadID, fromTurnIndex,
	); err != nil {
		return nil, fmt.Errorf("store: delete checkpoints from turn for thread %s: %w", threadID, err)
	}
	return refs, nil
}

func (s *Store) UpdateCheckpointProviderIDs(threadID, userItemID, providerUserMessageID, providerParentUUID string) error {
	if threadID == "" || userItemID == "" {
		return fmt.Errorf("store: update checkpoint provider ids requires thread and user item id")
	}
	if providerUserMessageID == "" && providerParentUUID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE thread_checkpoints
		    SET provider_user_message_id = CASE WHEN ? != '' THEN ? ELSE provider_user_message_id END,
		        provider_parent_uuid = CASE WHEN ? != '' THEN ? ELSE provider_parent_uuid END
		  WHERE thread_id = ? AND user_item_id = ?`,
		providerUserMessageID, providerUserMessageID,
		providerParentUUID, providerParentUUID,
		threadID, userItemID,
	)
	if err != nil {
		return fmt.Errorf("store: update checkpoint provider ids thread=%s user_item=%s: %w", threadID, userItemID, err)
	}
	return nil
}

func (s *Store) UpsertTrackedFiles(threadID string, turnIndex int, paths []string) error {
	if threadID == "" || len(paths) == 0 {
		return nil
	}
	if turnIndex < 0 {
		return fmt.Errorf("store: upsert tracked files: turn index must be >= 0, got %d", turnIndex)
	}
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		cleaned, err := cleanTrackedFilePath(p)
		if err != nil {
			return fmt.Errorf("store: upsert tracked file %s/%s: %w", threadID, p, err)
		}
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		normalized = append(normalized, cleaned)
	}
	if len(normalized) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tracked files tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO thread_tracked_files (thread_id, turn_index, path) VALUES (?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("store: prepare tracked files insert: %w", err)
	}
	defer stmt.Close()
	for _, p := range normalized {
		if _, err := stmt.Exec(threadID, turnIndex, p); err != nil {
			return fmt.Errorf("store: upsert tracked file %s/%s: %w", threadID, p, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tracked files tx: %w", err)
	}
	return nil
}

func (s *Store) ListTrackedFiles(threadID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT path FROM thread_tracked_files WHERE thread_id = ? ORDER BY path ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list tracked files for %s: %w", threadID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan tracked file: %w", err)
		}
		if p != "" {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tracked files for %s: %w", threadID, err)
	}
	return out, nil
}

func (s *Store) ListTrackedFilesFromTurn(threadID string, fromTurnIndex int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT path
		   FROM thread_tracked_files
		  WHERE thread_id = ? AND turn_index >= ?
		  ORDER BY path ASC`,
		threadID, fromTurnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list tracked files from turn for %s: %w", threadID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan tracked file from turn: %w", err)
		}
		if p != "" {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tracked files from turn for %s: %w", threadID, err)
	}
	return out, nil
}

func cleanTrackedFilePath(p string) (string, error) {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, ":") {
		return "", fmt.Errorf("pathspec magic is not allowed")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path must stay inside the workspace")
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("path contains invalid component %q", part)
		}
		if part == ".git" {
			return "", fmt.Errorf("paths under .git are not tracked")
		}
	}
	return cleaned, nil
}
