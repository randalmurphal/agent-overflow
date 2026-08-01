package store

import "fmt"

func (s *Store) CreateChannel(ch Channel) error {
	_, err := s.db.Exec(
		`INSERT INTO channels (id, thread_id, type, status, max_turns, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ch.ID, ch.ThreadID, ch.Type, ch.Status, ch.MaxTurns, ch.CreatedAt, ch.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create channel %s: %w", ch.ID, err)
	}
	return nil
}

func (s *Store) GetChannel(id string) (Channel, error) {
	row := s.reader().QueryRow(
		`SELECT id, thread_id, type, status, max_turns, created_at, updated_at
		 FROM channels WHERE id = ?`,
		id,
	)

	var ch Channel
	if err := row.Scan(&ch.ID, &ch.ThreadID, &ch.Type, &ch.Status, &ch.MaxTurns, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		return Channel{}, fmt.Errorf("store: get channel %s: %w", id, err)
	}
	return ch, nil
}

func (s *Store) UpdateChannelStatus(id, status string) error {
	updatedAt := nowMillis()
	result, err := s.db.Exec(
		`UPDATE channels
		 SET status = ?,
		     updated_at = CASE WHEN updated_at >= ? THEN updated_at + 1 ELSE ? END
		 WHERE id = ?`,
		status, updatedAt, updatedAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update channel status %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update channel status %s", id))
}

func (s *Store) DeleteChannel(id string) error {
	result, err := s.db.Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete channel %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete channel %s", id))
}

// InsertChannelMessage inserts a message with a caller-supplied sequence number.
// Production code should use InsertChannelMessageAtomic instead to avoid
// sequence collisions under concurrency. This method is retained for tests that
// need deterministic sequence values.
func (s *Store) InsertChannelMessage(msg ChannelMessage) error {
	_, err := s.db.Exec(
		`INSERT INTO channel_messages (
			id, channel_id, sequence, from_type, from_id, from_role, content, meta, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.Sequence, msg.FromType, msg.FromID, nilIfEmpty(msg.FromRole),
		msg.Content, nilIfEmpty(msg.Meta), msg.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert channel message %s: %w", msg.ID, err)
	}
	return nil
}

// InsertChannelMessageAtomic inserts a channel message with an atomically
// computed sequence number. Returns the assigned sequence. This avoids the
// race where two concurrent PostMessage calls read the same max sequence
// and the second INSERT fails on the UNIQUE(channel_id, sequence) constraint.
func (s *Store) InsertChannelMessageAtomic(msg ChannelMessage) (int, error) {
	var sequence int
	err := s.db.QueryRow(
		`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, from_role, content, meta, created_at)
		 SELECT ?, ?, COALESCE(MAX(sequence), -1) + 1, ?, ?, ?, ?, ?, ?
		 FROM channel_messages WHERE channel_id = ?
		 RETURNING sequence`,
		msg.ID, msg.ChannelID, msg.FromType, msg.FromID, nilIfEmpty(msg.FromRole),
		msg.Content, nilIfEmpty(msg.Meta), msg.CreatedAt, msg.ChannelID,
	).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("store: insert channel message atomic %s: %w", msg.ID, err)
	}
	return sequence, nil
}

// LastChannelMessageSeqFrom returns the highest sequence number of any
// message posted by fromID into the channel, or -1 when that
// participant has never posted. Used by promptDiscussionSpeaker to
// find the cursor a speaker's next turn prompt should read messages
// after — the speaker has already "seen" everything up to and
// including its own last post.
//
// The never-posted fallback MUST be -1, not 0: sequences are
// zero-based (the channel's first-ever message is sequence 0), and
// ListChannelMessages's cursor is an exclusive "after" bound
// (`sequence > afterSeq`). Falling back to 0 would silently exclude
// that very first message from a never-yet-posted participant's
// first-ever turn prompt — exactly the case a new discussion's first
// speaker hits when a human's kickoff message is the channel's only
// content so far. -1 matches the "from the beginning" sentinel already
// used throughout the codebase (e.g. GetChannelMessages(id, -1, ...)).
func (s *Store) LastChannelMessageSeqFrom(channelID, fromID string) (int, error) {
	var seq int
	err := s.reader().QueryRow(
		`SELECT COALESCE(MAX(sequence), -1) FROM channel_messages WHERE channel_id = ? AND from_id = ?`,
		channelID, fromID,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: last channel message seq for %s/%s: %w", channelID, fromID, err)
	}
	return seq, nil
}

// CountChannelMessagesByType returns the number of messages in the
// channel with the given from_type ("agent", "human", "system"). Used
// by the restart-rebuild path (deliberationForChannel) to recompute
// Deliberation.TurnCount — the FSM's turn counter tracks agent posts
// only, matching RecordPost's caller (syncDiscussionTurn), which is
// invoked exactly once per participant turn.
func (s *Store) CountChannelMessagesByType(channelID, fromType string) (int, error) {
	var count int
	err := s.reader().QueryRow(
		`SELECT COUNT(*) FROM channel_messages WHERE channel_id = ? AND from_type = ?`,
		channelID, fromType,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count channel messages %s/%s: %w", channelID, fromType, err)
	}
	return count, nil
}

func (s *Store) ListChannelMessages(channelID string, afterSeq, limit int) ([]ChannelMessage, error) {
	baseQuery := `SELECT id, channel_id, sequence, from_type, from_id, COALESCE(from_role, ''), content, COALESCE(meta, ''), created_at
		FROM channel_messages WHERE channel_id = ? AND sequence > ? ORDER BY sequence ASC`
	args := []any{channelID, afterSeq}
	if limit > 0 {
		baseQuery += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.reader().Query(baseQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list channel messages for %s: %w", channelID, err)
	}
	defer rows.Close()

	var messages []ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		if err := rows.Scan(
			&msg.ID,
			&msg.ChannelID,
			&msg.Sequence,
			&msg.FromType,
			&msg.FromID,
			&msg.FromRole,
			&msg.Content,
			&msg.Meta,
			&msg.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan channel message: %w", err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel messages for %s: %w", channelID, err)
	}
	return messages, nil
}
