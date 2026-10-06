package store

import (
	"database/sql"
	"errors"
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
	one, chunk, err := prepareHistoryInsertTx(tx,
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)`,
		`(?, ?, ?, ?, NULL, '', '', '', '', ?)`, len(turns))
	if err != nil {
		return fmt.Errorf("store: prepare thread history turn insert for thread %s: %w", threadID, err)
	}
	defer one.Close()
	if chunk != nil {
		defer chunk.Close()
	}
	err = execHistoryRows(one, chunk, turns, func(turn Turn) []any {
		return []any{turn.TurnID, turn.ThreadID, turn.TurnIndex, turn.StartedAt, turn.ProviderTurnID}
	}, func(turn Turn) string { return turn.TurnID })
	if err != nil {
		return fmt.Errorf("store: insert thread history turn %w", err)
	}
	return nil
}

// historyRowsPerInsert is how many rows one multi-row INSERT of
// InsertThreadHistory writes. An items or turns INSERT fires triggers and
// can abort, so inside a transaction SQLite first copies every existing
// page it changes to a statement journal, a temp file once it passes
// 64 KiB. A statement per row copies the same index leaves and thread row
// once per row; a statement per chunk copies them once per chunk. The
// driver binds parameters in time quadratic in their number per
// statement, which bounds the chunk.
const historyRowsPerInsert = 64

// prepareHistoryInsertTx prepares the single-row INSERT prefix+values and,
// when rows can fill one, the INSERT of historyRowsPerInsert rows.
// Prepared statements keep the chunk text out of the statement cache.
func prepareHistoryInsertTx(tx *sql.Tx, prefix, values string, rows int) (one, chunk *sql.Stmt, err error) {
	one, err = tx.Prepare(prefix + ` VALUES ` + values)
	if err != nil || rows < historyRowsPerInsert {
		return one, nil, err
	}
	chunk, err = tx.Prepare(importInsertQuery(prefix, values, historyRowsPerInsert))
	if err != nil {
		return nil, nil, errors.Join(err, one.Close())
	}
	return one, chunk, nil
}

// execHistoryRows inserts rows in order: each full chunk through chunk,
// the rest through one. SQLite fires row triggers for each row in
// insertion order within a statement, so the rows, stamps and trigger
// effects are those of a statement per row.
func execHistoryRows[T any](one, chunk *sql.Stmt, rows []T, argsFor func(T) []any, label func(T) string) error {
	for chunk != nil && len(rows) >= historyRowsPerInsert {
		block := rows[:historyRowsPerInsert]
		var args []any
		for _, row := range block {
			args = append(args, argsFor(row)...)
		}
		if _, err := chunk.Exec(args...); err != nil {
			return fmt.Errorf("%s..%s: %w", label(block[0]), label(block[len(block)-1]), err)
		}
		rows = rows[historyRowsPerInsert:]
	}
	for _, row := range rows {
		if _, err := one.Exec(argsFor(row)...); err != nil {
			return fmt.Errorf("%s: %w", label(row), err)
		}
	}
	return nil
}

// insertHistoryRowsTx writes each row's payload before the item that
// references it, and runs the settle-time search index hook on the rows
// in order, which is what insertItemTx does for a single row, as it
// records each row in w. A row that may adopt rows already stored (an
// anchor, or a row with a parent) is written alone so its RETURNING sees
// every row before it; the runs of other rows between them go through
// execHistoryRows.
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
	itemStmt, chunkStmt, err := prepareHistoryInsertTx(tx, itemInsertPrefix, itemInsertValues, len(rows))
	if err != nil {
		return fmt.Errorf("store: prepare thread history item insert for thread %s: %w", threadID, err)
	}
	defer itemStmt.Close()
	if chunkStmt != nil {
		defer chunkStmt.Close()
	}
	adoptingStmt, err := tx.Prepare(itemInsertAdoptingSQL)
	if err != nil {
		return fmt.Errorf("store: prepare thread history adopting item insert for thread %s: %w", threadID, err)
	}
	defer adoptingStmt.Close()

	indexBatch := make([]Item, 0, 128)
	written := func(item Item) error {
		indexBatch = append(indexBatch, item)
		if len(indexBatch) < cap(indexBatch) {
			return nil
		}
		err := indexSettledItemsTx(tx, indexBatch, ThreadSearchSourceItem)
		indexBatch = indexBatch[:0]
		return err
	}
	// rows[plain:end] are rows that adopt nothing, not yet written.
	plain := 0
	flush := func(end int) error {
		run := rows[plain:end]
		plain = end
		if err := execHistoryRows(itemStmt, chunkStmt, run,
			func(row HistoryRow) []any { return itemInsertArgs(row.Item) },
			func(row HistoryRow) string { return row.Item.ID },
		); err != nil {
			return fmt.Errorf("store: insert thread history item %w", err)
		}
		for _, row := range run {
			w.inserted(subagentRowOf(row.Item), false)
			if err := written(row.Item); err != nil {
				return err
			}
		}
		return nil
	}
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
		if !card.anchorable() && card.parentID == "" {
			continue
		}
		if err := flush(i); err != nil {
			return err
		}
		plain = i + 1
		hasChild := false
		if err := adoptingStmt.QueryRow(itemInsertArgs(row.Item)...).Scan(&hasChild); err != nil {
			return fmt.Errorf("store: insert thread history item %s: %w", row.Item.ID, err)
		}
		w.inserted(card, hasChild)
		if err := written(row.Item); err != nil {
			return err
		}
	}
	if err := flush(len(rows)); err != nil {
		return err
	}
	return indexSettledItemsTx(tx, indexBatch, ThreadSearchSourceItem)
}
