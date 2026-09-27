package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TurnPreview is the nav rail's hover card for one turn: the reader's
// ask and the final assistant reply in the same timeline scope.
type TurnPreview struct {
	UserText      string `json:"userText"`
	AssistantText string `json:"assistantText"`
}

// turnPreviewMaxRunes bounds each preview half at the wire. The card
// renders ~400 characters after whitespace collapse; shipping a giant
// message's full body for a hover would be pure waste.
const turnPreviewMaxRunes = 1000

// turnPreviewScanLimit bounds the walk below one turn. A turn with more
// top-level text rows than this is pathological; the preview then
// reflects the first rows, which is still an honest hover hint.
const turnPreviewScanLimit = 400

// ThreadTurnPreview resolves a nav tick within its own transcript scope.
func (s *Store) ThreadTurnPreview(threadID, itemID string) (TurnPreview, bool, error) {
	type result struct {
		preview TurnPreview
		found   bool
	}
	value, err := readSnapshot(s.reader(), "turn preview", func(q sqlQueryer) (result, error) {
		preview, found, err := s.threadTurnPreview(q, threadID, itemID)
		return result{preview, found}, err
	})
	return value.preview, value.found, err
}

func (s *Store) threadTurnPreview(q sqlQueryer, threadID, itemID string) (TurnPreview, bool, error) {
	var userText, parentID string
	var turnIndex, itemIndex int
	anchor, anchorArgs, err := timelineArms(q, threadID, timelineSelection{
		Columns: func(string, string) string {
			return "items.summary, items.turn_index, items.item_index, items.parent_id"
		},
		KeyFirst:  true,
		Where:     "items.id = ? AND " + userMessageTickFilterFor("items."),
		WhereArgs: []any{itemID},
	})
	if err != nil {
		return TurnPreview{}, false, err
	}
	err = q.QueryRow(anchor, anchorArgs...).Scan(&userText, &turnIndex, &itemIndex, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnPreview{}, false, nil
	}
	if err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview anchor %s on thread %s: %w", itemID, threadID, err)
	}
	filter := mainTimelineFilterFor("items.")
	var filterArgs []any
	if parentID != "" {
		scope, err := s.resolveTimelineScope(q, threadID, TimelineSelection{ScopeRootID: parentID})
		if err != nil {
			return TurnPreview{}, false, err
		}
		filter, filterArgs = scope.filter("items.")
	}
	walk := turnPreviewWalk(filter, filterArgs, TimelineCursor{TurnIndex: turnIndex, ItemIndex: itemIndex})
	walkSQL, walkArgs, err := timelineArms(q, threadID, walk)
	if err != nil {
		return TurnPreview{}, false, err
	}
	rows, err := q.Query(turnPreviewRowsSQL(walkSQL), walkArgs...)
	if err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview walk after %s on thread %s: %w", itemID, threadID, err)
	}
	defer rows.Close()
	assistantText := ""
	for rows.Next() {
		var kind, summary string
		var wireOnly, rowTurnIndex, rowItemIndex int
		if err := rows.Scan(&kind, &summary, &wireOnly, &rowTurnIndex, &rowItemIndex); err != nil {
			return TurnPreview{}, false, fmt.Errorf("store: scan turn preview row on thread %s: %w", threadID, err)
		}
		if kind == "user_text" {
			// A wire-only injection mid-turn is context, not the next
			// ask — same rule as the tick predicate above.
			if wireOnly == 1 {
				continue
			}
			break
		}
		if summary != "" {
			assistantText = summary
		}
	}
	if err := rows.Err(); err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview walk after %s on thread %s: %w", itemID, threadID, err)
	}
	return TurnPreview{
		UserText:      capRunes(userText, turnPreviewMaxRunes),
		AssistantText: capRunes(assistantText, turnPreviewMaxRunes),
	}, true, nil
}

// turnPreviewWalk selects the text rows filter keeps that follow anchor,
// in order, by kind and position alone: on the main timeline every arm
// then walks a covering top-level index, and turnPreviewRowsSQL reads the
// summary and meta of only the rows the walk keeps.
func turnPreviewWalk(filter string, filterArgs []any, anchor TimelineCursor) timelineSelection {
	sel := cursorBound(anchor, true)
	sel.Columns = func(string, string) string {
		return `items.kind AS kind, items.turn_index AS turn_index, items.item_index AS item_index`
	}
	sel.RowIDs = true
	sel.Where = filter + `
		   AND items.kind IN ('user_text', 'assistant_text')
		   AND ` + sel.Where
	sel.WhereArgs = append(filterArgs, sel.WhereArgs...)
	sel.OrderBy, sel.Limit = "turn_index ASC, item_index ASC", turnPreviewScanLimit
	return sel
}

// turnPreviewRowsSQL reads the kind, summary, wire_only flag and position
// of each row walk, a rendered turnPreviewWalk, selects.
func turnPreviewRowsSQL(walk string) string {
	meta := locatedColumn("meta")
	return locatedRowsSQL(`w.kind, `+locatedColumn("summary")+`,
		       COALESCE(CASE WHEN json_valid(`+meta+`) THEN json_extract(`+meta+`, '$.wire_only') END, 0),
		       w.turn_index, w.item_index`, walk, "ASC")
}

// capRunes truncates on a rune boundary with an ellipsis marker. Wire
// bound only — display truncation is the frontend's.
func capRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
