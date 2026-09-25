package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// Turn is one row in the turns table — a record of a single user → assistant
// round-trip on a thread.
//
// CompletedAt is a pointer because NULL is load-bearing while the app
// runs: it means "in-flight right now." A NULL row can only outlive its
// provider session when the whole app dies mid-turn — every in-app
// session death settles the row through triage's synthesized truncated
// turn-complete (stop_reason='interrupted'). RecoverCrashedTurns runs
// at boot, before any session can spawn, and settles those crash
// leftovers the same way, so a persisted NULL CompletedAt is never
// carried across app restarts. The durable "interrupted" signal the
// sidebar and rehydration read is stop_reason='interrupted', not the
// NULL itself.
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
	// ProviderTurnID is the provider-assigned wire turn id (Codex
	// `turn/started`), or "" when the provider has none on the wire
	// (Claude). TurnID is always thread-scoped; this field remains verbatim.
	// Kept separate from the TurnID PRIMARY KEY because a fork's copies
	// of its source's turns (the turns at its cut, or all of them once
	// materialized) take fresh row ids while preserving this value. It is
	// the `thread/fork` lastTurnId anchor the Codex revert/fork flows cut
	// history on.
	ProviderTurnID string `json:"providerTurnId,omitempty"`
}

// turnColumns is the canonical SELECT projection for scanTurnRow. Keep in
// sync with the Turn struct and every INSERT/UPDATE site in this file so
// the column order is defined in exactly one place.
const turnColumns = `turn_id, thread_id, turn_index, started_at, completed_at,
    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id`

// scanTurnRow hydrates one Turn from a *sql.Row or *sql.Rows. completed_at
// is scanned via sql.NullInt64 so we can preserve NULL as `*int64 == nil`
// in the returned struct (see the struct doc for why nullability matters).
func scanTurnRow(scanner interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	var completedAt sql.NullInt64
	if err := scanner.Scan(
		&t.TurnID, &t.ThreadID, &t.TurnIndex, &t.StartedAt, &completedAt,
		&t.StopReason, &t.AssistantMessageID, &t.TokenUsageJSON, &t.ErrorMessage,
		&t.ProviderTurnID,
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
// passes turn_index (triage computes it under the per-thread action lock); the store
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
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 VALUES (?, ?, ?, ?, NULL, '', '', '', '', ?)`,
		turn.TurnID, turn.ThreadID, turn.TurnIndex, turn.StartedAt, turn.ProviderTurnID,
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
// prior InsertTurn. A turn row a fork shows goes to the fork first
// (requireMutableTurnTx).
func (s *Store) UpdateTurnCompleted(
	turnID string,
	completedAt int64,
	stopReason, assistantMessageID, tokenUsageJSON, errorMessage string,
) error {
	if turnID == "" {
		return fmt.Errorf("store: update turn completed: turn id is required")
	}
	label := fmt.Sprintf("store: update turn %s", turnID)
	return s.writeTurn(turnID, label, func(tx *sql.Tx) error {
		result, err := tx.Exec(
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
			return fmt.Errorf("%s: %w", label, err)
		}
		return requireRowsAffected(result, label)
	}, func() error { return fmt.Errorf("%s: %w", label, sql.ErrNoRows) })
}

// writeTurn runs write on the turn row turnID in one transaction, after
// the forks that show the row got their copy (requireMutableTurnTx). With
// no turn of that id it returns missing's result instead.
func (s *Store) writeTurn(turnID, label string, write func(*sql.Tx) error, missing func() error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", label, err)
	}
	defer tx.Rollback()
	found, err := requireMutableTurnTx(tx, turnID, label)
	if err != nil {
		return err
	}
	if !found {
		return missing()
	}
	if err := write(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", label, err)
	}
	return nil
}

// LateTurnPayload names the per-column fold strategy for late turn-complete
// data. The SQL below keeps the update atomic; this struct keeps callers from
// passing two plain strings whose different semantics are only visible inside
// CASE expressions.
type LateTurnPayload struct {
	TokenUsageJSONIfEmpty       string
	AssistantMessageIDOverwrite string
	StopReasonOverwrite         string
	ErrorMessageOverwrite       string
}

// UpdateTurnLatePayload folds late-arriving payload onto an
// already-settled turn row in a single statement so the normal-case
// soft-then-real cascade pays one autocommit boundary instead of two.
// Per-column semantics (different intentionally — see below):
//
//   - `token_usage_json`: first non-empty wins. The first settle's
//     usage is preserved across late arrivals; an empty input is a
//     no-op.
//   - `assistant_message_id`: last non-empty wins (overwrite). A
//     multi-round logical turn settles on round 1's amid first and
//     overwrites with each subsequent round so the persisted column
//     always reflects the FINAL assistant message of the turn — the
//     documented contract on `SettledTurn.assistantMessageId` and
//     `TurnCompletedEvent.assistantMessageId`. An empty input is a
//     no-op (preserves whatever the row already has).
//   - `stop_reason` / `error_message`: late error wins when the caller
//     passes non-empty values. A soft message_delta close can settle the row
//     before the trailing `result{is_error:true}` arrives; the late real
//     result must still mark the persisted turn as failed.
//
// Passing every payload field empty is a silent no-op (no SQL roundtrip).
//
// The first settlement may have come from the parser's soft
// round-close (which fires from message_delta — usage may not be on
// the wire yet, but the assistant_message_id is peeked from the
// parser if observed) or from a multi-result cascade. The trailing
// real `result` envelope folds in cumulative usage if still empty
// and overwrites the amid with the final-round id.
func (s *Store) UpdateTurnLatePayload(turnID string, payload LateTurnPayload) error {
	if turnID == "" {
		return fmt.Errorf("store: update turn late payload: turn id is required")
	}
	if payload.TokenUsageJSONIfEmpty == "" &&
		payload.AssistantMessageIDOverwrite == "" &&
		payload.StopReasonOverwrite == "" &&
		payload.ErrorMessageOverwrite == "" {
		return nil
	}
	label := fmt.Sprintf("store: update turn %s late payload", turnID)
	return s.writeTurn(turnID, label, func(tx *sql.Tx) error {
		return updateTurnLatePayloadTx(tx, turnID, payload, label)
	}, func() error { return nil })
}

func updateTurnLatePayloadTx(tx *sql.Tx, turnID string, payload LateTurnPayload, label string) error {
	_, err := tx.Exec(
		`UPDATE turns
		    SET token_usage_json = CASE
		          WHEN token_usage_json = '' AND ? != '' THEN ?
		          ELSE token_usage_json
		        END,
		        assistant_message_id = CASE
		          WHEN ? != '' THEN ?
		          ELSE assistant_message_id
		        END,
		        stop_reason = CASE
		          WHEN ? != '' THEN ?
		          ELSE stop_reason
		        END,
		        error_message = CASE
		          WHEN ? != '' THEN ?
		          ELSE error_message
		        END
		  WHERE turn_id = ?`,
		payload.TokenUsageJSONIfEmpty, payload.TokenUsageJSONIfEmpty,
		payload.AssistantMessageIDOverwrite, payload.AssistantMessageIDOverwrite,
		payload.StopReasonOverwrite, payload.StopReasonOverwrite,
		payload.ErrorMessageOverwrite, payload.ErrorMessageOverwrite,
		turnID,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// BackfillTurnProviderID stamps the provider's wire turn id onto a turn
// row that has none — the echo-opened `<thread>:<index>` row a later wire
// turn start ADOPTS (triage's turn-index reconciliation). First-write-wins
// in SQL: a row already carrying a provider id is never overwritten, so a
// misdirected backfill costs nothing. Zero rows affected is a normal
// outcome (the row already has an id, or was relocated meanwhile), not an
// error — provider_turn_id is diagnostic identity plus the Codex fork/revert
// anchor, and both readers tolerate absence. A turn row a fork shows goes
// to the fork first (requireMutableTurnTx).
func (s *Store) BackfillTurnProviderID(turnID, providerTurnID string) error {
	if turnID == "" {
		return fmt.Errorf("store: backfill turn provider id: turn id is required")
	}
	if providerTurnID == "" {
		return fmt.Errorf("store: backfill turn provider id %s: provider turn id is required", turnID)
	}
	var missing bool
	if err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM turns WHERE turn_id = ? AND provider_turn_id = '')`, turnID).Scan(&missing); err != nil {
		return fmt.Errorf("store: backfill turn %s provider id: %w", turnID, err)
	}
	if !missing {
		return nil
	}
	label := fmt.Sprintf("store: backfill turn %s provider id", turnID)
	return s.writeTurn(turnID, label, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE turns SET provider_turn_id = ? WHERE turn_id = ? AND provider_turn_id = ''`,
			providerTurnID, turnID); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}, func() error { return nil })
}

// GetTurn returns a single turn by its provider-assigned id. Returns
// (Turn{}, false, nil) when no row exists — the miss is not an error,
// so callers can use the bool to branch cleanly.
func (s *Store) GetTurn(turnID string) (Turn, bool, error) {
	row := s.reader().QueryRow(
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

// GetTurnByThreadIndex returns a single turn by (thread, turn_index).
// Used by turn-lifecycle reconciliation paths that know the logical turn
// index but not the provider-assigned turn id. Returns (Turn{}, false, nil)
// when no row exists.
func (s *Store) GetTurnByThreadIndex(threadID string, turnIndex int) (Turn, bool, error) {
	row := s.reader().QueryRow(
		`SELECT `+turnColumns+` FROM timeline_turns WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	)
	turn, err := scanTurnRow(row)
	if err == sql.ErrNoRows {
		return Turn{}, false, nil
	}
	if err != nil {
		return Turn{}, false, fmt.Errorf("store: get turn %s/%d: %w", threadID, turnIndex, err)
	}
	return turn, true, nil
}

// TurnIDsForThread returns every turn id already recorded for a thread.
//
// `turns.turn_id` is the primary key and ApplyImportBatch INSERTs, so an
// import or refresh that re-opens an id the thread already holds would
// fail the transaction after a check RPC has already promised the user it
// would succeed. The import writer loads this set up front and refuses the
// batch instead — see internal/sessionimport/AGENTS.md §Writer contract.
func (s *Store) TurnIDsForThread(threadID string) (map[string]struct{}, error) {
	rows, err := s.reader().Query(
		`SELECT turn_id FROM turns WHERE thread_id = ?`, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: turn ids for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan turn id for %s: %w", threadID, err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: turn ids for %s: %w", threadID, err)
	}
	return out, nil
}

// ListRecentTurns returns the N most recent turns for a thread, newest
// first (turn_index DESC). Used by the frontend on thread-switch to
// hydrate latestSettledTurn. A non-positive limit returns an empty
// slice without hitting the database.
func (s *Store) ListRecentTurns(threadID string, limit int) ([]Turn, error) {
	if limit <= 0 {
		return nil, nil
	}
	// A top-level compound, so the thread's own arm walks its index in
	// order and stops at the limit; a fork's inherited turns follow its own.
	rows, err := s.reader().Query(
		`SELECT `+turnColumns+` FROM turns
		  WHERE thread_id = ?
		 UNION ALL
		 SELECT turns.turn_id, l.thread_id, turns.turn_index, turns.started_at, turns.completed_at,
		        turns.stop_reason, turns.assistant_message_id, turns.token_usage_json,
		        turns.error_message, turns.provider_turn_id
		   FROM thread_fork_lineage l
		   CROSS JOIN turns ON turns.thread_id = l.ancestor_id
		  WHERE l.thread_id = ? AND `+forkTurnVisibleSQL+`
		  ORDER BY turn_index DESC
		  LIMIT `+strconv.Itoa(limit),
		threadID, threadID,
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

// GetActiveTurn returns the latest turn for a thread only when that latest
// turn is still in-flight (completed_at=NULL).
//
// In normal operation at most one turn per thread is in-flight at a
// time (triage serialises turn-start via the per-thread action lock).
// RecoverCrashedTurns settles crash leftovers at boot, so during an app
// run an active result here means genuinely live provider work. A bug
// can still leave older NULL rows behind; once a newer turn exists
// those rows are historical corruption, not live provider work.
//
// Returns (Turn{}, false, nil) when no turn exists or the latest row is
// already settled.
func (s *Store) GetActiveTurn(threadID string) (Turn, bool, error) {
	row := s.reader().QueryRow(
		`SELECT `+turnColumns+` FROM turns
		  WHERE thread_id = ?
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
	if turn.CompletedAt != nil {
		return Turn{}, false, nil
	}
	return turn, true, nil
}

// FirstTurnIndexAtOrAfter returns the lowest turn index of a thread whose
// turn started at or after startedAt, which is the turn a "since this
// timestamp" window begins at.
//
// found is false when no turn of the thread started that late. anyTurns
// tells the two reasons apart in the same round trip: a thread that has
// turns but none since the timestamp has simply been quiet, while a
// thread with no turn rows at all has no anchor to resolve against and
// leaves the caller to fall back to the whole thread.
//
// The aggregate is deliberately one statement over the thread's turn rows,
// a fork's inherited ones included, rather than a walk of a bounded recent
// listing: a thread with more turns than any listing cap would otherwise
// resolve the anchor against the tail it happened to read.
func (s *Store) FirstTurnIndexAtOrAfter(threadID string, startedAt int64) (turnIndex int, found, anyTurns bool, err error) {
	var index sql.NullInt64
	var total int64
	err = s.reader().QueryRow(
		`SELECT MIN(CASE WHEN started_at >= ? THEN turn_index END), COUNT(*)
		   FROM timeline_turns WHERE thread_id = ?`,
		startedAt, threadID,
	).Scan(&index, &total)
	if err != nil {
		return 0, false, false, fmt.Errorf("store: first turn index at or after %d for %s: %w", startedAt, threadID, err)
	}
	return int(index.Int64), index.Valid, total > 0, nil
}
