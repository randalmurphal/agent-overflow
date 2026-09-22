package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrNotUnsentDraft = errors.New("Only an unsent draft can change projects.")

// The transfer journal carries this private snapshot until destination
// activation. Consuming it and marking completion share the durable transaction.
func consumeTransferredDraft(tx *sql.Tx, transfer ThreadTransfer) error {
	var private struct {
		Draft *ThreadDraft `json:"draftToConsume"`
	}
	if err := json.Unmarshal(transfer.PrivateState, &private); err != nil {
		return err
	}
	if private.Draft == nil {
		return nil
	}
	if transfer.Direction != "outgoing" || transfer.Kind != "copy" || private.Draft.ThreadID != transfer.ThreadID {
		return errors.New("draft transfer source does not match its journal")
	}
	if err := checkUnsentDraft(tx, transfer.ThreadID); err != nil {
		return err
	}
	current, _, err := getThreadDraft(tx, transfer.ThreadID)
	if err != nil {
		return err
	}
	current.UpdatedAt, private.Draft.UpdatedAt = 0, 0
	if current != *private.Draft {
		return errors.New("The source draft changed during transfer.")
	}
	_, err = tx.Exec(`DELETE FROM thread_drafts WHERE thread_id = ?`, transfer.ThreadID)
	return err
}

// MoveThreadDraft publishes the destination and consumes the exact source in
// one transaction. Both threads must still be unsent chat/plan drafts.
func (s *Store) MoveThreadDraft(expected, destination ThreadDraft) error {
	if expected.ThreadID == "" || destination.ThreadID == "" || expected.ThreadID == destination.ThreadID {
		return errors.New("draft move requires two different threads")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range []string{expected.ThreadID, destination.ThreadID} {
		if err := checkThreadTransferAccess(tx, id); err != nil {
			return err
		}
		if err := checkUnsentDraft(tx, id); err != nil {
			return err
		}
	}
	current, _, err := getThreadDraft(tx, expected.ThreadID)
	if err != nil {
		return err
	}
	current.UpdatedAt, expected.UpdatedAt = 0, 0
	if current != expected {
		return errors.New("The draft changed while switching projects. Try again.")
	}
	target, _, err := getThreadDraft(tx, destination.ThreadID)
	if err != nil {
		return err
	}
	if target.Content != "" || target.Attachments != "[]" || target.TerminalChips != "[]" || target.PendingPlanImplementation != "" {
		return errors.New("The destination already contains a draft.")
	}
	if _, err = upsertThreadDraft(tx, destination); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM thread_drafts WHERE thread_id = ?`, expected.ThreadID); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckUnsentDraft rejects history, accepted sends, and provider sessions, even
// when the current timeline window happens to be empty.
func (s *Store) CheckUnsentDraft(threadID string) error {
	return checkUnsentDraft(s.reader(), threadID)
}

func checkUnsentDraft(q sqlQueryer, threadID string) error {
	var eligible bool
	err := q.QueryRow(`SELECT mode IN ('chat','plan')
		AND COALESCE(session_ref,'') = '' AND COALESCE(pending_fork_session_ref,'') = ''
		AND NOT EXISTS (SELECT 1 FROM timeline_items WHERE thread_id = threads.id)
		AND NOT EXISTS (SELECT 1 FROM turns WHERE thread_id = threads.id)
		AND NOT EXISTS (SELECT 1 FROM flush_queue_items WHERE thread_id = threads.id)
		AND NOT EXISTS (SELECT 1 FROM thread_draft_recoveries WHERE thread_id = threads.id)
		FROM threads WHERE id = ?`, threadID).Scan(&eligible)
	if err != nil {
		return fmt.Errorf("check draft: %w", err)
	}
	if !eligible {
		return ErrNotUnsentDraft
	}
	return nil
}
