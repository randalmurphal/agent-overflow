package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SubagentReportPreviewRunes bounds a served report preview: the scan
// window the card's preview rule reads (PREVIEW_SCAN_CHARS in
// frontend/src/lib/utils/subagentGrouping.ts). A rune is at least one
// UTF-16 unit, so the head always covers the window. mirror_pins_test.go
// fails if the two drift.
const SubagentReportPreviewRunes = 512

// SubagentReport is a transcript root's newest direct assistant_text
// child: a parked agent's report (claude-wire.md §E6b).
type SubagentReport struct {
	ID string
	// Preview is the head of the row's summary, SubagentReportPreviewRunes
	// long.
	Preview string
}

// latestSubagentReportSelection selects one root's newest direct
// assistant_text child with a nonblank summary over every timeline arm,
// by the parent key like latestDirectSubagentToolSelection. SQLite's
// substr counts characters, so the preview is cut on a rune boundary.
func latestSubagentReportSelection(rootID string) timelineSelection {
	return timelineSelection{
		Columns: func(_, _ string) string {
			return fmt.Sprintf(`items.id AS id, substr(trim(items.summary, %s), 1, %d) AS preview,
			        items.turn_index AS turn_index, items.item_index AS item_index`, aggBlankSQL, SubagentReportPreviewRunes)
		},
		KeyFirst: true,
		Where: "items.parent_id = ? AND items.parent_id <> '' AND items.kind = 'assistant_text' AND trim(items.summary, " +
			aggBlankSQL + ") <> ''",
		WhereArgs: []any{rootID},
		OrderBy:   "turn_index DESC, item_index DESC",
		Limit:     1,
	}
}

// LatestSubagentReport returns rootID's newest direct assistant_text
// child. Every round of a Claude agent, woken or resumed, is parented to
// its transcript root, so across wakes the newest report wins.
func (s *Store) LatestSubagentReport(threadID, rootID string) (SubagentReport, bool, error) {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return SubagentReport{}, false, nil
	}
	q := s.reader()
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return SubagentReport{}, false, err
	}
	query, args := renderTimelineArms(threadID, depth, latestSubagentReportSelection(rootID))
	var report SubagentReport
	var turnIndex, itemIndex int
	err = q.QueryRow(query, args...).Scan(&report.ID, &report.Preview, &turnIndex, &itemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return SubagentReport{}, false, nil
	}
	if err != nil {
		return SubagentReport{}, false, fmt.Errorf("store: read latest subagent report %s/%s: %w", threadID, rootID, err)
	}
	return report, true, nil
}
