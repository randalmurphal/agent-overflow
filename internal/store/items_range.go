package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The timeline coordinate reads in this file answer "where does this
// thread start and end" and "give me the rows between these two
// coordinates" without walking the thread turn by turn. They are the
// store half of the agent thread tools' transcript window, which names a
// position range and never a turn.
//
// Both go through timelineArms (timeline_arms.go): an ordered, limited
// read of the logical timeline must be rendered as the two physical arms
// so each walks its own (thread, turn_index, item_index) index instead of
// pouring both arms into a temp b-tree.

// ThreadTimelineBounds returns the oldest and newest rows of a thread's
// logical timeline as timeline coordinates. ok is false for a thread with
// no rows at all, including one whose newest turn has been recorded but
// has not persisted an item yet.
//
// Every stored row counts, including subagent children and the rows the
// rendered timeline drops (plan_update notifications): the bounds describe
// the coordinate space a transcript window is expressed in, not what a
// window renders. A head-healed prompt's negative item_index is a real
// coordinate and is the oldest bound when it is the oldest row.
//
// Two statements, one index probe each. Nothing here reads a payload.
func (s *Store) ThreadTimelineBounds(threadID string) (oldest, newest TimelineCursor, ok bool, err error) {
	oldest, ok, err = s.threadTimelineEdge(threadID, "turn_index ASC, item_index ASC")
	if err != nil || !ok {
		return TimelineCursor{}, TimelineCursor{}, false, err
	}
	newest, ok, err = s.threadTimelineEdge(threadID, "turn_index DESC, item_index DESC")
	if err != nil || !ok {
		return TimelineCursor{}, TimelineCursor{}, false, err
	}
	return oldest, newest, true, nil
}

func (s *Store) threadTimelineEdge(threadID, orderBy string) (TimelineCursor, bool, error) {
	query, args, err := timelineArms(s.reader(), threadID, timelineSelection{
		Columns: timelineIDColumns,
		OrderBy: orderBy,
		Limit:   1,
	})
	if err != nil {
		return TimelineCursor{}, false, err
	}
	var cursor TimelineCursor
	err = s.reader().QueryRow(query, args...).Scan(&cursor.ItemID, &cursor.TurnIndex, &cursor.ItemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return TimelineCursor{}, false, nil
	}
	if err != nil {
		return TimelineCursor{}, false, fmt.Errorf("store: thread timeline edge for %s: %w", threadID, err)
	}
	return cursor, true, nil
}

// ListItemsInRange returns a thread's timeline rows whose coordinate falls
// inclusively between from and to, oldest first, at most limit rows. The
// cursors' ItemID is ignored: the range is the (turn_index, item_index)
// tuple pair, compared as SQLite row values so one predicate expresses the
// bound the ordering index is built on.
//
// includeChildren decides whether subagent children (rows with a non-empty
// parent_id) are in the range. Everything else the thread stored is:
// unlike the rendered timeline's pagers this read does NOT drop
// plan_update notifications, because a transcript reports what the thread
// holds rather than what the timeline mounts, and a reader asking for a
// position range would otherwise find gaps it cannot account for.
//
// The limit is applied in SQL. A caller that wants the next page passes a
// from one coordinate past the last row it received.
func (s *Store) ListItemsInRange(threadID string, from, to TimelineCursor, limit int, includeChildren bool) ([]Item, error) {
	if limit <= 0 {
		return []Item{}, nil
	}
	where := `(items.turn_index, items.item_index) >= (?, ?)
		   AND (items.turn_index, items.item_index) <= (?, ?)`
	if !includeChildren {
		where += "\n		   AND " + topLevelItemsFilterFor("items.")
	}
	selectedSQL, selectedArgs, err := timelineIDSelection(s.reader(), threadID, timelineSelection{
		Where:     where,
		WhereArgs: []any{from.TurnIndex, from.ItemIndex, to.TurnIndex, to.ItemIndex},
		OrderBy:   "turn_index ASC, item_index ASC",
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	items, err := queryHydratedTimelineItems(s.reader(), threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: list items in range for %s: %w", threadID, err)
	}
	return items, nil
}
