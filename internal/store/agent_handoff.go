package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// New Thread, Same Assistant in the store: the handoff's one transaction, and
// the reads and moves of what it superseded.

// MovedDraftOf is the composer draft a handoff moved from the past thread
// threadID to the next one, its text and attachment ids; found is false for
// a thread that is no assistant's past thread.
func (s *Store) MovedDraftOf(threadID string) (moved ThreadDraft, found bool, err error) {
	err = s.reader().QueryRow(`SELECT moved_draft, moved_draft_attachments FROM agent_past_threads WHERE thread_id = ?`, threadID).
		Scan(&moved.Content, &moved.Attachments)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadDraft{}, false, nil
	}
	if err != nil {
		return ThreadDraft{}, false, fmt.Errorf("store: read the draft a handoff moved from %s: %w", threadID, err)
	}
	moved.ThreadID = threadID
	return moved, true, nil
}

// AssistantCurrentThreadOf maps a thread a handoff superseded to its
// assistant's current thread. Found is false for every other thread,
// current threads included.
func (s *Store) AssistantCurrentThreadOf(threadID string) (string, bool, error) {
	var current string
	err := s.reader().QueryRow(`SELECT agents.thread_id FROM agent_past_threads
		JOIN agents ON agents.id = agent_past_threads.agent_id
		WHERE agent_past_threads.thread_id = ?`, threadID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the assistant thread %s was superseded for: %w", threadID, err)
	}
	return current, true, nil
}

// AssistantHandOff is what a handoff changed.
type AssistantHandOff struct {
	// Agent is the record, now on the successor.
	Agent StandingAgent
	// Old is the superseded thread as it stands, archived.
	Old Thread
	// MovedWakeIDs are the live loops and watches now on the successor.
	MovedWakeIDs []string
	// MovedRequests counts the requests now the successor's.
	MovedRequests int64
}

// ErrNotAssistantCurrentThread refuses a handoff of a thread that is no
// assistant's current thread.
var ErrNotAssistantCurrentThread = errors.New("the thread is not an assistant's current thread")

// ErrAssistantArchived refuses a handoff of an archived assistant.
var ErrAssistantArchived = errors.New("this assistant is archived; unarchive it first")

// HandOffAssistant makes newThreadID the current thread of the assistant
// whose current thread is oldThreadID, in one transaction, so the old
// thread's state is read as it stands at the commit: an archived assistant
// is refused (ErrAssistantArchived), one whose delete committed
// (ErrThreadGone) and one a delete claimed (ErrThreadDeleting); the
// successor must pass the checks
// creation makes (requireAssistantThreadTx) under the old thread's title,
// which is the name, and takes that title and the old thread's model
// settings (handOffSettingsSQL); the record moves; the old thread is
// recorded as superseded at now and archived; its live loops and watches
// and the remote jobs still owed to it move; and every request it is the
// caller of moves with the thread that made it kept in made_in_thread_id (a
// reminder's target moves with it). The old thread keeps its placement,
// which a thread outside every project does not use. Receipts addressed to
// the old thread are not moved here: the messages they wait on are in the
// old thread's queue, and the app queues each again on the successor as it
// takes it back.
func (s *Store) HandOffAssistant(oldThreadID, newThreadID string, now int64) (handOff AssistantHandOff, err error) {
	action := fmt.Sprintf("store: hand assistant thread %s off to %s", oldThreadID, newThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	agent, err := scanStandingAgent(tx.QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE thread_id = ?`, oldThreadID))
	if errors.Is(err, sql.ErrNoRows) {
		return AssistantHandOff{}, ErrNotAssistantCurrentThread
	}
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: read the assistant: %w", action, err)
	}
	var name string
	var archived, deleting, claimed bool
	if err := tx.QueryRow(`SELECT title, archived, deleting, delete_claim <> '' FROM threads WHERE id = ?`, oldThreadID).Scan(&name, &archived, &deleting, &claimed); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: read the old thread: %w", action, err)
	}
	// Its delete takes the assistant with everything under it: a successor
	// would be left outside the family that delete read.
	if deleting {
		return AssistantHandOff{}, fmt.Errorf("%s: %w", action, ErrThreadGone)
	}
	if claimed {
		return AssistantHandOff{}, fmt.Errorf("%s: %w", action, ErrThreadDeleting)
	}
	if archived {
		return AssistantHandOff{}, ErrAssistantArchived
	}
	if err := requireAssistantThreadTx(tx, agent, newThreadID, name); err != nil {
		return AssistantHandOff{}, err
	}
	if _, err := tx.Exec(`UPDATE agents SET thread_id = ? WHERE id = ?`, newThreadID, agent.ID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the record: %w", action, err)
	}
	agent.ThreadID = newThreadID
	if err := updateTitleTx(tx, newThreadID, name); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: name the successor: %w", action, err)
	}
	if _, err := tx.Exec(handOffSettingsSQL, oldThreadID, newThreadID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: copy the model settings: %w", action, err)
	}
	if _, err := tx.Exec(`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES (?, ?, ?)`,
		oldThreadID, agent.ID, now); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: record the old thread: %w", action, err)
	}
	handOff.Agent = agent
	if handOff.MovedWakeIDs, err = queryIDs(tx, `UPDATE agent_wakes SET thread_id = ?
		WHERE thread_id = ? AND state IN ('armed','running') RETURNING id`, newThreadID, oldThreadID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the loops and watches: %w", action, err)
	}
	// A remote job still owed to the old thread reports to the successor:
	// the watcher skips an archived thread's watches.
	if _, err := tx.Exec(`UPDATE remote_watches SET thread_id = ?
		WHERE thread_id = ? AND (notification = 'pending' OR (notification = 'queued' AND confirmed_at IS NULL))`,
		newThreadID, oldThreadID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the remote jobs: %w", action, err)
	}
	moved, err := tx.Exec(`UPDATE thread_requests SET
		made_in_thread_id = CASE WHEN made_in_thread_id = '' THEN caller_thread_id ELSE made_in_thread_id END,
		target_thread_id = CASE WHEN kind = '`+ThreadRequestRemind+`' AND target_computer_id = '' AND target_thread_id = ?1
			THEN ?2 ELSE target_thread_id END,
		caller_thread_id = ?2
		WHERE caller_thread_id = ?1`, oldThreadID, newThreadID)
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the requests: %w", action, err)
	}
	if handOff.MovedRequests, err = moved.RowsAffected(); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the requests: %w", action, err)
	}
	if handOff.Old, _, err = setThreadArchivedTx(tx, oldThreadID, true, workerFence{}); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: archive the old thread: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{CallerThreadID: oldThreadID})
	s.noteThreadRequestChanged(ThreadRequestChange{CallerThreadID: newThreadID})
	return handOff, nil
}

// handOffSettingsSQL gives the successor (?2) the old thread's (?1) model
// settings as they stand in the handoff's transaction, so a change made
// while the successor was being created is kept. The successor has no
// history yet, so its provider may still change.
const handOffSettingsSQL = `UPDATE threads SET (provider, model, reasoning_effort, fast_mode, context_window,
	auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode) =
	(SELECT provider, model, reasoning_effort, fast_mode, context_window,
	   auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode FROM threads WHERE id = ?1)
	WHERE id = ?2`

// RetargetThreadReceipt points a receipt that waits in `accepted` on a
// superseded thread at its assistant's current thread, with the request's
// own row when the request was made on this computer. False when the
// receipt is not in that state on fromThreadID.
func (s *Store) RetargetThreadReceipt(token, fromThreadID, toThreadID string) (moved bool, err error) {
	action := fmt.Sprintf("store: move receipt %s to %s", token, toThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	result, err := tx.Exec(`UPDATE thread_request_receipts SET target_thread_id = ?
		WHERE token = ? AND target_thread_id = ? AND state = '`+ThreadReceiptAccepted+`'`, toThreadID, token, fromThreadID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE thread_requests SET target_thread_id = ?
		WHERE token = ? AND target_computer_id = '' AND target_thread_id = ?`, toThreadID, token, fromThreadID); err != nil {
		return false, fmt.Errorf("%s: the request's row: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return true, nil
}
