package store

import (
	"database/sql"
	"errors"
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

func (s *Store) PendingSpawnRevisions(device, after string, limit int) (_ []SpawnRevision, err error) {
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
	defer closeAgentWakeRows(rows, "read pending spawn revisions", &err)
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

// ApplySpawnRevision records the end separately from the historical target.
// A returned caller needs its Agents rows refreshed.
func (s *Store) ApplySpawnRevision(computer string, change SpawnRevision) (caller string, err error) {
	if computer == "" || change.Token == "" {
		return "", fmt.Errorf("store: a spawn revision requires a computer and token")
	}
	const action = "store: apply remote spawn revision"
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	err = tx.QueryRow(`UPDATE thread_requests SET link_end = ?3,
	  target_thread_title = CASE WHEN ?4 <> '' THEN ?4 ELSE target_thread_title END,
	  nested_open = CASE WHEN ?3 IN ('gone','detached') THEN 0 ELSE nested_open END
	  WHERE token = ?1 AND target_computer_id = ?2 AND kind = 'spawn'
	    AND (link_end <> ?3 OR (?4 <> '' AND target_thread_title <> ?4))
	  RETURNING caller_thread_id`, change.Token, computer, change.End, change.Title).Scan(&caller)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if change.End != "" {
		if _, err = tx.Exec(`DELETE FROM thread_owner_ops WHERE token = ? AND computer_id = ?`, change.Token, computer); err != nil {
			return "", fmt.Errorf("%s: %w", action, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if caller != "" {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: change.Token, CallerThreadID: caller})
	}
	return caller, nil
}
