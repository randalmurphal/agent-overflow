package store

import (
	"fmt"
	"slices"
	"strings"
)

var subagentPreviewKinds = []string{"tool_call", "tool_completion", "terminal_interaction", "error", "api_error"}
var subagentPreviewKindPredicate = "items.kind IN ('" + strings.Join(subagentPreviewKinds, "','") + "')"

// The descendant walk already deduplicates (root, id). Aggregate its narrow
// result as it arrives; a SQL window count plus ranked preview sorts and
// retains the entire descendant set merely to return one row per anchor.
type subagentAggregateRow struct {
	root, id, kind, status, summary string
	turnIndex, itemIndex            int
}

func (s *Store) forEachSubagentAggregateRow(q sqlQueryer, threadID string, rootIDs []string, visit func(subagentAggregateRow)) error {
	if len(rootIDs) == 0 {
		return nil
	}
	resolvedSQL, resolvedArgs, err := timelineArms(q, threadID, timelineSelection{
		Columns: func(string, string) string {
			return `rel.root, items.id, items.kind, items.status,
			        CASE WHEN ` + subagentPreviewKindPredicate + ` THEN items.summary ELSE '' END,
			        items.turn_index, items.item_index`
		},
		Source: "rel",
		Where:  "items.id = rel.id",
	})
	if err != nil {
		return err
	}
	args := append(descendantsCTEArgs(threadID, rootIDs), resolvedArgs...)
	rows, err := q.Query(descendantsCTEFromRoots(len(rootIDs))+" SELECT * FROM ("+resolvedSQL+")", args...)
	if err != nil {
		return fmt.Errorf("store: query subagent aggregates for %s: %w", threadID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row subagentAggregateRow
		if err := rows.Scan(&row.root, &row.id, &row.kind, &row.status, &row.summary, &row.turnIndex, &row.itemIndex); err != nil {
			return fmt.Errorf("store: scan subagent aggregate row: %w", err)
		}
		visit(row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate subagent aggregates for %s: %w", threadID, err)
	}
	return nil
}

type subagentAggregateState struct {
	aggregate subagentAnchorAggregate
	preview   subagentAggregateRow
	hasPick   bool
}

func previewKind(kind string) bool {
	return slices.Contains(subagentPreviewKinds, kind)
}

func activeStatus(status string) bool { return status == "running" || status == "streaming" }

func rankedSubagentSummary(row subagentAggregateRow) bool {
	// SQLite's TRIM(summary) in the old rank strips ASCII spaces, not every
	// Unicode whitespace character. Keep that rank; output normalization
	// below still uses TrimSpace, as the previous read did after ranking.
	return previewKind(row.kind) && strings.Trim(row.summary, " ") != ""
}

// The comparison is the exact ORDER BY of the former SQL rank: eligible
// nonblank summary, active status, newest coordinate, then smallest id.
func betterSubagentPreview(a, b subagentAggregateRow) bool {
	aSummary := rankedSubagentSummary(a)
	bSummary := rankedSubagentSummary(b)
	if aSummary != bSummary {
		return aSummary
	}
	if aActive, bActive := activeStatus(a.status), activeStatus(b.status); aActive != bActive {
		return aActive
	}
	if a.turnIndex != b.turnIndex {
		return a.turnIndex > b.turnIndex
	}
	if a.itemIndex != b.itemIndex {
		return a.itemIndex > b.itemIndex
	}
	return a.id < b.id
}

func (state *subagentAggregateState) add(row subagentAggregateRow) {
	state.aggregate.descendantCount++
	if !state.hasPick || betterSubagentPreview(row, state.preview) {
		state.preview = row
		state.hasPick = true
		if previewKind(row.kind) && strings.TrimSpace(row.summary) != "" {
			state.aggregate.latestChildSummary = row.summary
		} else {
			state.aggregate.latestChildSummary = ""
		}
	}
}

// subagentAggregatesByRoot is the whole-transcript aggregate for roots
// without resume rounds. Only counters and one winning preview are retained.
func (s *Store) subagentAggregatesByRoot(q sqlQueryer, threadID string, rootIDs []string) (map[string]subagentAnchorAggregate, error) {
	states := make(map[string]*subagentAggregateState, len(rootIDs))
	err := s.forEachSubagentAggregateRow(q, threadID, rootIDs, func(row subagentAggregateRow) {
		state := states[row.root]
		if state == nil {
			state = &subagentAggregateState{}
			states[row.root] = state
		}
		state.add(row)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]subagentAnchorAggregate, len(states))
	for root, state := range states {
		out[root] = state.aggregate
	}
	return out, nil
}

func subagentBoundContains(bound subagentRoundBounds, row subagentAggregateRow) bool {
	position := TimelineCursor{TurnIndex: row.turnIndex, ItemIndex: row.itemIndex}
	return (bound.lo == nil || !cursorBefore(position, *bound.lo)) &&
		(bound.hi == nil || cursorBefore(position, *bound.hi))
}

// subagentAggregatesByRound partitions the same walk by execution bounds.
// A prompt-less carrier may intentionally overlap real rounds; each bound
// receives its own count, while the transcript total sums real rounds only.
func (s *Store) subagentAggregatesByRound(
	q sqlQueryer, threadID string, rootIDs []string, bounds []subagentRoundBounds,
) (map[string]subagentAnchorAggregate, error) {
	if len(bounds) == 0 {
		return nil, nil
	}
	states := make(map[string]*subagentAggregateState, len(bounds))
	byRoot := make(map[string][]subagentRoundBounds, len(rootIDs))
	for _, bound := range bounds {
		if _, exists := states[bound.anchorID]; exists {
			return nil, fmt.Errorf("store: duplicate subagent aggregate anchor %s", bound.anchorID)
		}
		states[bound.anchorID] = &subagentAggregateState{}
		byRoot[bound.rootID] = append(byRoot[bound.rootID], bound)
	}
	err := s.forEachSubagentAggregateRow(q, threadID, rootIDs, func(row subagentAggregateRow) {
		for _, bound := range byRoot[row.root] {
			if subagentBoundContains(bound, row) {
				states[bound.anchorID].add(row)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]subagentAnchorAggregate, len(states))
	for anchor, state := range states {
		out[anchor] = state.aggregate
	}
	transcript := make(map[string]int, len(rootIDs))
	hasRounds := make(map[string]bool, len(rootIDs))
	for _, bound := range bounds {
		if !bound.round {
			continue
		}
		transcript[bound.rootID] += out[bound.anchorID].descendantCount
		if bound.anchorID != bound.rootID {
			hasRounds[bound.rootID] = true
		}
	}
	for root, total := range transcript {
		if !hasRounds[root] {
			continue
		}
		agg := out[root]
		agg.transcriptDescendantCount = total
		agg.hasTranscriptCount = true
		out[root] = agg
	}
	return out, nil
}
