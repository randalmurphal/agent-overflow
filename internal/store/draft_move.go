package store

import (
	"context"
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

// DraftMove is what MoveThreadDraft wrote on each thread.
type DraftMove struct {
	Destination DraftWrite
	Source      DraftWrite
}

// MoveThreadDraft publishes the destination and consumes the exact source in
// one transaction. Both threads must still be unsent chat/plan drafts. It
// reports the write on each: the destination gains the draft, the source
// loses it.
func (s *Store) MoveThreadDraft(expected, destination ThreadDraft) (DraftMove, error) {
	if expected.ThreadID == "" || destination.ThreadID == "" || expected.ThreadID == destination.ThreadID {
		return DraftMove{}, errors.New("draft move requires two different threads")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return DraftMove{}, err
	}
	defer release()
	defer tx.Rollback()
	for _, id := range []string{expected.ThreadID, destination.ThreadID} {
		if err := checkThreadTransferAccess(tx, id); err != nil {
			return DraftMove{}, err
		}
		if err := checkUnsentDraft(tx, id); err != nil {
			return DraftMove{}, err
		}
	}
	current, _, err := getThreadDraft(tx, expected.ThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	current.UpdatedAt, expected.UpdatedAt = 0, 0
	if current != expected {
		return DraftMove{}, errors.New("The draft changed while switching projects. Try again.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
	}
	target, _, err := getThreadDraft(tx, destination.ThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	if target.Content != "" || target.Attachments != "[]" || target.TerminalChips != "[]" || target.PendingPlanImplementation != "" {
		return DraftMove{}, errors.New("The destination already contains a draft.") //nolint:staticcheck // ST1005: the user sees this sentence as written.
	}
	sourceListed, err := threadListed(tx, expected.ThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	var move DraftMove
	if move.Destination, err = upsertThreadDraft(tx, destination); err != nil {
		return DraftMove{}, err
	}
	if _, err = tx.Exec(`DELETE FROM thread_drafts WHERE thread_id = ?`, expected.ThreadID); err != nil {
		return DraftMove{}, err
	}
	sourceStillListed, err := threadListed(tx, expected.ThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	move.Source = DraftWrite{Changed: true, Listing: listingChange(sourceListed, sourceStillListed)}
	return move, tx.Commit()
}

// HandOffThreadDraft writes an assistant's next thread's draft and deletes
// its previous thread's draft in one transaction, so a handoff moves the
// unsent text whole or not at all, and records the text and attachment ids
// it took on the previous thread's agent_past_threads row (MovedDraftOf).
// expected is the previous thread's draft as the move read it, nil when it
// had none; a draft that changed since refuses the move. It reports the
// write on each thread as MoveThreadDraft does, the listing included: the
// next thread gains the draft, and the previous one loses it.
func (s *Store) HandOffThreadDraft(sourceThreadID string, expected *ThreadDraft, destination ThreadDraft) (DraftMove, error) {
	if sourceThreadID == "" || destination.ThreadID == "" || sourceThreadID == destination.ThreadID {
		return DraftMove{}, errors.New("draft handoff requires two different threads")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return DraftMove{}, err
	}
	defer release()
	defer func() { _ = tx.Rollback() }()
	current, found, err := getThreadDraft(tx, sourceThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	if found != (expected != nil) || (found && !SameDraft(current, *expected)) {
		return DraftMove{}, errors.New("the previous thread's draft changed during the handoff")
	}
	sourceListed, err := threadListed(tx, sourceThreadID)
	if err != nil {
		return DraftMove{}, err
	}
	var move DraftMove
	if move.Destination, err = upsertThreadDraft(tx, destination); err != nil {
		return DraftMove{}, err
	}
	if found {
		if _, err = tx.Exec(`DELETE FROM thread_drafts WHERE thread_id = ?`, sourceThreadID); err != nil {
			return DraftMove{}, fmt.Errorf("clear the previous thread's draft: %w", err)
		}
		moved := current.stored()
		if _, err = tx.Exec(`UPDATE agent_past_threads SET moved_draft = ?, moved_draft_attachments = ? WHERE thread_id = ?`,
			moved.Content, moved.Attachments, sourceThreadID); err != nil {
			return DraftMove{}, fmt.Errorf("record the draft the handoff moved: %w", err)
		}
		sourceStillListed, err := threadListed(tx, sourceThreadID)
		if err != nil {
			return DraftMove{}, err
		}
		move.Source = DraftWrite{Changed: true, Listing: listingChange(sourceListed, sourceStillListed)}
	}
	return move, tx.Commit()
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
