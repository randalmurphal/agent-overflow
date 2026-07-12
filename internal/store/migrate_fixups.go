package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

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

// trimCodexV2EncryptedCollabPromptsFixup removes opaque model-service
// ciphertext that older adapters copied from raw MultiAgentV2 function-call
// arguments into meta.input.prompt. V2 rows are identified by their canonical
// activityKind field; legacy V1 rows keep their plaintext prompt previews.
// Summaries proven to contain the removed prompt preview are rebuilt without
// it while preserving any terminal status suffix.
func trimCodexV2EncryptedCollabPromptsFixup(tx *sql.Tx) error {
	type update struct {
		rowID    int64
		threadID string
		id       string
		summary  string
		meta     string
	}
	const batchSize = 128
	var lastRowID int64
	for {
		rows, err := tx.Query(`SELECT rowid, thread_id, id, tool_name, summary, meta FROM items
			WHERE rowid > ? AND meta LIKE '%"activityKind"%' AND meta LIKE '%"prompt"%'
			ORDER BY rowid LIMIT ?`, lastRowID, batchSize)
		if err != nil {
			return fmt.Errorf("scan Codex V2 collaboration prompts: %w", err)
		}

		updates := make([]update, 0, batchSize)
		scanned := 0
		for rows.Next() {
			var candidate update
			var toolName, meta string
			if err := rows.Scan(&candidate.rowID, &candidate.threadID, &candidate.id, &toolName, &candidate.summary, &meta); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan Codex V2 collaboration prompt row: %w", err)
			}
			scanned++
			lastRowID = candidate.rowID
			prompt := encryptedPromptFromMeta(meta)
			rewritten, changed := itemmeta.TrimEncryptedCollabPrompt([]byte(meta))
			if !changed {
				continue
			}
			candidate.meta = string(rewritten)
			if repaired, changed := summaryWithoutEncryptedPrompt(candidate.summary, toolName, prompt); changed {
				candidate.summary = repaired
			}
			updates = append(updates, candidate)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate Codex V2 collaboration prompt rows: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close Codex V2 collaboration prompt scan: %w", err)
		}

		for _, candidate := range updates {
			if _, err := tx.Exec(
				`UPDATE items SET meta = ?, summary = ? WHERE rowid = ?`,
				candidate.meta, candidate.summary, candidate.rowID,
			); err != nil {
				return fmt.Errorf("rewrite Codex V2 collaboration prompt %s/%s: %w", candidate.threadID, candidate.id, err)
			}
		}
		if scanned < batchSize {
			return nil
		}
	}
}

func encryptedPromptFromMeta(meta string) string {
	var decoded struct {
		Input struct {
			Prompt string `json:"prompt"`
		} `json:"input"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Input.Prompt)
}

func summaryWithoutEncryptedPrompt(summary, toolName, prompt string) (string, bool) {
	if prompt == "" {
		return summary, false
	}
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "tool"
	}
	prefix := name + ": "
	if !strings.HasPrefix(summary, prefix) {
		return summary, false
	}
	detail, suffix := splitToolCompletionSuffix(strings.TrimPrefix(summary, prefix))
	preview := strings.TrimSuffix(detail, "…")
	if preview == "" || !strings.HasPrefix(prompt, preview) {
		return summary, false
	}
	return name + suffix, true
}

func splitToolCompletionSuffix(detail string) (string, string) {
	for _, suffix := range []string{" (error)", " (failed)", " (errored)", " (killed)", " (declined)"} {
		if strings.HasSuffix(detail, suffix) {
			return strings.TrimSuffix(detail, suffix), suffix
		}
	}
	if exitStart := strings.LastIndex(detail, " (exit "); exitStart >= 0 && strings.HasSuffix(detail, ")") {
		return detail[:exitStart], detail[exitStart:]
	}
	return detail, ""
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
