package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CrashedTurn identifies one turn row that RecoverCrashedTurns settled:
// the previous app instance died while this (thread, turn) was
// in-flight.
type CrashedTurn struct {
	ThreadID  string
	TurnIndex int
}

// RecoverCrashedTurns settles every turn row left with
// completed_at=NULL by a previous app instance and flips that turn's
// stranded streaming/running items to errored. Callers MUST only run
// this while no provider session exists (App boot, before any session
// can spawn); at that point a NULL completed_at is provably a crash
// leftover, because every in-app session death already settles its row
// via triage's synthesized truncated turn-complete.
//
// The settle mirrors what that synthesized turn-complete writes:
// completed_at=now, stop_reason='interrupted'. The item flip mirrors
// triage's flipTurnItemsErrored: summarise rewrites each flipped
// row's summary (callers pass the idempotent " — interrupted" suffix
// convention) and backgrounded tool_call launches are exempt
// (invariant 24: their disposition belongs to the background recovery
// sweep, which writes completion siblings instead).
//
// Everything runs in a single transaction so an N-thread crash pays
// one WAL commit and a mid-sweep crash leaves all rows untouched for
// the next boot. The SELECT is O(crashed rows) via the partial index
// idx_turns_inflight. Thread activity is deliberately NOT bumped:
// sweeping crash residue is not a user interaction (matches
// RecoverCodexBackgroundRuntime).
//
// Before the sweep, RecoverSubagentCards recomputes the subagent cards
// of every agent the previous instance left running, which the sweep is
// about to settle: rows written under them after their last flush are in
// no stamp. A failure there does not stop the sweep; both are returned.
//
// Returns the settled turns so the caller can log the repair.
func (s *Store) RecoverCrashedTurns(summarise func(string) string, now int64) ([]CrashedTurn, error) {
	var cardsErr error
	if _, err := s.RecoverSubagentCards(context.Background()); err != nil {
		cardsErr = fmt.Errorf("store: recover subagent cards: %w", err)
	}
	crashed, err := s.recoverCrashedTurns(summarise, now)
	return crashed, errors.Join(cardsErr, err)
}

// recoverCrashedTurns stops agents in threads whose cards it does not
// lock (sweepItemWrites): any card left is flushed first.
func (s *Store) recoverCrashedTurns(summarise func(string) string, now int64) ([]CrashedTurn, error) {
	flushErr := s.FlushAllSubagentCards()
	crashed, err := s.sweepCrashedTurns(summarise, now)
	return crashed, errors.Join(flushErr, err)
}

func (s *Store) sweepCrashedTurns(summarise func(string) string, now int64) ([]CrashedTurn, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin crashed-turn recovery tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT thread_id, turn_index FROM turns
		  WHERE completed_at IS NULL
		  ORDER BY thread_id, turn_index`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: crashed-turn select: %w", err)
	}
	var crashed []CrashedTurn
	for rows.Next() {
		var c CrashedTurn
		if err := rows.Scan(&c.ThreadID, &c.TurnIndex); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: crashed-turn scan: %w", err)
		}
		crashed = append(crashed, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: crashed-turn rows err: %w", err)
	}
	rows.Close()

	if len(crashed) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit crashed-turn recovery (no rows): %w", err)
		}
		return nil, nil
	}

	if _, err := tx.Exec(
		`UPDATE turns
		    SET completed_at = ?, stop_reason = 'interrupted'
		  WHERE completed_at IS NULL`,
		now,
	); err != nil {
		return nil, fmt.Errorf("store: crashed-turn settle: %w", err)
	}

	for _, c := range crashed {
		if err := s.flipCrashedTurnItemsTx(tx, c, summarise, now); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit crashed-turn recovery tx: %w", err)
	}
	return crashed, nil
}

// flipCrashedTurnItemsTx flips one crashed turn's streaming/running
// items to errored inside the recovery transaction: no live process can
// ever finish them. A fork settles the running rows it inherits the same
// way when it is created (CreatePointerFork).
//
// Rows flip to `errored` with `summarise(summary)` and updated_at=now.
// Backgrounded tool_call launches are EXEMPT (invariant 24), matching
// triage's flipTurnItemsErrored: their disposition belongs to the
// background recovery sweep, which writes completion siblings instead.
// So is every row a background agent owns (agent_rows.go): the sweep's
// completion sibling settles those with it (UpsertAgentEnd).
// Nothing else on the row is touched, `decision` included. An
// approval that never resolved is only answerable while triage holds
// its pending request in memory, so the status flip is the whole
// settle for those rows too (a rebooted app has no triage state). A
// flipped agent child's summary can move its launch's card; the settle
// carries no card and recomputes those chains through the sweep's bulk
// writes (sweepItemWrites).
func (s *Store) flipCrashedTurnItemsTx(tx *sql.Tx, c CrashedTurn, summarise func(string) string, now int64) error {
	threadID := c.ThreadID
	rows, err := tx.Query(
		`SELECT `+subagentRowColumns("")+` FROM items
		  WHERE thread_id = ? AND turn_index = ?
		    AND status IN ('streaming', 'running')
		    AND NOT (is_background = 1 AND kind = 'tool_call')`,
		threadID, c.TurnIndex,
	)
	if err != nil {
		return fmt.Errorf("store: stranded item select %s/%d: %w", threadID, c.TurnIndex, err)
	}
	var flips []subagentRow
	for rows.Next() {
		f, err := scanSubagentRow(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("store: stranded item scan: %w", err)
		}
		flips = append(flips, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: stranded item rows err: %w", err)
	}
	rows.Close()
	// An agent's rows are settled with its session_died completion
	// (Router.RecoverOrphanedBackgroundTasks), whichever turn wrote them.
	flips, err = withoutAgentOwned(tx, threadID, flips, func(f subagentRow) string { return f.parentID })
	if err != nil {
		return fmt.Errorf("store: stranded item ownership %s/%d: %w", threadID, c.TurnIndex, err)
	}

	w := s.sweepItemWrites(tx, threadID)
	for _, f := range flips {
		row := f
		row.status, row.summary = "errored", summarise(f.summary)
		if err := w.updated(f, row); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE items
			    SET status = 'errored', summary = ?, updated_at = ?
			  WHERE thread_id = ? AND id = ?`,
			row.summary, now, threadID, f.id,
		); err != nil {
			return fmt.Errorf("store: stranded item flip %s: %w", f.id, err)
		}
	}
	return w.finish()
}
