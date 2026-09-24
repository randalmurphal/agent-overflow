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

func forEachSubagentAggregateRow(q sqlQueryer, threadID string, rootIDs []string, visit func(subagentAggregateRow)) error {
	if len(rootIDs) == 0 {
		return nil
	}
	resolvedSQL, resolvedArgs, err := timelineArms(q, threadID, timelineSelection{
		Columns: func(string, string) string {
			return `rel.root, items.id, items.kind,
			        CASE WHEN ` + subagentPreviewKindPredicate + ` THEN items.summary ELSE '' END,
			        items.turn_index, items.item_index`
		},
		Source: "rel",
		Where:  "items.id = rel.id",
	})
	if err != nil {
		return err
	}
	walk, walkArgs, err := descendantsWalk(q, threadID, rootIDs, visibleItemsFilterFor)
	if err != nil {
		return err
	}
	rows, err := q.Query(walk+" SELECT * FROM ("+resolvedSQL+")", append(walkArgs, resolvedArgs...)...)
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

// subagentAggregateAccumulator folds one anchor's rows: the card values,
// plus the preview row and the newest position the write-time stamp keeps
// beside them (subagent_aggregates).
type subagentAggregateAccumulator struct {
	aggregate subagentAnchorAggregate
	preview   subagentAggregateRow
	hasPick   bool
	newest    TimelineCursor
	hasNewest bool
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

func (acc *subagentAggregateAccumulator) add(row subagentAggregateRow) {
	acc.aggregate.descendantCount++
	acc.noteNewest(TimelineCursor{TurnIndex: row.turnIndex, ItemIndex: row.itemIndex})
	if !previewableSubagentRow(row.kind, row.summary) {
		return
	}
	if !acc.hasPick || betterSubagentPreview(row, acc.preview) {
		acc.preview = row
		acc.hasPick = true
		acc.aggregate.latestChildSummary = row.summary
	}
}

func (acc *subagentAggregateAccumulator) noteNewest(position TimelineCursor) {
	if !acc.hasNewest || cursorBefore(acc.newest, position) {
		acc.newest = position
		acc.hasNewest = true
	}
}

// subagentAggregatesByRoot is the whole-transcript aggregate for roots
// without resume rounds. Only counters and one winning preview are retained.
func subagentAggregatesByRoot(q sqlQueryer, threadID string, rootIDs []string) (map[string]subagentAnchorAggregate, error) {
	states := make(map[string]*subagentAggregateAccumulator, len(rootIDs))
	err := forEachSubagentAggregateRow(q, threadID, rootIDs, func(row subagentAggregateRow) {
		state := states[row.root]
		if state == nil {
			state = &subagentAggregateAccumulator{}
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
func subagentAggregatesByRound(
	q sqlQueryer, threadID string, rootIDs []string, bounds []subagentRoundBounds,
) (map[string]subagentAnchorAggregate, error) {
	accumulators, transcripts, err := subagentAccumulatorsByRound(q, threadID, rootIDs, bounds)
	if err != nil || accumulators == nil {
		return nil, err
	}
	out := make(map[string]subagentAnchorAggregate, len(accumulators))
	for anchor, acc := range accumulators {
		out[anchor] = acc.aggregate
	}
	for root, transcript := range transcripts {
		agg := out[root]
		agg.transcriptDescendantCount = transcript.aggregate.descendantCount
		agg.hasTranscriptCount = true
		out[root] = agg
	}
	return out, nil
}

// subagentAccumulatorsByRound is subagentAggregatesByRound before it is
// reduced to card values: one accumulator per bound, and one per root
// whose transcript has rounds holding the whole-transcript count and
// newest position. Only a root with a bound of its own has a transcript:
// one without is another root's carrier (subagentRoundBoundsFor).
func subagentAccumulatorsByRound(
	q sqlQueryer, threadID string, rootIDs []string, bounds []subagentRoundBounds,
) (map[string]*subagentAggregateAccumulator, map[string]*subagentAggregateAccumulator, error) {
	if len(bounds) == 0 {
		return nil, nil, nil
	}
	states := make(map[string]*subagentAggregateAccumulator, len(bounds))
	byRoot := make(map[string][]subagentRoundBounds, len(rootIDs))
	for _, bound := range bounds {
		if _, exists := states[bound.anchorID]; exists {
			return nil, nil, fmt.Errorf("store: duplicate subagent aggregate anchor %s", bound.anchorID)
		}
		states[bound.anchorID] = &subagentAggregateAccumulator{}
		byRoot[bound.rootID] = append(byRoot[bound.rootID], bound)
	}
	err := forEachSubagentAggregateRow(q, threadID, rootIDs, func(row subagentAggregateRow) {
		for _, bound := range byRoot[row.root] {
			if subagentBoundContains(bound, row) {
				states[bound.anchorID].add(row)
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	transcripts := make(map[string]*subagentAggregateAccumulator, len(rootIDs))
	hasRounds := make(map[string]bool, len(rootIDs))
	ownBound := make(map[string]bool, len(rootIDs))
	for _, bound := range bounds {
		if bound.round && bound.anchorID == bound.rootID {
			ownBound[bound.rootID] = true
		}
	}
	for _, bound := range bounds {
		if !bound.round {
			continue
		}
		round := states[bound.anchorID]
		transcript := transcripts[bound.rootID]
		if transcript == nil {
			transcript = &subagentAggregateAccumulator{}
			transcripts[bound.rootID] = transcript
		}
		transcript.aggregate.descendantCount += round.aggregate.descendantCount
		if round.hasNewest {
			transcript.noteNewest(round.newest)
		}
		if bound.anchorID != bound.rootID {
			hasRounds[bound.rootID] = true
		}
	}
	for root := range transcripts {
		if !hasRounds[root] || !ownBound[root] {
			delete(transcripts, root)
		}
	}
	return states, transcripts, nil
}
