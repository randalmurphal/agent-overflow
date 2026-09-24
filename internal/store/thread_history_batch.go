package store

import (
	"database/sql"
	"fmt"
)

// HistoryRow is one settled timeline row of a batch: the item plus the
// payload it owns, or nil when it has none.
type HistoryRow struct {
	Item    Item
	Payload *Payload
}

// ThreadHistoryBatch is a block of already-settled local history for one
// thread: the turns, the settlement of each, and the rows that belong to
// them, in the order they are to be written.
type ThreadHistoryBatch struct {
	Turns       []Turn
	Completions []TurnCompletion
	Rows        []HistoryRow
}

// InsertThreadHistory writes one block of settled history for threadID in a
// single transaction.
//
// It is the bulk form of InsertTurn + InsertItem / InsertItemWithPayload +
// UpdateTurnCompleted: the same columns, the same settle-time search
// indexing and the same per-row history triggers, at one commit instead of
// one per row. Tens of thousands of rows are a realistic thread and a
// transaction per row is the whole cost of writing one.
//
// ApplyImportBatch is the sibling for the shared imported-history arm.
// This one writes `items` and `payloads`, so the thread stamp moves per row
// exactly as a one-at-a-time writer moves it; nothing here suppresses a
// trigger.
//
// Like InsertItem it does not bump thread activity or updated_at: the batch
// is history that already happened, and the turn settlements it writes are
// what the sidebar's activity clock reads.
func (s *Store) InsertThreadHistory(threadID string, batch ThreadHistoryBatch) error {
	if threadID == "" {
		return fmt.Errorf("store: insert thread history: thread id is required")
	}
	turns, rows, err := scopeThreadHistoryBatch(threadID, batch)
	if err != nil {
		return err
	}

	// The block carries no card: it recomputes the chains its rows join.
	return s.bulkWriteItems(threadID, "thread history", func(tx *sql.Tx, w *cardWrite) error {
		if err := insertHistoryTurnsTx(tx, threadID, turns); err != nil {
			return err
		}
		if err := importTurnCompletionsTx(tx, threadID, batch.Completions); err != nil {
			return err
		}
		return insertHistoryRowsTx(tx, w, rows)
	})
}

// scopeThreadHistoryBatch stamps threadID onto every row that carries one
// and applies the item defaults, returning copies so the caller's batch is
// not mutated. A row naming a DIFFERENT thread is refused rather than
// rewritten, exactly as scopeImportBatch refuses one.
func scopeThreadHistoryBatch(threadID string, batch ThreadHistoryBatch) ([]Turn, []HistoryRow, error) {
	turns := make([]Turn, len(batch.Turns))
	for i, turn := range batch.Turns {
		if turn.TurnID == "" {
			return nil, nil, fmt.Errorf("store: thread history batch for thread %s: turn id is required", threadID)
		}
		if turn.ThreadID != "" && turn.ThreadID != threadID {
			return nil, nil, fmt.Errorf(
				"store: thread history batch turn %s belongs to thread %s, not %s",
				turn.TurnID, turn.ThreadID, threadID,
			)
		}
		turn.ThreadID = threadID
		turns[i] = turn
	}

	rows := make([]HistoryRow, len(batch.Rows))
	for i, row := range batch.Rows {
		if row.Item.ID == "" {
			return nil, nil, fmt.Errorf("store: thread history batch for thread %s: item id is required", threadID)
		}
		if row.Item.ThreadID != "" && row.Item.ThreadID != threadID {
			return nil, nil, fmt.Errorf(
				"store: thread history batch item %s belongs to thread %s, not %s",
				row.Item.ID, row.Item.ThreadID, threadID,
			)
		}
		if row.Payload != nil && row.Item.PayloadID != row.Payload.ID {
			return nil, nil, fmt.Errorf(
				"store: thread history batch item %s references payload %q, not the payload %q it carries",
				row.Item.ID, row.Item.PayloadID, row.Payload.ID,
			)
		}
		row.Item.ThreadID = threadID
		applyItemDefaults(&row.Item)
		rows[i] = row
	}
	return turns, rows, nil
}

func insertHistoryTurnsTx(tx *sql.Tx, threadID string, turns []Turn) error {
	if len(turns) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 VALUES (?, ?, ?, ?, NULL, '', '', '', '', ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare thread history turn insert for thread %s: %w", threadID, err)
	}
	defer stmt.Close()

	for _, turn := range turns {
		if _, err := stmt.Exec(
			turn.TurnID, turn.ThreadID, turn.TurnIndex, turn.StartedAt, turn.ProviderTurnID,
		); err != nil {
			return fmt.Errorf("store: insert thread history turn %s: %w", turn.TurnID, err)
		}
	}
	return nil
}

// insertHistoryRowsTx writes each row's payload before the item that
// references it, and runs the settle-time search index hook per row, which
// is what insertItemTx does for a single row, as it records each row in w.
func insertHistoryRowsTx(tx *sql.Tx, w *cardWrite, rows []HistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	threadID := w.threadID
	payloadStmt, err := tx.Prepare(payloadInsertSQL)
	if err != nil {
		return fmt.Errorf("store: prepare thread history payload insert for thread %s: %w", threadID, err)
	}
	defer payloadStmt.Close()
	itemStmt, err := tx.Prepare(itemInsertSQL)
	if err != nil {
		return fmt.Errorf("store: prepare thread history item insert for thread %s: %w", threadID, err)
	}
	defer itemStmt.Close()
	adoptingStmt, err := tx.Prepare(itemInsertAdoptingSQL)
	if err != nil {
		return fmt.Errorf("store: prepare thread history adopting item insert for thread %s: %w", threadID, err)
	}
	defer adoptingStmt.Close()

	indexBatch := make([]Item, 0, 128)
	for i, row := range rows {
		if row.Payload != nil {
			if _, err := payloadStmt.Exec(payloadInsertArgs(threadID, *row.Payload)...); err != nil {
				return fmt.Errorf("store: insert thread history payload %s: %w", row.Payload.ID, err)
			}
		}
		card := subagentRowOf(row.Item)
		if err := w.check(card); err != nil {
			return err
		}
		hasChild := false
		if card.anchorable() || card.parentID != "" {
			if err := adoptingStmt.QueryRow(itemInsertArgs(row.Item)...).Scan(&hasChild); err != nil {
				return fmt.Errorf("store: insert thread history item %s: %w", row.Item.ID, err)
			}
		} else if _, err := itemStmt.Exec(itemInsertArgs(row.Item)...); err != nil {
			return fmt.Errorf("store: insert thread history item %s: %w", row.Item.ID, err)
		}
		w.inserted(card, hasChild)
		indexBatch = append(indexBatch, row.Item)
		if len(indexBatch) == cap(indexBatch) || i == len(rows)-1 {
			if err := indexSettledItemsTx(tx, indexBatch, ThreadSearchSourceItem); err != nil {
				return err
			}
			indexBatch = indexBatch[:0]
		}
	}
	return nil
}
