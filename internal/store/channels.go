package store

import "fmt"

func (s *Store) CreateChannel(ch Channel) error {
	_, err := s.db.Exec(
		`INSERT INTO channels (id, thread_id, type, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ch.ID, ch.ThreadID, ch.Type, ch.Status, ch.CreatedAt, ch.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create channel %s: %w", ch.ID, err)
	}
	return nil
}

func (s *Store) GetChannel(id string) (Channel, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, type, status, created_at, updated_at
		 FROM channels WHERE id = ?`,
		id,
	)

	var ch Channel
	if err := row.Scan(&ch.ID, &ch.ThreadID, &ch.Type, &ch.Status, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
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

func (s *Store) InsertChannelMessage(msg ChannelMessage) error {
	_, err := s.db.Exec(
		`INSERT INTO channel_messages (
			id, channel_id, sequence, from_type, from_id, from_role, content, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ChannelID, msg.Sequence, msg.FromType, msg.FromID, nilIfEmpty(msg.FromRole), msg.Content, msg.CreatedAt,
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
		`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, from_role, content, created_at)
		 SELECT ?, ?, COALESCE(MAX(sequence), -1) + 1, ?, ?, ?, ?, ?
		 FROM channel_messages WHERE channel_id = ?
		 RETURNING sequence`,
		msg.ID, msg.ChannelID, msg.FromType, msg.FromID, nilIfEmpty(msg.FromRole), msg.Content, msg.CreatedAt, msg.ChannelID,
	).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("store: insert channel message atomic %s: %w", msg.ID, err)
	}
	return sequence, nil
}

func (s *Store) ListChannelMessages(channelID string, afterSeq, limit int) ([]ChannelMessage, error) {
	baseQuery := `SELECT id, channel_id, sequence, from_type, from_id, COALESCE(from_role, ''), content, created_at
		FROM channel_messages WHERE channel_id = ? AND sequence > ? ORDER BY sequence ASC`
	args := []any{channelID, afterSeq}
	if limit > 0 {
		baseQuery += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.Query(baseQuery, args...)
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
