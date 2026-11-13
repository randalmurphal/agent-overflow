package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxEditFileSnapshotBytes is the capture limit of an edit file snapshot:
// PutEditFileSnapshot refuses longer content, and a read of a longer row
// fails as corrupt rather than hand a truncated file to its caller.
const MaxEditFileSnapshotBytes = 4 << 20

// PutEditFileSnapshot stores the gzip-compressed new-side content of
// path as it stood right after the edit payload's diff applied. The
// payload-existence guard runs in the same statement, so a payload
// deleted under the async persist worker surfaces as wrapped
// sql.ErrNoRows (a benign drop for the caller) instead of a foreign-key
// violation. Re-persisting the same payload overwrites the row.
func (s *Store) PutEditFileSnapshot(threadID, payloadID, path, content string, createdAt int64) error {
	if len(content) > MaxEditFileSnapshotBytes {
		return fmt.Errorf("store: edit file snapshot %s %s: %d bytes exceeds the %d byte capture limit", payloadID, path, len(content), MaxEditFileSnapshotBytes)
	}
	blob, err := gzipSnapshotContent(content)
	if err != nil {
		return fmt.Errorf("store: compress edit file snapshot %s %s: %w", payloadID, path, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin put edit file snapshot %s %s: %w", payloadID, path, err)
	}
	defer tx.Rollback()
	label := fmt.Sprintf("store: put edit file snapshot %s %s", payloadID, path)
	// A snapshot is a cache of the edit's content, so it is written where
	// the payload lives, as UpdatePayloadSpans does: every fork that reads
	// the payload reads the snapshot.
	holder, err := payloadHolderTx(tx, threadID, payloadID)
	if err != nil {
		return err
	}
	if err := ensureLocalPayloadTx(tx, holder, payloadID, label); err != nil {
		return err
	}
	threadID = holder
	result, err := tx.Exec(
		`INSERT INTO edit_file_snapshots (thread_id, payload_id, path, content, created_at)
		 SELECT ?, ?, ?, ?, ?
		  WHERE EXISTS (SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?)
		 ON CONFLICT(thread_id, payload_id, path) DO UPDATE
		    SET content = excluded.content, created_at = excluded.created_at`,
		threadID, payloadID, path, blob, createdAt, threadID, payloadID,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := requireRowsAffected(result, label); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit put edit file snapshot %s %s: %w", payloadID, path, err)
	}
	return nil
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
		`SELECT content
		   FROM timeline_edit_file_snapshots
		  WHERE thread_id = ? AND payload_id = ? AND path = ?`,
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

// GetEditFileSnapshotBlob returns one edit file snapshot row as stored,
// compressed, for a caller that decompresses it into a buffer of its own
// (DecompressEditFileSnapshot). Same containment as GetEditFileSnapshot.
func (s *Store) GetEditFileSnapshotBlob(threadID, payloadID, path string) ([]byte, bool, error) {
	var blob []byte
	err := s.reader().QueryRow(
		`SELECT content
		   FROM timeline_edit_file_snapshots
		  WHERE thread_id = ? AND payload_id = ? AND path = ?`,
		threadID, payloadID, path,
	).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read edit file snapshot %s %s: %w", payloadID, path, err)
	}
	return blob, true, nil
}

// DecompressEditFileSnapshot decompresses a snapshot row: it reads the
// stored size first (the gzip trailer), refuses one past
// MaxEditFileSnapshotBytes, asks take for a buffer of exactly that size and
// decompresses into it, failing on content of any other length. take is
// not called for empty content.
func DecompressEditFileSnapshot(blob []byte, take func(n int64) ([]byte, error)) (int64, error) {
	size, err := readSnapshotInto(blob, take)
	if err != nil {
		return 0, fmt.Errorf("store: decompress edit file snapshot: %w", err)
	}
	return size, nil
}

// readSnapshotInto decompresses a snapshot blob into the buffer take
// answers for its recorded size (gzip's ISIZE trailer, the length mod 2^32,
// exact below the capture limit).
func readSnapshotInto(blob []byte, take func(n int64) ([]byte, error)) (int64, error) {
	if len(blob) < 18 {
		return 0, errors.New("snapshot is not gzip")
	}
	size := int64(binary.LittleEndian.Uint32(blob[len(blob)-4:]))
	if size > MaxEditFileSnapshotBytes {
		return 0, fmt.Errorf("snapshot exceeds %d byte cap", MaxEditFileSnapshotBytes)
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return 0, err
	}
	// Closing a gzip.Reader releases nothing the read below has not checked.
	defer func() { _ = zr.Close() }()
	var buf []byte
	if size > 0 {
		if buf, err = take(size); err != nil {
			return 0, err
		}
	}
	if _, err := io.ReadFull(zr, buf); err != nil {
		return 0, fmt.Errorf("snapshot is shorter than its recorded %d bytes: %w", size, err)
	}
	var probe [1]byte
	if n, err := zr.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("snapshot is not its recorded %d bytes", size)
	}
	return size, nil
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
	content, err := io.ReadAll(io.LimitReader(zr, MaxEditFileSnapshotBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > MaxEditFileSnapshotBytes {
		return "", fmt.Errorf("snapshot exceeds %d byte cap", MaxEditFileSnapshotBytes)
	}
	return string(content), nil
}
