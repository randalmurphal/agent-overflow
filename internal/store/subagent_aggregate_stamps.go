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
// the walk, and a child write changes a few such rows per nesting level,
// never the anchor's items row beyond the revision stamp every child
// write already gives it:
//
//   - the card: descendant_count, latest_child_summary and
//     transcript_count, by the read-time rules: rounds cut by resume
//     prompts, nested launches counted transitively, plan_update
//     notifications excluded, the newest previewable summary;
//   - the tray: tool_summary, tool_turn and tool_item, the newest direct
//     tool_call child with a nonblank summary
//     (decorateLatestDirectSubagentTools);
//   - what the keyed writes need: the round's preview row (pick_*), its
//     newest descendant position (newest_*), a root's whole-transcript
//     newest position (transcript_newest_*), the tray row (tool_id), and
//     gen, which only a recompute moves.
//
// A local item read serves a clean row's public values merged into the
// anchor's meta (subagentServedMetaSQL), and a Claude completion sibling
// its launch's card. The internal columns never reach a client. The
// stored meta never holds the public keys: the item triggers strip them
// from a written meta (subagentStripServedKeysSQL), so a writer that
// writes back the meta of a row it read stores nothing stale.
//
// state is clean (0), dirty (1) or readTime (2). A clean row's values are
// the read. dirty means a write changed what the values describe and no
// keyed write kept them exact; idx_subagent_aggregates_dirty finds it
// and the recompute rewrites it from the read-time aggregator. readTime
// means the recompute found a shape the keyed writes do not maintain
// (imported or duplicate round prompts, a carrier no prompt names, a
// round resumed from a carrier) and the anchor stays on the read-time
// path. An anchor without a row is unstamped: no child since its insert,
// a carrier whose prompt has not arrived, or a legacy anchor in a thread
// still listed in subagent_aggregate_backfill.
//
// Codex spawn rows (collab_agent) take none of this: their card values are
// write-time snapshots on the completion row, and the spawn row itself is
// immutable (docs/specs/agent-visibility.md#immutable-agent-history).
//
// Two paths write the values. A write that names its row's subagent
// anchor (Item.SubagentAnchor, ItemPartialUpdate.SubagentAnchor) is
// claimed: the store checks the anchor against the row's parent chain and
// applies the write's effect with keyed reads and writes per nesting
// level, in the write's transaction (subagent_aggregate_writes.go). Every
// other write is left to the item triggers, which only mark the stamps it
// may have changed dirty (subagentMarkInsertSQL and its siblings); the
// writer settles them before it commits (settleSubagentAggregatesTx). A
// claimed write that changes a row's place in a walk is marked the same
// way. The recompute is the one general derivation: the settle, the
// bulk-load rebuild, a shadowed imported parent and migration v121's
// deferred phase all derive their values through it
// (computeSubagentStamps) and write them with writeSubagentStampsTx.
//
// A write to subagent_aggregates stamps its anchor and the anchor's
// completion siblings with the thread's history_rev
// (subagentAggregateTriggersSQL). A keyed write follows the item write
// whose trigger bumped the thread and stamped the same rows, so it
// changes no revision a second time; a recompute bumps the thread first.
// A row follows its item through the foreign key: deleting the item
// deletes it.
//
// Under history_bulk_load neither path does aggregate work; every
// bulk-load writer that changes a subtree recomputes the stamps before it
// commits. Folding sealed history back (UnsealThreadHistory) moves rows
// between the arms without changing any subtree, so the stamps stay as
// they are. Sealing never took a tool call, so no anchor moves.

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
// thread's history_rev. A row already at that revision is left alone: a
// second write of the same value would fire the item update trigger. Two
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
// card is its own snapshot, or a Codex wait carrier's. Only an anchor
// lends one.
func aggBorrowsCardSQL(a string) string {
	return "(" + a + "kind = 'tool_completion' AND " + a + "completion_of <> '' AND " +
		a + "tool_name NOT IN ('wait_agent', 'collab_agent'))"
}

// subagentStripServedKeysSQL is the item triggers' guard on the stored
// meta: the written meta of an anchor, or of a completion whose launch is
// one, that carries a public key, as a writer holding a served row writes
// it back or a copy carries it, has the keys removed again, so no read
// can serve a stale value from the stored meta. The instr terms keep
// every other write off a JSON parse. The write stamps the row, so it
// does not fire the update trigger again.
func subagentStripServedKeysSQL(ref string) string {
	paths := make([]string, 0, len(subagentServedKeys))
	present := make([]string, 0, len(subagentServedKeys))
	for _, served := range subagentServedKeys {
		paths = append(paths, "'$."+served.key+"'")
		present = append(present, aggJT(ref+".meta", "$."+served.key)+" IS NOT NULL")
	}
	return `UPDATE items SET meta = json_remove(meta, ` + strings.Join(paths, ", ") + `), rev = ` + aggStampRevSQL + `
	 WHERE thread_id = ` + ref + `.thread_id AND id = ` + ref + `.id
	   AND (` + aggAnchorableSQL(ref+".") + ` OR ` + aggBorrowsCardSQL(ref+".") + `)
	   AND (instr(` + ref + `.meta, '"subagentDescendantCount"') OR instr(` + ref + `.meta, '"subagentLatest')
	        OR instr(` + ref + `.meta, '"subagentTranscriptDescendantCount"'))
	   AND (` + aggAnchorableSQL(ref+".") + `
	        OR EXISTS (SELECT 1 FROM items l WHERE l.thread_id = ` + ref + `.thread_id AND l.id = ` + ref + `.completion_of
	                      AND ` + aggAnchorableSQL("l.") + `))
	   AND (` + strings.Join(present, " OR ") + `);`
}

// subagentAggregateUnanchorSQL drops the stamp of a row that stops being
// an anchor: no read decorates it any more.
var subagentAggregateUnanchorSQL = `DELETE FROM subagent_aggregates
	 WHERE ` + aggAnchorableSQL("OLD.") + ` AND NOT ` + aggAnchorableSQL("NEW.") + `
	   AND thread_id = NEW.thread_id AND item_id = NEW.id;`

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

// aggKeyedTranscriptRootSQL is aggTranscriptRootSQL for a row read by
// key. The row's meta, which a launch's input can make large, is parsed
// only when idx_items_transcript_root lists the row; the probe reads the
// thread's entries in that index, one per row naming a transcript root,
// and no table row.
func aggKeyedTranscriptRootSQL(a string) string {
	return "(CASE WHEN EXISTS (SELECT 1 FROM items agg_tr INDEXED BY idx_items_transcript_root" +
		" WHERE agg_tr.thread_id = " + a + "thread_id AND " + aggJX("agg_tr.meta", "$."+metaKeyTranscriptRootID) +
		" IS NOT NULL AND agg_tr.rowid = " + a + "rowid) THEN " + aggTranscriptRootSQL(a) + " END)"
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

// aggIsPromptSQL is aggPromptSQL as a value: 0 or 1, never NULL.
func aggIsPromptSQL(a string) string { return "COALESCE(" + aggPromptSQL(a) + ", 0)" }

// aggPromptCarrierSQL is the carrier a prompt row names, or the empty
// string for none.
func aggPromptCarrierSQL(a string) string {
	return "COALESCE(trim(" + aggJX(a+"meta", "$."+metaKeyResumeCarrierID) + ", " + aggBlankSQL + "), '')"
}

// aggHasChildSQL is "the row has a visible child other than except",
// over both physical arms of the thread's timeline. Its aliases are
// prefixed so a caller's own aliases (c, refs, o) cannot be captured.
func aggHasChildSQL(threadExpr, idExpr, exceptExpr string) string {
	return "(" + aggHasLocalChildSQL(threadExpr, idExpr, exceptExpr) + `
	 OR EXISTS (SELECT 1 FROM import_history_items agg_hc
	             CROSS JOIN thread_import_chunks agg_hc_refs
	                ON agg_hc_refs.chunk_id = agg_hc.chunk_id AND agg_hc_refs.thread_id = ` + threadExpr + `
	            WHERE agg_hc.parent_id = ` + idExpr + ` AND agg_hc.parent_id <> ''
	              AND ` + visibleItemsFilterFor("agg_hc.") + `
	              AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides agg_hc_o
	                               WHERE agg_hc_o.thread_id = agg_hc_refs.thread_id AND agg_hc_o.item_id = agg_hc.id)))`
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

// aggBulkIdleSQL is "the thread is not bulk loading".
func aggBulkIdleSQL(threadExpr string) string {
	return "((SELECT history_bulk_load FROM threads WHERE id = " + threadExpr + ") = 0)"
}

const aggStampRevSQL = "(SELECT history_rev FROM threads WHERE id = items.thread_id)"

// subagentClaimRev is the rev a claimed write stores: the write names its
// row's subagent anchor, so the store applies its effect on the stamps
// itself (subagent_aggregate_writes.go) and the item triggers leave that
// effect's mark out. The trigger's row stamp replaces the value in the
// same statement, so no row keeps it and no read sees it. It is the one
// value Go writes to rev.
const subagentClaimRev = -2

// aggClaimedSQL is "the write stored subagentClaimRev".
func aggClaimedSQL(ref string) string {
	return "(" + ref + ".rev IS " + strconv.Itoa(subagentClaimRev) + ")"
}

// aggChainCTE walks ref's ancestors by primary key. walk is the row's
// visibility: a walk from an ancestor reaches ref only through visible
// rows, so the chain stops above a hidden one. gate is the statement's
// condition on the trigger row; on the walk's start it keeps the chain
// empty whenever the statement marks nothing through it.
func aggChainCTE(name, ref, gate string) string {
	return name + `(thread_id, id, parent_id, kind, tool_name, meta, depth, walk) AS (
	    SELECT thread_id, id, parent_id, kind, tool_name, meta, 1, ` + visibleItemsFilterFor("") + `
	      FROM items
	     WHERE thread_id = ` + ref + `.thread_id AND id = ` + ref + `.parent_id
	       AND ` + ref + `.parent_id <> '' AND ` + visibleItemsFilterFor(ref+".") + ` AND ` + gate + `
	    UNION ALL
	    SELECT items.thread_id, items.id, items.parent_id, items.kind, items.tool_name, items.meta,
	           ` + name + `.depth + 1, ` + visibleItemsFilterFor("items.") + `
	      FROM ` + name + ` CROSS JOIN items
	     WHERE ` + name + `.walk AND ` + name + `.parent_id <> '' AND ` + name + `.depth < 64
	       AND items.thread_id = ` + name + `.thread_id AND items.id = ` + name + `.parent_id
	)`
}

// aggChainMarksSQL selects the stamps a write under a walked chain may
// have changed: the chain's anchors that are clean, or unstamped and not
// carriers (a carrier stays unstamped until its prompt arrives), and the
// carriers the resume prompts under those anchors name, whose rounds the
// same prompts cut. A readTime anchor on the chain is its family's
// verdict and stays; a named carrier is marked whatever its state, so its
// family is recomputed.
func aggChainMarksSQL(chain string) string {
	return `SELECT ch.thread_id AS thread_id, ch.id AS id
	      FROM ` + chain + ` ch
	      LEFT JOIN subagent_aggregates cs ON cs.thread_id = ch.thread_id AND cs.item_id = ch.id
	     WHERE ` + aggAnchorableSQL("ch.") + `
	       AND (cs.state IS ` + aggCleanLiteral + ` OR (cs.state IS NULL AND NOT ` + aggCarrierSQL("ch.") + `))
	    UNION ALL
	    SELECT ch.thread_id, ` + aggPromptCarrierSQL("p.") + `
	      FROM ` + chain + ` ch CROSS JOIN items p
	     WHERE ` + aggAnchorableSQL("ch.") + `
	       AND p.thread_id = ch.thread_id AND p.parent_id = ch.id AND ` + aggPromptSQL("p.")
}

// aggMarkDirtySQL is an item trigger statement that marks dirty the
// stamps rows selects: local anchorable rows only, whose stamp is not
// already dirty or that have none. A dirty stamp loses its values, and
// reads walk the anchor until the writer's settle recomputes it.
//
// gate is a condition on the trigger row alone. SQLite tests such a term
// before the query's first row, so a write it rules out runs nothing;
// each chain walk repeats it on its start (aggChainCTE).
func aggMarkDirtySQL(gate, ctes, rows string) string {
	with := ""
	if ctes != "" {
		with = "WITH RECURSIVE " + ctes + "\n\t    "
	}
	nulls := make([]string, 0, len(subagentAggregateValueColumns))
	for _, column := range subagentAggregateValueColumns {
		nulls = append(nulls, column+" = NULL")
	}
	return `INSERT INTO subagent_aggregates (thread_id, item_id, state)
	SELECT m.thread_id, m.id, ` + aggDirtyLiteral + `
	  FROM (
	    ` + with + rows + `
	  ) AS m
	  CROSS JOIN items a ON a.thread_id = m.thread_id AND a.id = m.id
	 WHERE ` + gate + ` AND m.id <> '' AND ` + aggAnchorableSQL("a.") + `
	ON CONFLICT (thread_id, item_id) DO UPDATE SET state = ` + aggDirtyLiteral + `, ` + strings.Join(nulls, ", ") + `
	 WHERE subagent_aggregates.state <> ` + aggDirtyLiteral + `;`
}

// The item triggers' marks. They cover every write the store does not
// apply itself: each write that does not carry subagentClaimRev, and the
// shapes a claimed write leaves to the recompute (a row inserted after
// rows written under it, a change to a row's place in a walk, a row that
// becomes an anchor). Under bulk load no mark runs; the loading writer
// recomputes before it commits.

// subagentMarkInsertSQL marks the chain of an unclaimed child: every stamp
// the child counts toward. A row inserted after rows already written
// under it adopts them: its chain is marked whether or not the write is
// claimed, and so is the row itself when it is an anchor, whose card
// holds them from its first read.
func subagentMarkInsertSQL() string {
	adopts := aggHasChildSQL("NEW.thread_id", "NEW.id", "")
	gate := `(` + aggBulkIdleSQL("NEW.thread_id") + `
	   AND ((NEW.parent_id <> '' AND ` + visibleItemsFilterFor("NEW.") + ` AND NOT ` + aggClaimedSQL("NEW") + `)
	        OR ((NEW.parent_id <> '' OR ` + aggAnchorableSQL("NEW.") + `) AND ` + adopts + `)))`
	return aggMarkDirtySQL(gate, aggChainCTE("agg_chain", "NEW", gate), aggChainMarksSQL("agg_chain")+`
	    UNION ALL
	    SELECT NEW.thread_id, NEW.id WHERE `+aggAnchorableSQL("NEW.")+` AND `+adopts)
}

// subagentMarkDeleteSQL marks OLD's chain, which loses OLD and its
// subtree, and for a prompt the carrier whose round it opened.
func subagentMarkDeleteSQL() string {
	gate := `(` + aggBulkIdleSQL("OLD.thread_id") + ` AND OLD.parent_id <> '' AND ` + visibleItemsFilterFor("OLD.") + `)`
	return aggMarkDirtySQL(gate, aggChainCTE("agg_chain", "OLD", gate), aggChainMarksSQL("agg_chain")+`
	    UNION ALL
	    SELECT OLD.thread_id, `+aggPromptCarrierSQL("OLD.")+` WHERE `+aggIsPromptSQL("OLD."))
}

// subagentMarkUpdateSQL marks what an update changed:
//
//   - a structural change, to the row's parent, position, thread,
//     visibility or prompt identity, moves it between rounds or cards:
//     both chains, and the carriers an old and a new prompt name, whether
//     or not the write is claimed;
//   - an unclaimed change to a preview-kind child's summary, kind or tool
//     may move its rounds' previews or its parent's tray: NEW's chain;
//   - a row that becomes an anchor, or whose transcript root changes,
//     when it has a stamp or children to count: the row itself.
//
// Status, updated_at, payload and meta-only writes match none of these
// and run nothing. A row that stops being an anchor loses its stamp
// (subagentAggregateUnanchorSQL).
func subagentMarkUpdateSQL() string {
	visOld, visNew := visibleItemsFilterFor("OLD."), visibleItemsFilterFor("NEW.")
	structural := `((` + visOld + ` OR ` + visNew + `) AND (OLD.parent_id <> '' OR NEW.parent_id <> '') AND (
	      OLD.id IS NOT NEW.id OR OLD.thread_id IS NOT NEW.thread_id OR OLD.parent_id IS NOT NEW.parent_id
	   OR OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index
	   OR (` + visOld + `) IS NOT (` + visNew + `)
	   OR ` + aggIsPromptSQL("OLD.") + ` IS NOT ` + aggIsPromptSQL("NEW.") + `
	   OR (` + aggIsPromptSQL("NEW.") + ` AND ` + aggPromptCarrierSQL("OLD.") + ` IS NOT ` + aggPromptCarrierSQL("NEW.") + `)))`
	previewKind := func(a string) string {
		return "(" + a + "kind IN ('" + strings.Join(subagentPreviewKinds, "', '") + "'))"
	}
	content := `(NOT ` + aggClaimedSQL("NEW") + ` AND NEW.parent_id <> '' AND ` + visNew + `
	   AND (OLD.summary IS NOT NEW.summary OR OLD.kind IS NOT NEW.kind OR OLD.tool_name IS NOT NEW.tool_name)
	   AND (` + previewKind("OLD.") + ` OR ` + previewKind("NEW.") + `))`
	rootChanged := "(OLD.meta IS NOT NEW.meta AND " + aggJX("OLD.meta", "$."+metaKeyTranscriptRootID) +
		" IS NOT " + aggJX("NEW.meta", "$."+metaKeyTranscriptRootID) + ")"
	self := `(` + aggAnchorableSQL("NEW.") + ` AND (NOT ` + aggAnchorableSQL("OLD.") + ` OR ` + rootChanged + `)
	   AND (NOT ` + aggAnchorableSQL("OLD.") + `
	        OR EXISTS (SELECT 1 FROM subagent_aggregates ss WHERE ss.thread_id = NEW.thread_id AND ss.item_id = NEW.id)
	        OR ` + aggHasChildSQL("NEW.thread_id", "NEW.id", "") + `))`
	idle := aggBulkIdleSQL("NEW.thread_id")
	gate := `(` + idle + ` AND (` + structural + ` OR ` + content + ` OR ` + self + `))`
	return aggMarkDirtySQL(gate,
		aggChainCTE("agg_chain_new", "NEW", `(`+idle+` AND (`+structural+` OR `+content+`))`)+`,
	    `+aggChainCTE("agg_chain_old", "OLD", `(`+idle+` AND `+structural+`)`),
		aggChainMarksSQL("agg_chain_new")+`
	    UNION ALL
	    `+aggChainMarksSQL("agg_chain_old")+`
	    UNION ALL
	    SELECT OLD.thread_id, `+aggPromptCarrierSQL("OLD.")+` WHERE `+aggIsPromptSQL("OLD.")+` AND `+structural+`
	    UNION ALL
	    SELECT NEW.thread_id, `+aggPromptCarrierSQL("NEW.")+` WHERE `+aggIsPromptSQL("NEW.")+` AND `+structural+`
	    UNION ALL
	    SELECT NEW.thread_id, NEW.id WHERE `+self)
}

// The statements are built once: the trigger DDL embeds them, and the
// plan tests prepare the same text.
var (
	subagentMarkInsertStmt      = subagentMarkInsertSQL()
	subagentMarkUpdateStmt      = subagentMarkUpdateSQL()
	subagentMarkDeleteStmt      = subagentMarkDeleteSQL()
	subagentStripServedKeysStmt = subagentStripServedKeysSQL("NEW")
)
