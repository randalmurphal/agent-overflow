package store

import (
	"database/sql"
	"fmt"
)

// Turn is one row in the turns table — a record of a single user → assistant
// round-trip on a thread.
//
// CompletedAt is a pointer because NULL is load-bearing: it means
// "in-flight or crashed mid-turn." We never write a synthetic CompletedAt
// to dismiss a stuck row. The frontend treats a NULL CompletedAt on
// rehydration as "interrupted," separate from the live-push
// "provider:turn_started" path that drives the working indicator.
//
// See docs/architecture/turn-lifecycle.md §Turn lifecycle for the full
// mental model and docs/architecture/invariants.md #22-24 for the rules
// that depend on this shape.
type Turn struct {
	TurnID             string `json:"turnId"`
	ThreadID           string `json:"threadId"`
	TurnIndex          int    `json:"turnIndex"`
	StartedAt          int64  `json:"startedAt"`
	CompletedAt        *int64 `json:"completedAt,omitempty"`
	StopReason         string `json:"stopReason,omitempty"`
	AssistantMessageID string `json:"assistantMessageId,omitempty"`
	TokenUsageJSON     string `json:"tokenUsageJson,omitempty"`
	ErrorMessage       string `json:"errorMessage,omitempty"`
}

// turnColumns is the canonical SELECT projection for scanTurnRow. Keep in
// sync with the Turn struct and every INSERT/UPDATE site in this file so
// the column order is defined in exactly one place.
const turnColumns = `turn_id, thread_id, turn_index, started_at, completed_at,
    stop_reason, assistant_message_id, token_usage_json, error_message`

// scanTurnRow hydrates one Turn from a *sql.Row or *sql.Rows. completed_at
// is scanned via sql.NullInt64 so we can preserve NULL as `*int64 == nil`
// in the returned struct (see the struct doc for why nullability matters).
func scanTurnRow(scanner interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	var completedAt sql.NullInt64
	if err := scanner.Scan(
		&t.TurnID, &t.ThreadID, &t.TurnIndex, &t.StartedAt, &completedAt,
		&t.StopReason, &t.AssistantMessageID, &t.TokenUsageJSON, &t.ErrorMessage,
	); err != nil {
		return Turn{}, err
	}
	if completedAt.Valid {
		v := completedAt.Int64
		t.CompletedAt = &v
	}
	return t, nil
}

// InsertTurn creates a new turn row with completed_at=NULL. The caller
// passes turn_index (triage computes it under the send mutex); the store
// does not auto-assign it. A duplicate (thread_id, turn_index) or
// duplicate turn_id returns a UNIQUE-constraint error — callers should
// treat it as a bug, not a recoverable collision.
func (s *Store) InsertTurn(turn Turn) error {
	if turn.TurnID == "" {
		return fmt.Errorf("store: insert turn: turn id is required")
	}
	if turn.ThreadID == "" {
		return fmt.Errorf("store: insert turn: thread id is required")
	}
	_, err := s.db.Exec(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message)
		 VALUES (?, ?, ?, ?, NULL, '', '', '', '')`,
		turn.TurnID, turn.ThreadID, turn.TurnIndex, turn.StartedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert turn %s: %w", turn.TurnID, err)
	}
	return nil
}

// UpdateTurnCompleted flips completed_at + stop_reason +
// assistant_message_id + token_usage_json + error_message on an existing
// turn. Callers pass every settle-time field (use empty string / zero
// for values they don't have). started_at and turn_index are preserved.
// Returns sql.ErrNoRows when no row matches the turn_id — triage treats
// that as a bug because UpdateTurnCompleted is always paired with a
// prior InsertTurn.
func (s *Store) UpdateTurnCompleted(
	turnID string,
	completedAt int64,
	stopReason, assistantMessageID, tokenUsageJSON, errorMessage string,
) error {
	if turnID == "" {
		return fmt.Errorf("store: update turn completed: turn id is required")
	}
	result, err := s.db.Exec(
		`UPDATE turns
		    SET completed_at = ?,
		        stop_reason = ?,
		        assistant_message_id = ?,
		        token_usage_json = ?,
		        error_message = ?
		  WHERE turn_id = ?`,
		completedAt, stopReason, assistantMessageID, tokenUsageJSON, errorMessage, turnID,
	)
	if err != nil {
		return fmt.Errorf("store: update turn %s: %w", turnID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update turn %s", turnID)); err != nil {
		return err
	}
	return nil
}

// GetTurn returns a single turn by its provider-assigned id. Returns
// (Turn{}, false, nil) when no row exists — the miss is not an error,
// so callers can use the bool to branch cleanly.
func (s *Store) GetTurn(turnID string) (Turn, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+turnColumns+` FROM turns WHERE turn_id = ?`,
		turnID,
	)
	turn, err := scanTurnRow(row)
	if err == sql.ErrNoRows {
		return Turn{}, false, nil
	}
	if err != nil {
		return Turn{}, false, fmt.Errorf("store: get turn %s: %w", turnID, err)
	}
	return turn, true, nil
}

// ListRecentTurns returns the N most recent turns for a thread, newest
// first (turn_index DESC). Used by the frontend on thread-switch to
// hydrate latestSettledTurn. A non-positive limit returns an empty
// slice without hitting the database.
func (s *Store) ListRecentTurns(threadID string, limit int) ([]Turn, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT `+turnColumns+` FROM turns
		  WHERE thread_id = ?
		  ORDER BY turn_index DESC
		  LIMIT ?`,
		threadID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list recent turns for %s: %w", threadID, err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		turn, err := scanTurnRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn: %w", err)
		}
		out = append(out, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate recent turns for %s: %w", threadID, err)
	}
	return out, nil
}

// GetActiveTurn returns the most recent in-flight (completed_at=NULL)
// turn for a thread. Called on resume to decide whether a crash-
// interrupted turn is still visible to the UI.
//
// In normal operation at most one turn per thread is in-flight at a
// time (triage serialises turn-start via the send mutex), but the
// ORDER BY defends against stale rows left over from prior crashes:
// we want the latest surviving in-flight turn, not the earliest.
//
// Returns (Turn{}, false, nil) when no in-flight row exists.
func (s *Store) GetActiveTurn(threadID string) (Turn, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+turnColumns+` FROM turns
		  WHERE thread_id = ? AND completed_at IS NULL
		  ORDER BY turn_index DESC
		  LIMIT 1`,
		threadID,
	)
	turn, err := scanTurnRow(row)
	if err == sql.ErrNoRows {
		return Turn{}, false, nil
	}
	if err != nil {
		return Turn{}, false, fmt.Errorf("store: get active turn for %s: %w", threadID, err)
	}
	return turn, true, nil
}
