package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// InsertDesignSnapshot persists snapshot metadata. The on-disk copy of
// the working directory at dir_path is the caller's responsibility.
func (s *Store) InsertDesignSnapshot(snap DesignSnapshot) error {
	var parent any
	if snap.ParentSnapshotID != "" {
		parent = snap.ParentSnapshotID
	}
	autoVal := 0
	if snap.Auto {
		autoVal = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO design_snapshots (
			id, thread_id, label, dir_path, parent_snapshot_id, auto, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snap.ID,
		snap.ThreadID,
		snap.Label,
		snap.DirPath,
		parent,
		autoVal,
		snap.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert design snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// GetDesignSnapshot returns a single snapshot by (threadID, snapshotID).
func (s *Store) GetDesignSnapshot(threadID, snapshotID string) (DesignSnapshot, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, label, dir_path, parent_snapshot_id, auto, created_at
		 FROM design_snapshots
		 WHERE thread_id = ? AND id = ?`,
		threadID,
		snapshotID,
	)
	return scanDesignSnapshot(row, snapshotID)
}

// ListDesignSnapshots returns snapshots for a thread ordered newest first.
func (s *Store) ListDesignSnapshots(threadID string) ([]DesignSnapshot, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, label, dir_path, parent_snapshot_id, auto, created_at
		 FROM design_snapshots
		 WHERE thread_id = ?
		 ORDER BY created_at DESC, id DESC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list design snapshots for %s: %w", threadID, err)
	}
	defer rows.Close()

	var snaps []DesignSnapshot
	for rows.Next() {
		snap, err := scanDesignSnapshot(rows, "")
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate design snapshots for %s: %w", threadID, err)
	}
	return snaps, nil
}

// DeleteDesignSnapshot removes the metadata row. The on-disk dir is the
// caller's responsibility.
func (s *Store) DeleteDesignSnapshot(threadID, snapshotID string) error {
	_, err := s.db.Exec(
		`DELETE FROM design_snapshots WHERE thread_id = ? AND id = ?`,
		threadID,
		snapshotID,
	)
	if err != nil {
		return fmt.Errorf("store: delete design snapshot %s: %w", snapshotID, err)
	}
	return nil
}

// HasDesignSnapshotChildren reports whether any snapshot points at this
// one as parent. Used by GC to keep ancestors of live branches alive.
func (s *Store) HasDesignSnapshotChildren(snapshotID string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM design_snapshots WHERE parent_snapshot_id = ?`,
		snapshotID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: count children of design snapshot %s: %w", snapshotID, err)
	}
	return count > 0, nil
}

type designSnapshotScanner interface {
	Scan(dest ...any) error
}

func scanDesignSnapshot(scanner designSnapshotScanner, snapshotID string) (DesignSnapshot, error) {
	var (
		snap   DesignSnapshot
		parent sql.NullString
		auto   int64
	)
	if err := scanner.Scan(
		&snap.ID,
		&snap.ThreadID,
		&snap.Label,
		&snap.DirPath,
		&parent,
		&auto,
		&snap.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) && snapshotID != "" {
			return DesignSnapshot{}, fmt.Errorf("store: design snapshot %s not found", snapshotID)
		}
		if snapshotID == "" {
			return DesignSnapshot{}, fmt.Errorf("store: scan design snapshot: %w", err)
		}
		return DesignSnapshot{}, fmt.Errorf("store: get design snapshot %s: %w", snapshotID, err)
	}
	if parent.Valid {
		snap.ParentSnapshotID = parent.String
	}
	snap.Auto = auto != 0
	return snap, nil
}
