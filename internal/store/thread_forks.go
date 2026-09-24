package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/itemmeta"
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
// forkLineageMaxDepth levels.
var ErrForkChainTooDeep = errors.New("store: fork chain is too deep")

// ErrForkSourceDeleted reports a fork whose source is gone or whose
// delete has begun (threads.deleting, DeleteThreadPaced). The delete
// detaches the forks the source already has; a fork made after it began
// would read rows the delete is removing.
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
	// the row of the turn its cut falls in and every later one.
	turnsThrough int
}

// forkOrigin is the divider row's meta.
type forkOrigin struct {
	Kind           string `json:"kind"`
	SourceThreadID string `json:"sourceThreadId"`
	SourceTitle    string `json:"sourceTitle"`
	SourceItemID   string `json:"sourceItemId,omitempty"`
	SourceDeleted  bool   `json:"sourceDeleted,omitempty"`
}

// CreatePointerFork creates fork as a pointer fork of sourceID in one
// writer transaction. The fork copies no history: it records its source and
// cut, and every timeline read resolves the rows before the cut from the
// source (docs/architecture/sqlite-store.md#pointer-forks). The writes are
// the thread row, one lineage row per level, the rows the fork must own
// because they are still running in the source (settled as interrupted),
// the cut turn's row, the question state of inherited rows, and a divider
// row at the cut.
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
	// no card and recompute those chains before it commits.
	settled, err := inheritedRowsByID(tx, forkID, settle, allLevels)
	if err != nil {
		return err
	}
	if err := withHistoryBulkLoadTx(tx, forkID, func() error {
		return copyInheritedRowsTx(tx, forkID, settled)
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
	if err := placeForkDividerTx(tx, w, forkID, forkOrigin{SourceThreadID: sourceID, SourceTitle: title}, plan.turn, plan.item, now); err != nil {
		return err
	}
	if err := recomputeTurnErrorsTx(tx, forkID); err != nil {
		return err
	}
	return w.finish()
}

// resolveForkCutTx reads the cut from the source's timeline. Every cut is
// placed right after the last row the fork inherits, so a row the source
// adds later at a higher position is never inside it.
func resolveForkCutTx(tx *sql.Tx, sourceID string, cut ForkCut) (forkCut, error) {
	switch {
	case cut.BeforeItemID != "":
		return resolveBeforeItemCutTx(tx, sourceID, cut.BeforeItemID)
	case cut.ThroughTurn != nil:
		if *cut.ThroughTurn < 0 {
			return forkCut{empty: true}, nil
		}
		plan, err := lastRowCutTx(tx, sourceID, "items.turn_index <= ?", *cut.ThroughTurn)
		plan.turnsThrough = *cut.ThroughTurn
		return plan, err
	default:
		plan, err := lastRowCutTx(tx, sourceID, "")
		plan.turnsThrough = math.MaxInt32
		return plan, err
	}
}

// timelineRow is one row's identity and position.
type timelineRow struct {
	id         string
	turn, item int
}

// lastRowTx reads the last row of threadID's timeline that matches where:
// of the whole timeline, or of one turn when turn is not negative.
func lastRowTx(tx *sql.Tx, threadID string, turn int, where string, args ...any) (timelineRow, bool, error) {
	sel := timelineSelection{
		Columns:   timelineIDColumns,
		Where:     where,
		WhereArgs: args,
		OrderBy:   "turn_index DESC, item_index DESC",
		Limit:     1,
	}
	if turn >= 0 {
		sel.Turn, sel.TurnArgs = "?", []any{turn}
	}
	query, binds, err := timelineArms(tx, threadID, sel)
	if err != nil {
		return timelineRow{}, false, err
	}
	var row timelineRow
	err = tx.QueryRow(query, binds...).Scan(&row.id, &row.turn, &row.item)
	if errors.Is(err, sql.ErrNoRows) {
		return timelineRow{}, false, nil
	}
	if err != nil {
		return timelineRow{}, false, fmt.Errorf("store: read last row of %s: %w", threadID, err)
	}
	return row, true, nil
}

// lastRowCutTx places a cut after the last row of threadID's timeline that
// matches where.
func lastRowCutTx(tx *sql.Tx, threadID, where string, args ...any) (forkCut, error) {
	row, found, err := lastRowTx(tx, threadID, -1, where, args...)
	if err != nil {
		return forkCut{}, err
	}
	if !found {
		return forkCut{empty: true}, nil
	}
	return forkCut{turn: row.turn, item: row.item + 1}, nil
}

// lastRowProbeTurns bounds lastRowThroughTurnTx's turn-by-turn search.
const lastRowProbeTurns = 4

// lastRowThroughTurnTx is lastRowTx over the turns at or below maxTurn.
// The rows it looks for (a revert's last survivor, the row a divider links
// to) sit in maxTurn or just below it, so the turns are probed newest
// first through the turn-ranged arms, which read only the chunk
// references that can hold the turn. After lastRowProbeTurns empty turns,
// one ordered read covers the rest.
func lastRowThroughTurnTx(tx *sql.Tx, threadID string, maxTurn int, where string, args ...any) (timelineRow, bool, error) {
	floor := max(maxTurn-lastRowProbeTurns+1, 0)
	for turn := maxTurn; turn >= floor; turn-- {
		row, found, err := lastRowTx(tx, threadID, turn, where, args...)
		if err != nil || found {
			return row, found, err
		}
	}
	if floor == 0 {
		return timelineRow{}, false, nil
	}
	return lastRowTx(tx, threadID, -1, "items.turn_index < ? AND ("+where+")", append([]any{floor}, args...)...)
}

// resolveBeforeItemCutTx is the fork twin of DeleteConversationFromItem's
// kept set: earlier turns, the anchor turn's rows before the anchor, and for
// an interrupt-promoted anchor (itemmeta promotion marker) its turn's
// content successors up to the echo boundary. Same-turn top-level user rows
// after a promoted anchor are later-queued messages and stay behind; the
// ones that sit among kept content are hidden. Whenever the cut excludes
// same-turn content, the fork's copy of the turn row trims its settle
// metadata, as the revert does.
func resolveBeforeItemCutTx(tx *sql.Tx, sourceID, anchorID string) (forkCut, error) {
	var turnIndex, itemIndex int
	var meta string
	anchor, anchorArgs, err := timelineArms(tx, sourceID, timelineSelection{
		Columns: func(string, string) string {
			return "items.turn_index AS turn_index, items.item_index AS item_index, items.meta AS meta"
		},
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{anchorID},
	})
	if err != nil {
		return forkCut{}, err
	}
	if err := tx.QueryRow(anchor, anchorArgs...).Scan(&turnIndex, &itemIndex, &meta); err != nil {
		return forkCut{}, fmt.Errorf("store: fork anchor lookup %s/%s: %w", sourceID, anchorID, err)
	}
	promotion, err := itemmeta.DecodePromotionState(meta)
	if err != nil {
		// Corrupt anchor meta leaves the provider-order cut undecidable.
		return forkCut{}, fmt.Errorf("store: fork anchor %s/%s: %w", sourceID, anchorID, err)
	}

	query, binds, err := timelineArms(tx, sourceID, timelineSelection{
		Columns: func(string, string) string {
			return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index,
			        items.role AS role, items.parent_id AS parent_id`
		},
		Where:     "items.turn_index = ? AND items.item_index > ?",
		WhereArgs: []any{turnIndex, itemIndex},
		OrderBy:   "turn_index, item_index",
	})
	if err != nil {
		return forkCut{}, err
	}
	rows, err := tx.Query(query, binds...)
	if err != nil {
		return forkCut{}, fmt.Errorf("store: read fork anchor turn %s/%d: %w", sourceID, turnIndex, err)
	}
	type successor struct {
		id   string
		item int
	}
	var queued []successor
	lastKept := itemIndex
	kept := false
	excluded := false
	for rows.Next() {
		var id, role, parent string
		var turn, item int
		if err := rows.Scan(&id, &turn, &item, &role, &parent); err != nil {
			return forkCut{}, errors.Join(fmt.Errorf("store: scan fork anchor turn: %w", err), rows.Close())
		}
		topLevelUser := role == "user" && parent == ""
		switch {
		case !promotion.Promoted:
			// Everything after a plain anchor stays behind; only content
			// matters, for the trim.
			excluded = excluded || !topLevelUser
		case topLevelUser:
			queued = append(queued, successor{id, item})
		case promotion.HasEchoBoundary && item > promotion.EchoBoundary:
			excluded = true
		default:
			kept = true
			lastKept = item
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return forkCut{}, fmt.Errorf("store: iterate fork anchor turn: %w", err)
	}

	if !kept {
		plan, err := lastRowCutTx(tx, sourceID, "(items.turn_index, items.item_index) < (?, ?)", turnIndex, itemIndex)
		plan.turnsThrough = plan.turn
		plan.trim = excluded && !plan.empty && plan.turn == turnIndex
		return plan, err
	}
	plan := forkCut{turn: turnIndex, item: lastKept + 1, turnsThrough: turnIndex, trim: excluded}
	plan.hidden = append(plan.hidden, anchorID)
	for _, row := range queued {
		if row.item < lastKept {
			plan.hidden = append(plan.hidden, row.id)
		}
	}
	return plan, nil
}

// forkUnsettledRowsTx decides what the fork does with the source's
// unsettled rows below the cut, read through idx_items_unsettled, and
// returns every row the fork hides:
//
//   - a background launch with no completion inside the cut is live work
//     of the source's provider process, which the fork's own process can
//     never finish. It is hidden (the fork shows no ghost row that can
//     never complete).
//   - a settled background launch (running forever beside its completion
//     sibling, invariant 24) is finished history and stays.
//   - every other running or streaming row is settled in the fork's copy.
//
// A background completion lands at the write head, possibly turns after its
// launch, so a settled launch's completion is read unless the cut is at the
// source's tail, where every row the source has is inside it. Unsettled
// imported history is not examined: imported rows come from
// a finished provider history. The hidden launches and the rows the cut
// itself hides are expanded to everything that hangs off them
// (forkHiddenClosureTx).
func forkUnsettledRowsTx(tx *sql.Tx, sourceID string, plan forkCut) (hidden, settle []string, err error) {
	rows, err := tx.Query(forkUnsettledRowsSQL, sourceID, plan.turn, plan.item, sourceID, plan.turn, plan.item)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read unsettled rows of %s: %w", sourceID, err)
	}
	type unsettled struct {
		id         string
		background bool
		kind       string
		turn       int
		live       bool
	}
	var found []unsettled
	for rows.Next() {
		var row unsettled
		if err := rows.Scan(&row.id, &row.background, &row.kind, &row.turn, &row.live); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("store: scan unsettled row: %w", err), rows.Close())
		}
		found = append(found, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, fmt.Errorf("store: iterate unsettled rows of %s: %w", sourceID, err)
	}

	tail, err := forkCutAtTailTx(tx, sourceID, plan)
	if err != nil {
		return nil, nil, err
	}
	var roots []string
	candidates := make([]unsettled, 0, len(found))
	for _, row := range found {
		if !row.background {
			candidates = append(candidates, row)
			continue
		}
		completed := !row.live
		if completed && !tail {
			completion, completionArgs, err := timelineArms(tx, sourceID, timelineSelection{
				Columns:   func(string, string) string { return "1" },
				KeyFirst:  true,
				Where:     "items.completion_of <> '' AND items.completion_of = ? AND (items.turn_index, items.item_index) < (?, ?)",
				WhereArgs: []any{row.id, plan.turn, plan.item},
			})
			if err != nil {
				return nil, nil, err
			}
			if err := tx.QueryRow(`SELECT EXISTS(`+completion+`)`, completionArgs...).Scan(&completed); err != nil {
				return nil, nil, fmt.Errorf("store: read completion of %s/%s: %w", sourceID, row.id, err)
			}
		}
		if !completed {
			roots = append(roots, row.id)
			continue
		}
		if row.kind != "tool_call" {
			candidates = append(candidates, row)
		}
	}
	hidden, err = forkHiddenClosureTx(tx, sourceID, append(append([]string{}, plan.hidden...), roots...))
	if err != nil {
		return nil, nil, err
	}
	skip := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		skip[id] = true
	}
	for _, row := range candidates {
		if !skip[row.id] {
			settle = append(settle, row.id)
		}
	}
	return hidden, settle, nil
}

// forkUnsettledRowsSQL reads the running and streaming rows of a source's
// timeline below a cut, its own and the ones it inherits, through
// idx_items_unsettled. It binds (source, cut turn, cut item) twice.
var forkUnsettledRowsSQL = `
		SELECT items.id, items.is_background, items.kind, items.turn_index,
		       COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		  FROM items
		 WHERE items.thread_id = ? AND items.status IN ('running', 'streaming')
		   AND (items.turn_index, items.item_index) < (?, ?)
		UNION ALL
		SELECT items.id, items.is_background, items.kind, items.turn_index,
		       COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		  FROM thread_fork_lineage l
		  CROSS JOIN items ON items.thread_id = l.ancestor_id
		 WHERE l.thread_id = ? AND items.status IN ('running', 'streaming')
		   AND (items.turn_index, items.item_index) < (?, ?)
		   AND ` + inheritedItemVisibleSQL

// forkCutAtTailTx reports whether the cut follows every row of the source's
// timeline.
func forkCutAtTailTx(tx *sql.Tx, sourceID string, plan forkCut) (bool, error) {
	query, binds, err := timelineArms(tx, sourceID, timelineSelection{
		Columns:   timelineIDColumns,
		Where:     "(items.turn_index, items.item_index) >= (?, ?)",
		WhereArgs: []any{plan.turn, plan.item},
		OrderBy:   "turn_index, item_index",
		Limit:     1,
	})
	if err != nil {
		return false, err
	}
	var id string
	var turn, item int
	switch err := tx.QueryRow(query, binds...).Scan(&id, &turn, &item); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("store: read past the fork cut of %s: %w", sourceID, err)
	}
	return false, nil
}

// forkHiddenClosureTx expands the rows a fork hides to what hangs off them
// in the source's timeline: rows under them through parent_id and rows that
// complete them through completion_of, in any order. No row the fork shows
// then references a row it hides.
func forkHiddenClosureTx(tx *sql.Tx, sourceID string, roots []string) ([]string, error) {
	seen := make(map[string]bool, len(roots))
	var out []string
	frontier := roots
	for len(frontier) > 0 {
		subtree, err := timelineSubtreeTx(tx, sourceID, frontier)
		if err != nil {
			return nil, err
		}
		var added []string
		for _, id := range subtree {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
				added = append(added, id)
			}
		}
		frontier = nil
		for start := 0; start < len(added); start += forkCopyBatch {
			clause, args := inClause("items.completion_of", added[start:min(start+forkCopyBatch, len(added))])
			query, binds, err := timelineArms(tx, sourceID, timelineSelection{
				Columns:   timelineIDColumns,
				Where:     "items.completion_of <> '' AND " + clause,
				WhereArgs: args,
				OrderBy:   "turn_index, item_index",
			})
			if err != nil {
				return nil, err
			}
			ids, err := queryIDs(tx, "SELECT id FROM ("+query+")", binds...)
			if err != nil {
				return nil, fmt.Errorf("store: read completions of hidden rows in %s: %w", sourceID, err)
			}
			for _, id := range ids {
				if !seen[id] {
					frontier = append(frontier, id)
				}
			}
		}
	}
	return out, nil
}

// timelineSubtreeTx returns roots and every row under them in viewer's
// timeline, walking parent_id through every physical arm.
func timelineSubtreeTx(q sqlQueryer, viewer string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	walk, args, err := descendantsWalk(q, viewer, roots, func(string) string { return "1" })
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(walk+` SELECT id FROM rel`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read subtree in %s: %w", viewer, err)
	}
	out := append([]string{}, roots...)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan subtree row: %w", err), rows.Close())
		}
		out = append(out, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate subtree in %s: %w", viewer, err)
	}
	return out, nil
}

// copyForkTurnsTx gives the fork its own row for the cut turn and for every
// later turn through `through` the source knows about. turn_id is a global
// key, so the copies take `<fork>:<turn_index>`; provider_turn_id is kept,
// which is what lets a later revert or fork inside the fork resolve a Codex
// `lastTurnId` without reading the source.
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

// placeForkDividerTx writes the fork's divider at its cut, replacing one a
// revert moved. It links to the last top-level row the fork inherits, which
// is where "view in source" lands. The divider has no parent, so it counts
// toward no card; w records it with the rest of the write.
func placeForkDividerTx(tx *sql.Tx, w *cardWrite, forkID string, origin forkOrigin, turn, item int, now int64) error {
	target, _, err := lastRowThroughTurnTx(tx, forkID, turn,
		"items.parent_id = '' AND (items.turn_index, items.item_index) < (?, ?)", turn, item)
	if err != nil {
		return fmt.Errorf("store: read fork %s link target: %w", forkID, err)
	}
	origin.SourceItemID = target.id
	origin.Kind = forkDividerToolName
	meta, err := json.Marshal(origin)
	if err != nil {
		return fmt.Errorf("store: encode fork %s divider: %w", forkID, err)
	}
	id := forkDividerID(forkID)
	if _, err := tx.Exec(`DELETE FROM items WHERE thread_id = ? AND id = ?`, forkID, id); err != nil {
		return fmt.Errorf("store: clear fork %s divider: %w", forkID, err)
	}
	return insertItemTx(tx, w, Item{
		ID:        id,
		ThreadID:  forkID,
		TurnIndex: turn,
		ItemIndex: item,
		Kind:      "notification",
		Role:      "system",
		Status:    "completed",
		Summary:   "Forked from " + origin.SourceTitle,
		ToolName:  forkDividerToolName,
		Meta:      string(meta),
		CreatedAt: now,
		UpdatedAt: now,
	}, fmt.Sprintf("store: write fork %s divider", forkID))
}

// retractInheritedTx is a fork's own revert of rows it inherits. The rows
// are the source's, so they are not deleted: the fork's cut moves down to
// the last surviving row, and an inherited row that survives the cut but is
// reverted (a queued message among a promoted anchor's kept content) is
// hidden. Rows between the new and the old cut are never copied; a thread
// that forked from this one keeps reading them through its own lineage.
// predicate selects the reverted rows with unqualified item columns, all at
// or after fromTurn; every surviving row sits at or below maxTurn. The
// caller deletes its own reverted rows first, after handing them off. The
// stamps of the fork's copied anchors, whose subtrees can hold the rows
// that leave, are recomputed by w's finish (forkCopyStampsTx).
func retractInheritedTx(tx *sql.Tx, w *cardWrite, threadID string, fromTurn int, predicate string, args []any, maxTurn int) error {
	var cutTurn, cutItem int
	var source, title string
	var forkedAt int64
	var divider bool
	err := tx.QueryRow(
		`SELECT l.cut_turn_index, l.cut_item_index, t.fork_source_thread_id, t.fork_source_title, t.created_at,
		        EXISTS(SELECT 1 FROM items WHERE items.thread_id = t.id AND items.id = ?)
		   FROM thread_fork_lineage l JOIN threads t ON t.id = l.thread_id
		  WHERE l.thread_id = ? AND l.depth = 1`, forkDividerID(threadID), threadID,
	).Scan(&cutTurn, &cutItem, &source, &title, &forkedAt, &divider)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read fork cut of %s: %w", threadID, err)
	}
	copies, err := forkCopyStampsTx(tx, threadID)
	if err != nil {
		return err
	}
	survivor, survives, err := lastRowThroughTurnTx(tx, threadID, maxTurn,
		"NOT ("+predicate+") AND items.id <> ?", append(append([]any{}, args...), forkDividerID(threadID))...)
	if err != nil {
		return err
	}
	if !survives {
		// Nothing inherited survives: the fork is an ordinary thread from
		// here on. Its hides stay, since its forks read through them.
		if _, err := tx.Exec(`DELETE FROM thread_fork_lineage WHERE thread_id = ?`, threadID); err != nil {
			return fmt.Errorf("store: unlink fork %s: %w", threadID, err)
		}
		if _, err := tx.Exec(`DELETE FROM items WHERE thread_id = ? AND id = ?`, threadID, forkDividerID(threadID)); err != nil {
			return fmt.Errorf("store: clear fork %s divider: %w", threadID, err)
		}
		return forkViewChangedTx(tx, w, threadID, copies)
	}
	// The cut sits just after the survivor.
	lowered := survivor.turn < cutTurn || (survivor.turn == cutTurn && survivor.item+1 < cutItem)
	newTurn, newItem := cutTurn, cutItem
	if lowered {
		newTurn, newItem = survivor.turn, survivor.item+1
	}
	reverted, err := queryInheritedRows(tx, threadID, allLevels, timelineSelection{
		Turn: "?", TurnArgs: []any{fromTurn}, FromTurn: true,
		Where:     "(" + predicate + ") AND (items.turn_index, items.item_index) < (?, ?)",
		WhereArgs: append(append([]any{}, args...), newTurn, newItem),
	})
	if err != nil {
		return err
	}
	if !lowered && len(reverted) == 0 && divider {
		return nil
	}
	if len(reverted) > 0 {
		ids := make([]string, len(reverted))
		for i, row := range reverted {
			ids[i] = row.id
		}
		if err := handOffRemovedIDsTx(tx, threadID, ids); err != nil {
			return err
		}
		if err := hideForkRowsTx(tx, threadID, ids); err != nil {
			return err
		}
	}
	if lowered {
		// The fork owns the row of the turn its cut falls in.
		if _, err := tx.Exec(
			`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
			 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
			   FROM timeline_turns t
			  WHERE t.thread_id = ? AND t.turn_index = ?
			    AND NOT EXISTS (SELECT 1 FROM turns own WHERE own.thread_id = ? AND own.turn_index = t.turn_index)`,
			threadID, threadID, threadID, newTurn, threadID,
		); err != nil {
			return fmt.Errorf("store: copy fork %s cut turn: %w", threadID, err)
		}
		if _, err := tx.Exec(
			`UPDATE thread_fork_lineage SET cut_turn_index = ?, cut_item_index = ?
			  WHERE thread_id = ? AND (cut_turn_index, cut_item_index) > (?, ?)`,
			newTurn, newItem, threadID, newTurn, newItem,
		); err != nil {
			return fmt.Errorf("store: lower fork %s cut: %w", threadID, err)
		}
		if _, err := tx.Exec(
			`UPDATE threads SET fork_cut_turn_index = ?, fork_cut_item_index = ? WHERE id = ?`,
			newTurn, newItem, threadID,
		); err != nil {
			return fmt.Errorf("store: record fork %s cut: %w", threadID, err)
		}
	}
	// The caller's delete takes the divider with it when the revert reaches
	// the cut; it goes back at the (possibly lowered) cut, dated to the fork.
	if lowered || !divider {
		if err := placeForkDividerTx(tx, w, threadID, forkOrigin{SourceThreadID: source, SourceTitle: title}, newTurn, newItem, forkedAt); err != nil {
			return err
		}
	}
	return forkViewChangedTx(tx, w, threadID, copies)
}

// forkViewChangedTx records a change to which inherited rows threadID
// shows: its stamps move (bumpForkViewTx), its turn-error pair is
// recomputed, because the change writes no row or turn whose trigger
// would, and w's finish recomputes the stamps of copies, the copied
// anchors forkCopyStampsTx listed before the change.
func forkViewChangedTx(tx *sql.Tx, w *cardWrite, threadID string, copies []string) error {
	if err := bumpForkViewTx(tx, threadID); err != nil {
		return err
	}
	if err := recomputeTurnErrorsTx(tx, threadID); err != nil {
		return err
	}
	w.subtreesChanged(copies)
	return nil
}

// forkCopyStampsSQL lists the stamped rows below a pointer fork's cut. Only
// a copy of an inherited row sits there, and a copy is the only own row
// whose subtree can hold inherited rows, so these are the stamps a change
// to the inherited rows the fork shows can move.
const forkCopyStampsSQL = `SELECT s.item_id FROM thread_fork_lineage l
  CROSS JOIN subagent_aggregates s ON s.thread_id = l.thread_id
  CROSS JOIN items i ON i.thread_id = s.thread_id AND i.id = s.item_id
 WHERE l.thread_id = ? AND l.depth = 1
   AND (i.turn_index, i.item_index) < (l.cut_turn_index, l.cut_item_index)`

// forkCopyStampsTx runs forkCopyStampsSQL, before the change it serves
// moves the cut.
func forkCopyStampsTx(q sqlQueryer, threadID string) ([]string, error) {
	ids, err := queryIDs(q, forkCopyStampsSQL, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: list copied anchors of fork %s: %w", threadID, err)
	}
	return ids, nil
}

// bumpForkViewTx records a change to which inherited rows a thread shows.
// Rows left the timeline, so the epoch moves with the revision.
func bumpForkViewTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(
		`UPDATE threads SET history_rev = history_rev + 1, history_epoch = history_epoch + 1 WHERE id = ?`, threadID,
	); err != nil {
		return fmt.Errorf("store: stamp fork %s view: %w", threadID, err)
	}
	return nil
}

// hideInheritedItemTx removes one inherited row from threadID's timeline.
// It reports false when threadID does not show itemID as inherited.
func hideInheritedItemTx(tx *sql.Tx, w *cardWrite, threadID, itemID string) (bool, error) {
	rows, err := inheritedRowsByID(tx, threadID, []string{itemID}, allLevels)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	copies, err := forkCopyStampsTx(tx, threadID)
	if err != nil {
		return false, err
	}
	if err := handOffRemovedIDsTx(tx, threadID, []string{itemID}); err != nil {
		return false, err
	}
	if err := hideForkRowsTx(tx, threadID, []string{itemID}); err != nil {
		return false, err
	}
	return true, forkViewChangedTx(tx, w, threadID, copies)
}

// detachForkDescendantsTx runs before threadID's row is deleted. The history
// belongs to threadID, so it goes with it: every fork that reads through
// threadID stops at the level before it. The divider of each fork made from
// threadID, including one that has since materialized and every copy of it,
// records that the source is gone and its title, so rendering it never has
// to look for the source. A fork that stops reading rows recomputes what
// its copied anchors count and its turn-error pair (forkViewChangedTx).
//
// A detached fork whose materialization has not finished first loses the
// copies it made of rows it read through threadID (rollBackForkCopiesTx),
// so it keeps no part of that history.
//
// It records against tx (fork_moves.go) every thread whose stamps it moves:
// the detached forks, each thread holding a divider it marks, and the forks
// that show a marked divider in place. It is idempotent: a fork already
// detached reads nothing through threadID and its dividers are marked, so
// a later detach of threadID (a delete retried after one that failed
// partway) neither marks nor stamps it again.
func (s *Store) detachForkDescendantsTx(tx *sql.Tx, threadID string) error {
	var title string
	if err := tx.QueryRow(`SELECT title FROM threads WHERE id = ?`, threadID).Scan(&title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("store: read fork source %s: %w", threadID, err)
	}
	detached, err := queryIDs(tx, `SELECT DISTINCT thread_id FROM thread_fork_lineage WHERE ancestor_id = ?`, threadID)
	if err != nil {
		return fmt.Errorf("store: list forks of %s: %w", threadID, err)
	}
	if len(detached) == 0 {
		// A materialized fork reads nothing through threadID, but its
		// divider still names it.
		var forked bool
		if err := tx.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM threads WHERE fork_source_thread_id = ? AND fork_source_thread_id <> '')`, threadID,
		).Scan(&forked); err != nil {
			return fmt.Errorf("store: look for forks of %s: %w", threadID, err)
		}
		if !forked {
			return nil
		}
	}
	// The copies an unfinished materialization made of the rows a fork
	// stops reading are rolled back before it stops. DeleteThreadPaced has
	// rolled them back in paced transactions; what is left goes here.
	writes := make(map[string]*cardWrite, len(detached))
	copies := make(map[string][]string, len(detached))
	for _, id := range detached {
		w := s.bulkItemWrites(tx, id, false)
		for {
			removed, err := rollBackForkCopiesTx(tx, w, id, threadID, forkCopyRollbackChunk)
			if err != nil {
				return err
			}
			if removed < forkCopyRollbackChunk {
				break
			}
		}
		writes[id] = w
		if copies[id], err = forkCopyStampsTx(tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM thread_fork_lineage
		  WHERE EXISTS (SELECT 1 FROM thread_fork_lineage gone
		                 WHERE gone.ancestor_id = ? AND gone.thread_id = thread_fork_lineage.thread_id
		                   AND thread_fork_lineage.depth >= gone.depth)`, threadID,
	); err != nil {
		return fmt.Errorf("store: detach forks of %s: %w", threadID, err)
	}
	if _, err := tx.Exec(
		`UPDATE threads SET fork_source_title = ? WHERE fork_source_thread_id = ? AND fork_source_thread_id <> ''`,
		title, threadID,
	); err != nil {
		return fmt.Errorf("store: record deleted fork source %s: %w", threadID, err)
	}
	// A fork's divider is also copied into the forks made from it when it
	// is handed off or materialized, so each thread descended from a fork
	// of threadID is checked for a copy, by primary key. The mark stamps
	// the holder, and trg_items_fork_reader_stamp the forks that show the
	// holder's divider, which the mark returns. A divider already marked
	// is left alone.
	marked, err := tx.Query(
		`WITH RECURSIVE forks(id) AS (
		   SELECT id FROM threads WHERE fork_source_thread_id = ?1 AND fork_source_thread_id <> ''
		 ), holders(id) AS (
		   SELECT id FROM forks
		   UNION
		   SELECT t.id FROM holders h JOIN threads t ON t.fork_source_thread_id = h.id AND t.fork_source_thread_id <> ''
		 )
		 UPDATE items
		    SET meta = json_set(CASE WHEN json_valid(meta) THEN meta ELSE '{}' END,
		                        '$.sourceDeleted', json('true'), '$.sourceTitle', ?2)
		  WHERE (thread_id, id) IN (SELECT holders.id, 'fork-origin-' || forks.id FROM holders CROSS JOIN forks)
		    AND tool_name = '`+forkDividerToolName+`'
		    AND json_extract(CASE WHEN json_valid(meta) THEN meta ELSE '{}' END, '$.sourceDeleted') IS NOT 1
		 RETURNING thread_id, `+forkReadersOfRowSQL,
		threadID, title,
	)
	if err != nil {
		return fmt.Errorf("store: mark fork dividers of %s: %w", threadID, err)
	}
	var readers []string
	for marked.Next() {
		var holder, shown string
		if err := marked.Scan(&holder, &shown); err != nil {
			return errors.Join(fmt.Errorf("store: scan a marked fork divider of %s: %w", threadID, err), marked.Close())
		}
		recordForkMovesTx(tx, holder)
		readers = append(readers, shown)
	}
	if err := errors.Join(marked.Err(), marked.Close()); err != nil {
		return fmt.Errorf("store: mark fork dividers of %s: %w", threadID, err)
	}
	for _, shown := range readers {
		if err := recordForkReadersTx(tx, shown); err != nil {
			return err
		}
	}
	recordForkMovesTx(tx, detached...)
	for _, id := range detached {
		w := writes[id]
		if err := forkViewChangedTx(tx, w, id, copies[id]); err != nil {
			return err
		}
		if err := w.finish(); err != nil {
			return err
		}
	}
	return nil
}

// materializeForkBatch bounds how many inherited rows one materialization
// transaction copies.
const materializeForkBatch = 500

// ErrForkSourceDeleting reports a materialization that stopped because a
// thread the fork reads from is being deleted. The delete rolls back the
// copies of the rows the fork read through that thread
// (rollBackForkCopiesTx); a later materialization copies what the fork
// reads once the delete has detached it.
var ErrForkSourceDeleting = errors.New("store: a thread this fork reads from is being deleted")

// MaterializeForkHistory makes threadID own every row it reads from its
// ancestors, then drops its lineage. The transfer export runs it, because
// a conversation that leaves this database must carry its history.
//
// It copies in bounded transactions. Each batch replaces inherited rows
// with identical own rows, so the timeline reads the same between batches
// and nothing waits on the whole copy, and records its copies
// (thread_fork_copied). The last transaction copies the rows that list
// attachments, with the ownership of the attachments they show
// (copyInheritedRowsTx), the rest of the rows and the turn rows, drops
// the lineage and the records. Until then the fork owns no attachment
// through the copies, and the delete of an ancestor can roll back the
// copies of the rows the fork reads through it.
//
// A batch or the last transaction that finds an ancestor being deleted
// copies nothing and returns ErrForkSourceDeleting. A batch after the
// delete has detached the fork copies what the fork reads without that
// ancestor. The records outlive an error, a cancel or a crash; a later
// materialization copies what is left.
func (s *Store) MaterializeForkHistory(ctx context.Context, threadID string) error {
	return s.materializeForkHistory(ctx, threadID, nil)
}

// materializeForkHistory is MaterializeForkHistory with a hook run after
// each committed batch, for tests.
func (s *Store) materializeForkHistory(ctx context.Context, threadID string, afterBatch func()) error {
	depth, err := forkLineageDepth(s.reader(), threadID)
	if err != nil || depth == 0 {
		return err
	}
	pending, err := queryInheritedRows(s.reader(), threadID, allLevels, timelineSelection{
		Where: "NOT (" + attachmentBearingSQL + ")",
	})
	if err != nil {
		return err
	}
	for start := 0; start < len(pending); start += materializeForkBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids := make([]string, 0, materializeForkBatch)
		for _, row := range pending[start:min(start+materializeForkBatch, len(pending))] {
			ids = append(ids, row.id)
		}
		if err := s.materializeForkRows(threadID, ids); err != nil {
			return err
		}
		if afterBatch != nil {
			afterBatch()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.finishForkMaterialization(threadID)
}

// deletingForkAncestorSQL reports whether a thread reads from one whose
// delete has begun.
const deletingForkAncestorSQL = `SELECT EXISTS (SELECT 1 FROM thread_fork_lineage l JOIN threads t ON t.id = l.ancestor_id
  WHERE l.thread_id = ? AND t.deleting = 1)`

// requireNoDeletingAncestorTx fails a materialization transaction whose
// fork reads from a thread whose delete has begun (threads.deleting). From
// that mark until its detach, the delete rolls back the fork's copies, so
// a copy made in between would outlive it.
func requireNoDeletingAncestorTx(tx *sql.Tx, threadID string) error {
	var deleting bool
	if err := tx.QueryRow(deletingForkAncestorSQL, threadID).Scan(&deleting); err != nil {
		return fmt.Errorf("store: check the ancestors of %s: %w", threadID, err)
	}
	if deleting {
		return fmt.Errorf("store: materialize %s: %w", threadID, ErrForkSourceDeleting)
	}
	return nil
}

func (s *Store) materializeForkRows(threadID string, ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin materialize %s: %w", threadID, err)
	}
	defer tx.Rollback()
	if err := requireNoDeletingAncestorTx(tx, threadID); err != nil {
		return err
	}
	rows, err := inheritedRowsByID(tx, threadID, ids, allLevels)
	if err != nil {
		return err
	}
	if err := copyInheritedRowsStampedTx(tx, threadID, rows); err != nil {
		return err
	}
	copied := make([][2]string, len(rows))
	for i, row := range rows {
		copied[i] = [2]string{row.id, row.owner}
	}
	list, err := json.Marshal(copied)
	if err != nil {
		return fmt.Errorf("store: encode the rows materialized into %s: %w", threadID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO thread_fork_copied (thread_id, item_id, source_id)
		 SELECT ?, json_extract(value, '$[0]'), json_extract(value, '$[1]') FROM json_each(?)`, threadID, string(list),
	); err != nil {
		return fmt.Errorf("store: record the rows materialized into %s: %w", threadID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit materialize %s: %w", threadID, err)
	}
	return nil
}

func (s *Store) finishForkMaterialization(threadID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin finish materialize %s: %w", threadID, err)
	}
	defer tx.Rollback()
	if err := requireNoDeletingAncestorTx(tx, threadID); err != nil {
		return err
	}
	// Batches only ever shrink what is left to copy: a write between them
	// copies or hides rows, and nothing new appears below a cut. This read
	// runs in the transaction that drops the lineage, so no inherited row
	// is left behind whatever ran between batches.
	rest, err := queryInheritedRows(tx, threadID, allLevels, timelineSelection{})
	if err != nil {
		return err
	}
	if err := copyInheritedRowsStampedTx(tx, threadID, rest); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ? || ':' || t.turn_index, ?, t.turn_index, t.started_at, t.completed_at,
		    t.stop_reason, t.assistant_message_id, t.token_usage_json, t.error_message, t.provider_turn_id
		   FROM timeline_turns t
		  WHERE t.thread_id = ?
		    AND NOT EXISTS (SELECT 1 FROM turns own WHERE own.thread_id = ? AND own.turn_index = t.turn_index)`,
		threadID, threadID, threadID, threadID,
	); err != nil {
		return fmt.Errorf("store: materialize %s turns: %w", threadID, err)
	}
	// The thread's hides stay: a thread forked from it reads its ancestors
	// through the levels beyond it, filtered by those hides, including rows
	// below a cut this thread lowered and therefore did not copy.
	if _, err := tx.Exec(`DELETE FROM thread_fork_lineage WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: unlink materialized %s: %w", threadID, err)
	}
	// The copies are the thread's own history from here on.
	if _, err := tx.Exec(`DELETE FROM thread_fork_copied WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: settle the rows materialized into %s: %w", threadID, err)
	}
	if err := bumpHistoryRevTx(tx, threadID, "store: stamp materialized fork"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit finish materialize %s: %w", threadID, err)
	}
	return nil
}

// forkCopyRollbackChunk bounds how many copies one rollback statement
// removes, as deleteThreadItemChunk bounds a delete's.
const forkCopyRollbackChunk = 500

// forkCopiesToRollBackSQL lists, in timeline order, a chunk of the
// recorded copies fork ?1 made of rows it reads through thread ?2: rows
// ?2 holds, or a thread ?1 reads through ?2, which the detach of ?2 stops
// ?1 reading. Copies of rows a nearer ancestor holds stay: the fork reads
// them after the detach too, and the ancestor may have rewritten its own
// rows since. In timeline order a chunk rolls back an anchor with the
// children that follow it, so a later chunk's rows have no copied anchor
// left to recompute.
const forkCopiesToRollBackSQL = `SELECT c.item_id FROM thread_fork_lineage gone
  CROSS JOIN thread_fork_lineage l ON l.thread_id = gone.thread_id AND l.depth >= gone.depth
  CROSS JOIN thread_fork_copied c ON c.thread_id = l.thread_id AND c.source_id = l.ancestor_id
  CROSS JOIN items i ON i.thread_id = c.thread_id AND i.id = c.item_id
 WHERE gone.thread_id = ?1 AND gone.ancestor_id = ?2
 ORDER BY i.turn_index, i.item_index LIMIT ?3`

// rollBackForkCopiesTx removes up to limit of the copies forkID's
// unfinished materialization made of rows it reads through goneID
// (forkCopiesToRollBackSQL), with the hides that let them sit below its cut,
// so the fork reads those rows from its ancestors again, as it did before
// it copied them. It returns how many it removed.
//
// The rows are an ancestor's history, identical to what the fork reads in
// their place, and no row the fork wrote sits under one
// (settleForkCopiesTx): the fork shows the rows and cards it showed before
// it copied them, and no copied anchor it keeps counts a changed subtree.
// A thread forked from forkID that showed a copy reads the same row one
// level further, so nothing is handed off to it: a hand-off would keep
// the row after goneID's delete detaches that fork too.
func rollBackForkCopiesTx(tx *sql.Tx, w *cardWrite, forkID, goneID string, limit int) (int, error) {
	ids, err := queryIDs(tx, forkCopiesToRollBackSQL, forkID, goneID, limit)
	if err != nil {
		return 0, fmt.Errorf("store: list the rows %s copied through %s: %w", forkID, goneID, err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return 0, err
	}
	action := "store: roll back the rows " + forkID + " copied through " + goneID
	if err := withHistoryBulkLoadTx(tx, forkID, func() error {
		removed, err := deleteItemRowsTx(tx, w, `id IN (SELECT value FROM json_each(?))`, []any{list}, action)
		if err != nil {
			return err
		}
		if removed != int64(len(ids)) {
			return fmt.Errorf("%s: removed %d of %d", action, removed, len(ids))
		}
		if _, err := tx.Exec(
			`DELETE FROM thread_fork_hidden WHERE thread_id = ? AND item_id IN (SELECT value FROM json_each(?))`, forkID, list,
		); err != nil {
			return fmt.Errorf("%s: unhide: %w", action, err)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// forksWithCopiesSQL lists the forks that read through thread ?1 and hold
// recorded copies of rows they read through it (forkCopiesToRollBackSQL).
const forksWithCopiesSQL = `SELECT DISTINCT gone.thread_id FROM thread_fork_lineage gone
 WHERE gone.ancestor_id = ?1 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   CROSS JOIN thread_fork_copied c ON c.thread_id = l.thread_id AND c.source_id = l.ancestor_id
  WHERE l.thread_id = gone.thread_id AND l.depth >= gone.depth)`

// rollBackForkCopiesThrough rolls back, in bounded transactions with pause
// between them, the copies unfinished materializations made of rows their
// forks read through threadID (rollBackForkCopiesTx). DeleteThreadPaced
// runs it after the mark, which stops every materialization through
// threadID from copying more (requireNoDeletingAncestorTx), and before the
// detach, so each fork reads the same rows throughout and the detach has
// none left to roll back. A chunk moves the fork's stamps and recomputes
// its turn-error pair (forkViewChangedTx); it changes no subtree of an
// anchor the fork keeps, so it names no copied anchor to recompute.
func (s *Store) rollBackForkCopiesThrough(threadID string, pause ChunkPause) error {
	forks, err := queryIDs(s.reader(), forksWithCopiesSQL, threadID)
	if err != nil {
		return fmt.Errorf("store: list the forks of %s with unfinished copies: %w", threadID, err)
	}
	for _, fork := range forks {
		for {
			var removed int
			if err := s.bulkWriteItems(fork, "roll back materialized rows", func(tx *sql.Tx, w *cardWrite) error {
				var err error
				if removed, err = rollBackForkCopiesTx(tx, w, fork, threadID, forkCopyRollbackChunk); err != nil || removed == 0 {
					return err
				}
				recordForkMovesTx(tx, fork)
				return forkViewChangedTx(tx, w, fork, nil)
			}); err != nil {
				return err
			}
			if removed < forkCopyRollbackChunk {
				break
			}
			if pause != nil {
				pause()
			}
		}
	}
	return nil
}
