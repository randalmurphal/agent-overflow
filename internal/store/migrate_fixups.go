package store

import (
	"database/sql"
	"fmt"

	"agent-overflow/internal/itemmeta"
)

// trimToolResultEchoMetaFixup (migration v8) applies
// itemmeta.TrimToolResultEcho to every persisted items.meta row that
// still carries the Claude completion echo (`tool_result` /
// `tool_use_result`). Rows written before the triage write path gained
// the trim could hold the full tool output twice over — megabytes per
// subagent-heavy thread — even though the same bytes live in the lazy
// payload row. The fixup rewrites only rows the trim actually changes;
// updated_at is left alone because the item's content is semantically
// unchanged.
//
// The LIKE pre-filter mirrors the byte pre-check inside
// TrimToolResultEcho: `"tool_use_result"` does not contain the
// substring `"tool_result"` (the quote breaks it), so both patterns are
// needed. A full-table scan is acceptable for a one-time migration.
func trimToolResultEchoMetaFixup(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT thread_id, id, tool_name, meta
	   FROM items
	  WHERE meta LIKE '%"tool_result"%' OR meta LIKE '%"tool_use_result"%'`)
	if err != nil {
		return fmt.Errorf("scan items for tool_result echo: %w", err)
	}
	defer rows.Close()

	type metaUpdate struct {
		threadID string
		id       string
		meta     string
	}
	var updates []metaUpdate
	for rows.Next() {
		var threadID, id, toolName, meta string
		if err := rows.Scan(&threadID, &id, &toolName, &meta); err != nil {
			return fmt.Errorf("scan item meta row: %w", err)
		}
		trimmed, changed := itemmeta.TrimToolResultEcho(toolName, []byte(meta))
		if !changed {
			continue
		}
		updates = append(updates, metaUpdate{threadID: threadID, id: id, meta: string(trimmed)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate item meta rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close item meta scan: %w", err)
	}

	for _, u := range updates {
		if _, err := tx.Exec(
			`UPDATE items SET meta = ? WHERE thread_id = ? AND id = ?`,
			u.meta, u.threadID, u.id,
		); err != nil {
			return fmt.Errorf("trim item meta %s/%s: %w", u.threadID, u.id, err)
		}
	}
	return nil
}
