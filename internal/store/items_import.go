package store

import (
	"database/sql"
	"fmt"
)

// ImportRow is one timeline row an import produces: the item plus the
// payloads it owns. Payload is the item's result/content blob and
// InputPayload the tool-call launch input; both are nil when the row has
// none, which is the common case.
type ImportRow struct {
	Item         Item
	Payload      *Payload
	InputPayload *Payload
}

// TurnCompletion settles one turn of an import. It carries every field
// UpdateTurnCompleted takes, including the original CompletedAt — the
// sidebar's activity ordering and LatestTurnCompletedAt derive from that
// column, so an imported thread that restamped it would sort as if it had
// just run.
type TurnCompletion struct {
	TurnID             string
	CompletedAt        int64
	StopReason         string
	AssistantMessageID string
	TokenUsageJSON     string
	ErrorMessage       string
}

// ImportBatch is everything one imported session branch contributes to a
// thread. It is built whole by the import writer and applied in one
// transaction: a 400-row session costs one fsync instead of 400, and a
// failure part-way leaves no half-imported thread behind.
//
// Turns and Rows must be NEW — ApplyImportBatch inserts them, so a
// re-applied batch fails on the primary key rather than silently
// overwriting history. TurnCompletions is the one exception: a refresh
// settles a turn whose row an earlier import already wrote.
type ImportBatch struct {
	Turns           []Turn
	TurnCompletions []TurnCompletion
	Rows            []ImportRow
	Usage           []UsageLedgerRow
}

// ApplyImportBatch writes one session import into threadID in a single
// transaction, in dependency order: turns, their completions, then each
// row's payloads before the item that references them, then usage.
//
// It deliberately does NOT touch threads.updated_at or thread activity.
// An import replays history that already happened; bumping either would
// float every imported thread to the top of the sidebar and mark it
// unread, which is the opposite of what the original timestamps say.
// Item timestamps are likewise written verbatim — applyItemDefaults only
// fills zeros, and imported rows always carry the provider's own.
func (s *Store) ApplyImportBatch(threadID string, batch ImportBatch) error {
	if threadID == "" {
		return fmt.Errorf("store: apply import batch: thread id is required")
	}
	turns, rows, usage, err := scopeImportBatch(threadID, batch)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin import batch tx for thread %s: %w", threadID, err)
	}
	defer tx.Rollback()

	// One prepared statement per shape, reused across the batch. A real
	// session is hundreds of rows and each is the same INSERT; re-parsing
	// the SQL per row is the cost this avoids, and it is the same shape
	// appendUsageTx already uses for the usage rows below.
	if err := importTurnsTx(tx, threadID, turns); err != nil {
		return err
	}
	if err := importTurnCompletionsTx(tx, threadID, batch.TurnCompletions); err != nil {
		return err
	}
	if err := importRowsTx(tx, rows); err != nil {
		return err
	}
	if err := appendUsageTx(tx, usage); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit import batch tx for thread %s: %w", threadID, err)
	}
	return nil
}

// importTurnsTx inserts the batch's turns. Turns are born with a NULL
// completed_at and their completions arrive next; see turnState in
// internal/sessionimport for why none may survive the batch that way.
func importTurnsTx(tx *sql.Tx, threadID string, turns []Turn) error {
	if len(turns) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 VALUES (?, ?, ?, ?, NULL, '', '', '', '', ?)`)
	if err != nil {
		return fmt.Errorf("store: import turn prepare for thread %s: %w", threadID, err)
	}
	defer stmt.Close()

	for _, turn := range turns {
		if turn.TurnID == "" {
			return fmt.Errorf("store: import batch for thread %s: turn id is required", threadID)
		}
		if _, err := stmt.Exec(
			turn.TurnID, turn.ThreadID, turn.TurnIndex, turn.StartedAt, turn.ProviderTurnID,
		); err != nil {
			return fmt.Errorf("store: import turn %s into thread %s: %w", turn.TurnID, threadID, err)
		}
	}
	return nil
}

// importTurnCompletionsTx settles the batch's turns. These are UPDATEs, not
// inserts, because a refresh settles a turn an earlier import wrote — which
// is also why a completion that matched no row is an error rather than a
// silent no-op.
func importTurnCompletionsTx(tx *sql.Tx, threadID string, completions []TurnCompletion) error {
	if len(completions) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`UPDATE turns
		    SET completed_at = ?,
		        stop_reason = ?,
		        assistant_message_id = ?,
		        token_usage_json = ?,
		        error_message = ?
		  WHERE turn_id = ? AND thread_id = ?`)
	if err != nil {
		return fmt.Errorf("store: import turn completion prepare for thread %s: %w", threadID, err)
	}
	defer stmt.Close()

	for _, completion := range completions {
		if completion.TurnID == "" {
			return fmt.Errorf("store: import batch for thread %s: turn completion id is required", threadID)
		}
		result, err := stmt.Exec(
			completion.CompletedAt, completion.StopReason, completion.AssistantMessageID,
			completion.TokenUsageJSON, completion.ErrorMessage, completion.TurnID, threadID,
		)
		if err != nil {
			return fmt.Errorf("store: import turn completion %s: %w", completion.TurnID, err)
		}
		if err := requireRowsAffected(
			result, fmt.Sprintf("store: import turn completion %s", completion.TurnID),
		); err != nil {
			return err
		}
	}
	return nil
}

// importRowsTx writes each row's payloads before the item that references
// them, so the FK is satisfied without deferring it.
func importRowsTx(tx *sql.Tx, rows []ImportRow) error {
	if len(rows) == 0 {
		return nil
	}
	payloadStmt, err := tx.Prepare(payloadInsertSQL)
	if err != nil {
		return fmt.Errorf("store: import payload prepare: %w", err)
	}
	defer payloadStmt.Close()
	itemStmt, err := tx.Prepare(itemInsertSQL)
	if err != nil {
		return fmt.Errorf("store: import item prepare: %w", err)
	}
	defer itemStmt.Close()

	for _, row := range rows {
		for _, payload := range []*Payload{row.InputPayload, row.Payload} {
			if payload == nil {
				continue
			}
			if _, err := payloadStmt.Exec(payloadInsertArgs(*payload)...); err != nil {
				return fmt.Errorf("store: import payload for item %s: %w", row.Item.ID, err)
			}
		}
		if _, err := itemStmt.Exec(itemInsertArgs(row.Item)...); err != nil {
			return fmt.Errorf("store: import item %s: %w", row.Item.ID, err)
		}
	}
	return nil
}

// scopeImportBatch stamps threadID onto every row that carries one and
// applies the item defaults, returning copies so the caller's batch is not
// mutated. A row whose own thread id names a DIFFERENT thread is refused
// rather than silently rewritten: that is a writer bug, and rewriting it
// would file one session's history under another thread.
func scopeImportBatch(threadID string, batch ImportBatch) ([]Turn, []ImportRow, []UsageLedgerRow, error) {
	turns := make([]Turn, len(batch.Turns))
	for i, turn := range batch.Turns {
		if turn.ThreadID != "" && turn.ThreadID != threadID {
			return nil, nil, nil, fmt.Errorf(
				"store: import batch turn %s belongs to thread %s, not %s",
				turn.TurnID, turn.ThreadID, threadID,
			)
		}
		turn.ThreadID = threadID
		turns[i] = turn
	}

	rows := make([]ImportRow, len(batch.Rows))
	for i, row := range batch.Rows {
		if row.Item.ThreadID != "" && row.Item.ThreadID != threadID {
			return nil, nil, nil, fmt.Errorf(
				"store: import batch item %s belongs to thread %s, not %s",
				row.Item.ID, row.Item.ThreadID, threadID,
			)
		}
		row.Item.ThreadID = threadID
		applyItemDefaults(&row.Item)
		rows[i] = row
	}

	usage := make([]UsageLedgerRow, len(batch.Usage))
	for i, entry := range batch.Usage {
		if entry.ThreadID != "" && entry.ThreadID != threadID {
			return nil, nil, nil, fmt.Errorf(
				"store: import batch usage row for turn %s belongs to thread %s, not %s",
				entry.TurnID, entry.ThreadID, threadID,
			)
		}
		entry.ThreadID = threadID
		usage[i] = entry
	}
	return turns, rows, usage, nil
}
