package store

import (
	"database/sql"
	"fmt"
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
	_, err := s.db.Exec(
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
		return fmt.Errorf("store: update turn %s late payload: %w", turnID, err)
	}
	return nil
}

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
// can spawn) — at that point a NULL completed_at is provably a crash
// leftover, because every in-app session death already settles its row
// via triage's synthesized truncated turn-complete.
//
// The settle mirrors what that synthesized turn-complete writes:
// completed_at=now, stop_reason='interrupted'. The item flip mirrors
// triage's flipTurnItemsErrored — summarise rewrites each flipped
// row's summary (callers pass the idempotent " — interrupted" suffix
// convention) and backgrounded tool_call launches are exempt
// (invariant 24: their disposition belongs to the background recovery
// sweep, which writes completion siblings instead).
//
// Everything runs in a single transaction so an N-thread crash pays
// one WAL commit and a mid-sweep crash leaves all rows untouched for
// the next boot. The SELECT is O(crashed rows) via the partial index
// idx_turns_inflight. Thread activity is deliberately NOT bumped —
// sweeping crash residue is not a user interaction (matches
// FlipGhostBackgroundRowsOnStart).
//
// Returns the settled turns so the caller can log the repair.
func (s *Store) RecoverCrashedTurns(summarise func(string) string, now int64) ([]CrashedTurn, error) {
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
		if err := flipCrashedTurnItemsTx(tx, c, summarise, now); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit crashed-turn recovery tx: %w", err)
	}
	return crashed, nil
}

// flipCrashedTurnItemsTx flips one crashed turn's streaming/running
// items to errored inside the recovery transaction. Backgrounded
// tool_call launches are exempt — see RecoverCrashedTurns.
func flipCrashedTurnItemsTx(tx *sql.Tx, c CrashedTurn, summarise func(string) string, now int64) error {
	rows, err := tx.Query(
		`SELECT id, summary FROM items
		  WHERE thread_id = ?
		    AND turn_index = ?
		    AND status IN ('streaming', 'running')
		    AND NOT (is_background = 1 AND kind = 'tool_call')`,
		c.ThreadID, c.TurnIndex,
	)
	if err != nil {
		return fmt.Errorf("store: crashed-turn item select %s/%d: %w", c.ThreadID, c.TurnIndex, err)
	}
	type flip struct{ id, summary string }
	var flips []flip
	for rows.Next() {
		var f flip
		if err := rows.Scan(&f.id, &f.summary); err != nil {
			rows.Close()
			return fmt.Errorf("store: crashed-turn item scan: %w", err)
		}
		flips = append(flips, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: crashed-turn item rows err: %w", err)
	}
	rows.Close()

	for _, f := range flips {
		if _, err := tx.Exec(
			`UPDATE items
			    SET status = 'errored', summary = ?, updated_at = ?
			  WHERE thread_id = ? AND id = ?`,
			summarise(f.summary), now, c.ThreadID, f.id,
		); err != nil {
			return fmt.Errorf("store: crashed-turn item flip %s: %w", f.id, err)
		}
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

// GetTurnByThreadIndex returns a single turn by (thread, turn_index).
// Used by turn-lifecycle reconciliation paths that know the logical turn
// index but not the provider-assigned turn id. Returns (Turn{}, false, nil)
// when no row exists.
func (s *Store) GetTurnByThreadIndex(threadID string, turnIndex int) (Turn, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+turnColumns+` FROM turns WHERE thread_id = ? AND turn_index = ?`,
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

// PickInitialFloorTurn chooses the inclusive `turn_index` floor for a
// windowed thread open. The caller asks for up to `turnLimit` most
// recent turns but the window grows or shrinks around that target so
// the loaded item count stays within a sane range — at least
// `minItems` if any more turns are available, and never more than
// `maxItems` even if a single turn is unusually large.
//
// Strategy:
//  1. Read per-turn item counts from newest to oldest (bounded scan so
//     the query doesn't walk the whole thread on enormous histories).
//     Counts cover visible top-level rows only — plan_update
//     notifications and subagent children never render as timeline
//     rows, so they must not consume window budget (matches
//     paging.go's loaders).
//  2. Accumulate counts. Stop growing the window as soon as items have
//     reached `minItems` and we've seen at least `turnLimit` turns.
//  3. If a single turn's item count would push the cumulative past
//     `maxItems`, include the turn anyway (every turn is loaded whole;
//     mid-turn cuts break downstream per-turn derivations) and stop.
//  4. If the thread has fewer turns than `turnLimit`, return the
//     smallest turn_index the thread has.
//
// `hasMore` reports whether any older turn was excluded from the
// returned floor — i.e. older history exists below `floor` and the
// frontend should render its "Load older" control. Returns
// (-1, false, nil) for threads with no turns.
//
// Inputs are coerced: `turnLimit < 1` defaults to 1; `minItems < 0`
// defaults to 0; `maxItems <= 0` or `maxItems < minItems` defaults to
// the effective minItems so the "never less than minItems" invariant
// holds.
func (s *Store) PickInitialFloorTurn(
	threadID string,
	turnLimit, minItems, maxItems int,
) (floorTurnIndex int, hasMore bool, err error) {
	if turnLimit < 1 {
		turnLimit = 1
	}
	if minItems < 0 {
		minItems = 0
	}
	if maxItems <= 0 || maxItems < minItems {
		maxItems = minItems
	}

	// Query the most recent (turn_index, item_count) pairs. The scan
	// must cover both the caller's turnLimit target AND enough turns to
	// hit minItems when every turn is tiny (burst Q&A threads). A fixed
	// overshoot on turnLimit doesn't cover the latter — for turnLimit=10
	// / minItems=500 on a thread of 1-item turns we'd need to scan 500
	// rows just to count far enough down. `absoluteScanCap` still limits
	// the blast radius on pathological threads.
	const overshootFactor = 4
	const absoluteScanCap = 5000
	scanLimit := turnLimit * overshootFactor
	if minItems > scanLimit {
		scanLimit = minItems
	}
	if scanLimit > absoluteScanCap {
		scanLimit = absoluteScanCap
	}

	rows, err := s.db.Query(
		`SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ?
		    AND `+visibleItemsFilter+`
		    AND `+topLevelItemsFilter+`
		  GROUP BY turn_index
		  ORDER BY turn_index DESC
		  LIMIT ?`,
		threadID, scanLimit,
	)
	if err != nil {
		return -1, false, fmt.Errorf("store: pick initial floor for %s: %w", threadID, err)
	}
	defer rows.Close()

	type turnCount struct {
		turnIndex int
		count     int
	}
	var turns []turnCount
	for rows.Next() {
		var tc turnCount
		if err := rows.Scan(&tc.turnIndex, &tc.count); err != nil {
			return -1, false, fmt.Errorf("store: scan turn count: %w", err)
		}
		turns = append(turns, tc)
	}
	if err := rows.Err(); err != nil {
		return -1, false, fmt.Errorf("store: iterate turn counts for %s: %w", threadID, err)
	}

	// Empty thread (no items on any turn). The turns table may still
	// hold an in-flight turn with no items yet — look for it so a
	// just-started turn is included on first paint. Even when the only
	// surviving row is an active turn, probe for older items so a
	// crashed-then-restored thread with an orphan NULL turn-row below
	// the active one still reports hasMore=true.
	if len(turns) == 0 {
		activeFloor, ok, err := s.activeTurnFloor(threadID)
		if err != nil {
			return -1, false, err
		}
		if !ok {
			return -1, false, nil
		}
		older, err := s.hasOlderTurns(threadID, activeFloor)
		if err != nil {
			return -1, false, err
		}
		return activeFloor, older, nil
	}

	// Walk newest → oldest accumulating item counts. `picked` is the
	// current candidate floor.
	cumulative := 0
	picked := turns[0].turnIndex
	for i, tc := range turns {
		// Adding this turn would push us past maxItems: stop BEFORE we
		// include it, unless it's the only turn we have so far (we
		// never return an empty window when turns exist).
		if i > 0 && cumulative+tc.count > maxItems {
			break
		}
		cumulative += tc.count
		picked = tc.turnIndex
		// Break once we've met both the turn-count target and the
		// minItems target — further turns are only added when the
		// thread is small.
		if i+1 >= turnLimit && cumulative >= minItems {
			break
		}
	}

	// Include the active turn (if any) BEFORE probing hasMore. The
	// normal just-started case has an active latest turn with no items
	// yet; when items already exist, activeFloor >= picked and this is
	// a no-op. The lowering path is defensive for malformed histories
	// where item rows exist above the latest turn row. Older NULL turns
	// followed by newer turn rows are not considered active by
	// GetActiveTurn.
	//
	// Cap the lowering so a deep crashed-active-turn scenario can't
	// silently blow the caller's maxItems budget. The walk above
	// already has the per-turn counts for turns we scanned; if the
	// active turn is within the scanned range we can check precisely,
	// and for turns below the scan we fall back to a conservative
	// "only lower if activeFloor is within one scan-overshoot of
	// picked" heuristic. Keeping the original picked in the
	// over-budget case is safe: the active turn's items will still
	// reach the user via the streaming path (triage upserts them into
	// the live pane), and the user can Load Older to scroll back to
	// the in-flight row if it matters for context.
	activeFloor, active, err := s.activeTurnFloor(threadID)
	if err != nil {
		return -1, false, err
	}
	if active && activeFloor < picked {
		// Sum items for every scanned turn whose turnIndex falls in
		// the gap [activeFloor, picked). The scan returns every turn
		// that has at least one item — a turn in the gap that doesn't
		// appear was empty, so the GROUP BY undercount is zero.
		extra := 0
		for _, tc := range turns {
			if tc.turnIndex >= activeFloor && tc.turnIndex < picked {
				extra += tc.count
			}
		}
		// "Reachable" means the scan reached deep enough that we've
		// accounted for every item-bearing turn in the gap: the
		// oldest scanned turn_index is at or below activeFloor. The
		// scan is ORDER BY turn_index DESC so turns[last] is the
		// oldest scanned row.
		lastScanned := turns[len(turns)-1].turnIndex
		reachable := lastScanned <= activeFloor
		if reachable && cumulative+extra <= maxItems {
			picked = activeFloor
		} else if !reachable && picked-activeFloor <= 2 {
			// Gap extends below the scan — fall back to a narrow-gap
			// heuristic. Two-turn-and-under is safe because each
			// unscanned turn can only carry whatever item_count
			// triage will land; two adjacent empty turns are the
			// common "just-started turn below picked" happy path.
			picked = activeFloor
		}
	}

	// The scan stops at scanLimit; if we walked every scanned row,
	// older turns may still exist below. Check cheaply for "any item
	// below picked".
	hasMore, err = s.hasOlderTurns(threadID, picked)
	if err != nil {
		return -1, false, err
	}
	return picked, hasMore, nil
}

// activeTurnFloor returns the turn_index of the active (completed_at
// NULL) turn for a thread, or (0, false, nil) when none is in-flight.
// Used by PickInitialFloorTurn to guarantee a newly-started turn with
// no items yet is still inside the loaded window.
func (s *Store) activeTurnFloor(threadID string) (int, bool, error) {
	turn, ok, err := s.GetActiveTurn(threadID)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	return turn.TurnIndex, true, nil
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
	row := s.db.QueryRow(
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
