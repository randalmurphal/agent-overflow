package store

import (
	"database/sql"
	"fmt"
)

// MessageAnchor is the per-real-user-message correlation row written
// immediately after the message's items row persists. It carries the
// provider-side identity of the message — Claude's wire uuid + parent
// uuid, and the AO turn index Codex anchors resolve against — so
// fork-from-message and revert-on-interrupt can slice provider history
// at the message boundary. Pure SQLite bookkeeping: the git snapshot
// machinery that used to hang off this row is gone.
type MessageAnchor struct {
	ThreadID              string `json:"threadId"`
	UserItemID            string `json:"userItemId"`
	TurnIndex             int    `json:"turnIndex"`
	ProviderUserMessageID string `json:"providerUserMessageId,omitempty"`
	ProviderParentUUID    string `json:"providerParentUuid,omitempty"`
	CreatedAt             int64  `json:"createdAt"`
}

const messageAnchorColumns = `thread_id, user_item_id, turn_index,
    provider_user_message_id, provider_parent_uuid, created_at`

// messageAnchorColumnsQualified is messageAnchorColumns with an `a.`
// alias prefix for queries that join message_anchors against items.
const messageAnchorColumnsQualified = `a.thread_id, a.user_item_id, a.turn_index,
    a.provider_user_message_id, a.provider_parent_uuid, a.created_at`

func scanMessageAnchor(scanner interface{ Scan(...any) error }) (MessageAnchor, error) {
	var a MessageAnchor
	if err := scanner.Scan(
		&a.ThreadID, &a.UserItemID, &a.TurnIndex,
		&a.ProviderUserMessageID, &a.ProviderParentUUID, &a.CreatedAt,
	); err != nil {
		return MessageAnchor{}, err
	}
	return a, nil
}

// UpsertMessageAnchor writes the anchor row for a user message,
// replacing any prior row for the same (thread, user item) — a resend
// of the same optimistic row re-anchors at the newest send.
func (s *Store) UpsertMessageAnchor(a MessageAnchor) error {
	if a.ThreadID == "" {
		return fmt.Errorf("store: upsert message anchor: thread id is required")
	}
	if a.UserItemID == "" {
		return fmt.Errorf("store: upsert message anchor: user item id is required")
	}
	_, err := s.db.Exec(
		`INSERT INTO message_anchors (
			thread_id, user_item_id, turn_index,
			provider_user_message_id, provider_parent_uuid, created_at
		 ) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread_id, user_item_id) DO UPDATE SET
			turn_index = excluded.turn_index,
			provider_user_message_id = excluded.provider_user_message_id,
			provider_parent_uuid = excluded.provider_parent_uuid,
			created_at = excluded.created_at`,
		a.ThreadID, a.UserItemID, a.TurnIndex,
		a.ProviderUserMessageID, a.ProviderParentUUID, a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert message anchor %s/%s: %w", a.ThreadID, a.UserItemID, err)
	}
	return nil
}

func (s *Store) GetMessageAnchor(threadID, userItemID string) (MessageAnchor, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+messageAnchorColumns+` FROM message_anchors
		 WHERE thread_id = ? AND user_item_id = ?`,
		threadID, userItemID,
	)
	a, err := scanMessageAnchor(row)
	if err == sql.ErrNoRows {
		return MessageAnchor{}, false, nil
	}
	if err != nil {
		return MessageAnchor{}, false, fmt.Errorf("store: get message anchor thread=%s user_item=%s: %w",
			threadID, userItemID, err)
	}
	return a, true, nil
}

// ListMessageAnchors returns the thread's anchors in timeline order —
// (turn_index, item_index) of each anchor's user item, not created_at:
// echo-time replaces and interrupt-batch writes make created_at
// non-monotonic against the timeline, and consumers (the fork remap)
// read the order as message order.
func (s *Store) ListMessageAnchors(threadID string) ([]MessageAnchor, error) {
	rows, err := s.db.Query(
		`SELECT `+messageAnchorColumnsQualified+` FROM message_anchors a
		 JOIN items i ON i.thread_id = a.thread_id AND i.id = a.user_item_id
		 WHERE a.thread_id = ?
		 ORDER BY i.turn_index ASC, i.item_index ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list message anchors for %s: %w", threadID, err)
	}
	defer rows.Close()

	var out []MessageAnchor
	for rows.Next() {
		a, err := scanMessageAnchor(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan message anchor: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate message anchors for %s: %w", threadID, err)
	}
	return out, nil
}

// UpdateMessageAnchorProviderIDs stamps provider-echo identity onto an
// anchor. Empty-string args preserve the stored value; both-empty is a
// no-op. Callers (triage echo replay, the fork remap) rely on the
// preserve contract — an echo that carries only one id must not blank
// the other.
func (s *Store) UpdateMessageAnchorProviderIDs(threadID, userItemID, providerUserMessageID, providerParentUUID string) error {
	if threadID == "" || userItemID == "" {
		return fmt.Errorf("store: update message anchor provider ids requires thread and user item id")
	}
	if providerUserMessageID == "" && providerParentUUID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE message_anchors
		    SET provider_user_message_id = CASE WHEN ? != '' THEN ? ELSE provider_user_message_id END,
		        provider_parent_uuid = CASE WHEN ? != '' THEN ? ELSE provider_parent_uuid END
		  WHERE thread_id = ? AND user_item_id = ?`,
		providerUserMessageID, providerUserMessageID,
		providerParentUUID, providerParentUUID,
		threadID, userItemID,
	)
	if err != nil {
		return fmt.Errorf("store: update message anchor provider ids thread=%s user_item=%s: %w", threadID, userItemID, err)
	}
	return nil
}
