package store

import (
	"strconv"
	"strings"
)

// Write-time subagent anchor aggregates.
//
// decorateSubagentAnchors derives an anchor's card from a walk of its
// descendants. The item triggers keep the same values in
// subagent_aggregates, one narrow row per anchor keyed (thread_id,
// item_id), so a child write updates a few such rows per nesting level
// instead of walking, and never rewrites an anchor's items row beyond the
// revision stamp every child write already gives it:
//
//   - the card: descendant_count, latest_child_summary and
//     transcript_count, by the read-time rules: rounds cut by resume
//     prompts, nested launches counted transitively, plan_update
//     notifications excluded, the newest previewable summary;
//   - the tray: tool_summary, tool_turn and tool_item, the newest direct
//     tool_call child with a nonblank summary
//     (decorateLatestDirectSubagentTools);
//   - what the incremental rules need: the round's preview row (pick_*),
//     its newest descendant position (newest_*), a root's whole-transcript
//     newest position (transcript_newest_*), the tray row (tool_id), and
//     gen, which only a Go write of a stamp moves.
//
// A local item read serves a clean row's public values merged into the
// anchor's meta (subagentServedMetaSQL), and a Claude completion sibling
// its launch's card. The internal columns never reach a client. The
// stored meta never holds the public keys: the item triggers strip them
// from a written meta (subagentStripServedKeysSQL), so a writer that
// writes back the meta of a row it read stores nothing stale.
//
// state is clean (0), dirty (1) or readTime (2). A clean row's values are
// the read. dirty means an incremental rule could not keep them exact (the
// pick or the newest row was deleted, a prompt landed before existing
// rows, a row moved); idx_subagent_aggregates_dirty finds it and
// RecomputeSubagentAggregates rewrites it from the read-time aggregator.
// readTime means the recompute found a shape the triggers do not maintain
// (imported or duplicate round prompts, a carrier no prompt names, a round
// resumed from a carrier) and the anchor stays on the read-time path. An
// anchor without a row is unstamped: no child since its insert, a carrier
// whose prompt has not arrived, or a legacy anchor in a thread still
// listed in subagent_aggregate_backfill.
//
// Codex spawn rows (collab_agent) take none of this: their card values are
// write-time snapshots on the completion row, and the spawn row itself is
// immutable (docs/specs/agent-visibility.md#immutable-agent-history).
//
// Each item trigger runs its aggregate statement after the thread bump and
// before the row stamp. A write to subagent_aggregates stamps its anchor
// and the anchor's completion siblings with the thread's history_rev
// (subagentAggregateTriggersSQL); the row stamp then skips them.
// A Go write bumps the thread first. A row follows its item through the
// foreign key: deleting the item deletes it.
//
// Under history_bulk_load the triggers do no aggregate work; every
// bulk-load writer that changes a subtree recomputes the stamps before it
// commits. Folding sealed history back (UnsealThreadHistory) moves rows
// between the arms without changing any subtree, so the stamps stay as
// they are. Sealing never took a tool call, so no anchor moves.
// RecomputeSubagentAggregates is the one recompute: the settle after a
// write, the bulk-load rebuild, a shadowed imported parent and migration
// v121's deferred phase all derive their values through it
// (computeSubagentStamps) and write them with writeSubagentStampsTx.

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

// subagentServedMetaSQL is a local row's meta as a read serves it. A clean
// stamped anchor's public values are merged into it, and a Claude
// completion sibling takes its clean launch's card, each by one
// primary-key probe of subagent_aggregates. A stamp with nothing to show,
// a meta that does not parse, and every other row serve the stored meta.
// `a` is the item alias with its trailing dot.
func subagentServedMetaSQL(a string) string {
	return `CASE
	  WHEN ` + aggAnchorableSQL(a) + ` THEN COALESCE((
	    SELECT json_patch(` + a + `meta, ` + subagentServedPatchSQL("agg_served", len(subagentServedKeys)) + `)
	      FROM subagent_aggregates agg_served
	     WHERE agg_served.thread_id = ` + a + `thread_id AND agg_served.item_id = ` + a + `id
	       AND agg_served.state = ` + aggCleanLiteral + `
	       AND (agg_served.descendant_count IS NOT NULL OR agg_served.tool_summary IS NOT NULL)
	       AND json_valid(` + a + `meta)), ` + a + `meta)
	  WHEN ` + aggBorrowsCardSQL(a) + ` THEN COALESCE((
	    SELECT json_patch(` + a + `meta, ` + subagentServedPatchSQL("agg_served", subagentServedCardKeys) + `)
	      FROM subagent_aggregates agg_served
	     WHERE agg_served.thread_id = ` + a + `thread_id AND agg_served.item_id = ` + a + `completion_of
	       AND agg_served.state = ` + aggCleanLiteral + ` AND agg_served.descendant_count IS NOT NULL
	       AND json_valid(` + a + `meta)), ` + a + `meta)
	  ELSE ` + a + `meta END`
}

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

// aggCarrierSQL is a §E6 resume carrier: its transcript_root_id names
// another row. A carrier's own children never count toward its card; its
// round is counted under the root.
func aggCarrierSQL(a string) string {
	return "(COALESCE(" + aggTranscriptRootSQL(a) + " NOT IN ('', " + a + "id), 0))"
}

// aggPreviewableSQL is previewableSubagentRow.
func aggPreviewableSQL(a string) string {
	return "(" + a + "kind IN ('" + strings.Join(subagentPreviewKinds, "', '") + "') AND trim(" +
		a + "summary, " + aggBlankSQL + ") <> '')"
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

func aggPosSQL(a string) string { return "(" + a + "turn_index, " + a + "item_index)" }

// aggAtSQL is "the stored position (turn, item) is ref's", NULL-safe.
func aggAtSQL(turn, item, ref string) string {
	return "(((" + turn + ", " + item + ") = " + aggPosSQL(ref) + ") IS 1)"
}

// aggAfterSQL is "the stored position (turn, item) is after ref's",
// NULL-safe.
func aggAfterSQL(turn, item, ref string) string {
	return "(((" + turn + ", " + item + ") > " + aggPosSQL(ref) + ") IS 1)"
}

// aggNewerSQL is "ref is newer than the stored position (turn, item), or
// nothing is stored".
func aggNewerSQL(turn, item, ref string) string {
	return "(" + turn + " IS NULL OR " + aggPosSQL(ref) + " > (" + turn + ", " + item + "))"
}

// aggBetterPickSQL is betterSubagentPreview(ref, the pick stored on s).
func aggBetterPickSQL(s, ref string) string {
	stored := "(" + s + ".pick_turn, " + s + ".pick_item)"
	return "(" + aggPreviewableSQL(ref) + " AND (" + s + ".pick_turn IS NULL OR " +
		aggPosSQL(ref) + " > " + stored + " OR (" + aggPosSQL(ref) + " = " + stored + " AND " +
		ref + "id < " + s + ".pick_id)))"
}

// aggBetterToolSQL is the tray rule against the tool stored on s: ref is
// newer, ties by smallest id.
func aggBetterToolSQL(s, ref string) string {
	stored := "(" + s + ".tool_turn, " + s + ".tool_item)"
	return "(" + aggToolableSQL(ref) + " AND (" + s + ".tool_turn IS NULL OR " +
		aggPosSQL(ref) + " > " + stored + " OR (" + aggPosSQL(ref) + " = " + stored + " AND " +
		ref + "id < " + s + ".tool_id)))"
}

// aggUnroundDirtySQL is "deleting ref takes a row or position the stamp
// names": its preview, its round's newest or its transcript's newest.
func aggUnroundDirtySQL(pickID, newestTurn, newestItem, transcriptTurn, transcriptItem, ref string) string {
	return "(" + pickID + " IS " + ref + "id OR " + aggAtSQL(newestTurn, newestItem, ref) + " OR " +
		aggAtSQL(transcriptTurn, transcriptItem, ref) + ")"
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

// aggChainCTE walks ref's ancestors by primary key. walk is the row's
// visibility: a walk from an ancestor reaches ref only through visible
// rows, so the chain stops above a hidden one. gate is a further condition
// on the walk's start. Under bulk load the chain is empty, which leaves
// every statement built on it nothing to do.
func aggChainCTE(name, ref, gate string) string {
	return name + `(thread_id, id, parent_id, kind, tool_name, meta, depth, walk) AS (
	    SELECT thread_id, id, parent_id, kind, tool_name, meta, 1, ` + visibleItemsFilterFor("") + `
	      FROM items
	     WHERE thread_id = ` + ref + `.thread_id AND id = ` + ref + `.parent_id
	       AND ` + ref + `.parent_id <> '' AND ` + visibleItemsFilterFor(ref+".") + gate + `
	       AND ` + aggBulkIdleSQL(ref+".thread_id") + `
	    UNION ALL
	    SELECT items.thread_id, items.id, items.parent_id, items.kind, items.tool_name, items.meta,
	           ` + name + `.depth + 1, ` + visibleItemsFilterFor("items.") + `
	      FROM ` + name + ` CROSS JOIN items
	     WHERE ` + name + `.walk AND ` + name + `.parent_id <> '' AND ` + name + `.depth < 64
	       AND items.thread_id = ` + name + `.thread_id AND items.id = ` + name + `.parent_id
	)`
}

// aggRoundsCTE resolves ref's round under every anchor on the chain: the
// latest resume prompt directly under the anchor at or before ref's
// position (one backwards probe of idx_items_subagent_resume_prompt), and
// the carrier that prompt names. No prompt means ref is in the anchor's
// own first round. Carriers are not anchors here: nothing under a carrier
// counts toward its card. Each anchor carries what the rules read of its
// stamp; st is NULL when it has none.
//
// carrier_ok is false when the named carrier is a local anchorable row
// stamped as another root's carrier. Its card is that root's round, so
// this anchor's rows must not touch it; the anchor goes dirty and its
// recompute marks the family readTime.
//
// Both CTEs are MATERIALIZED, as are the trigger statements' CTEs that
// read them. Flattening a derived table substitutes each of its result
// expressions at every reference, so an inlined prompt probe would be
// compiled, and run, once per mention of prompt_id or carrier_id
// downstream: every item write would prepare a program many times this
// size (TestSubagentAggregateStatementPlans counts the probes).
func aggRoundsCTE(name, chain, ref string) string {
	return name + `_prompt(thread_id, id, depth, prompt_id) AS MATERIALIZED (
	    SELECT ch.thread_id, ch.id, ch.depth,
	           (SELECT p.id FROM items p
	             WHERE p.thread_id = ch.thread_id AND p.parent_id = ch.id
	               AND ` + aggPromptSQL("p.") + `
	               AND ` + aggPosSQL("p.") + ` <= ` + aggPosSQL(ref+".") + `
	             ORDER BY p.turn_index DESC, p.item_index DESC LIMIT 1)
	      FROM ` + chain + ` ch
	     WHERE ` + aggAnchorableSQL("ch.") + ` AND NOT ` + aggCarrierSQL("ch.") + `
	),
	` + name + `(thread_id, id, depth, prompt_id, carrier_id, carrier_ok, st, pid, nt, ni, xt, xi, tc) AS MATERIALIZED (
	    SELECT a.thread_id, a.id, a.depth, a.prompt_id, ` + aggPromptCarrierSQL("p.") + `,
	           CASE WHEN c.id IS NULL THEN 1 ELSE ` + aggTranscriptRootSQL("c.") + ` IS a.id END,
	           s.state, s.pick_id, s.newest_turn, s.newest_item,
	           s.transcript_newest_turn, s.transcript_newest_item, s.transcript_count
	      FROM ` + name + `_prompt a
	      LEFT JOIN items p ON p.thread_id = a.thread_id AND p.id = a.prompt_id
	      LEFT JOIN items c ON c.thread_id = a.thread_id AND c.id = ` + aggPromptCarrierSQL("p.") + ` AND c.id <> ''
	            AND ` + aggAnchorableSQL("c.") + `
	      LEFT JOIN subagent_aggregates s ON s.thread_id = a.thread_id AND s.item_id = a.id
	)`
}

// aggFamilyDirtySQL marks every carrier a prompt under a dirty anchor names:
// their rounds are cut by the same prompts, so a stamp the anchor can no
// longer vouch for can take theirs with it.
func aggFamilyDirtySQL(roles string) string {
	return `SELECT r.thread_id, ` + aggPromptCarrierSQL("p.") + `, 'dirty'
	      FROM ` + roles + ` r CROSS JOIN items p
	     WHERE r.role = 'dirty' AND p.thread_id = r.thread_id AND p.parent_id = r.id
	       AND ` + aggPromptSQL("p.")
}

// aggTargetsCTE is the rows a statement writes: one per anchor in agg that
// exists and can carry a stamp, with its current stamp's values as old_*.
// A dirty stamp is not written dirty again, and nothing is written in a
// thread under bulk load. flags are named expressions over agg, s and the
// trigger row, evaluated once per target.
func aggTargetsCTE(where string, flags ...string) string {
	columns := make([]string, 0, len(subagentAggregateValueColumns)+len(flags))
	for _, column := range subagentAggregateValueColumns {
		columns = append(columns, "s."+column+" AS old_"+column)
	}
	columns = append(columns, flags...)
	return `agg_targets AS MATERIALIZED (
	    SELECT agg.thread_id AS thread_id, agg.id AS id, agg.op AS op,
	           ` + strings.Join(columns, ",\n\t           ") + `
	      FROM agg
	      CROSS JOIN items i ON i.thread_id = agg.thread_id AND i.id = agg.id
	      LEFT JOIN subagent_aggregates s ON s.thread_id = agg.thread_id AND s.item_id = agg.id
	     WHERE ` + aggAnchorableSQL("i.") + ` AND ` + aggBulkIdleSQL("agg.thread_id") + `
	       AND NOT (agg.op = 'dirty' AND s.state IS ` + aggDirtyLiteral + `)` + where + `
	)`
}

// aggWriteSQL is a trigger statement's write: the CTEs, then one whole row
// per target, upserted. values maps a value column to its expression over
// agg_targets; a column it leaves out keeps its old value, and a dirty row
// loses every value. gen is left as it is.
func aggWriteSQL(ctes string, values map[string]string) string {
	columns := make([]string, 0, len(subagentAggregateValueColumns))
	for _, column := range subagentAggregateValueColumns {
		value, ok := values[column]
		if !ok {
			value = "old_" + column
		}
		columns = append(columns, "CASE WHEN op = 'dirty' THEN NULL ELSE "+value+" END AS "+column)
	}
	list := strings.Join(subagentAggregateValueColumns, ", ")
	return `INSERT INTO subagent_aggregates (thread_id, item_id, state, ` + list + `)
	SELECT thread_id, id, state, ` + list + `
	  FROM (
	    WITH RECURSIVE
	    ` + ctes + `
	    SELECT thread_id, id,
	           CASE WHEN op = 'dirty' THEN ` + aggDirtyLiteral + ` ELSE ` + aggCleanLiteral + ` END AS state,
	           ` + strings.Join(columns, ",\n\t           ") + `
	      FROM agg_targets
	  ) AS agg_values
	 WHERE true
	` + subagentAggregateUpsertSQL("") + `;`
}

const aggStampRevSQL = "(SELECT history_rev FROM threads WHERE id = items.thread_id)"

// subagentAggregateInsertSQL is the insert trigger's aggregate statement.
//
// Roles, per target row:
//   - NEW itself, when it adopts existing children: dirty;
//   - each anchor on NEW's chain: `round` when NEW is in its first round,
//     `transcript` when NEW is in a carrier's round (the root's
//     whole-transcript count), `promptRoot` when NEW is the root's next
//     round prompt; an unstamped parent with no other child is initialized
//     from NEW (`init`, or `initRoot` for a prompt); anything else unstamped
//     becomes dirty;
//   - the carrier NEW's round belongs to: `round`, or `carrierInit` when
//     NEW is the prompt that opens its round;
//   - the direct parent: `tray` when NEW is a tool call.
//
// Two roles on one row cannot both be applied incrementally (except tray),
// so the row goes dirty instead.
func subagentAggregateInsertSQL() string {
	adopts := "(SELECT adopts FROM agg_orphan)"
	roles := `agg_roles(thread_id, id, depth, prompt_id, carrier_id, carrier_ok, role) AS MATERIALIZED (
	    SELECT r.thread_id, r.id, r.depth, r.prompt_id, r.carrier_id, r.carrier_ok,
	      CASE
	        WHEN ` + adopts + ` THEN 'dirty'
	        WHEN r.st IS NULL THEN
	          CASE WHEN r.depth = 1 AND (r.prompt_id IS NULL OR r.prompt_id = NEW.id)
	                    AND NOT ` + aggHasChildSQL("r.thread_id", "r.id", "NEW.id") + `
	               THEN CASE WHEN r.prompt_id IS NULL THEN 'init'
	                         WHEN r.carrier_id <> r.id AND r.carrier_ok THEN 'initRoot'
	                         ELSE 'dirty' END
	               ELSE 'dirty' END
	        WHEN r.st <> ` + aggCleanLiteral + ` THEN NULL
	        WHEN NOT r.carrier_ok THEN 'dirty'
	        WHEN r.prompt_id IS NULL THEN 'round'
	        WHEN r.prompt_id = NEW.id THEN
	          CASE WHEN r.carrier_id = r.id
	                 OR ` + aggAfterSQL("r.nt", "r.ni", "NEW.") + `
	                 OR ` + aggAfterSQL("r.xt", "r.xi", "NEW.") + `
	                 OR EXISTS (SELECT 1 FROM subagent_aggregates cs WHERE cs.thread_id = r.thread_id AND cs.item_id = r.carrier_id)
	               THEN 'dirty' ELSE 'promptRoot' END
	        WHEN r.tc IS NULL THEN 'dirty'
	        ELSE 'transcript'
	      END
	      FROM agg_rounds r
	  )`
	ops := `agg_ops(thread_id, id, role) AS MATERIALIZED (
	    SELECT NEW.thread_id, NEW.id, 'dirty' WHERE ` + aggAnchorableSQL("NEW.") + ` AND ` + adopts + `
	    UNION ALL
	    SELECT thread_id, id, role FROM agg_roles WHERE role IS NOT NULL
	    UNION ALL
	    SELECT r.thread_id, c.id,
	      CASE
	        WHEN ` + adopts + ` THEN 'dirty'
	        WHEN r.prompt_id = NEW.id THEN
	          CASE WHEN r.role IN ('promptRoot', 'initRoot')
	                    AND NOT ` + aggHasLocalChildSQL("c.thread_id", "c.id", "") + `
	               THEN 'carrierInit' ELSE 'dirty' END
	        WHEN cs.state IS ` + aggCleanLiteral + ` THEN 'round'
	        WHEN cs.state IS NOT NULL THEN NULL
	        ELSE 'dirty'
	      END
	      FROM agg_roles r CROSS JOIN items c
	      LEFT JOIN subagent_aggregates cs ON cs.thread_id = c.thread_id AND cs.item_id = c.id
	     WHERE r.carrier_id <> '' AND r.carrier_id <> r.id AND r.carrier_ok
	       AND c.thread_id = r.thread_id AND c.id = r.carrier_id AND ` + aggAnchorableSQL("c.") + `
	    UNION ALL
	    ` + aggFamilyDirtySQL("agg_roles") + `
	    UNION ALL
	    SELECT ch.thread_id, ch.id, 'tray'
	      FROM agg_chain ch CROSS JOIN subagent_aggregates ts
	     WHERE ch.depth = 1 AND ` + aggToolableSQL("NEW.") + `
	       AND ts.thread_id = ch.thread_id AND ts.item_id = ch.id AND ts.state = ` + aggCleanLiteral + `
	  )`
	aggregate := `agg(thread_id, id, op, tray) AS MATERIALIZED (
	    SELECT thread_id, id,
	           CASE WHEN SUM(role = 'dirty') > 0
	                  OR SUM(role IN ('init', 'initRoot', 'carrierInit', 'round', 'transcript', 'promptRoot')) > 1
	                  OR SUM(role = 'tray') > 1
	                THEN 'dirty'
	                ELSE COALESCE(MAX(CASE WHEN role <> 'tray' THEN role END), 'none') END,
	           SUM(role = 'tray') > 0
	      FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	     GROUP BY thread_id, id
	  )`
	targets := aggTargetsCTE(" AND NOT (agg.op = 'none' AND NOT agg.tray)",
		"agg.op = 'round' AND "+aggBetterPickSQL("s", "NEW.")+" AS pick",
		"agg.op = 'round' AND "+aggNewerSQL("s.newest_turn", "s.newest_item", "NEW.")+" AS newest",
		"(agg.op IN ('transcript', 'promptRoot') OR (agg.op = 'round' AND s.transcript_count IS NOT NULL)) AND "+
			aggNewerSQL("s.transcript_newest_turn", "s.transcript_newest_item", "NEW.")+" AS xnewest",
		"agg.tray AND "+aggBetterToolSQL("s", "NEW.")+" AS tool")

	// column is a value for each op that starts a row (init, initRoot,
	// carrierInit), then for the ops that move one on.
	column := func(init, initRoot, carrierInit, kept string) string {
		return "CASE op WHEN 'init' THEN " + init + " WHEN 'initRoot' THEN " + initRoot +
			" WHEN 'carrierInit' THEN " + carrierInit + " ELSE " + kept + " END"
	}
	previewable, toolable := aggPreviewableSQL("NEW."), aggToolableSQL("NEW.")
	pick := func(value, old string) string {
		return column("CASE WHEN "+previewable+" THEN "+value+" END", "NULL", "NULL",
			"CASE WHEN pick THEN "+value+" ELSE "+old+" END")
	}
	tool := func(value, old string) string {
		return column("CASE WHEN "+toolable+" THEN "+value+" END", "NULL", "NULL",
			"CASE WHEN tool THEN "+value+" ELSE "+old+" END")
	}
	return aggWriteSQL(aggChainCTE("agg_chain", "NEW", "")+`,
	    `+aggRoundsCTE("agg_rounds", "agg_chain", "NEW")+`,
	    agg_orphan(adopts) AS MATERIALIZED (
	      SELECT `+aggBulkIdleSQL("NEW.thread_id")+` AND `+aggHasChildSQL("NEW.thread_id", "NEW.id", "")+`),
	    `+roles+`,
	    `+ops+`,
	    `+aggregate+`,
	    `+targets, map[string]string{
		"descendant_count": column("1", "0", "1",
			"CASE op WHEN 'round' THEN COALESCE(old_descendant_count, 0) + 1"+
				" WHEN 'promptRoot' THEN COALESCE(old_descendant_count, 0) ELSE old_descendant_count END"),
		"latest_child_summary": pick("NEW.summary", "old_latest_child_summary"),
		"transcript_count": column("NULL", "1", "NULL",
			"CASE op WHEN 'transcript' THEN old_transcript_count + 1 WHEN 'round' THEN old_transcript_count + 1"+
				" WHEN 'promptRoot' THEN COALESCE(old_transcript_count, old_descendant_count, 0) + 1 ELSE old_transcript_count END"),
		"tool_summary": tool("trim(NEW.summary, "+aggBlankSQL+")", "old_tool_summary"),
		"tool_turn":    tool("NEW.turn_index", "old_tool_turn"),
		"tool_item":    tool("NEW.item_index", "old_tool_item"),
		"tool_id":      tool("NEW.id", "old_tool_id"),
		"pick_turn":    pick("NEW.turn_index", "old_pick_turn"),
		"pick_item":    pick("NEW.item_index", "old_pick_item"),
		"pick_id":      pick("NEW.id", "old_pick_id"),
		"newest_turn": column("NEW.turn_index", "NULL", "NEW.turn_index",
			"CASE WHEN newest THEN NEW.turn_index ELSE old_newest_turn END"),
		"newest_item": column("NEW.item_index", "NULL", "NEW.item_index",
			"CASE WHEN newest THEN NEW.item_index ELSE old_newest_item END"),
		"transcript_newest_turn": column("NULL", "NEW.turn_index", "NULL",
			"CASE WHEN xnewest THEN NEW.turn_index ELSE old_transcript_newest_turn END"),
		"transcript_newest_item": column("NULL", "NEW.item_index", "NULL",
			"CASE WHEN xnewest THEN NEW.item_index ELSE old_transcript_newest_item END"),
	})
}

// subagentAggregateDeleteSQL is the delete trigger's aggregate statement.
// A deleted row leaves its round (`unround`: count minus one) and, in a
// carrier's round, its root's whole-transcript count (`untranscript`).
// Deleting the row a stamp names as its preview, newest or tray row, a
// prompt (two rounds merge), or a row with children (its subtree leaves
// every walk above it) makes the affected anchors dirty.
func subagentAggregateDeleteSQL() string {
	adopts := "(SELECT adopts FROM agg_orphan)"
	roles := `agg_roles(thread_id, id, depth, prompt_id, carrier_id, carrier_ok, role) AS MATERIALIZED (
	    SELECT r.thread_id, r.id, r.depth, r.prompt_id, r.carrier_id, r.carrier_ok,
	      CASE
	        WHEN r.st IS NULL THEN 'dirty'
	        WHEN r.st <> ` + aggCleanLiteral + ` THEN NULL
	        WHEN ` + adopts + ` OR NOT r.carrier_ok OR (r.depth = 1 AND ` + aggIsPromptSQL("OLD.") + `) THEN 'dirty'
	        WHEN r.prompt_id IS NULL THEN
	          CASE WHEN ` + aggUnroundDirtySQL("r.pid", "r.nt", "r.ni", "r.xt", "r.xi", "OLD.") + ` THEN 'dirty' ELSE 'unround' END
	        WHEN r.tc IS NULL OR ` + aggAtSQL("r.xt", "r.xi", "OLD.") + ` THEN 'dirty'
	        ELSE 'untranscript'
	      END
	      FROM agg_rounds r
	  )`
	ops := `agg_ops(thread_id, id, role) AS MATERIALIZED (
	    SELECT thread_id, id, role FROM agg_roles WHERE role IS NOT NULL
	    UNION ALL
	    SELECT r.thread_id, c.id,
	      CASE
	        WHEN ` + adopts + ` OR cs.state IS NULL THEN 'dirty'
	        WHEN cs.state <> ` + aggCleanLiteral + ` THEN NULL
	        WHEN ` + aggUnroundDirtySQL("cs.pick_id", "cs.newest_turn", "cs.newest_item",
		"cs.transcript_newest_turn", "cs.transcript_newest_item", "OLD.") + ` THEN 'dirty'
	        ELSE 'unround'
	      END
	      FROM agg_roles r CROSS JOIN items c
	      LEFT JOIN subagent_aggregates cs ON cs.thread_id = c.thread_id AND cs.item_id = c.id
	     WHERE r.carrier_id <> '' AND r.carrier_id <> r.id AND r.carrier_ok
	       AND c.thread_id = r.thread_id AND c.id = r.carrier_id AND ` + aggAnchorableSQL("c.") + `
	    UNION ALL
	    ` + aggFamilyDirtySQL("agg_roles") + `
	    UNION ALL
	    SELECT OLD.thread_id, ` + aggPromptCarrierSQL("OLD.") + `, 'dirty' WHERE ` + aggIsPromptSQL("OLD.") + `
	    UNION ALL
	    SELECT ch.thread_id, ch.id, 'dirty'
	      FROM agg_chain ch CROSS JOIN subagent_aggregates ts
	     WHERE ch.depth = 1 AND ts.thread_id = ch.thread_id AND ts.item_id = ch.id
	       AND ts.state = ` + aggCleanLiteral + ` AND ts.tool_id IS OLD.id
	  )`
	aggregate := `agg(thread_id, id, op) AS MATERIALIZED (
	    SELECT thread_id, id, CASE WHEN SUM(role = 'dirty') > 0 OR COUNT(*) > 1 THEN 'dirty' ELSE MAX(role) END
	      FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	     GROUP BY thread_id, id
	  )`
	return aggWriteSQL(aggChainCTE("agg_chain", "OLD", "")+`,
	    `+aggRoundsCTE("agg_rounds", "agg_chain", "OLD")+`,
	    agg_orphan(adopts) AS MATERIALIZED (SELECT `+aggHasChildSQL("OLD.thread_id", "OLD.id", "")+`),
	    `+roles+`,
	    `+ops+`,
	    `+aggregate+`,
	    `+aggTargetsCTE(""), map[string]string{
		"descendant_count": "CASE op WHEN 'unround' THEN old_descendant_count - 1 ELSE old_descendant_count END",
		"transcript_count": "old_transcript_count - 1",
	})
}

// subagentAggregateUpdateSQL is the update trigger's aggregate statement.
//
//   - A structural change (the row's parent, position, thread, visibility
//     or prompt identity) makes every stamped anchor on both the old and
//     the new chain dirty, with their carriers.
//   - A summary or kind change re-stamps the preview of NEW's round anchor
//     when NEW is its pick or now newer than it, and the tray of NEW's
//     parent likewise; a pick that stops qualifying makes the anchor dirty.
//   - On the row itself: becoming an anchor, or a changed carrier root,
//     forces a recompute. A row that stops being an anchor loses its stamp
//     (subagentAggregateUnanchorSQL).
//
// Status, updated_at, payload, meta and rev-only writes match none of
// these: the chains are not walked and every stamp is left alone.
//
// The three change classes are computed once, in agg_change, which both
// chains and every branch read. Inlining them in each branch multiplied
// the text the update program is compiled from, and every INSERT, UPDATE
// and DELETE on items compiles it.
func subagentAggregateUpdateSQL() string {
	visOld, visNew := visibleItemsFilterFor("OLD."), visibleItemsFilterFor("NEW.")
	structuralTerms := `((` + visOld + ` OR ` + visNew + `) AND (OLD.parent_id <> '' OR NEW.parent_id <> '') AND (
	      OLD.id IS NOT NEW.id OR OLD.thread_id IS NOT NEW.thread_id OR OLD.parent_id IS NOT NEW.parent_id
	   OR OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index
	   OR (` + visOld + `) IS NOT (` + visNew + `)
	   OR ` + aggIsPromptSQL("OLD.") + ` IS NOT ` + aggIsPromptSQL("NEW.") + `
	   OR (` + aggIsPromptSQL("NEW.") + ` AND ` + aggPromptCarrierSQL("OLD.") + ` IS NOT ` + aggPromptCarrierSQL("NEW.") + `)))`
	previewKind := func(a string) string {
		return "(" + a + "kind IN ('" + strings.Join(subagentPreviewKinds, "', '") + "'))"
	}
	contentTerms := `(NEW.parent_id <> '' AND ` + visNew + `
	   AND (OLD.summary IS NOT NEW.summary OR OLD.kind IS NOT NEW.kind OR OLD.tool_name IS NOT NEW.tool_name)
	   AND (` + previewKind("OLD.") + ` OR ` + previewKind("NEW.") + `))`
	rootChanged := "(OLD.meta IS NOT NEW.meta AND " + aggJX("OLD.meta", "$."+metaKeyTranscriptRootID) +
		" IS NOT " + aggJX("NEW.meta", "$."+metaKeyTranscriptRootID) + ")"
	selfTerms := `(` + aggAnchorableSQL("NEW.") + ` AND (NOT ` + aggAnchorableSQL("OLD.") + ` OR ` + rootChanged + `))`
	change := `agg_change(structural, content, self) AS MATERIALIZED (
	    SELECT ` + structuralTerms + `, ` + contentTerms + `, ` + selfTerms + `)`
	structural := "(SELECT structural FROM agg_change)"
	content := "(SELECT content AND NOT structural FROM agg_change)"
	self := "(SELECT self FROM agg_change)"

	chainDirty := func(chain string) string {
		return `SELECT ch.thread_id, ch.id, 'dirty' FROM ` + chain + ` ch
	      LEFT JOIN subagent_aggregates cs ON cs.thread_id = ch.thread_id AND cs.item_id = ch.id
	     WHERE ` + structural + ` AND ` + aggAnchorableSQL("ch.") + `
	       AND (cs.state IS ` + aggCleanLiteral + ` OR (cs.state IS NULL AND NOT ` + aggCarrierSQL("ch.") + `))
	    UNION ALL
	    SELECT ch.thread_id, ` + aggPromptCarrierSQL("p.") + `, 'dirty'
	      FROM ` + chain + ` ch CROSS JOIN items p
	     WHERE ` + structural + ` AND ` + aggAnchorableSQL("ch.") + `
	       AND p.thread_id = ch.thread_id AND p.parent_id = ch.id AND ` + aggPromptSQL("p.")
	}
	ops := `agg_ops(thread_id, id, role) AS MATERIALIZED (
	    ` + chainDirty("agg_chain_new") + `
	    UNION ALL
	    ` + chainDirty("agg_chain_old") + `
	    UNION ALL
	    SELECT OLD.thread_id, ` + aggPromptCarrierSQL("OLD.") + `, 'dirty' WHERE ` + structural + ` AND ` + aggIsPromptSQL("OLD.") + `
	    UNION ALL
	    SELECT NEW.thread_id, ` + aggPromptCarrierSQL("NEW.") + `, 'dirty' WHERE ` + structural + ` AND ` + aggIsPromptSQL("NEW.") + `
	    UNION ALL
	    SELECT xs.thread_id, xs.item_id,
	      CASE WHEN xs.pick_id IS NEW.id
	             THEN CASE WHEN ` + aggPreviewableSQL("NEW.") + ` THEN 'pick' ELSE 'dirty' END
	           WHEN ` + aggBetterPickSQL("xs", "NEW.") + ` THEN 'pick'
	      END
	      FROM agg_rounds r CROSS JOIN subagent_aggregates xs
	     WHERE ` + content + `
	       AND (r.prompt_id IS NULL OR r.carrier_ok)
	       AND xs.thread_id = r.thread_id AND xs.item_id = CASE WHEN r.prompt_id IS NULL THEN r.id ELSE r.carrier_id END
	       AND xs.state = ` + aggCleanLiteral + `
	    UNION ALL
	    SELECT ts.thread_id, ts.item_id,
	      CASE WHEN ts.tool_id IS NEW.id
	             THEN CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN 'tool' ELSE 'dirty' END
	           WHEN ` + aggBetterToolSQL("ts", "NEW.") + ` THEN 'tool'
	      END
	      FROM agg_chain_new ch CROSS JOIN subagent_aggregates ts
	     WHERE ` + content + ` AND ch.depth = 1
	       AND ts.thread_id = ch.thread_id AND ts.item_id = ch.id AND ts.state = ` + aggCleanLiteral + `
	    UNION ALL
	    SELECT NEW.thread_id, NEW.id, 'dirty'
	     WHERE ` + self + ` AND (NOT ` + aggAnchorableSQL("OLD.") + `
	        OR EXISTS (SELECT 1 FROM subagent_aggregates ss WHERE ss.thread_id = NEW.thread_id AND ss.item_id = NEW.id)
	        OR ` + aggHasChildSQL("NEW.thread_id", "NEW.id", "") + `)
	  )`
	aggregate := `agg(thread_id, id, op, pick, tool) AS MATERIALIZED (
	    SELECT thread_id, id,
	           CASE WHEN SUM(role = 'dirty') > 0 OR SUM(role = 'pick') > 1 OR SUM(role = 'tool') > 1
	                THEN 'dirty' ELSE 'patch' END,
	           SUM(role = 'pick') > 0, SUM(role = 'tool') > 0
	      FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	     GROUP BY thread_id, id
	  )`
	pick := func(value, old string) string { return "CASE WHEN pick THEN " + value + " ELSE " + old + " END" }
	tool := func(value, old string) string { return "CASE WHEN tool THEN " + value + " ELSE " + old + " END" }
	return aggWriteSQL(change+`,
	    `+aggChainCTE("agg_chain_new", "NEW", " AND (SELECT structural OR content FROM agg_change)")+`,
	    `+aggChainCTE("agg_chain_old", "OLD", " AND (SELECT structural FROM agg_change)")+`,
	    `+aggRoundsCTE("agg_rounds", "agg_chain_new", "NEW")+`,
	    `+ops+`,
	    `+aggregate+`,
	    `+aggTargetsCTE("", "agg.pick AS pick", "agg.tool AS tool"),
		map[string]string{
			"latest_child_summary": pick("NEW.summary", "old_latest_child_summary"),
			"pick_turn":            pick("NEW.turn_index", "old_pick_turn"),
			"pick_item":            pick("NEW.item_index", "old_pick_item"),
			"pick_id":              pick("NEW.id", "old_pick_id"),
			"tool_summary":         tool("trim(NEW.summary, "+aggBlankSQL+")", "old_tool_summary"),
			"tool_turn":            tool("NEW.turn_index", "old_tool_turn"),
			"tool_item":            tool("NEW.item_index", "old_tool_item"),
			"tool_id":              tool("NEW.id", "old_tool_id"),
		})
}

// The statements are built once: the trigger DDL embeds them, and the
// plan tests prepare the same text.
var (
	subagentAggregateInsertStmt = subagentAggregateInsertSQL()
	subagentAggregateUpdateStmt = subagentAggregateUpdateSQL()
	subagentAggregateDeleteStmt = subagentAggregateDeleteSQL()
	subagentStripServedKeysStmt = subagentStripServedKeysSQL("NEW")
)
