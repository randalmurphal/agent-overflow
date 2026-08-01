package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"errors"
	"fmt"
	"io"
)

// maxEditFileSnapshotBytes bounds a decompressed snapshot read. Writes
// are capped well below this by the persist tap (highlight prime-size
// limits), so hitting it means the row is corrupt — fail loudly rather
// than hand a truncated file to verification.
const maxEditFileSnapshotBytes = 32 << 20

// PutEditFileSnapshot stores the gzip-compressed new-side content of
// path as it stood right after the edit payload's diff applied. The
// payload-existence guard runs in the same statement, so a payload
// deleted under the async persist worker surfaces as wrapped
// sql.ErrNoRows (a benign drop for the caller) instead of a foreign-key
// violation. Re-persisting the same payload overwrites the row.
func (s *Store) PutEditFileSnapshot(payloadID, path, content string, createdAt int64) error {
	blob, err := gzipSnapshotContent(content)
	if err != nil {
		return fmt.Errorf("store: compress edit file snapshot %s %s: %w", payloadID, path, err)
	}
	result, err := s.db.Exec(
		`INSERT INTO edit_file_snapshots (payload_id, path, content, created_at)
		 SELECT ?, ?, ?, ?
		  WHERE EXISTS (SELECT 1 FROM payloads WHERE id = ?)
		 ON CONFLICT(payload_id, path) DO UPDATE
		    SET content = excluded.content, created_at = excluded.created_at`,
		payloadID, path, blob, createdAt, payloadID,
	)
	if err != nil {
		return fmt.Errorf("store: put edit file snapshot %s %s: %w", payloadID, path, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: put edit file snapshot %s %s", payloadID, path))
}

// GetEditFileSnapshot returns the decompressed snapshot content for one
// edit payload and path, scoped to the thread the payload's item
// belongs to (a payload id from another thread reads as a miss — same
// containment the turn variant gets from its items join). found=false
// means no snapshot was captured (pre-feature history or a size-capped
// write) — the caller falls back to workspace verification.
func (s *Store) GetEditFileSnapshot(threadID, payloadID, path string) (string, bool, error) {
	var blob []byte
	err := s.reader().QueryRow(
		`SELECT s.content
		   FROM edit_file_snapshots s
		   JOIN items i ON i.payload_id = s.payload_id
		  WHERE i.thread_id = ? AND s.payload_id = ? AND s.path = ?`,
		threadID, payloadID, path,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get edit file snapshot %s %s: %w", payloadID, path, err)
	}
	content, err := gunzipSnapshotContent(blob)
	if err != nil {
		return "", false, fmt.Errorf("store: decompress edit file snapshot %s %s: %w", payloadID, path, err)
	}
	return content, true, nil
}

// GetLatestTurnEditFileSnapshot returns the decompressed snapshot of
// path from the LAST edit payload of a turn that touched it — the same
// item order ListTurnEditDiffPatches concatenates in, so the snapshot
// matches the final merged section the whole-turn Edits view renders.
func (s *Store) GetLatestTurnEditFileSnapshot(threadID string, turnIndex int, path string) (string, bool, error) {
	var blob []byte
	err := s.reader().QueryRow(
		`SELECT s.content
		   FROM edit_file_snapshots s
		   JOIN items i ON i.payload_id = s.payload_id
		  WHERE i.thread_id = ? AND i.turn_index = ? AND s.path = ?
		  ORDER BY i.item_index DESC
		  LIMIT 1`,
		threadID, turnIndex, path,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get latest turn edit file snapshot %s/%d %s: %w", threadID, turnIndex, path, err)
	}
	content, err := gunzipSnapshotContent(blob)
	if err != nil {
		return "", false, fmt.Errorf("store: decompress latest turn edit file snapshot %s/%d %s: %w", threadID, turnIndex, path, err)
	}
	return content, true, nil
}

func gzipSnapshotContent(content string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, content); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipSnapshotContent(blob []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return "", err
	}
	defer zr.Close()
	content, err := io.ReadAll(io.LimitReader(zr, maxEditFileSnapshotBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxEditFileSnapshotBytes {
		return "", fmt.Errorf("snapshot exceeds %d byte cap", maxEditFileSnapshotBytes)
	}
	return string(content), nil
}
