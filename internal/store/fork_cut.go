package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"

	"agent-overflow/internal/itemmeta"
)

// Fork cuts: where a pointer fork's inherited history ends
// (docs/architecture/sqlite-store.md#pointer-forks).

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
// The rows it looks for (a revert's last survivor, a fork cut's last row)
// sit in maxTurn or just below it, so the turns are probed newest
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
