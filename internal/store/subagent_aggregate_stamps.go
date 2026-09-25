package store

import (
	"strconv"
	"strings"
)

// Write-time subagent anchor aggregates.
//
// decorateSubagentAnchors derives an anchor's card from a walk of its
// descendants. subagent_aggregates keeps the same values, one narrow row
// per anchor keyed (thread_id, item_id), so a read of a clean row skips
// the walk, and a child write changes no items row beyond the revision
// stamps every child write already gives its anchors:
//
//   - the card: descendant_count, latest_child_summary and
//     transcript_count, by the read-time rules: rounds cut by resume
//     prompts, nested launches counted transitively, plan_update
//     notifications excluded, the newest previewable summary;
//   - the tray: tool_summary, tool_turn and tool_item, the newest direct
//     tool_call child with a nonblank summary
//     (decorateLatestDirectSubagentTools);
//   - what the card accumulators need: the round's preview row (pick_*),
//     its newest descendant position (newest_*), a root's
//     whole-transcript newest position (transcript_newest_*), the tray
//     row (tool_id), and gen, the generation every write moves to a
//     value no stamp has held (newSubagentGen).
//
// A local item read serves a clean row's public values merged into the
// anchor's meta (subagentServedMetaSQL), and a Claude completion sibling
// its launch's card. The internal columns never reach a client, and the
// stored meta never holds the public keys: no writer stores a meta it
// read back (TestSubagentServedKeysAreNeverStored).
//
// state is clean (0), dirty (1) or readTime (2). A clean row's values are
// the read as of its last flush (subagent_card.go). dirty means no value
// describes the rows: RecoverSubagentCards marked the stamps of the
// agents a stopped process was running, and reads walk them until its
// recompute rewrites them (idx_subagent_aggregates_dirty finds them).
// readTime means
// the recompute found a shape the card rules do not maintain (imported or
// duplicate round prompts, a carrier no prompt names, a round resumed
// from a carrier) and the anchor stays on the read-time path. An anchor
// without a row is unstamped: no child flushed since its insert, a
// carrier whose prompt has not arrived, or a legacy anchor in a thread
// still listed in subagent_aggregate_backfill.
//
// Codex spawn rows (collab_agent) take none of this: their card values are
// write-time snapshots on the completion row, and the spawn row itself is
// immutable (docs/specs/agent-visibility.md#immutable-agent-history).
//
// Two paths write the values. A card's flush writes the accumulators the
// rows written under it fed, one keyed statement per changed stamp
// (subagent_card.go). The recompute is the one general derivation
// (computeSubagentFamilies): a card seeding a dirty or legacy stamp, a
// flush whose stamp moved, a write the card rules do not follow, a bulk
// writer (recomputeSubagentChainsTx, restampSubagentAggregatesTx),
// RecoverSubagentCards and migration v121's deferred phase all write
// through it (writeSubagentStampsTx).
//
// A write to subagent_aggregates stamps its anchor and the anchor's
// completion siblings with the thread's history_rev
// (subagentAggregateTriggersSQL). The writer advances the thread stamp
// first, unless an item write in the same transaction already did. A row
// follows its item through the foreign key: deleting the item deletes it.
//
// Under history_bulk_load the thread stamp is frozen; every bulk-load
// writer that changes a subtree recomputes the stamps before it commits.
// Folding sealed history back (UnsealThreadHistory) moves rows between
// the arms without changing any subtree, so the stamps stay as they are.
// Sealing never took a tool call, so no anchor moves.

// Stamp states.
const (
	aggStateClean    = 0
	aggStateDirty    = 1
	aggStateReadTime = 2
)

var (
	aggCleanLiteral = strconv.Itoa(aggStateClean)
	aggDirtyLiteral = strconv.Itoa(aggStateDirty)
)

// subagentAggregatesTableSQL is the table and its dirty index.
var subagentAggregatesTableSQL = `
CREATE TABLE subagent_aggregates (
    thread_id              TEXT    NOT NULL,
    item_id                TEXT    NOT NULL,
    state                  INTEGER NOT NULL DEFAULT 0 CHECK (state IN (0, 1, 2)),
    gen                    INTEGER NOT NULL DEFAULT 0,
    descendant_count       INTEGER,
    latest_child_summary   TEXT,
    transcript_count       INTEGER,
    tool_summary           TEXT,
    tool_turn              INTEGER,
    tool_item              INTEGER,
    tool_id                TEXT,
    pick_turn              INTEGER,
    pick_item              INTEGER,
    pick_id                TEXT,
    newest_turn            INTEGER,
    newest_item            INTEGER,
    transcript_newest_turn INTEGER,
    transcript_newest_item INTEGER,
    PRIMARY KEY (thread_id, item_id),
    FOREIGN KEY (thread_id, item_id) REFERENCES items(thread_id, id) ON DELETE CASCADE ON UPDATE CASCADE
) WITHOUT ROWID;

CREATE INDEX idx_subagent_aggregates_dirty
    ON subagent_aggregates(thread_id)
 WHERE state = ` + aggDirtyLiteral + `;
`

// subagentAggregateTriggersSQL stamps an anchor when its row is written.
// Deleting a row needs no stamp: an item delete, an anchor that stops
// being one and a restamp each stamp the rows whose read changed on their
// own. RestoreFrom brackets its row copy with
// dropSubagentAggregateTriggersSQL: the copied rows describe the copied
// items, whose revisions are the snapshot's.
var subagentAggregateTriggersSQL = `
CREATE TRIGGER trg_subagent_aggregates_stamp_insert AFTER INSERT ON subagent_aggregates BEGIN
  ` + subagentAggregateStampSQL + `
END;

CREATE TRIGGER trg_subagent_aggregates_stamp_update AFTER UPDATE ON subagent_aggregates BEGIN
  ` + subagentAggregateStampSQL + `
END;
`

const dropSubagentAggregateTriggersSQL = `DROP TRIGGER IF EXISTS trg_subagent_aggregates_stamp_insert;
DROP TRIGGER IF EXISTS trg_subagent_aggregates_stamp_update;`

// subagentAggregateStampSQL stamps the anchor a subagent_aggregates write
// changed, and the completion siblings that borrow its card, with the
// thread's history_rev. A row already at that revision is left alone. Two
// statements, so each reaches its rows by index: SQLite cannot serve an
// OR of the two lookups under the shared thread_id term with an index.
const subagentAggregateStampSQL = subagentAggregateStampAnchorSQL + "\n  " + subagentAggregateStampSiblingsSQL

const subagentAggregateStampAnchorSQL = `UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = NEW.thread_id)
   WHERE thread_id = NEW.thread_id AND id = NEW.item_id
     AND rev IS NOT (SELECT history_rev FROM threads WHERE id = NEW.thread_id);`

const subagentAggregateStampSiblingsSQL = `UPDATE items SET rev = (SELECT history_rev FROM threads WHERE id = NEW.thread_id)
   WHERE thread_id = NEW.thread_id AND completion_of = NEW.item_id AND completion_of <> ''
     AND rev IS NOT (SELECT history_rev FROM threads WHERE id = NEW.thread_id);`

// subagentAggregateValueColumns are subagent_aggregates' value columns in
// the order every writer lists them.
var subagentAggregateValueColumns = []string{
	"descendant_count", "latest_child_summary", "transcript_count",
	"tool_summary", "tool_turn", "tool_item", "tool_id",
	"pick_turn", "pick_item", "pick_id",
	"newest_turn", "newest_item", "transcript_newest_turn", "transcript_newest_item",
}

// subagentAggregateUpsertSQL ends an INSERT of whole rows: a row that
// exists takes the new state and values. gen is the generation's SET
// term, or "" to keep it.
func subagentAggregateUpsertSQL(gen string) string {
	sets := []string{"state = excluded.state"}
	if gen != "" {
		sets = append(sets, gen)
	}
	for _, column := range subagentAggregateValueColumns {
		sets = append(sets, column+" = excluded."+column)
	}
	return "ON CONFLICT (thread_id, item_id) DO UPDATE SET " + strings.Join(sets, ", ")
}

// subagentServedKeys are the public keys a clean row serves, card keys
// first, and the one place their JSON names meet their columns.
var subagentServedKeys = []struct{ key, column string }{
	{metaKeySubagentDescendantCount, "descendant_count"},
	{metaKeySubagentLatestChildSummary, "latest_child_summary"},
	{metaKeySubagentTranscriptDescendantCount, "transcript_count"},
	{metaKeySubagentLatestToolSummary, "tool_summary"},
	{metaKeySubagentLatestToolTurn, "tool_turn"},
	{metaKeySubagentLatestToolItem, "tool_item"},
}

// subagentServedCardKeys is how many of subagentServedKeys a completion
// sibling borrows from its launch: the card, not the tray.
const subagentServedCardKeys = 3

// subagentServedPatchSQL is the merge patch of a row's public values. A
// NULL value removes its key, so the stamp owns each key whether or not
// it has a value for it, as mergeSubagentAnchorMeta does for a walk.
func subagentServedPatchSQL(agg string, keys int) string {
	pairs := make([]string, 0, keys)
	for _, served := range subagentServedKeys[:keys] {
		pairs = append(pairs, "'"+served.key+"', "+agg+"."+served.column)
	}
	return "json_object(" + strings.Join(pairs, ", ") + ")"
}

// subagentServedMetaSQL is a local row's meta as a read serves it, over
// the aggregate row subagentServedJoinSQL joins as agg_served. A clean
// stamped anchor's public values are merged into it, and a Claude
// completion sibling takes its clean launch's card. A stamp with nothing
// to show, a meta that does not parse, and a row with no aggregate row
// serve the stored meta; only a merge parses it. `a` is the item alias
// with its trailing dot.
func subagentServedMetaSQL(a string) string {
	return `CASE
	  WHEN agg_served.item_id IS NULL THEN ` + a + `meta
	  WHEN ` + aggAnchorableSQL(a) + ` THEN CASE
	    WHEN (agg_served.descendant_count IS NOT NULL OR agg_served.tool_summary IS NOT NULL)
	     AND json_valid(` + a + `meta)
	    THEN json_patch(` + a + `meta, ` + subagentServedPatchSQL("agg_served", len(subagentServedKeys)) + `)
	    ELSE ` + a + `meta END
	  WHEN agg_served.descendant_count IS NOT NULL AND json_valid(` + a + `meta)
	  THEN json_patch(` + a + `meta, ` + subagentServedPatchSQL("agg_served", subagentServedCardKeys) + `)
	  ELSE ` + a + `meta END`
}

// subagentServedJoinSQL joins a local item row, alias `a` with its
// trailing dot, to the aggregate row its served meta merges, as
// agg_served: an anchor's own clean row, or a borrowing completion's
// launch's. One primary-key probe; a row that is neither probes nothing.
func subagentServedJoinSQL(a string) string {
	return `
		  LEFT JOIN subagent_aggregates agg_served
		    ON agg_served.thread_id = ` + a + `thread_id
		   AND agg_served.item_id = (CASE WHEN ` + aggAnchorableSQL(a) + ` THEN ` + a + `id
		                                  WHEN ` + aggBorrowsCardSQL(a) + ` THEN ` + a + `completion_of END)
		   AND agg_served.state = ` + aggCleanLiteral
}

// servedItemJoin is subagentServedJoinSQL for a projection over the
// `items` alias: every query that selects itemColumns,
// itemColumnsSansPayload or localItemHydrationColumns names it after its
// items row.
var servedItemJoin = subagentServedJoinSQL("items.")

// aggBorrowsCardSQL is a completion row that renders its launch's card
// (decorateSubagentAnchors): any completion but a Codex spawn's, whose
// card is its own snapshot, a Codex wait carrier's, or a parked stop's,
// which records one run and must not follow the launch's card as later
// runs grow it (agent_stops.go). Only an anchor lends one.
func aggBorrowsCardSQL(a string) string {
	return "(" + a + "kind = 'tool_completion' AND " + a + "completion_of <> '' AND " +
		a + "tool_name NOT IN ('wait_agent', 'collab_agent') AND " + a + "status <> '" + ItemStatusParked + "')"
}

// aggBlankSQL is subagentPreviewBlank spelled for SQL trim(): the same
// ASCII set, so a summary is blank to the triggers exactly when it is
// blank to betterSubagentPreview.
const aggBlankSQL = "char(32, 9, 10, 11, 12, 13)"

func aggJX(expr, path string) string { return "json_extract(" + expr + ", '" + path + "')" }
func aggJT(expr, path string) string { return "json_type(" + expr + ", '" + path + "')" }

// aggAnchorableSQL is a row that can carry a stamp: any tool_call except a
// Codex spawn. `a` is the row reference with its trailing dot.
func aggAnchorableSQL(a string) string {
	return "(" + a + "kind = 'tool_call' AND " + a + "tool_name <> 'collab_agent')"
}

// aggTranscriptRootSQL is transcriptRootFromMeta: the trimmed
// transcript_root_id string, or NULL.
func aggTranscriptRootSQL(a string) string {
	path := "$." + metaKeyTranscriptRootID
	return "(CASE WHEN " + aggJT(a+"meta", path) + " = 'text' THEN trim(" +
		aggJX(a+"meta", path) + ", " + aggBlankSQL + ") END)"
}

// aggCarrierSQL is a §E6 resume carrier: its transcript_root_id names
// another row. A carrier's own children never count toward its card; its
// round is counted under the root.
func aggCarrierSQL(a string) string {
	return "(COALESCE(" + aggTranscriptRootSQL(a) + " NOT IN ('', " + a + "id), 0))"
}

// aggToolableSQL is a row the tray reports as its parent's latest tool:
// a tool_call that is not a Codex spawn, with a nonblank summary.
func aggToolableSQL(a string) string {
	return "(" + aggAnchorableSQL(a) + " AND trim(" + a + "summary, " + aggBlankSQL + ") <> '')"
}

// aggPromptSQL is a §E6 resume-prompt row, for WHERE clauses. Its first
// three terms are idx_items_subagent_resume_prompt's predicate verbatim,
// which is what lets a probe written with it use that index. The carrier
// term is the read-time decoder's own refusal: a non-string carrier id
// fails to decode and the row is not a prompt.
func aggPromptSQL(a string) string {
	return "(" + a + "kind = 'user_text' AND " + a + "parent_id <> '' AND " +
		aggJT(a+"meta", "$."+metaKeySubagentResumePrompt) + " = 'true' AND COALESCE(" +
		aggJT(a+"meta", "$."+metaKeyResumeCarrierID) + ", 'null') IN ('text', 'null'))"
}

// aggPromptCarrierSQL is the carrier a prompt row names, or the empty
// string for none.
func aggPromptCarrierSQL(a string) string {
	return "COALESCE(trim(" + aggJX(a+"meta", "$."+metaKeyResumeCarrierID) + ", " + aggBlankSQL + "), '')"
}

// aggHasChildSQL is "the row has a visible child other than except",
// over the thread's whole timeline: both physical arms, and the rows a
// pointer fork reads through its lineage, where only a copy of an
// inherited anchor finds children. The lineage arms state the timeline
// arms' visibility rule, which names the ancestor row `items` and the
// lineage row `l`, so threadExpr and idExpr must not name either; the
// other aliases are prefixed so a caller's own (c, refs, o) cannot be
// captured. A thread without lineage probes thread_fork_lineage once per
// lineage arm.
func aggHasChildSQL(threadExpr, idExpr, exceptExpr string) string {
	return "(" + aggHasLocalChildSQL(threadExpr, idExpr, exceptExpr) + `
	 OR EXISTS (SELECT 1 FROM import_history_items agg_hc
	             CROSS JOIN thread_import_chunks agg_hc_refs
	                ON agg_hc_refs.chunk_id = agg_hc.chunk_id AND agg_hc_refs.thread_id = ` + threadExpr + `
	            WHERE agg_hc.parent_id = ` + idExpr + ` AND agg_hc.parent_id <> ''
	              AND ` + visibleItemsFilterFor("agg_hc.") + `
	              AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides agg_hc_o
	                               WHERE agg_hc_o.thread_id = agg_hc_refs.thread_id AND agg_hc_o.item_id = agg_hc.id))
	 OR EXISTS (SELECT 1 FROM thread_fork_lineage l
	             CROSS JOIN items ON items.thread_id = l.ancestor_id AND items.parent_id = ` + idExpr + `
	            WHERE l.thread_id = ` + threadExpr + ` AND items.parent_id <> ''
	              AND ` + visibleItemsFilterFor("items.") + `
	              AND ` + inheritedKeyedItemVisibleSQL + `)
	 OR EXISTS (SELECT 1 FROM thread_fork_lineage l
	             CROSS JOIN import_history_items items ON items.parent_id = ` + idExpr + `
	             CROSS JOIN thread_import_chunks agg_hc_refs
	                ON agg_hc_refs.chunk_id = items.chunk_id AND agg_hc_refs.thread_id = l.ancestor_id
	            WHERE l.thread_id = ` + threadExpr + ` AND items.parent_id <> ''
	              AND ` + visibleItemsFilterFor("items.") + `
	              AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides agg_hc_o
	                               WHERE agg_hc_o.thread_id = l.ancestor_id AND agg_hc_o.item_id = items.id)
	              AND ` + inheritedKeyedItemVisibleSQL + `))`
}

func aggHasLocalChildSQL(threadExpr, idExpr, exceptExpr string) string {
	except := ""
	if exceptExpr != "" {
		except = " AND agg_hc.id <> " + exceptExpr
	}
	return `EXISTS (SELECT 1 FROM items agg_hc
	          WHERE agg_hc.thread_id = ` + threadExpr + ` AND agg_hc.parent_id = ` + idExpr + ` AND agg_hc.parent_id <> ''` + except + `
	            AND ` + visibleItemsFilterFor("agg_hc.") + `)`
}
