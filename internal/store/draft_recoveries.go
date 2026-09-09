package store

import (
	"context"
	"database/sql"
	"fmt"
)

const threadDraftRecoveriesV94SQL = `CREATE TABLE thread_draft_recoveries (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 send_id TEXT NOT NULL,
 content TEXT NOT NULL,
 attachments TEXT NOT NULL
);`

// ThreadDraftRecovery protects a replacement until it has a persisted user row
// or has been atomically returned to the editable draft. It is never dispatched at boot.
type ThreadDraftRecovery struct {
	ThreadID    string
	SendID      string
	Content     string
	Attachments string
}

func (s *Store) StageThreadDraftRecovery(r ThreadDraftRecovery) error {
	if r.ThreadID == "" || r.SendID == "" {
		return fmt.Errorf("draft recovery: thread and send IDs required")
	}
	_, err := s.db.Exec(`INSERT INTO thread_draft_recoveries(thread_id, send_id, content, attachments) VALUES (?, ?, ?, ?)`, r.ThreadID, r.SendID, r.Content, r.Attachments)
	return err
}

func (s *Store) ListThreadDraftRecoveries() ([]ThreadDraftRecovery, error) {
	rows, err := s.reader().Query(`SELECT thread_id, send_id, content, attachments FROM thread_draft_recoveries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadDraftRecovery
	for rows.Next() {
		var r ThreadDraftRecovery
		if err := rows.Scan(&r.ThreadID, &r.SendID, &r.Content, &r.Attachments); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteThreadDraftRecovery(threadID, sendID string) error {
	_, err := s.db.Exec(`DELETE FROM thread_draft_recoveries WHERE thread_id = ? AND send_id = ?`, threadID, sendID)
	return err
}

// CommitThreadDraftRecovery compares the current draft and moves recovery into it
// in one transaction. A racing edit returns false so the caller can merge again.
func (s *Store) CommitThreadDraftRecovery(r ThreadDraftRecovery, expected, merged ThreadDraft) (committed bool, changed bool, err error) {
	if r.ThreadID == "" || r.SendID == "" || expected.ThreadID != r.ThreadID || merged.ThreadID != r.ThreadID {
		return false, false, fmt.Errorf("draft recovery: matching thread and send IDs required")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	current, _, err := getThreadDraft(tx, r.ThreadID)
	if err != nil {
		return false, false, err
	}
	if current != expected {
		return false, false, nil
	}
	var id string
	err = tx.QueryRow(`SELECT send_id FROM thread_draft_recoveries WHERE thread_id = ?`, r.ThreadID).Scan(&id)
	if err == sql.ErrNoRows {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if id != r.SendID {
		return false, false, fmt.Errorf("draft recovery changed")
	}
	changed, err = upsertThreadDraft(tx, merged)
	if err != nil {
		return false, false, err
	}
	if _, err := tx.Exec(`DELETE FROM thread_draft_recoveries WHERE thread_id = ? AND send_id = ?`, r.ThreadID, r.SendID); err != nil {
		return false, false, err
	}
	return true, changed, tx.Commit()
}
