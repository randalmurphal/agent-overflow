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
	err := q.QueryRow(
		`SELECT summary, turn_index, item_index, parent_id FROM timeline_items
		  WHERE thread_id = ? AND id = ?
		    AND `+userMessageTickFilterFor(""),
		threadID, itemID,
	).Scan(&userText, &turnIndex, &itemIndex, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnPreview{}, false, nil
	}
	if err != nil {
		return TurnPreview{}, false, fmt.Errorf("store: turn preview anchor %s on thread %s: %w", itemID, threadID, err)
	}
	filter := topLevelItemsFilterFor("items.")
	var filterArgs []any
	if parentID != "" {
		scope, err := s.resolveTimelineScope(q, threadID, TimelineSelection{ScopeRootID: parentID})
		if err != nil {
			return TurnPreview{}, false, err
		}
		filter, filterArgs = scope.filter("items.")
	}
	// The ordering keys ride the projection because the compound needs
	// them (timeline_arms.go); the scan drops them.
	walkSQL, walkArgs := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string {
			return `items.kind AS kind, items.summary AS summary,
			        COALESCE(CASE WHEN json_valid(items.meta)
			                      THEN json_extract(items.meta, '$.wire_only') END, 0) AS wire_only,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Where: filter + `
		   AND items.kind IN ('user_text', 'assistant_text')
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`,
		WhereArgs: append(filterArgs, turnIndex, turnIndex, itemIndex),
		OrderBy:   "turn_index ASC, item_index ASC",
		Limit:     turnPreviewScanLimit,
	})
	rows, err := q.Query(walkSQL, walkArgs...)
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

// capRunes truncates on a rune boundary with an ellipsis marker. Wire
// bound only — display truncation is the frontend's.
func capRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
