package store

import (
	"database/sql"
	"fmt"
)

// ImportedHistoryPrefix is the state copied by CloneImportedHistoryPrefix.
// Cursor fields describe the last logical item in the prefix; Model is the
// newest model attributed by its usage rows.
type ImportedHistoryPrefix struct {
	ItemCount      int
	LastTurnIndex  int
	LastItemIndex  int
	LastSourceUUID string
	Model          string
}

// CloneImportedHistoryPrefix gives target the source history strictly before
// beforeTurnIndex. Whole immutable chunks are attached by reference; at most
// the source's final partial chunk is copied into the mutable overlay. Turns
// and usage are small thread-owned records and are copied with target ids.
// boundaryAt is the target branch's first suffix prompt timestamp. Claude
// timestamps the preceding turn completion and its usage at that next prompt,
// so the final copied turn must use the target boundary rather than the
// donor's divergent prompt time.
//
// Both threads must be newly imported branches under the same import
// operation: target must be empty, and source may not have hidden imported
// overrides. Those preconditions keep this optimization semantics-preserving
// instead of turning it into a general-purpose history merge API.
func (s *Store) CloneImportedHistoryPrefix(
	sourceThreadID, targetThreadID string,
	beforeTurnIndex int,
	boundaryAt int64,
) (ImportedHistoryPrefix, error) {
	result := ImportedHistoryPrefix{LastTurnIndex: -1, LastItemIndex: -1}
	if sourceThreadID == "" || targetThreadID == "" || beforeTurnIndex <= 0 || boundaryAt <= 0 {
		return result, fmt.Errorf("store: clone imported history prefix requires source, target, positive turn boundary, and boundary timestamp")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, fmt.Errorf("store: begin clone imported history prefix: %w", err)
	}
	defer tx.Rollback()

	var targetRows, targetTurns, hiddenSourceRows int
	if err := tx.QueryRow(
		`SELECT
		   (SELECT EXISTS(SELECT 1 FROM items WHERE thread_id = ?) OR
		           EXISTS(SELECT 1 FROM thread_import_chunks WHERE thread_id = ?)),
		   (SELECT EXISTS(SELECT 1 FROM turns WHERE thread_id = ?)),
		   (SELECT EXISTS(
		      SELECT 1
		      FROM thread_import_item_overrides overrides
		      JOIN thread_import_chunks refs ON refs.thread_id = overrides.thread_id
		      JOIN import_history_items source
		        ON source.chunk_id = refs.chunk_id AND source.id = overrides.item_id
		     WHERE overrides.thread_id = ? AND source.turn_index < ?))`,
		targetThreadID, targetThreadID, targetThreadID, sourceThreadID, beforeTurnIndex,
	).Scan(&targetRows, &targetTurns, &hiddenSourceRows); err != nil {
		return result, fmt.Errorf("store: inspect imported prefix clone: %w", err)
	}
	if targetRows != 0 || targetTurns != 0 {
		return result, fmt.Errorf("store: imported prefix target %s is not empty", targetThreadID)
	}
	if hiddenSourceRows != 0 {
		return result, fmt.Errorf("store: imported prefix source %s has hidden rows before turn %d", sourceThreadID, beforeTurnIndex)
	}

	if _, err := tx.Exec(
		`INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id)
		 SELECT ?, ROW_NUMBER() OVER (ORDER BY source.chunk_order) - 1, source.chunk_id
		   FROM thread_import_chunks source
		   JOIN import_history_chunks chunk ON chunk.id = source.chunk_id
		  WHERE source.thread_id = ? AND chunk.max_turn_index < ?
		  ORDER BY source.chunk_order`,
		targetThreadID, sourceThreadID, beforeTurnIndex,
	); err != nil {
		return result, fmt.Errorf("store: attach imported prefix chunks to %s: %w", targetThreadID, err)
	}

	coveredMax := -1
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(chunk.max_turn_index), -1)
		   FROM thread_import_chunks refs
		   JOIN import_history_chunks chunk ON chunk.id = refs.chunk_id
		  WHERE refs.thread_id = ?`,
		targetThreadID,
	).Scan(&coveredMax); err != nil {
		return result, fmt.Errorf("store: read attached prefix boundary for %s: %w", targetThreadID, err)
	}

	localCount := int64(0)
	// Chunks never split a turn. In the production branch-import path the
	// complete-turn boundary therefore lands exactly after the final attached
	// chunk, and there is no tail to inspect. Keep the tail copy for callers
	// whose source also has a mutable prefix, but do not scan both logical views
	// on every ordinary branch clone to prove an impossible range is empty.
	if coveredMax < beforeTurnIndex-1 {
		if _, err := tx.Exec(
			`WITH needed_payloads(id) AS MATERIALIZED (
			    SELECT payload_id
			      FROM timeline_items
			     WHERE thread_id = ?
			       AND turn_index > ? AND turn_index < ?
			       AND payload_id IS NOT NULL
			    UNION
			    SELECT input_payload_id
			      FROM timeline_items
			     WHERE thread_id = ?
			       AND turn_index > ? AND turn_index < ?
			       AND input_payload_id IS NOT NULL
			 )
			 INSERT OR IGNORE INTO payloads (
			    thread_id, id, kind, meta, data, created_at, preview_spans, spans
			 )
			 SELECT ?, payload.id, payload.kind, payload.meta, payload.data,
			        payload.created_at, payload.preview_spans, payload.spans
			   FROM timeline_payloads payload
			   JOIN needed_payloads needed ON needed.id = payload.id
			  WHERE payload.thread_id = ?`,
			sourceThreadID, coveredMax, beforeTurnIndex,
			sourceThreadID, coveredMax, beforeTurnIndex,
			targetThreadID, sourceThreadID,
		); err != nil {
			return result, fmt.Errorf("store: copy imported prefix payload tail to %s: %w", targetThreadID, err)
		}

		if err := setHistoryBulkLoadTx(tx, targetThreadID, true, "store: clone imported history prefix"); err != nil {
			return result, err
		}
		inserted, err := tx.Exec(
			`INSERT INTO items (
			    id, thread_id, turn_index, item_index, kind, role, status, summary,
			    payload_id, input_payload_id, parent_id, is_background, completion_of,
			    tool_name, decision, meta, created_at, updated_at
			 )
			 SELECT source.id, ?, source.turn_index, source.item_index, source.kind,
			        source.role, source.status, source.summary, source.payload_id,
			        source.input_payload_id, source.parent_id, source.is_background,
			        source.completion_of, source.tool_name, source.decision, source.meta,
			        source.created_at, source.updated_at
			   FROM timeline_items source
			  WHERE source.thread_id = ?
			    AND source.turn_index > ? AND source.turn_index < ?
			  ORDER BY source.turn_index, source.item_index`,
			targetThreadID, sourceThreadID, coveredMax, beforeTurnIndex,
		)
		if err != nil {
			return result, fmt.Errorf("store: copy imported prefix item tail to %s: %w", targetThreadID, err)
		}
		localCount, err = inserted.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("store: count imported prefix item tail for %s: %w", targetThreadID, err)
		}
		if err := setHistoryBulkLoadTx(tx, targetThreadID, false, "store: clone imported history prefix"); err != nil {
			return result, err
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO turns (
		    turn_id, thread_id, turn_index, started_at, completed_at, stop_reason,
		    assistant_message_id, token_usage_json, error_message, provider_turn_id
		 )
		 SELECT ? || ':' || turn_index, ?, turn_index, started_at,
		        CASE WHEN turn_index = ? - 1 THEN ? ELSE completed_at END,
		        stop_reason, assistant_message_id, token_usage_json, error_message,
		        provider_turn_id
		   FROM turns
		  WHERE thread_id = ? AND turn_index < ?`,
		targetThreadID, targetThreadID, beforeTurnIndex, boundaryAt,
		sourceThreadID, beforeTurnIndex,
	); err != nil {
		return result, fmt.Errorf("store: copy imported prefix turns to %s: %w", targetThreadID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO usage_ledger (
		    created_at, thread_id, project_id, work_item_id, turn_id, provider, model,
		    input_tokens, output_tokens, cache_read_input_tokens,
		    cache_creation_input_tokens, reasoning_output_tokens, cost_usd, cost_source
		 )
		 SELECT CASE WHEN source_turn.turn_index = ? - 1 THEN ? ELSE usage.created_at END,
		        ?, usage.project_id, usage.work_item_id,
		        ? || ':' || source_turn.turn_index, usage.provider, usage.model,
		        usage.input_tokens, usage.output_tokens, usage.cache_read_input_tokens,
		        usage.cache_creation_input_tokens, usage.reasoning_output_tokens,
		        usage.cost_usd, usage.cost_source
		   FROM usage_ledger usage
		   JOIN turns source_turn
		     ON source_turn.thread_id = ? AND source_turn.turn_id = usage.turn_id
		  WHERE usage.thread_id = ? AND source_turn.turn_index < ?`,
		beforeTurnIndex, boundaryAt, targetThreadID, targetThreadID,
		sourceThreadID, sourceThreadID, beforeTurnIndex,
	); err != nil {
		return result, fmt.Errorf("store: copy imported prefix usage to %s: %w", targetThreadID, err)
	}

	var sharedCount int
	if err := tx.QueryRow(
		`SELECT COALESCE(SUM(chunk.item_count), 0)
		   FROM thread_import_chunks refs
		   JOIN import_history_chunks chunk ON chunk.id = refs.chunk_id
		  WHERE refs.thread_id = ?`,
		targetThreadID,
	).Scan(&sharedCount); err != nil {
		return result, fmt.Errorf("store: count attached imported prefix for %s: %w", targetThreadID, err)
	}
	result.ItemCount = sharedCount + int(localCount)
	if result.ItemCount > 0 {
		if _, err := tx.Exec(
			`UPDATE threads SET history_rev = history_rev + ? WHERE id = ?`,
			result.ItemCount, targetThreadID,
		); err != nil {
			return result, fmt.Errorf("store: stamp imported prefix for %s: %w", targetThreadID, err)
		}
		cursorQuery := `SELECT turn_index, item_index,
		        COALESCE(json_extract(meta, '$.import_source_uuid'), '')
		   FROM items
		  WHERE thread_id = ?
		  ORDER BY turn_index DESC, item_index DESC
		  LIMIT 1`
		cursorArgs := []any{targetThreadID}
		if localCount == 0 {
			cursorQuery = `SELECT imported.turn_index, imported.item_index,
			        COALESCE(json_extract(imported.meta, '$.import_source_uuid'), '')
			   FROM thread_import_chunks refs
			   JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
			  WHERE refs.thread_id = ?
			    AND refs.chunk_order = (
			        SELECT MAX(chunk_order) FROM thread_import_chunks WHERE thread_id = ?
			    )
			  ORDER BY imported.turn_index DESC, imported.item_index DESC
			  LIMIT 1`
			cursorArgs = append(cursorArgs, targetThreadID)
		}
		if err := tx.QueryRow(cursorQuery, cursorArgs...).Scan(
			&result.LastTurnIndex, &result.LastItemIndex, &result.LastSourceUUID,
		); err != nil {
			return result, fmt.Errorf("store: read imported prefix cursor for %s: %w", targetThreadID, err)
		}
	}
	if err := tx.QueryRow(
		`SELECT model FROM usage_ledger
		  WHERE thread_id = ? AND model <> '' AND model <> 'unknown'
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		targetThreadID,
	).Scan(&result.Model); err != nil && err != sql.ErrNoRows {
		return result, fmt.Errorf("store: read imported prefix model for %s: %w", targetThreadID, err)
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("store: commit imported history prefix for %s: %w", targetThreadID, err)
	}
	return result, nil
}
