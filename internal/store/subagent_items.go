package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Subagent child rows (items whose parent_id points at a launch row)
// are not part of any history window — see paging.go's
// topLevelItemsFilter. Two read-time surfaces replace them:
//
//   - decorateSubagentAnchors stamps every windowed anchor with the
//     aggregates its collapsed SubagentGroup card renders (descendant
//     count + latest-child summary), so the card looks identical
//     whether or not children are loaded.
//   - ListSubagentDescendants loads the full child transcript on
//     demand when the user expands the card.

// Decoration keys merged into the anchor's item meta. The frontend
// grouping (frontend/src/lib/utils/subagentGrouping.ts) reads these as
// fallbacks when no child rows are loaded; computed values from loaded
// children win once they exist.
const (
	metaKeySubagentDescendantCount    = "subagentDescendantCount"
	metaKeySubagentLatestChildSummary = "subagentLatestChildSummary"
)

// maxSubagentDescendants caps one expansion load, mirroring the
// maxWindowItems DoS guard on the window pagers (app_paging.go):
// ListSubagentDescendants is a wire RPC, so a malicious LAN-attached
// caller must not be able to stream an unbounded subtree per call.
// Real subagent transcripts run one to two orders of magnitude below
// this. When the cap binds, the newest rows win — transcripts resolve
// tail-first like every other capped read in this package — and the
// collapsed card still reports the full total via
// decorateSubagentAnchors.
const maxSubagentDescendants = 2000

type subagentAnchorAggregate struct {
	descendantCount    int
	latestChildSummary string
}

// descendantsCTE walks parent_id edges downward from a set of root item
// ids, carrying the originating root through the recursion so per-root
// aggregates fall out of a GROUP BY. UNION (not UNION ALL) dedups
// (root, id) pairs during recursion, so a pathological parent_id cycle
// terminates instead of looping forever.
//
// Plan notes (verified with EXPLAIN QUERY PLAN, SQLite 3.45):
//   - Both hops probe the partial idx_items_parent (thread_id,
//     parent_id) WHERE parent_id <> ”. The explicit `parent_id <> ”`
//     terms below are load-bearing for that: SQLite cannot prove the
//     index predicate from a bound parameter or the `rel.id` join term
//     alone, and without the proof both hops degrade to a whole-thread
//     PK-prefix scan per recursion level.
//   - CROSS JOIN in the recursive hop is a planner directive, not
//     style: with a plain JOIN the planner puts `items` on the outer
//     side and rescans the whole thread once per queued row. CROSS
//     JOIN pins rel(outer) → items(inner), one index probe per row.
//
// The visible-items filter matches the window loaders: plan_update
// notifications never render, so they must not count against the
// collapsed card's "N entries" badge either.
func descendantsCTEFromRoots(rootCount int) string {
	visible := visibleItemsFilterFor("i.")
	return `WITH RECURSIVE rel(root, id) AS (
		SELECT i.parent_id, i.id
		  FROM items i
		 WHERE i.thread_id = ?
		   AND i.parent_id IN (` + placeholders(rootCount) + `)
		   AND i.parent_id <> ''
		   AND ` + visible + `
		UNION
		SELECT rel.root, i.id
		  FROM rel
		  CROSS JOIN items i ON i.parent_id = rel.id
		 WHERE i.thread_id = ?
		   AND i.parent_id <> ''
		   AND ` + visible + `
	)`
}

// decorateSubagentAnchors merges descendant aggregates into the meta of
// every item in `items` that anchors subagent children. Items without
// children are returned untouched. Runs one batched recursive query per
// window load; cost is proportional to the total descendant count of
// in-window anchors.
func (s *Store) decorateSubagentAnchors(threadID string, items []Item) ([]Item, error) {
	if len(items) == 0 {
		return items, nil
	}
	// Only tool_call rows can anchor subagent transcripts (Claude
	// Task/Agent launches, Codex collab_agent spawns). Filtering here
	// keeps the IN list short on plain text-heavy windows.
	rootIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Kind == "tool_call" && strings.TrimSpace(item.ID) != "" {
			rootIDs = append(rootIDs, item.ID)
		}
	}
	if len(rootIDs) == 0 {
		return items, nil
	}

	aggregates, err := s.subagentAggregatesByRoot(threadID, rootIDs)
	if err != nil {
		return nil, err
	}
	if len(aggregates) == 0 {
		return items, nil
	}
	for i := range items {
		agg, ok := aggregates[items[i].ID]
		if !ok {
			continue
		}
		items[i].Meta = mergeSubagentAnchorMeta(items[i].Meta, agg)
	}
	return items, nil
}

// subagentAggregatesByRoot returns, for each root id that has visible
// descendants, the total transitive descendant count and the summary of
// the preview descendant. Preview selection mirrors the frontend's
// pickLatestChildSummary: among descendants with a non-empty summary,
// an active (running/streaming) row beats a terminal one, and within
// each class the highest (turn_index, item_index) wins. The trailing
// i.id key only breaks coordinate ties (corrupt data) so the pick stays
// deterministic.
func (s *Store) subagentAggregatesByRoot(threadID string, rootIDs []string) (map[string]subagentAnchorAggregate, error) {
	// Placeholder order: CTE base hop (threadID, rootIDs...), CTE
	// recursive hop (threadID), aggregate join (threadID).
	args := make([]any, 0, len(rootIDs)+3)
	args = append(args, threadID)
	for _, id := range rootIDs {
		args = append(args, id)
	}
	args = append(args, threadID, threadID)

	rows, err := s.reader().Query(descendantsCTEFromRoots(len(rootIDs))+`
		SELECT root, total, summary FROM (
			SELECT rel.root,
			       COUNT(*) OVER (PARTITION BY rel.root) AS total,
			       i.summary,
			       ROW_NUMBER() OVER (
			           PARTITION BY rel.root
			           ORDER BY (TRIM(i.summary) <> '') DESC,
			                    (i.status IN ('running','streaming')) DESC,
			                    i.turn_index DESC, i.item_index DESC,
			                    i.id
			       ) AS rn
			  FROM rel
			  CROSS JOIN items i ON i.thread_id = ? AND i.id = rel.id
		) WHERE rn = 1`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query subagent aggregates for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := make(map[string]subagentAnchorAggregate)
	for rows.Next() {
		var root, summary string
		var total int
		if err := rows.Scan(&root, &total, &summary); err != nil {
			return nil, fmt.Errorf("store: scan subagent aggregate row: %w", err)
		}
		if strings.TrimSpace(summary) == "" {
			summary = ""
		}
		out[root] = subagentAnchorAggregate{
			descendantCount:    total,
			latestChildSummary: summary,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subagent aggregates for %s: %w", threadID, err)
	}
	return out, nil
}

func mergeSubagentAnchorMeta(itemMeta string, agg subagentAnchorAggregate) string {
	merged := map[string]any{}
	if strings.TrimSpace(itemMeta) != "" {
		// A meta that doesn't parse stays untouched: losing the original
		// keys (tool input, path refs, ...) to gain a cosmetic badge is
		// the wrong trade. The partial-index predicates on items already
		// json_extract-validate meta on every write, so this only fires
		// on external corruption — but that failure mode must not be
		// "the decorator quietly emptied the row's meta".
		if err := json.Unmarshal([]byte(itemMeta), &merged); err != nil {
			return itemMeta
		}
	}
	merged[metaKeySubagentDescendantCount] = agg.descendantCount
	if agg.latestChildSummary != "" {
		merged[metaKeySubagentLatestChildSummary] = agg.latestChildSummary
	} else {
		// The decoration owns this key: drop any same-named stored value
		// so an empty computed summary can't leak a stale one through.
		delete(merged, metaKeySubagentLatestChildSummary)
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return itemMeta
	}
	return string(data)
}

// ListSubagentDescendants returns the visible transitive descendants of
// `rootItemID` in timeline order — the expand-on-demand companion to the
// collapsed-card aggregates. The result includes nested launches and
// their children, so a single call hydrates the whole group subtree, up
// to maxSubagentDescendants rows (newest win; see the const for why).
// Proposed-plan decoration applies the same way it does on window loads.
func (s *Store) ListSubagentDescendants(threadID, rootItemID string) ([]Item, error) {
	rootItemID = strings.TrimSpace(rootItemID)
	if rootItemID == "" {
		return []Item{}, nil
	}
	// Placeholder order: CTE base hop (threadID, rootItemID), CTE
	// recursive hop (threadID), selected ranking join (threadID, cap),
	// outer hydrate join (threadID). CROSS JOINs are PK probes per rel /
	// selected row — see descendantsCTEFromRoots plan notes.
	rows, err := s.reader().Query(descendantsCTEFromRoots(1)+`,
		selected(id) AS (
			SELECT id FROM (
				SELECT items.id AS id
				  FROM rel
				  CROSS JOIN items ON items.thread_id = ? AND items.id = rel.id
				 ORDER BY items.turn_index DESC, items.item_index DESC
				 LIMIT ?
			)
		)
		SELECT `+itemColumns+`
		  FROM selected
		  CROSS JOIN items ON items.thread_id = ? AND items.id = selected.id
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 ORDER BY items.turn_index, items.item_index`,
		threadID, rootItemID, threadID, threadID, maxSubagentDescendants, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		item, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan subagent descendant row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	decorated, err := s.decorateProposedPlanItems(threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	return decorated, nil
}
