package store

import "fmt"

// ListUserMessageMetadata reads only correlation-bearing rows, without loading
// tool payload metadata or previews for the rest of the conversation.
func (s *Store) ListUserMessageMetadata(threadID string) ([]ItemMetaUpdate, error) {
	rows, err := s.reader().Query(`SELECT id,meta FROM timeline_items WHERE thread_id=? AND kind='user_text' AND role='user'`, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: list user message metadata: %w", err)
	}
	defer rows.Close()
	var out []ItemMetaUpdate
	for rows.Next() {
		var row ItemMetaUpdate
		if err := rows.Scan(&row.ItemID, &row.Meta); err != nil {
			return nil, fmt.Errorf("store: scan user message metadata: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
