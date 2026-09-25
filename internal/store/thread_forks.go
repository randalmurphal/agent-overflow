package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/entityid"
)

// BuildForkedThread returns a Thread row populated from source plus the
// fork-only fields: a fresh UUID, a "(fork)"-suffixed title, the
// `ForkedFromThreadID` linkage, and a `created_at` / `updated_at` pair
// at the current millisecond. The session-state fields
// (`SessionRef`, `PendingForkRef`) are left empty: the app-side fork
// saga sets them once the provider-specific resume reference is known.
// AutoCompactStandard/Extended Percent are intentionally NOT copied, so
// a fork starts with zero overrides and picks up the live Settings
// value on the first session start (the same default-resolution path a
// brand-new thread follows).
//
// `LastTokenUsage` IS copied so the meter reflects the inherited
// conversation history from frame 0. The new resumed session emits a
// fresh `thread/tokenUsage/updated` on its first turn which overwrites
// this seed with the live measurement.
//
// `GroupID` IS copied: a fork of a grouped thread lands in the same
// sidebar group (migration v76). The fork carries no pin, so the
// "one pin per visible row" CHECK holds by construction.
//
// Pure: this only builds the row. CreatePointerFork persists it.
func BuildForkedThread(source Thread) Thread {
	now := time.Now().UnixMilli()
	return Thread{
		ID:                 entityid.New(),
		ProjectID:          source.ProjectID,
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		Mode:               source.Mode,
		ReasoningEffort:    source.ReasoningEffort,
		FastMode:           source.FastMode,
		ContextWindow:      source.ContextWindow,
		RuntimeMode:        source.RuntimeMode,
		LastTokenUsage:     source.LastTokenUsage,
		GroupID:            source.GroupID,
		ForkedFromThreadID: source.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

// ErrForkChainTooDeep reports a fork whose source already reads through
// forkLineageMaxDepth levels, or a write whose copy for the forks that
// show a row needs a level for such a reader.
var ErrForkChainTooDeep = errors.New("store: fork chain is too deep")

// ErrForkSourceDeleted reports a fork whose source is gone or whose
// delete has begun (threads.deleting, BeginThreadDelete), or a holder.
var ErrForkSourceDeleted = errors.New("store: the thread was deleted and cannot be forked")

// ForkCut says how much of the source a pointer fork inherits. The zero
// value is the whole timeline.
type ForkCut struct {
	// ThroughTurn keeps turns up to and including *ThroughTurn. A negative
	// value keeps nothing.
	ThroughTurn *int
	// BeforeItemID keeps what precedes this user row in provider order:
	// earlier turns, the anchor turn's rows before it, and for an
	// interrupt-promoted anchor its turn's content successors (the rule
	// DeleteConversationFromItem applies to a revert).
	BeforeItemID string
}

// forkCut is a resolved cut: the first position the fork does not inherit,
// placed right after the last row it does.
type forkCut struct {
	empty bool
	turn  int
	item  int
	// hidden are inherited ids below the cut the fork does not show.
	hidden []string
	// trim clears the cut turn row's settle metadata, which described
	// content the cut excluded.
	trim bool
	// turnsThrough is the last turn whose row the fork copies. A fork owns
	// the row of the turn its cut falls in and every later one, and of any
	// earlier turn the source has not settled (copyForkTurnsTx).
	turnsThrough int
}

// CreatePointerFork creates fork as a pointer fork of sourceID in one
// writer transaction. The fork copies no history: it records its source and
// cut, and every timeline read resolves the rows before the cut from the
// source (docs/architecture/sqlite-store.md#pointer-forks). The writes are
// the thread row, one lineage row per level, the rows the fork must own
// because they are still running in the source (settled as interrupted)
// or sit in a cut turn the source is still running, the cut turn's row
// and the question state of inherited rows. The fork's
// origin is its thread row (fork_source_thread_id, fork_source_title); no
// timeline row marks it.
//
// A cut that keeps nothing creates an ordinary empty thread.
func (s *Store) CreatePointerFork(fork Thread, sourceID string, cut ForkCut, summarise func(string) string, now int64) error {
	if summarise == nil {
		return fmt.Errorf("store: create fork: summarise is required")
	}
	if cut.ThroughTurn != nil && cut.BeforeItemID != "" {
		return fmt.Errorf("store: create fork: a cut is either turn-granular or item-granular")
	}
	prepared, lastReadAtArg, err := prepareThreadForCreate(fork)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin create fork: %w", err)
	}
	defer tx.Rollback()
	if err := insertThread(tx, prepared, lastReadAtArg); err != nil {
		return fmt.Errorf("store: create fork thread: %w", err)
	}
	if err := s.linkPointerForkTx(tx, prepared.ID, sourceID, cut, summarise, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit create fork: %w", err)
	}
	return nil
}

func (s *Store) linkPointerForkTx(tx *sql.Tx, forkID, sourceID string, cut ForkCut, summarise func(string) string, now int64) error {
	var title string
	var depth int
	if err := tx.QueryRow(
		`SELECT title, (SELECT COALESCE(MAX(depth), 0) FROM thread_fork_lineage WHERE thread_id = threads.id)
		   FROM owned_threads AS threads WHERE id = ?`, sourceID,
	).Scan(&title, &depth); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: read fork source %s: %w", sourceID, err)
		}
		// A source this computer gave away says so; one that is gone or
		// whose delete has begun is refused as deleted.
		if accessErr := checkThreadTransferAccess(tx, sourceID); accessErr != nil {
			return accessErr
		}
		return fmt.Errorf("store: fork %s: %w", sourceID, ErrForkSourceDeleted)
	}
	plan, err := resolveForkCutTx(tx, sourceID, cut)
	if err != nil || plan.empty {
		return err
	}
	if depth >= forkLineageMaxDepth {
		return fmt.Errorf("%w: %s reads through %d levels", ErrForkChainTooDeep, sourceID, depth)
	}
	hidden, settle, err := forkUnsettledRowsTx(tx, sourceID, plan)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO thread_fork_lineage (thread_id, depth, ancestor_id, cut_turn_index, cut_item_index)
		 VALUES (?, 1, ?, ?, ?)`, forkID, sourceID, plan.turn, plan.item,
	); err != nil {
		return fmt.Errorf("store: link fork %s: %w", forkID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO thread_fork_lineage (thread_id, depth, ancestor_id, cut_turn_index, cut_item_index)
		 SELECT ?, depth + 1, ancestor_id,
		        CASE WHEN (cut_turn_index, cut_item_index) < (?, ?) THEN cut_turn_index ELSE ? END,
		        CASE WHEN (cut_turn_index, cut_item_index) < (?, ?) THEN cut_item_index ELSE ? END
		   FROM thread_fork_lineage WHERE thread_id = ?`,
		forkID, plan.turn, plan.item, plan.turn, plan.turn, plan.item, plan.item, sourceID,
	); err != nil {
		return fmt.Errorf("store: link fork %s through %s: %w", forkID, sourceID, err)
	}
	if _, err := tx.Exec(
		`UPDATE threads SET fork_source_thread_id = ?, fork_cut_turn_index = ?, fork_cut_item_index = ?,
		        fork_source_title = ?
		  WHERE id = ?`, sourceID, plan.turn, plan.item, title, forkID,
	); err != nil {
		return fmt.Errorf("store: record fork %s source: %w", forkID, err)
	}
	if err := hideForkRowsTx(tx, forkID, hidden); err != nil {
		return err
	}

	// Rows still running in the source are the source's live work. The
	// fork owns a copy and settles it exactly as the crash sweep and a user
	// interrupt do: the fork is a snapshot "as if interrupted right now".
	// A settled summary can move an agent's card; the fork's writes carry
	// no card and recompute those chains before it commits. The fork also
	// owns the rest of a cut turn the source is still running
	// (forkRunningTurnRowsTx).
	settled, err := inheritedRowsByID(tx, forkID, settle)
	if err != nil {
		return err
	}
	running, err := forkRunningTurnRowsTx(tx, forkID, sourceID, plan, settle)
	if err != nil {
		return err
	}
	if err := withHistoryBulkLoadTx(tx, forkID, func() error {
		return snapshotRowsTx(tx, forkID, append(slices.Clip(settled), running...))
	}); err != nil {
		return err
	}
	w := s.bulkItemWrites(tx, forkID, false)
	for _, row := range settled {
		old, err := scanSubagentRow(tx.QueryRow(subagentRowSQL, forkID, row.id))
		if err != nil {
			return fmt.Errorf("store: read fork row %s/%s: %w", forkID, row.id, err)
		}
		next := old
		next.status, next.summary = "errored", summarise(old.summary)
		if err := w.updated(old, next); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE items SET status = 'errored', summary = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
			next.summary, now, forkID, row.id,
		); err != nil {
			return fmt.Errorf("store: settle fork row %s/%s: %w", forkID, row.id, err)
		}
		if err := indexItemByIDTx(tx, forkID, row.id); err != nil {
			return err
		}
	}

	if err := copyForkTurnsTx(tx, forkID, sourceID, plan.turn, plan.turnsThrough); err != nil {
		return err
	}
	if plan.trim {
		if err := trimTurnSettleToSurvivorsTx(tx, forkID, plan.turn); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`UPDATE turns SET completed_at = ?, stop_reason = 'interrupted'
		  WHERE thread_id = ? AND completed_at IS NULL`, now, forkID,
	); err != nil {
		return fmt.Errorf("store: settle fork %s turns: %w", forkID, err)
	}
	if err := copyForkQuestionsTx(tx, sourceID, forkID); err != nil {
		return err
	}
	if err := recomputeTurnErrorsTx(tx, forkID); err != nil {
		return err
	}
	return w.finish()
}

// copyForkTurnsTx gives the fork its own row for the cut turn and for every
// later turn through `through` the source knows about. turn_id is a global
// key, so the copies take `<fork>:<turn_index>`; provider_turn_id is kept,
// which is what lets a later revert or fork inside the fork resolve a Codex
// `lastTurnId` without reading the source.
//
// A turn row of the source's own below the cut turn that is not settled
// (an import whose completion has not landed, a crash the boot sweep has
// not reached) is the source's live state, like its running rows: the
// fork copies it too, and settles its copy with the rest, so the source's
// later settle changes no turn row a fork shows (trg_turns_shown_update).
// idx_turns_inflight holds only those rows. A thread the source reads
// through its lineage holds none below the source's cut: the source copied
// them when it was made.
func copyForkTurnsTx(tx *sql.Tx, forkID, sourceID string, from, through int) error {
	if _, err := tx.Exec(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
		   FROM timeline_turns
		  WHERE thread_id = ? AND turn_index >= ? AND turn_index <= ?`,
		forkID, forkID, sourceID, from, through,
	); err != nil {
		return fmt.Errorf("store: copy fork %s turns: %w", forkID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
		   FROM turns
		  WHERE thread_id = ? AND completed_at IS NULL AND turn_index < ?`,
		forkID, forkID, sourceID, from,
	); err != nil {
		return fmt.Errorf("store: copy fork %s unsettled turns: %w", forkID, err)
	}
	return nil
}

// copyForkQuestionsTx copies the question state of the rows the fork
// shows. A submission still waiting for its message in the source did not
// reach the fork's history, so it reopens.
func copyForkQuestionsTx(tx *sql.Tx, sourceID, forkID string) error {
	rows, err := tx.Query(`SELECT `+asyncQuestionColumns+` FROM async_questions
		WHERE thread_id = ? ORDER BY created_at, rowid`, sourceID)
	if err != nil {
		return fmt.Errorf("store: read fork questions of %s: %w", sourceID, err)
	}
	var questions []AsyncQuestion
	for rows.Next() {
		q, err := scanAsyncQuestion(rows)
		if err != nil {
			return errors.Join(err, rows.Close())
		}
		questions = append(questions, q)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: iterate fork questions of %s: %w", sourceID, err)
	}
	if len(questions) == 0 {
		return nil
	}
	ids := make([]any, 0, 2*len(questions))
	for _, q := range questions {
		ids = append(ids, q.ItemID)
		if q.UserItemID != "" {
			ids = append(ids, q.UserItemID)
		}
	}
	visibleQuery, visibleArgs, err := timelineArms(tx, forkID, timelineSelection{
		Columns:  timelineIDColumns,
		KeyFirst: true,
		Where:    "items.id IN (" + placeholders(len(ids)) + ")", WhereArgs: ids,
	})
	if err != nil {
		return err
	}
	shown, err := queryIDs(tx, `SELECT id FROM (`+visibleQuery+`)`, visibleArgs...)
	if err != nil {
		return fmt.Errorf("store: read fork question rows of %s: %w", forkID, err)
	}
	visible := make(map[string]bool, len(shown))
	for _, id := range shown {
		visible[id] = true
	}
	for _, q := range questions {
		if !visible[q.ItemID] {
			continue
		}
		if !visible[q.UserItemID] {
			q.UserItemID = ""
		}
		if q.State == "submitted" || q.State == "restored" || (q.State == "delivered" && q.UserItemID == "") {
			q.State, q.Answer, q.SendID, q.UserItemID = "unanswered", "", "", ""
		}
		if err := insertAsyncQuestionStateTx(tx, forkID, q); err != nil {
			return err
		}
	}
	return nil
}
