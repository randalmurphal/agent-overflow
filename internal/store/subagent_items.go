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
	// metaKeySubagentLatestToolSummary is a read-time decoration for the
	// background tray. It is deliberately not persisted on the launch: the
	// timeline spawn row's presentation is a fixed launch event, while the tray
	// is the live projection that may show the child's latest direct tool call.
	metaKeySubagentLatestToolSummary = "subagentLatestToolSummary"
	metaKeySubagentLatestToolTurn    = "subagentLatestToolTurnIndex"
	metaKeySubagentLatestToolItem    = "subagentLatestToolItemIndex"
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

// descendantsCTEFromRoots walks parent_id edges downward from an
// explicit list of root item ids over the LOGICAL timeline — local rows
// plus the thread's imported history — carrying the originating root
// through the recursion so per-root aggregates fall out of a GROUP BY.
// UNION (not UNION ALL) dedups (root, id) pairs during recursion, so a
// pathological parent_id cycle terminates instead of looping forever.
//
// It is FOUR arms, not two, and that is the whole point: a recursive
// step that names the `timeline_items` view makes SQLite MATERIALIZE the
// view — the entire thread — once per query, twice counting the final
// resolution join (129 ms and 33,160 pages on a 40-anchor window over a
// 35k-item thread, against 13 ms against `items` alone). Writing each
// hop as its own physical arm keeps every hop an index probe: 106 ms /
// 17,354 pages, which is the `items`-only floor for that same window.
// A compound recursive SELECT needs SQLite >= 3.34; modernc.org/sqlite
// is far past it.
//
// Plan notes (verified with EXPLAIN QUERY PLAN, SQLite 3.46):
//   - The local hops probe the partial idx_items_parent (thread_id,
//     parent_id) WHERE parent_id <> ”, the imported hops
//     idx_import_history_items_parent (chunk_id, parent_id) WHERE
//     parent_id <> ”. The explicit `parent_id <> ”` terms below are
//     load-bearing for that: SQLite cannot prove the index predicate
//     from a bound parameter or the `rel.id` join term alone, and
//     without the proof the hops degrade to a whole-thread PK-prefix
//     scan per recursion level.
//   - CROSS JOIN in the recursive hops is a planner directive, not
//     style: with a plain JOIN the planner puts the row source on the
//     outer side and rescans the whole thread once per queued row.
//     CROSS JOIN pins rel(outer) → source(inner), one index probe per
//     row.
//
// The visible-items filter matches the window loaders: plan_update
// notifications never render, so they must not count against the
// collapsed card's "N entries" badge either.
//
// Placeholder order: local base hop (thread id, roots...), imported base
// hop (thread id, roots...), local recursive hop (thread id), imported
// recursive hop (thread id).
func descendantsCTEFromRoots(rootCount int) string {
	roots := placeholders(rootCount)
	visible := visibleItemsFilterFor("items.")
	return `WITH RECURSIVE rel(root, id) AS (
		SELECT items.parent_id, items.id
		  FROM items
		 WHERE items.thread_id = ?
		   AND items.parent_id IN (` + roots + `)
		   AND items.parent_id <> ''
		   AND ` + visible + `
		UNION
		SELECT items.parent_id, items.id
		  FROM thread_import_chunks refs
		  JOIN import_history_items items ON items.chunk_id = refs.chunk_id
		 WHERE refs.thread_id = ?
		   AND items.parent_id IN (` + roots + `)
		   AND items.parent_id <> ''
		   AND ` + visible + `
		   AND ` + importedNotOverridden + `
		UNION
		SELECT rel.root, items.id
		  FROM rel
		  CROSS JOIN items ON items.parent_id = rel.id
		 WHERE items.thread_id = ?
		   AND items.parent_id <> ''
		   AND ` + visible + `
		UNION
		SELECT rel.root, items.id
		  FROM rel
		  CROSS JOIN thread_import_chunks refs
		  JOIN import_history_items items
		    ON items.chunk_id = refs.chunk_id AND items.parent_id = rel.id
		 WHERE refs.thread_id = ?
		   AND items.parent_id <> ''
		   AND ` + visible + `
		   AND ` + importedNotOverridden + `
	)`
}

// descendantsCTEArgs renders descendantsCTEFromRoots' bind values in the
// order its four arms consume them. One function, so the arm order and
// the arg order cannot drift apart.
func descendantsCTEArgs(threadID string, rootIDs []string) []any {
	args := make([]any, 0, 2*len(rootIDs)+4)
	for range 2 {
		args = append(args, threadID)
		for _, id := range rootIDs {
			args = append(args, id)
		}
	}
	return append(args, threadID, threadID)
}

// descendantsCTE is the LOCAL-ONLY `rel(root, id) AS (...)` clause,
// without the leading `WITH RECURSIVE`, so a caller that needs other
// CTEs alongside it (ListLiveBackgroundTasks stacks a background-root
// CTE under the same WITH) can compose one statement instead of forking
// the walk. `table` is the row source and `rootSet` is whatever yields
// the root ids: a `?` placeholder list or a subquery naming an earlier
// CTE. The one caller passes plain `items` and a subquery — the tray
// lists LIVE work, which is never imported history.
//
// Placeholder order: thread id for the base hop, then the rootSet's own
// parameters (if any), then thread id again for the recursive hop.
func descendantsCTE(table, rootSet string) string {
	visible := visibleItemsFilterFor("i.")
	return `rel(root, id) AS (
		SELECT i.parent_id, i.id
		  FROM ` + table + ` i
		 WHERE i.thread_id = ?
		   AND i.parent_id IN (` + rootSet + `)
		   AND i.parent_id <> ''
		   AND ` + visible + `
		UNION
		SELECT rel.root, i.id
		  FROM rel
		  CROSS JOIN ` + table + ` i ON i.parent_id = rel.id
		 WHERE i.thread_id = ?
		   AND i.parent_id <> ''
		   AND ` + visible + `
	)`
}

// subagentLaunchFilterFor is the provider-neutral "this tool_call row is
// an agent launch" predicate, and the one place that answers the
// question for SQL. It is STRUCTURAL, never a tool-name list: a launch
// is the tool_call that other rows are attributed to. That is exactly
// what decorateSubagentAnchors already treats as an anchor (a tool_call
// with visible descendants), and it covers every launch kind the spec
// names — Claude `Agent`/`Task`, a forked `Skill`, a SendMessage resume
// carrier, Codex `spawn_agent` — without this package having to know any
// of their names or which provider produced them.
//
// A launch that has not yet produced its first attributed row does not
// match. That is deliberate: an "agent" with nothing under it is
// indistinguishable from an ordinary tool call, and every consumer
// re-reads on the next event.
//
// `alias` is the row's table alias WITH its trailing dot ("items.").
// It is mandatory, not cosmetic: the EXISTS probe joins a second copy of
// `items`, and an unqualified `thread_id`/`id` inside it would bind to
// that inner copy and make the predicate vacuously true.
func subagentLaunchFilterFor(alias string) string {
	return alias + `kind = 'tool_call'
		    AND EXISTS (
		      SELECT 1 FROM items child
		       WHERE child.thread_id = ` + alias + `thread_id
		         AND child.parent_id = ` + alias + `id
		         AND child.parent_id <> ''
		    )`
}

// IsSubagentLaunch reports whether the row is an agent launch by the
// structural predicate above. The Go-side companion to
// subagentLaunchFilterFor, for triage's terminal paths: only a launch
// row carries the subagent's final progress numbers, and an ordinary
// tool call must never be stamped with them.
func (s *Store) IsSubagentLaunch(threadID, itemID string) (bool, error) {
	threadID = strings.TrimSpace(threadID)
	itemID = strings.TrimSpace(itemID)
	if threadID == "" || itemID == "" {
		return false, nil
	}
	var exists int
	if err := s.reader().QueryRow(
		`SELECT EXISTS(
		    SELECT 1 FROM items
		     WHERE items.thread_id = ?
		       AND items.id = ?
		       AND `+subagentLaunchFilterFor("items.")+`
		     LIMIT 1
		)`,
		threadID, itemID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: is subagent launch %s/%s: %w", threadID, itemID, err)
	}
	return exists != 0, nil
}

// decorateSubagentAnchors merges descendant aggregates into the meta of
// every item in `items` that anchors subagent children. Items without
// children are returned untouched. Runs one batched recursive query per
// window load; cost is proportional to the total descendant count of
// in-window anchors.
func (s *Store) decorateSubagentAnchors(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
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

	aggregates, err := s.subagentAggregatesByRoot(q, threadID, rootIDs)
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

// decorateLatestDirectSubagentTools merges the newest direct, non-launch tool
// summary under each supplied launch into a read-time copy of that launch.
// Direct ownership matters: a nested agent has its own tray row, so its tools
// must not also appear as the parent's latest activity. The one query handles
// every live launch in the thread and keeps tray refreshes free of N+1 reads.
func (s *Store) decorateLatestDirectSubagentTools(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	rootIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Kind == "tool_call" && strings.TrimSpace(item.ID) != "" {
			rootIDs = append(rootIDs, item.ID)
		}
	}
	if len(rootIDs) == 0 {
		return items, nil
	}

	args := make([]any, 0, len(rootIDs)+1)
	args = append(args, threadID)
	for _, id := range rootIDs {
		args = append(args, id)
	}
	rows, err := q.Query(`
		SELECT parent_id, summary, turn_index, item_index FROM (
			SELECT parent_id,
			       summary,
			       turn_index,
			       item_index,
			       ROW_NUMBER() OVER (
			           PARTITION BY parent_id
			           ORDER BY turn_index DESC, item_index DESC, id
			       ) AS rn
			  FROM items
			 WHERE thread_id = ?
			   AND parent_id IN (`+placeholders(len(rootIDs))+`)
			   AND parent_id <> ''
			   AND kind = 'tool_call'
			   AND tool_name <> 'collab_agent'
			   AND TRIM(summary) <> ''
		) WHERE rn = 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query latest direct subagent tools for %s: %w", threadID, err)
	}
	defer rows.Close()

	type latestTool struct {
		summary              string
		turnIndex, itemIndex int
	}
	latestByRoot := make(map[string]latestTool, len(rootIDs))
	for rows.Next() {
		var rootID, summary string
		var turnIndex, itemIndex int
		if err := rows.Scan(&rootID, &summary, &turnIndex, &itemIndex); err != nil {
			return nil, fmt.Errorf("store: scan latest direct subagent tool: %w", err)
		}
		latestByRoot[rootID] = latestTool{
			summary: strings.TrimSpace(summary), turnIndex: turnIndex, itemIndex: itemIndex,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate latest direct subagent tools for %s: %w", threadID, err)
	}

	for i := range items {
		latest := latestByRoot[items[i].ID]
		if latest.summary == "" {
			continue
		}
		decorated, err := mergeReadTimeMeta(items[i].Meta, map[string]any{
			metaKeySubagentLatestToolSummary: latest.summary,
			metaKeySubagentLatestToolTurn:    latest.turnIndex,
			metaKeySubagentLatestToolItem:    latest.itemIndex,
		})
		if err != nil {
			return nil, fmt.Errorf("store: decorate latest direct subagent tool for %s/%s: %w", threadID, items[i].ID, err)
		}
		items[i].Meta = decorated
	}
	return items, nil
}

func mergeReadTimeMeta(itemMeta string, decoration map[string]any) (string, error) {
	merged := map[string]any{}
	if strings.TrimSpace(itemMeta) != "" {
		if err := json.Unmarshal([]byte(itemMeta), &merged); err != nil {
			return "", err
		}
	}
	for key, value := range decoration {
		merged[key] = value
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// subagentAggregatesByRoot returns, for each root id that has visible
// descendants, the total transitive descendant count and the summary of
// the preview descendant. Preview selection mirrors the frontend's
// pickLatestChildSummary: among descendants with a non-empty summary,
// an active (running/streaming) row beats a terminal one, and within
// each class the highest (turn_index, item_index) wins. The trailing
// i.id key only breaks coordinate ties (corrupt data) so the pick stays
// deterministic.
func (s *Store) subagentAggregatesByRoot(q sqlQueryer, threadID string, rootIDs []string) (map[string]subagentAnchorAggregate, error) {
	// The walk yields ids; the rows behind them are resolved through the
	// two physical arms rather than the timeline_items view, which SQLite
	// would materialize whole (timeline_arms.go). The resolution carries
	// only the columns the ranking reads.
	resolvedSQL, resolvedArgs := timelineArms(threadID, timelineSelection{
		Columns: func(string) string {
			return `rel.root AS root, items.id AS id, items.kind AS kind,
			        items.status AS status, items.summary AS summary,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Source: "rel",
		Where:  "items.id = rel.id",
	})
	args := append(descendantsCTEArgs(threadID, rootIDs), resolvedArgs...)

	rows, err := q.Query(descendantsCTEFromRoots(len(rootIDs))+`
		SELECT root, total, summary FROM (
			SELECT i.root,
			       COUNT(*) OVER (PARTITION BY i.root) AS total,
			       CASE WHEN i.kind IN ('tool_call','tool_completion','terminal_interaction','error','api_error')
			            THEN i.summary ELSE '' END AS summary,
			       ROW_NUMBER() OVER (
			           PARTITION BY i.root
			           ORDER BY (i.kind IN ('tool_call','tool_completion','terminal_interaction','error','api_error')
			                     AND TRIM(i.summary) <> '') DESC,
			                    (i.status IN ('running','streaming')) DESC,
			                    i.turn_index DESC, i.item_index DESC,
			                    i.id
			       ) AS rn
			  FROM (`+resolvedSQL+`) i
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
	// The walk's ids are ordered and capped through the two physical
	// arms, never through the timeline_items view (timeline_arms.go);
	// queryHydratedTimelineItems then resolves the surviving ids the same
	// way and keeps every statement on one read pool.
	selectedSQL, selectedArgs := timelineIDSelection(threadID, timelineSelection{
		Source:  "rel",
		Where:   "items.id = rel.id",
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   maxSubagentDescendants,
	})
	items, err := queryHydratedTimelineItems(
		s.reader(), threadID,
		descendantsCTEFromRoots(1)+"\n"+selectedSQL,
		append(descendantsCTEArgs(threadID, []string{rootItemID}), selectedArgs...)...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	decorated, err := s.decorateProposedPlanItems(s.reader(), threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	return decorated, nil
}
