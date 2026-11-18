package store

import (
	"fmt"
	"strconv"
)

type SpawnRevisionAck struct {
	Token    string `json:"token"`
	Revision int64  `json:"revision"`
}

// SpawnRevision names the latest change to a spawn, together with the
// current facts read in the same database snapshot.
type SpawnRevision struct {
	SpawnRevisionAck
	End            string `json:"end,omitempty"`
	TargetThreadID string `json:"targetThreadId,omitempty"`
	Placement      string `json:"placement,omitempty"`
	Title          string `json:"title,omitempty"`
	Archived       bool   `json:"archived,omitempty"`
}

func (s *Store) PendingSpawnRevisions(device, after string, limit int) ([]SpawnRevision, error) {
	if device == "" || limit < 1 {
		return nil, fmt.Errorf("store: listing spawn revisions requires a device and a positive limit")
	}
	rows, err := s.reader().Query(`SELECT c.token, c.revision, c.end, COALESCE(t.id,''), COALESCE(t.placement,''), COALESCE(t.title,''), COALESCE(t.archived,0)
	  FROM spawn_revisions c LEFT JOIN thread_request_receipts r ON r.token = c.token AND r.owner_device_id = c.owner_device_id
	  LEFT JOIN threads t ON t.id = r.target_thread_id AND t.deleting = 0
	  WHERE c.owner_device_id = ? AND c.token > ? ORDER BY c.token LIMIT `+strconv.Itoa(limit), device, after)
	if err != nil {
		return nil, fmt.Errorf("store: read pending spawn revisions: %w", err)
	}
	defer rows.Close()
	var revisions []SpawnRevision
	for rows.Next() {
		var change SpawnRevision
		if err := rows.Scan(&change.Token, &change.Revision, &change.End, &change.TargetThreadID, &change.Placement, &change.Title, &change.Archived); err != nil {
			return nil, fmt.Errorf("store: read pending spawn revision: %w", err)
		}
		revisions = append(revisions, change)
	}
	return revisions, rows.Err()
}

func (s *Store) AcknowledgeSpawnRevisions(device string, acks []SpawnRevisionAck) (err error) {
	if len(acks) == 0 {
		return nil
	}
	action := "store: acknowledge spawn revisions"
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	for _, ack := range acks {
		if _, err := tx.Exec(`DELETE FROM spawn_revisions WHERE owner_device_id = ? AND token = ? AND revision = ?`, device, ack.Token, ack.Revision); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	return tx.Commit()
}
