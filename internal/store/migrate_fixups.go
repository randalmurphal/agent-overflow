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
	return rewriteItemMetas(tx,
		`meta LIKE '%"tool_result"%' OR meta LIKE '%"tool_use_result"%'`,
		itemmeta.TrimToolResultEcho,
	)
}

// trimCollabAgentStateMessagesMetaFixup (migration v9) applies
// itemmeta.TrimCollabAgentStateMessages to every persisted items.meta
// row still carrying Codex collab agentsStates messages. Rows written
// before the triage write path gained the trim hold each awaited
// child's full final message inline — on the wait carrier, the
// standalone wait completion, and any spawn row whose completion
// envelope echoed the map — duplicating the lazy tool_call_result
// payload. updated_at is left alone because the item's content is
// semantically unchanged.
//
// This also empties the data source of the frontend's historical
// wait-carrier preview fallback, removed in the same change: spawn
// sibling rows persisted since a249fb29 carry their own payload
// preview, so only pre-a249fb29 rows lose a collapsed preview line.
func trimCollabAgentStateMessagesMetaFixup(tx *sql.Tx) error {
	return rewriteItemMetas(tx,
		`meta LIKE '%"agentsStates"%'`,
		func(_ string, meta []byte) ([]byte, bool) {
			return itemmeta.TrimCollabAgentStateMessages(meta)
		},
	)
}

// rewriteItemMetas scans items matching the LIKE pre-filter, applies
// rewrite to each row's meta, and updates the rows the rewrite reports
// changed. The scan completes before any UPDATE runs so the fixup
// never mutates a table it is still iterating.
//
// `where` is concatenated into the query verbatim: it MUST be a
// compile-time constant SQL fragment. Never interpolate data — add an
// args parameter and `?` placeholders first if a future fixup needs
// dynamic filtering.
func rewriteItemMetas(tx *sql.Tx, where string, rewrite func(toolName string, meta []byte) ([]byte, bool)) error {
	rows, err := tx.Query(`SELECT thread_id, id, tool_name, meta FROM items WHERE ` + where)
	if err != nil {
		return fmt.Errorf("scan items for meta fixup: %w", err)
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
		rewritten, changed := rewrite(toolName, []byte(meta))
		if !changed {
			continue
		}
		updates = append(updates, metaUpdate{threadID: threadID, id: id, meta: string(rewritten)})
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
			return fmt.Errorf("rewrite item meta %s/%s: %w", u.threadID, u.id, err)
		}
	}
	return nil
}
