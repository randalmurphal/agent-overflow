package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Main-thread pages exclude subagent children. Read-time anchor decoration
// supplies each execution's counts and preview; scoped pages load direct
// children or an execution digest on demand.

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
	// metaKeySubagentLatestToolSummary is the background tray's activity
	// line: the launch's newest direct tool call. The history triggers keep
	// it on a stamped Claude launch with the card keys; a Codex spawn row is
	// immutable, so its tray reads it at read time
	// (decorateLatestDirectSubagentTools).
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
// maxWindowItems resource limit on the window pagers (app_paging.go):
// ListSubagentDescendants is a wire RPC, so its response must stay
// bounded even when a remote caller requests a large subtree.
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
	// imported marks a prompt read from the immutable history arm, which
	// the write-time round probe does not see.
	imported bool
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
	lo       *TimelineCursor
	hi       *TimelineCursor
	round    bool
}

// descendantsCTE walks parent_id edges downward from an
// explicit list of root item ids over the LOGICAL timeline — local rows
// plus the thread's imported history — carrying the originating root
// through the recursion so per-root aggregates fall out of a GROUP BY.
// UNION (not UNION ALL) dedups (root, id) pairs during recursion, so a
// pathological parent_id cycle terminates instead of looping forever.
//
// Each physical hop starts from the parent identity. Imported hops probe
// parent_id across chunks, then verify the candidate chunk belongs to this
// thread. Starting from the thread's chunk list would multiply every queued
// descendant by every attached chunk. CROSS JOIN preserves the lookup order;
// explicit nonempty parent predicates admit the partial parent indexes.
//
// The visible-items filter matches the window loaders: plan_update
// notifications never render, so they must not count against the
// collapsed card's "N entries" badge either.
//
// The roots are one JSON array (jsonList), so the statement has one text
// for any number of roots. Bind order: local base hop (thread id, roots),
// imported base hop (thread id, roots), local recursive hop (thread id),
// imported recursive hop (thread id).
var descendantsCTE = func() string {
	visible := visibleItemsFilterFor("items.")
	return `WITH RECURSIVE rel(root, id) AS (
		SELECT items.parent_id, items.id
		  FROM items
		 WHERE items.thread_id = ?
		   AND items.parent_id IN (SELECT value FROM json_each(?))
		   AND items.parent_id <> ''
		   AND ` + visible + `
		UNION
		SELECT items.parent_id, items.id
		  FROM import_history_items items
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id
		 WHERE refs.thread_id = ?
		   AND items.parent_id IN (SELECT value FROM json_each(?))
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
		  CROSS JOIN import_history_items items ON items.parent_id = rel.id
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id
		 WHERE refs.thread_id = ?
		   AND items.parent_id <> ''
		   AND ` + visible + `
		   AND ` + importedNotOverridden + `
	)`
}()

// descendantsCTEArgs renders descendantsCTE's bind values, roots a JSON
// array of root ids, in the order its four arms consume them. One
// function, so the arm order and the arg order cannot drift apart.
func descendantsCTEArgs(threadID, roots string) []any {
	return []any{threadID, roots, threadID, roots, threadID, threadID}
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
		      SELECT 1 FROM timeline_items child
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
// A local anchor the history triggers keep stamped (subagent_aggregate_
// stamps.go) is returned as stored: its stamp is this decoration, kept at
// write time. A completion sibling copies its clean launch's stamp. Only
// imported, dirty, readTime and unstamped carrier rows are walked, plus the
// unstamped anchors of a thread whose backfill has not finished.
//
// Cost of a walk: the descendant walk plus ONE narrow probe for the
// resume-prompt rows under the window's walk roots. It runs whenever the
// window holds a walked anchor, because a ROOT alone in the window can have
// rounds whose carriers are outside it, and nothing on the root row says
// so. When it finds nothing, the aggregate query and its cost are exactly
// what they were before rounds existed.
func (s *Store) decorateSubagentAnchors(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	if len(items) == 0 {
		return items, nil
	}
	// Only tool_call rows can anchor subagent transcripts (Claude
	// Task/Agent launches). Codex spawn events never acquire live decoration. Filtering here
	// keeps the IN list short on plain text-heavy windows.
	//
	// A Claude §E6 resume CARRIER is the exception that has to be walked
	// from somewhere else: the round it opened has rows, but they are
	// parented to the agent's transcript ROOT like every other round's
	// (claude-wire.md §E6), so the carrier's own subtree is empty and it
	// would render as an ordinary tool call. It borrows the root's walk.
	//
	// A detached Claude launch's completion sibling is an anchor too: the
	// launch row is the immutable spawn event and the card renders at the
	// sibling (docs/specs/agent-visibility.md §Immutable agent history),
	// so the sibling is the row that must carry the counts. It is walked
	// from its LAUNCH (or the launch's transcript root), because the
	// agent's rows are parented to the launch, never to the sibling. The
	// launch is usually in the same window; one batched read resolves the
	// rest. Codex completions are excluded: their aggregates are a
	// write-time snapshot (SnapshotSubagentExecutionMeta), never read-time.
	rootIDs := make([]string, 0, len(items))
	seenRoot := make(map[string]struct{}, len(items))
	var walkRootByAnchor map[string]string
	addAnchor := func(anchorID, meta string) {
		walkRoot := anchorID
		if root := transcriptRootFromMeta(meta); root != "" && root != anchorID {
			walkRoot = root
			if walkRootByAnchor == nil {
				walkRootByAnchor = make(map[string]string, 1)
			}
			walkRootByAnchor[anchorID] = root
		}
		if _, dup := seenRoot[walkRoot]; dup {
			return
		}
		seenRoot[walkRoot] = struct{}{}
		rootIDs = append(rootIDs, walkRoot)
	}
	walks := subagentWalkProbe(q, threadID)
	// walked holds the rows this read decorates; every other row is
	// returned byte for byte as stored.
	walked := make(map[string]struct{}, len(items))
	launchByID := make(map[string]Item, len(items))
	var completionLaunchIDs []string
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || item.ToolName == "collab_agent" {
			continue
		}
		switch item.Kind {
		case "tool_call":
			launchByID[item.ID] = item
			walk, err := walks(item)
			if err != nil {
				return nil, err
			}
			if walk {
				walked[item.ID] = struct{}{}
				addAnchor(item.ID, item.Meta)
			}
		case "tool_completion":
			// A Codex wait carrier's completion is a wait group, not an
			// agent card; its launch is walked as a tool_call above if
			// it ever anchors anything.
			if item.CompletionOf != "" && item.ToolName != "wait_agent" {
				completionLaunchIDs = append(completionLaunchIDs, item.CompletionOf)
			}
		}
	}
	// anchorByCompletion maps a completion row's id to the launch whose
	// aggregate it carries; only completions whose launch resolved to a
	// Claude tool_call are stamped.
	var anchorByCompletion map[string]string
	stampedByCompletion := make(map[string]subagentAnchorAggregate)
	if len(completionLaunchIDs) > 0 {
		missing := make([]string, 0, len(completionLaunchIDs))
		for _, id := range completionLaunchIDs {
			if _, inWindow := launchByID[id]; !inWindow {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			loaded, err := s.subagentLaunchRowsByID(q, threadID, missing)
			if err != nil {
				return nil, err
			}
			for id, launch := range loaded {
				launchByID[id] = launch
			}
		}
		anchorByCompletion = make(map[string]string, len(completionLaunchIDs))
		for _, item := range items {
			if item.Kind != "tool_completion" || item.CompletionOf == "" {
				continue
			}
			launch, ok := launchByID[item.CompletionOf]
			if !ok || launch.Kind != "tool_call" || launch.ToolName == "collab_agent" {
				continue
			}
			walk, err := walks(launch)
			if err != nil {
				return nil, err
			}
			walked[item.ID] = struct{}{}
			anchorByCompletion[item.ID] = launch.ID
			if walk {
				addAnchor(launch.ID, launch.Meta)
				continue
			}
			if agg, stamped := storedSubagentAnchorAggregate(launch.Meta); stamped {
				stampedByCompletion[item.ID] = agg
			}
		}
	}
	for i := range items {
		if agg, ok := stampedByCompletion[items[i].ID]; ok {
			items[i].Meta = mergeSubagentAnchorMeta(items[i].Meta, agg)
		}
	}
	if len(rootIDs) == 0 {
		return items, nil
	}

	rounds, err := subagentResumeRounds(q, threadID, rootIDs)
	if err != nil {
		return nil, err
	}

	var aggregates map[string]subagentAnchorAggregate
	if len(rounds) == 0 {
		// No round ever opened under any of these roots: one aggregate
		// per root, keyed by root, exactly as before.
		aggregates, err = subagentAggregatesByRoot(q, threadID, rootIDs)
	} else {
		aggregates, err = subagentAggregatesByRound(
			q, threadID, rootIDs, subagentRoundBoundsFor(rootIDs, rounds, walkRootByAnchor))
	}
	if err != nil {
		return nil, err
	}
	if len(aggregates) == 0 {
		return items, nil
	}
	for i := range items {
		if _, walk := walked[items[i].ID]; !walk {
			continue
		}
		if _, stamped := stampedByCompletion[items[i].ID]; stamped {
			continue
		}
		anchorID := items[i].ID
		if launchID, isCompletion := anchorByCompletion[items[i].ID]; isCompletion {
			anchorID = launchID
		}
		agg, ok := aggregates[anchorID]
		if !ok {
			// A carrier with no bounds row of its own (the no-rounds
			// path, where the map is keyed by root) borrows the root's.
			root, isCarrier := walkRootByAnchor[anchorID]
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

// subagentWalkProbe returns the read's decision for one anchorable row:
// true when the read-time aggregator answers for it, false when the stored
// row is the read. An unstamped local anchor is walked only while its
// thread's backfill is pending; one probe per read answers that.
func subagentWalkProbe(q sqlQueryer, threadID string) func(Item) (bool, error) {
	var listed *bool
	return func(item Item) (bool, error) {
		if item.Rev < 0 {
			return true, nil
		}
		switch subagentStampModeOf(item.Meta) {
		case subagentStampClean:
			return false, nil
		case subagentStampWalk:
			return true, nil
		}
		if root := transcriptRootFromMeta(item.Meta); root != "" && root != item.ID {
			return true, nil
		}
		if listed == nil {
			found, err := subagentBackfillListed(q, threadID)
			if err != nil {
				return false, err
			}
			listed = &found
		}
		return *listed, nil
	}
}

// storedSubagentAnchorAggregate reads a clean stamp's card values back as
// the aggregate a walk would produce. stamped is false for a row whose
// stamp decorates nothing (no descendants, no rounds).
func storedSubagentAnchorAggregate(meta string) (subagentAnchorAggregate, bool) {
	values, ok := storedSubagentStampValues(meta)
	if !ok || values.Count == nil {
		return subagentAnchorAggregate{}, false
	}
	agg := subagentAnchorAggregate{descendantCount: *values.Count, latestChildSummary: values.Summary}
	if values.TranscriptCount != nil {
		agg.transcriptDescendantCount = *values.TranscriptCount
		agg.hasTranscriptCount = true
	}
	return agg, true
}

// subagentResumeRounds finds every §E6 resume-prompt row parented to one
// of `rootIDs`, in position order. It asks for ALL of them, not just the
// ones whose carrier is in the window: a round-1 card in the window is
// bounded by a round-2 carrier that may be anywhere.
//
// The predicate is aggPromptSQL, the one the history triggers resolve
// rounds with, so a row cuts a round at read time exactly when it cuts
// one at write time. Its partial-index terms let the local arm probe
// idx_items_subagent_resume_prompt; the imported arm probes the parent
// lookup index.
//
// The ordering is done in Go, and that is a PLAN decision, not a style
// one: an `ORDER BY turn_index, item_index` makes SQLite prefer
// idx_items_thread_turn_item_unique and scan the thread's whole ordering
// index instead of probing the parent index (measured on the arms parity
// fixture). One agent has a handful of rounds, so the sort is free.
// TestSubagentResumeRoundProbeProbesTheParentIndexes is the tripwire.
func subagentResumeRounds(q sqlQueryer, threadID string, rootIDs []string) ([]subagentRound, error) {
	roots, err := jsonList(rootIDs)
	if err != nil {
		return nil, err
	}
	query, args := subagentResumeRoundsQuery(threadID, roots)
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query subagent resume rounds for %s: %w", threadID, err)
	}
	defer rows.Close()

	var out []subagentRound
	for rows.Next() {
		var round subagentRound
		if err := rows.Scan(&round.rootID, &round.promptID, &round.anchorID,
			&round.turnIndex, &round.itemIndex, &round.imported); err != nil {
			return nil, fmt.Errorf("store: scan subagent resume round: %w", err)
		}
		// A prompt row that names no carrier still CUTS the transcript
		// where it sits; it just has no card to be stamped onto. Keying
		// the bound on the row's own id gives it a slot nothing matches.
		if round.anchorID == "" {
			round.anchorID = round.promptID
		}
		out = append(out, round)
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

// subagentResumeRoundsQuery selects the resume prompts directly under the
// roots, a JSON array of ids, over both timeline arms. The local arm is
// served by idx_items_subagent_resume_prompt, whose predicate aggPromptSQL
// states.
func subagentResumeRoundsQuery(threadID, roots string) (string, []any) {
	return timelineArms(threadID, timelineSelection{
		Columns: func(_, revExpr string) string {
			return `items.parent_id AS root, items.id AS id,
			        ` + aggPromptCarrierSQL("items.") + ` AS carrier,
			        items.turn_index AS turn_index, items.item_index AS item_index,
			        ` + revExpr + ` < 0 AS imported`
		},
		KeyFirst: true,
		Where: `items.parent_id IN (SELECT value FROM json_each(?))
			   AND ` + aggPromptSQL("items."),
		WhereArgs: []any{roots},
	})
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
			rootBound.hi = &TimelineCursor{TurnIndex: rs[0].turnIndex, ItemIndex: rs[0].itemIndex}
		}
		add(rootBound)
		for i, r := range rs {
			b := subagentRoundBounds{
				anchorID: r.anchorID, rootID: root, round: true,
				lo: &TimelineCursor{TurnIndex: r.turnIndex, ItemIndex: r.itemIndex},
			}
			if i+1 < len(rs) {
				b.hi = &TimelineCursor{TurnIndex: rs[i+1].turnIndex, ItemIndex: rs[i+1].itemIndex}
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

// subagentLaunchRowsByID resolves the launch rows behind completion
// siblings whose launch fell outside the window: one read over the
// logical timeline (local and imported arms), projecting only what the
// decorator needs. Ids that do not resolve are simply absent.
func (s *Store) subagentLaunchRowsByID(q sqlQueryer, threadID string, ids []string) (map[string]Item, error) {
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	source, queryArgs := subagentLaunchRowsQuery(threadID, list)
	rows, err := q.Query(source, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: resolve subagent launches for completions in %s: %w", threadID, err)
	}
	defer rows.Close()
	out := make(map[string]Item, len(ids))
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.Kind, &item.ToolName, &item.Meta, &item.Rev); err != nil {
			return nil, fmt.Errorf("store: scan subagent launch row: %w", err)
		}
		item.ThreadID = threadID
		out[item.ID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subagent launch rows for %s: %w", threadID, err)
	}
	return out, nil
}

// subagentLaunchRowsQuery reads the rows a JSON array of ids names, each
// by key on both arms.
func subagentLaunchRowsQuery(threadID, ids string) (string, []any) {
	return timelineArms(threadID, timelineSelection{
		Columns: func(_, revExpr string) string {
			return "items.id AS id, items.kind AS kind, items.tool_name AS tool_name, items.meta AS meta, " + revExpr + " AS rev"
		},
		KeyFirst:  true,
		Where:     "items.id IN (SELECT value FROM json_each(?))",
		WhereArgs: []any{ids},
	})
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
	// The ASCII set aggTranscriptRootSQL trims, so Go and the triggers
	// agree on which rows are carriers.
	return strings.Trim(decoded.TranscriptRootID, subagentPreviewBlank)
}

// decorateLatestDirectSubagentTools merges the newest direct, non-launch tool
// summary under each supplied launch into a read-time copy of that launch.
// Direct ownership matters: a nested agent has its own tray row, so its tools
// must not also appear as the parent's latest activity. A clean stamped
// launch already carries the keys (the history triggers keep them); the one
// query handles every other launch in the thread and keeps tray refreshes
// free of N+1 reads.
func (s *Store) decorateLatestDirectSubagentTools(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	rootIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Kind != "tool_call" || strings.TrimSpace(item.ID) == "" {
			continue
		}
		if item.Rev >= 0 && item.ToolName != "collab_agent" && subagentStampModeOf(item.Meta) == subagentStampClean {
			continue
		}
		rootIDs = append(rootIDs, item.ID)
	}
	if len(rootIDs) == 0 {
		return items, nil
	}
	latestByRoot, err := latestDirectSubagentTools(q, threadID, rootIDs)
	if err != nil {
		return nil, err
	}
	for i := range items {
		latest, ok := latestByRoot[items[i].ID]
		if !ok {
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

// latestDirectSubagentTool is a launch's tray activity: its newest direct
// tool_call child that is not a Codex spawn and has a nonblank summary.
// (turn_index, item_index) is unique within a thread, so the order has no
// ties. The summary is trimmed with the ASCII set the triggers use, so a
// stamp and this read agree byte for byte.
type latestDirectSubagentTool struct {
	id, summary          string
	turnIndex, itemIndex int
}

// latestDirectSubagentToolSQL reads one launch's tray activity backwards
// along idx_items_parent, stopping at the first qualifying child.
var latestDirectSubagentToolSQL = `SELECT id, trim(summary, ` + aggBlankSQL + `), turn_index, item_index
  FROM items
 WHERE thread_id = ? AND parent_id = ? AND parent_id <> ''
   AND ` + aggToolableSQL("") + `
 ORDER BY turn_index DESC, item_index DESC
 LIMIT 1`

func latestDirectSubagentTools(q sqlQueryer, threadID string, rootIDs []string) (map[string]latestDirectSubagentTool, error) {
	out := make(map[string]latestDirectSubagentTool, len(rootIDs))
	for _, rootID := range rootIDs {
		var latest latestDirectSubagentTool
		err := q.QueryRow(latestDirectSubagentToolSQL, threadID, rootID).
			Scan(&latest.id, &latest.summary, &latest.turnIndex, &latest.itemIndex)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: read latest direct subagent tool %s/%s: %w", threadID, rootID, err)
		}
		out[rootID] = latest
	}
	return out, nil
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

// SubagentCompletedChildIndex restores the execution boundary from immutable
// completion records after the session's live projection has been discarded.
func (s *Store) SubagentCompletedChildIndex(threadID, launchID string) (int, error) {
	source, args := timelineArms(threadID, timelineSelection{
		Columns: func(string, string) string {
			return "json_extract(items.meta, '$.codex_execution_child_end_index') AS child_end"
		},
		KeyFirst:  true,
		Where:     "items.completion_of <> '' AND items.completion_of = ?",
		WhereArgs: []any{launchID},
	})
	var end int
	if err := s.reader().QueryRow("SELECT COALESCE(MAX(child_end), 0) FROM ("+source+")", args...).Scan(&end); err != nil {
		return 0, fmt.Errorf("store: read completed subagent boundary: %w", err)
	}
	return end, nil
}

// SnapshotSubagentExecutionMeta returns immutable aggregates for one completed
// execution. The caller persists the result on its new completion item only.
// Child coordinates belong to the original spawn's AO turn.
func (s *Store) SnapshotSubagentExecutionMeta(threadID, launchID, itemMeta string, turnIndex, startIndex, endIndex int) (string, error) {
	aggregates, err := subagentAggregatesByRound(s.reader(), threadID, []string{launchID}, []subagentRoundBounds{{
		anchorID: launchID, rootID: launchID,
		lo: &TimelineCursor{TurnIndex: turnIndex, ItemIndex: startIndex + 1},
		hi: &TimelineCursor{TurnIndex: turnIndex, ItemIndex: endIndex + 1},
	}})
	if err != nil {
		return "", err
	}
	aggregate := aggregates[launchID]
	return mergeReadTimeMeta(itemMeta, map[string]any{
		metaKeySubagentDescendantCount:    aggregate.descendantCount,
		metaKeySubagentLatestChildSummary: aggregate.latestChildSummary,
	})
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
	return readSnapshot(s.reader(), "subagent descendants", func(q sqlQueryer) ([]Item, error) {
		return s.listSubagentDescendants(q, threadID, rootItemID)
	})
}

func (s *Store) listSubagentDescendants(q sqlQueryer, threadID, rootItemID string) ([]Item, error) {
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
	if anchor, found, err := s.getThreadItem(q, threadID, rootItemID); err != nil {
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
	roots, err := jsonList([]string{rootItemID})
	if err != nil {
		return nil, err
	}
	items, err := queryHydratedTimelineItems(
		q, threadID,
		descendantsCTE+"\n"+selectedSQL,
		append(descendantsCTEArgs(threadID, roots), selectedArgs...)...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	decorated, err := s.decorateProposedPlanItems(q, threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate subagent descendants for %s/%s: %w", threadID, rootItemID, err)
	}
	return s.decorateCompletionLaunches(q, threadID, decorated)
}
