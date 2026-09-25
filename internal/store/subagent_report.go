package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// latestSubagentReportSelection selects one root's newest direct
// assistant_text child with a nonblank summary created at or after since,
// over every timeline arm, by the parent key like
// latestDirectSubagentToolSelection.
func latestSubagentReportSelection(rootID string, since int64) timelineSelection {
	return timelineSelection{
		Columns: func(_, _ string) string {
			return `items.id AS id, items.turn_index AS turn_index, items.item_index AS item_index`
		},
		KeyFirst: true,
		Where: "items.parent_id = ? AND items.parent_id <> '' AND items.kind = 'assistant_text' AND trim(items.summary, " +
			aggBlankSQL + ") <> '' AND items.created_at >= ?",
		WhereArgs: []any{rootID, since},
		OrderBy:   "turn_index DESC, item_index DESC",
		Limit:     1,
	}
}

// LatestSubagentReport returns the id of rootID's newest direct
// assistant_text child written at or after since: the report of an agent
// run that started then (claude-wire.md §E6b). Every run of a Claude
// agent, woken or resumed, is parented to its transcript root, so a run
// that wrote no text has no report rather than an earlier run's.
func (s *Store) LatestSubagentReport(threadID, rootID string, since int64) (string, bool, error) {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return "", false, nil
	}
	q := s.reader()
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return "", false, err
	}
	query, args := renderTimelineArms(threadID, depth, latestSubagentReportSelection(rootID, since))
	var id string
	var turnIndex, itemIndex int
	err = q.QueryRow(query, args...).Scan(&id, &turnIndex, &itemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read latest subagent report %s/%s: %w", threadID, rootID, err)
	}
	return id, true, nil
}
