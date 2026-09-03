package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Subagent child rows (items whose parent_id points at a launch row)
// are not part of any history window — see paging.go's
// topLevelItemsFilter. Two read-time surfaces replace them:
//
//   - decorateSubagentAnchors stamps every windowed anchor with the
//     aggregates its collapsed SubagentGroup card renders (descendant
//     count + latest-child summary), so the card looks identical
//     whether or not children are loaded. A card is one §E6 ROUND, so
//     the aggregates are per round, not per transcript.
//   - ListSubagentDescendants loads the full child transcript on
//     demand when the user expands the card.

// Decoration keys merged into the anchor's item meta. The frontend
// grouping (frontend/src/lib/utils/subagentGrouping.ts) reads these as
// fallbacks when no child rows are loaded; computed values from loaded
// children win once they exist.
const (
	metaKeySubagentDescendantCount    = "subagentDescendantCount"
	metaKeySubagentLatestChildSummary = "subagentLatestChildSummary"
	// metaKeySubagentTranscriptDescendantCount is the count over the
	// WHOLE root subtree — every §E6 round — and is stamped on a ROOT
	// anchor ONLY when that transcript has rounds. The root card's own
	// count covers round one alone, so the agent pane reads this as its
	// hydration expectation; its absence means "one round, the count you
	// already have".
	metaKeySubagentTranscriptDescendantCount = "subagentTranscriptDescendantCount"
	// metaKeySubagentLatestToolSummary is a read-time decoration for the
	// background tray. It is deliberately not persisted on the launch: the
	// timeline spawn row's presentation is a fixed launch event, while the tray
	// is the live projection that may show the child's latest direct tool call.
	metaKeySubagentLatestToolSummary = "subagentLatestToolSummary"
	metaKeySubagentLatestToolTurn    = "subagentLatestToolTurnIndex"
	metaKeySubagentLatestToolItem    = "subagentLatestToolItemIndex"
	// metaKeyTranscriptRootID is triage's stamp on a Claude §E6 resume
	// carrier (internal/provider MetaTranscriptRootIDKey), spelled here
	// because this package stays provider-free.
	metaKeyTranscriptRootID = "transcript_root_id"
	// metaKeySubagentResumePrompt / metaKeyResumeCarrierID are triage's
	// stamps on the user_text row that OPENS a resumed round
	// (MetaSubagentResumePromptKey / MetaResumeCarrierIDKey), spelled
	// here for the same reason. That row is parented to the transcript
	// ROOT like everything else the agent produced, and its position is
	// what cuts one round from the next.
	metaKeySubagentResumePrompt = "subagent_resume_prompt"
	metaKeyResumeCarrierID      = "resume_carrier_id"
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
	// transcriptDescendantCount is the whole root subtree's count, every
	// §E6 round included. Set only on a ROOT anchor whose transcript has
	// rounds; hasTranscriptCount=false omits the key entirely.
	transcriptDescendantCount int
	hasTranscriptCount        bool
}

// subagentRound is one §E6 resumed round, named by the resume-prompt row
// that opens it: the transcript ROOT it is parented to, the CARRIER whose
// card renders it, and the position that cuts it from the round before.
type subagentRound struct {
	rootID    string
	anchorID  string
	promptID  string
	turnIndex int
	itemIndex int
}

// subagentRoundBounds is one anchor's slice of its root's transcript as a
// half-open range over (turn_index, item_index). A nil lo or hi is
// unbounded on that side: the root's round starts before everything, the
// last round ends after everything, and a carrier whose prompt row is
// missing gets both — the whole-transcript fallback it had before rounds
// existed. `round` is false for exactly that fallback, which is what
// keeps it out of the transcript-total sum it would double-count.
type subagentRoundBounds struct {
	anchorID string
	rootID   string
	loTurn   any
	loItem   any
	hiTurn   any
	hiItem   any
	round    bool
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
// children are returned untouched.
//
// The aggregates are per ROUND, not per transcript. A Claude §E6 resume
// carrier opens a new round of one agent whose rows all stay under the
// ORIGINAL launch, and the frontend renders one card per round (the root
// card is round one, each carrier card its own). So each anchor is stamped
// with the count and latest activity of ITS round only, and a root that has
// rounds additionally carries the whole-transcript count the agent pane
// hydrates against.
//
// Cost: the descendant walk plus ONE narrow probe for the resume-prompt
// rows under the window's walk roots — direct children only, off the same
// partial parent index the walk's base hops use. It runs whenever the
// window holds any tool_call anchor, because a ROOT alone in the window can
// have rounds whose carriers are outside it, and nothing on the root row
// says so. When it finds nothing, the aggregate query and its cost are
// exactly what they were before rounds existed.
func (s *Store) decorateSubagentAnchors(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	if len(items) == 0 {
		return items, nil
	}
	// Only tool_call rows can anchor subagent transcripts (Claude
	// Task/Agent launches, Codex collab_agent spawns). Filtering here
	// keeps the IN list short on plain text-heavy windows.
	//
	// A Claude §E6 resume CARRIER is the exception that has to be walked
	// from somewhere else: the round it opened has rows, but they are
	// parented to the agent's transcript ROOT like every other round's
	// (claude-wire.md §E6), so the carrier's own subtree is empty and it
	// would render as an ordinary tool call. It borrows the root's walk.
	rootIDs := make([]string, 0, len(items))
	seenRoot := make(map[string]struct{}, len(items))
	var walkRootByAnchor map[string]string
	for _, item := range items {
		if item.Kind != "tool_call" || strings.TrimSpace(item.ID) == "" {
			continue
		}
		walkRoot := item.ID
		if root := transcriptRootFromMeta(item.Meta); root != "" && root != item.ID {
			walkRoot = root
			if walkRootByAnchor == nil {
				walkRootByAnchor = make(map[string]string, 1)
			}
			walkRootByAnchor[item.ID] = root
		}
		if _, dup := seenRoot[walkRoot]; dup {
			continue
		}
		seenRoot[walkRoot] = struct{}{}
		rootIDs = append(rootIDs, walkRoot)
	}
	if len(rootIDs) == 0 {
		return items, nil
	}

	rounds, err := s.subagentResumeRounds(q, threadID, rootIDs)
	if err != nil {
		return nil, err
	}

	var aggregates map[string]subagentAnchorAggregate
	if len(rounds) == 0 {
		// No round ever opened under any of these roots: one aggregate
		// per root, keyed by root, exactly as before.
		aggregates, err = s.subagentAggregatesByRoot(q, threadID, rootIDs)
	} else {
		aggregates, err = s.subagentAggregatesByRound(
			q, threadID, rootIDs, subagentRoundBoundsFor(rootIDs, rounds, walkRootByAnchor))
	}
	if err != nil {
		return nil, err
	}
	if len(aggregates) == 0 {
		return items, nil
	}
	for i := range items {
		agg, ok := aggregates[items[i].ID]
		if !ok {
			// A carrier with no bounds row of its own (the no-rounds
			// path, where the map is keyed by root) borrows the root's.
			root, isCarrier := walkRootByAnchor[items[i].ID]
			if !isCarrier {
				continue
			}
			if agg, ok = aggregates[root]; !ok {
				continue
			}
		}
		// A root with no visible descendants is not an anchor at all, and
		// the bounded query answers for it with a zero rather than by
		// omission. Leaving it undecorated is what keeps a plain tool call
		// a plain tool call.
		if agg.descendantCount == 0 && !agg.hasTranscriptCount {
			continue
		}
		items[i].Meta = mergeSubagentAnchorMeta(items[i].Meta, agg)
	}
	return items, nil
}

// subagentResumeRounds finds every §E6 resume-prompt row parented to one
// of `rootIDs`, in position order. It asks for ALL of them, not just the
// ones whose carrier is in the window: a round-1 card in the window is
// bounded by a round-2 carrier that may be anywhere.
//
// The predicate is a direct-child probe (`parent_id IN (...)`, which the
// partial idx_items_parent serves, plus the `parent_id <> ”` term it
// needs to be proven) narrowed by a substring pre-check on meta, so the
// JSON decode below runs on the handful of rows that are really prompts.
//
// The ordering is done in Go, and that is a PLAN decision, not a style
// one: an `ORDER BY turn_index, item_index` makes SQLite prefer
// idx_items_thread_turn_item_unique and scan the thread's whole ordering
// index instead of probing the parent index (measured on the arms parity
// fixture). One agent has a handful of rounds, so the sort is free.
// TestSubagentResumeRoundProbeProbesTheParentIndexes is the tripwire.
func (s *Store) subagentResumeRounds(q sqlQueryer, threadID string, rootIDs []string) ([]subagentRound, error) {
	rootArgs := make([]any, 0, len(rootIDs))
	for _, id := range rootIDs {
		rootArgs = append(rootArgs, id)
	}
	query, args := timelineArms(threadID, timelineSelection{
		Columns: func(string) string {
			return `items.parent_id AS root, items.id AS id, items.meta AS meta,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Where: `items.kind = 'user_text'
			   AND items.parent_id IN (` + placeholders(len(rootIDs)) + `)
			   AND items.parent_id <> ''
			   AND items.meta LIKE '%` + metaKeySubagentResumePrompt + `%'`,
		WhereArgs: rootArgs,
	})

	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query subagent resume rounds for %s: %w", threadID, err)
	}
	defer rows.Close()

	var out []subagentRound
	for rows.Next() {
		var root, id, meta string
		var turnIndex, itemIndex int
		if err := rows.Scan(&root, &id, &meta, &turnIndex, &itemIndex); err != nil {
			return nil, fmt.Errorf("store: scan subagent resume round: %w", err)
		}
		var decoded struct {
			ResumePrompt bool   `json:"subagent_resume_prompt"`
			CarrierID    string `json:"resume_carrier_id"`
		}
		if json.Unmarshal([]byte(meta), &decoded) != nil || !decoded.ResumePrompt {
			continue // the LIKE matched some other string
		}
		// A prompt row that names no carrier still CUTS the transcript
		// where it sits; it just has no card to be stamped onto. Keying
		// the bound on the row's own id gives it a slot nothing matches.
		anchorID := strings.TrimSpace(decoded.CarrierID)
		if anchorID == "" {
			anchorID = id
		}
		out = append(out, subagentRound{
			rootID: root, anchorID: anchorID, promptID: id,
			turnIndex: turnIndex, itemIndex: itemIndex,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subagent resume rounds for %s: %w", threadID, err)
	}
	slices.SortFunc(out, func(a, b subagentRound) int {
		if a.turnIndex != b.turnIndex {
			return a.turnIndex - b.turnIndex
		}
		if a.itemIndex != b.itemIndex {
			return a.itemIndex - b.itemIndex
		}
		return strings.Compare(a.promptID, b.promptID)
	})
	return out, nil
}

// subagentRoundBoundsFor turns the ordered prompt rows into the disjoint,
// exhaustive set of ranges the aggregate query partitions by: the root
// owns everything BEFORE its first prompt row, and round k owns
// [prompt_k, prompt_k+1) with the prompt row itself inside it.
//
// `carrierRoots` is the window's carrier→root map. A carrier in it that no
// prompt row named gets one unbounded range — the whole-transcript
// fallback — because there is nothing to cut its round at.
func subagentRoundBoundsFor(
	rootIDs []string, rounds []subagentRound, carrierRoots map[string]string,
) []subagentRoundBounds {
	// rounds arrive in position order, so each root's slice is too.
	byRoot := make(map[string][]subagentRound, len(rootIDs))
	for _, r := range rounds {
		byRoot[r.rootID] = append(byRoot[r.rootID], r)
	}

	out := make([]subagentRoundBounds, 0, len(rootIDs)+len(rounds))
	// The aggregate query partitions by anchor, so a repeated anchor
	// would count its rows twice. First bound wins.
	seen := make(map[string]struct{}, len(rootIDs)+len(rounds))
	add := func(b subagentRoundBounds) {
		if _, dup := seen[b.anchorID]; dup {
			return
		}
		seen[b.anchorID] = struct{}{}
		out = append(out, b)
	}

	for _, root := range rootIDs {
		rs := byRoot[root]
		rootBound := subagentRoundBounds{anchorID: root, rootID: root, round: true}
		if len(rs) > 0 {
			rootBound.hiTurn, rootBound.hiItem = rs[0].turnIndex, rs[0].itemIndex
		}
		add(rootBound)
		for i, r := range rs {
			b := subagentRoundBounds{
				anchorID: r.anchorID, rootID: root, round: true,
				loTurn: r.turnIndex, loItem: r.itemIndex,
			}
			if i+1 < len(rs) {
				b.hiTurn, b.hiItem = rs[i+1].turnIndex, rs[i+1].itemIndex
			}
			add(b)
		}
	}
	for carrier, root := range carrierRoots {
		if _, walked := byRoot[root]; !walked {
			// The root has no rounds; its own bound already covers the
			// whole transcript, and the carrier borrows it by lookup.
			continue
		}
		add(subagentRoundBounds{anchorID: carrier, rootID: root})
	}
	return out
}

// transcriptRootFromMeta reads a resume carrier's `transcript_root_id`
// stamp — the only rows that carry it — out of an already-materialized
// meta blob. The substring pre-check is what keeps the common window
// (no carriers at all) off a JSON parse per anchor.
func transcriptRootFromMeta(meta string) string {
	if !strings.Contains(meta, metaKeyTranscriptRootID) {
		return ""
	}
	var decoded struct {
		TranscriptRootID string `json:"transcript_root_id"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return ""
	}
	return strings.TrimSpace(decoded.TranscriptRootID)
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
// the preview descendant (subagentRankedPreviewSQL is the pick rule).
// This is the whole-transcript form, and it is what runs on every window
// that opened no §E6 round — which is every window on every thread that
// never resumed an idle agent.
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
			SELECT i.root AS root,
			       COUNT(*) OVER (PARTITION BY i.root) AS total,
			       `+fmt.Sprintf(subagentRankedPreviewSQL, "i.root")+`
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

// subagentRankedPreviewSQL is the pick rule both aggregate queries share:
// among a partition's rows, one with a tool-ish kind and a non-empty
// summary beats one without, an active row beats a terminal one, and the
// highest (turn_index, item_index) wins. The trailing id only breaks
// coordinate ties (corrupt data) so the pick stays deterministic. It
// mirrors the frontend's pickLatestChildSummary.
const subagentRankedPreviewSQL = `CASE WHEN i.kind IN ('tool_call','tool_completion','terminal_interaction','error','api_error')
			            THEN i.summary ELSE '' END AS summary,
			       ROW_NUMBER() OVER (
			           PARTITION BY %[1]s
			           ORDER BY (i.kind IN ('tool_call','tool_completion','terminal_interaction','error','api_error')
			                     AND TRIM(i.summary) <> '') DESC,
			                    (i.status IN ('running','streaming')) DESC,
			                    i.turn_index DESC, i.item_index DESC,
			                    i.id
			       ) AS rn`

// subagentAggregatesByRound is subagentAggregatesByRoot sliced per §E6
// round: the same descendant walk and the same pick rule, but partitioned
// by ANCHOR and with each anchor's rows narrowed to its own half-open
// range. One statement for the whole window.
//
// The bounds arrive as a VALUES CTE joined to the walk's resolved rows.
// The join is a LEFT one so an anchor with an empty range still reports
// (as zero, which the caller reads as "not an anchor" unless the round
// count says otherwise) instead of vanishing.
//
// The whole-transcript total is summed HERE rather than taken from a
// second window function: the fallback range a prompt-less carrier gets
// overlaps its siblings, so a PARTITION BY root would count those rows
// twice. The real rounds are disjoint and exhaustive, so their sum is
// exact.
func (s *Store) subagentAggregatesByRound(
	q sqlQueryer, threadID string, rootIDs []string, bounds []subagentRoundBounds,
) (map[string]subagentAnchorAggregate, error) {
	if len(bounds) == 0 {
		return nil, nil
	}
	resolvedSQL, resolvedArgs := timelineArms(threadID, timelineSelection{
		Columns: func(string) string {
			return `rel.root AS root, items.id AS id, items.kind AS kind,
			        items.status AS status, items.summary AS summary,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Source: "rel",
		Where:  "items.id = rel.id",
	})

	tuples := make([]string, 0, len(bounds))
	args := descendantsCTEArgs(threadID, rootIDs)
	for _, b := range bounds {
		tuples = append(tuples, "(?,?,?,?,?,?)")
		args = append(args, b.anchorID, b.rootID, b.loTurn, b.loItem, b.hiTurn, b.hiItem)
	}
	args = append(args, resolvedArgs...)

	rows, err := q.Query(descendantsCTEFromRoots(len(rootIDs))+`,
		bound(anchor, root, lo_turn, lo_item, hi_turn, hi_item) AS (
			VALUES `+strings.Join(tuples, ",")+`
		)
		SELECT anchor, total, summary FROM (
			SELECT b.anchor AS anchor,
			       COUNT(i.id) OVER (PARTITION BY b.anchor) AS total,
			       `+fmt.Sprintf(subagentRankedPreviewSQL, "b.anchor")+`
			  FROM bound b
			  LEFT JOIN (`+resolvedSQL+`) i
			    ON i.root = b.root
			   AND (b.lo_turn IS NULL
			        OR i.turn_index > b.lo_turn
			        OR (i.turn_index = b.lo_turn AND i.item_index >= b.lo_item))
			   AND (b.hi_turn IS NULL
			        OR i.turn_index < b.hi_turn
			        OR (i.turn_index = b.hi_turn AND i.item_index < b.hi_item))
		) WHERE rn = 1`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query subagent round aggregates for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := make(map[string]subagentAnchorAggregate, len(bounds))
	for rows.Next() {
		var anchor string
		var summary sql.NullString
		var total int
		if err := rows.Scan(&anchor, &total, &summary); err != nil {
			return nil, fmt.Errorf("store: scan subagent round aggregate row: %w", err)
		}
		latest := summary.String
		if strings.TrimSpace(latest) == "" {
			latest = "" // same blank rule as subagentAggregatesByRoot
		}
		out[anchor] = subagentAnchorAggregate{
			descendantCount:    total,
			latestChildSummary: latest,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subagent round aggregates for %s: %w", threadID, err)
	}

	// Roll the rounds up onto their root. Only a root that actually has
	// rounds gets the key: without one, "the whole transcript" and "this
	// card's count" are the same number and the frontend needs no second.
	transcript := make(map[string]int, len(rootIDs))
	hasRounds := make(map[string]bool, len(rootIDs))
	for _, b := range bounds {
		if !b.round {
			continue
		}
		transcript[b.rootID] += out[b.anchorID].descendantCount
		if b.anchorID != b.rootID {
			hasRounds[b.rootID] = true
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
	if agg.hasTranscriptCount {
		merged[metaKeySubagentTranscriptDescendantCount] = agg.transcriptDescendantCount
	} else {
		// The decoration owns this key too: a root that no longer has
		// rounds must not keep a stale whole-transcript number.
		delete(merged, metaKeySubagentTranscriptDescendantCount)
	}
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
	// A §E6 resume CARRIER has no subtree of its own: the agent's rows,
	// round one's and every resumed round's, stay under the ORIGINAL
	// launch (claude-wire.md §E6). Expanding one must walk from there, or
	// the card that decorateSubagentAnchors just stamped with a count
	// opens empty. ONE hop, because the stamp is always the fully
	// resolved root — triage writes the walk's END, never the chain.
	if anchor, found, err := s.GetThreadItem(threadID, rootItemID); err != nil {
		return nil, fmt.Errorf("store: resolve subagent walk root for %s/%s: %w", threadID, rootItemID, err)
	} else if found {
		if root := transcriptRootFromMeta(anchor.Meta); root != "" {
			rootItemID = root
		}
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
