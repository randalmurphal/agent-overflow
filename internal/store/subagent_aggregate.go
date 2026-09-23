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
	root, id, kind, summary string
	turnIndex, itemIndex    int
}

func (s *Store) forEachSubagentAggregateRow(q sqlQueryer, threadID string, rootIDs []string, visit func(subagentAggregateRow)) error {
	if len(rootIDs) == 0 {
		return nil
	}
	resolvedSQL, resolvedArgs := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string {
			return `rel.root, items.id, items.kind,
			        CASE WHEN ` + subagentPreviewKindPredicate + ` THEN items.summary ELSE '' END,
			        items.turn_index, items.item_index`
		},
		Source: "rel",
		Where:  "items.id = rel.id",
	})
	args := append(descendantsCTEArgs(threadID, rootIDs), resolvedArgs...)
	rows, err := q.Query(descendantsCTEFromRoots(len(rootIDs))+" SELECT * FROM ("+resolvedSQL+")", args...)
	if err != nil {
		return fmt.Errorf("store: query subagent aggregates for %s: %w", threadID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row subagentAggregateRow
		if err := rows.Scan(&row.root, &row.id, &row.kind, &row.summary, &row.turnIndex, &row.itemIndex); err != nil {
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

// subagentPreviewBlank is the whitespace a preview summary may consist
// of and still count as blank. It is ASCII so that a SQL spelling of
// the same test can agree with this one byte for byte.
const subagentPreviewBlank = " \t\n\v\f\r"

// previewableSubagentRow reports whether a descendant can be its round's
// preview: a preview-kind row with a nonblank summary.
func previewableSubagentRow(kind, summary string) bool {
	return previewKind(kind) && strings.Trim(summary, subagentPreviewBlank) != ""
}

// betterSubagentPreview is the preview rule: the newest previewable row
// in the round wins, ties by smallest id. A row's status does not enter
// into it; the card shows the agent's latest tool activity.
func betterSubagentPreview(a, b subagentAggregateRow) bool {
	aOK := previewableSubagentRow(a.kind, a.summary)
	if bOK := previewableSubagentRow(b.kind, b.summary); aOK != bOK {
		return aOK
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
	if !previewableSubagentRow(row.kind, row.summary) {
		return
	}
	if !state.hasPick || betterSubagentPreview(row, state.preview) {
		state.preview = row
		state.hasPick = true
		state.aggregate.latestChildSummary = row.summary
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
