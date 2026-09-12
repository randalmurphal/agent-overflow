package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// PutUsageProgress replaces reported token snapshots in one provider segment.
// Pending usage is a cache of provider facts, not an estimate. It deliberately
// survives interrupted turns, process teardown, and thread deletion, just like
// the ledger. A new process must use a new scope.
func (s *Store) PutUsageProgress(scope, segment string, rows []UsageLedgerRow) (changed bool, err error) {
	if scope == "" || segment == "" {
		return false, fmt.Errorf("store: usage progress requires scope and segment")
	}
	for _, row := range rows {
		if err := validateProgressRow(row); err != nil {
			return false, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: usage progress begin: %w", err)
	}
	defer rollbackUsageTx(tx, &err)
	for _, row := range rows {
		if usageTokens(row) == [5]int{} {
			continue
		}
		result, writeErr := tx.Exec(`INSERT INTO usage_pending
		(thread_id, scope, segment, model, created_at, project_id, work_item_id, turn_id, provider,
		 input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, reasoning_output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(thread_id, scope, segment, model) DO UPDATE SET
		 input_tokens = MAX(input_tokens, excluded.input_tokens),
		 output_tokens = MAX(output_tokens, excluded.output_tokens),
		 cache_read_input_tokens = MAX(cache_read_input_tokens, excluded.cache_read_input_tokens),
		 cache_creation_input_tokens = MAX(cache_creation_input_tokens, excluded.cache_creation_input_tokens),
		 reasoning_output_tokens = MAX(reasoning_output_tokens, excluded.reasoning_output_tokens)
		WHERE input_tokens < excluded.input_tokens OR output_tokens < excluded.output_tokens
		 OR cache_read_input_tokens < excluded.cache_read_input_tokens
		 OR cache_creation_input_tokens < excluded.cache_creation_input_tokens
		 OR reasoning_output_tokens < excluded.reasoning_output_tokens`,
			row.ThreadID, scope, segment, row.Model, row.CreatedAt, row.ProjectID, row.WorkItemID, row.TurnID, row.Provider,
			row.InputTokens, row.OutputTokens, row.CacheReadInputTokens, row.CacheCreationInputTokens, row.ReasoningOutputTokens)
		if writeErr != nil {
			return false, fmt.Errorf("store: usage progress write: %w", writeErr)
		}
		n, countErr := result.RowsAffected()
		if countErr != nil {
			return false, fmt.Errorf("store: usage progress changed: %w", countErr)
		}
		changed = changed || n > 0
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: usage progress commit: %w", err)
	}
	return changed, nil
}

func validateProgressRow(row UsageLedgerRow) error {
	if row.ThreadID == "" || row.TurnID == "" || row.Model == "" || row.Provider == "" {
		return fmt.Errorf("store: usage progress requires thread, turn, provider, and model")
	}
	if row.CostUSD != 0 || row.CostSource != "" {
		return fmt.Errorf("store: usage progress cannot supply cost")
	}
	for _, n := range usageTokens(row) {
		if n < 0 {
			return fmt.Errorf("store: usage progress tokens must be nonnegative")
		}
	}
	return nil
}

func usageTokens(row UsageLedgerRow) [5]int {
	return [5]int{row.InputTokens, row.OutputTokens, row.CacheReadInputTokens, row.CacheCreationInputTokens, row.ReasoningOutputTokens}
}

func rollbackUsageTx(tx *sql.Tx, result *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*result = errors.Join(*result, fmt.Errorf("store: usage rollback: %w", err))
	}
}

// AppendUsageAndReconcile atomically exchanges pending tokens for the final
// accounting that includes them. Only this process's matching model is
// consumed, since a provider's final deltas can cover earlier interrupted
// turns of the same process. A settled turn's own pending rows are then
// retired whatever scope reported them: the ledger row is that turn's final
// accounting. An empty interrupt result leaves unaccounted tokens available
// to usage queries. Cost is never subtracted from token progress.
func (s *Store) AppendUsageAndReconcile(scope string, rows []UsageLedgerRow) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: usage settlement begin: %w", err)
	}
	defer rollbackUsageTx(tx, &err)
	for _, row := range rows {
		if row.ThreadID == "" || row.Model == "" {
			return fmt.Errorf("store: usage settlement requires thread and model")
		}
		if scope == "" {
			continue
		}
		if err := consumeUsagePending(tx, scope, row); err != nil {
			return err
		}
	}
	if err := appendUsageTx(tx, rows); err != nil {
		return err
	}
	for _, row := range rows {
		if row.TurnID == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM usage_pending WHERE thread_id = ? AND turn_id = ?`, row.ThreadID, row.TurnID); err != nil {
			return fmt.Errorf("store: retire settled pending usage: %w", err)
		}
	}
	return tx.Commit()
}

func consumeUsagePending(tx *sql.Tx, scope string, row UsageLedgerRow) error {
	if row.AccountingModel != "" {
		row.Model = row.AccountingModel
	}
	remaining := usageTokens(row)
	for _, n := range remaining {
		if n < 0 {
			return fmt.Errorf("store: usage settlement tokens must be nonnegative")
		}
	}
	type pending struct {
		segment string
		created int64
		tokens  [5]int
	}
	var cursor *pending
	for remaining != [5]int{} {
		query := `SELECT segment, created_at, input_tokens, output_tokens, cache_read_input_tokens,
   cache_creation_input_tokens, reasoning_output_tokens FROM usage_pending
   WHERE thread_id = ? AND scope = ? AND model = ?`
		args := []any{row.ThreadID, scope, row.Model}
		if cursor != nil {
			query += ` AND (created_at, segment) > (?, ?)`
			args = append(args, cursor.created, cursor.segment)
		}
		query += ` ORDER BY created_at, segment LIMIT 64`
		rows, err := tx.Query(query, args...)
		if err != nil {
			return fmt.Errorf("store: read pending usage: %w", err)
		}
		batch := make([]pending, 0, 64)
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.segment, &p.created, &p.tokens[0], &p.tokens[1], &p.tokens[2], &p.tokens[3], &p.tokens[4]); err != nil {
				return errors.Join(fmt.Errorf("store: scan pending usage: %w", err), rows.Close())
			}
			for i, n := range p.tokens {
				used := min(n, remaining[i])
				p.tokens[i] -= used
				remaining[i] -= used
			}
			batch = append(batch, p)
			if remaining == [5]int{} {
				break
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("store: read pending usage: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, p := range batch {
			if p.tokens == [5]int{} {
				_, err = tx.Exec(`DELETE FROM usage_pending WHERE thread_id = ? AND scope = ? AND segment = ? AND model = ?`, row.ThreadID, scope, p.segment, row.Model)
			} else {
				_, err = tx.Exec(`UPDATE usage_pending SET input_tokens = ?, output_tokens = ?, cache_read_input_tokens = ?,
     cache_creation_input_tokens = ?, reasoning_output_tokens = ? WHERE thread_id = ? AND scope = ? AND segment = ? AND model = ?`,
					p.tokens[0], p.tokens[1], p.tokens[2], p.tokens[3], p.tokens[4], row.ThreadID, scope, p.segment, row.Model)
			}
			if err != nil {
				return fmt.Errorf("store: reconcile pending usage: %w", err)
			}
		}
		cursor = &batch[len(batch)-1]
	}
	return nil
}
