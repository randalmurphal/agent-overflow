package store

import "strings"

// Write-time subagent anchor aggregates.
//
// decorateSubagentAnchors derives an anchor's card from a walk of its
// descendants. The item triggers keep the same values on the anchor row
// itself, so a page read of a stamped anchor is its stored row and a child
// write costs a few JSON updates per nesting level instead of a walk:
//
//   - the card keys subagentDescendantCount, subagentLatestChildSummary and
//     subagentTranscriptDescendantCount, by the read-time rules: rounds cut
//     by resume prompts, nested launches counted transitively, plan_update
//     notifications excluded, the newest previewable summary;
//   - the tray keys subagentLatestToolSummary, subagentLatestToolTurnIndex
//     and subagentLatestToolItemIndex: the newest direct tool_call child
//     with a nonblank summary (decorateLatestDirectSubagentTools);
//   - subagentAggregateState, what the incremental rules need: gen (every
//     Go write of a stamp moves it by one), pick and newest (the round's
//     preview row and newest descendant position), transcriptNewest (a root
//     with rounds), toolPick (the tray row's id), and the dirty and
//     readTime flags.
//
// A row whose state carries neither flag is clean and its stored meta is
// the page read. dirty means an incremental rule could not keep the stamp
// exact (the pick or the newest row was deleted, a prompt landed before
// existing rows, a row moved); idx_items_subagent_aggregate_dirty finds it
// and RecomputeSubagentAggregates rewrites it from the read-time
// aggregator. readTime means the recompute found a shape the triggers do
// not maintain (imported or duplicate round prompts, a carrier no prompt
// names) and the row stays on the read-time path. A row without a state is
// unstamped: an anchor with no child since its insert, a carrier whose
// prompt has not arrived, or a legacy anchor in a thread still listed in
// subagent_aggregate_backfill.
//
// Codex spawn rows (collab_agent) take none of this: their card values are
// write-time snapshots on the completion row, and the spawn row itself is
// immutable (docs/specs/agent-visibility.md#immutable-agent-history).
//
// Every trigger statement runs after the thread bump and before the row
// stamp, sets rev to the thread's new history_rev on each row it writes,
// and so never re-enters the update trigger. The row stamp skips rows that
// already carry that revision. A Go write of a stamp (the recompute) goes
// through the update trigger like any other write; the update rule that
// restores the stamp against a stale whole-meta write recognizes it by
// gen = previous gen + 1.
//
// Under history_bulk_load the triggers do no aggregate work; every
// bulk-load writer that changes a subtree recomputes the stamps before it
// commits. Folding sealed history back (UnsealThreadHistory) moves rows
// between the arms without changing any subtree, so the stamps stay as
// they are. Sealing never took a tool call, so no anchor moves.
// RecomputeSubagentAggregates is the one recompute: the settle
// after a write, the bulk-load rebuild, a shadowed imported parent and
// migration v121's deferred phase all derive their values through it
// (computeSubagentStamps) and write them with writeSubagentStampsTx.

const metaKeySubagentAggregateState = "subagentAggregateState"

const (
	aggStatePath            = "$." + metaKeySubagentAggregateState
	aggGenPath              = aggStatePath + ".gen"
	aggDirtyPath            = aggStatePath + ".dirty"
	aggReadTimePath         = aggStatePath + ".readTime"
	aggPickPath             = aggStatePath + ".pick"
	aggNewestPath           = aggStatePath + ".newest"
	aggTranscriptNewestPath = aggStatePath + ".transcriptNewest"
	aggToolPickPath         = aggStatePath + ".toolPick"

	aggCountPath       = "$." + metaKeySubagentDescendantCount
	aggSummaryPath     = "$." + metaKeySubagentLatestChildSummary
	aggTranscriptPath  = "$." + metaKeySubagentTranscriptDescendantCount
	aggToolSummaryPath = "$." + metaKeySubagentLatestToolSummary
	aggToolTurnPath    = "$." + metaKeySubagentLatestToolTurn
	aggToolItemPath    = "$." + metaKeySubagentLatestToolItem
)

// aggKeyPaths are the flat keys a stamp owns, card keys then tray keys.
var aggKeyPaths = []string{
	aggCountPath, aggSummaryPath, aggTranscriptPath,
	aggToolSummaryPath, aggToolTurnPath, aggToolItemPath,
}

// aggBlankSQL is subagentPreviewBlank spelled for SQL trim(): the same
// ASCII set, so a summary is blank to the triggers exactly when it is
// blank to betterSubagentPreview.
const aggBlankSQL = "char(32, 9, 10, 11, 12, 13)"

func aggJX(expr, path string) string { return "json_extract(" + expr + ", '" + path + "')" }
func aggJT(expr, path string) string { return "json_type(" + expr + ", '" + path + "')" }

func aggQuotedPaths(paths ...string) string {
	return "'" + strings.Join(paths, "', '") + "'"
}

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

func aggStampedSQL(m string) string {
	return "(COALESCE(" + aggJT(m, aggStatePath) + ", '') = 'object')"
}

func aggCleanSQL(m string) string {
	return "(" + aggStampedSQL(m) + " AND " + aggJT(m, aggDirtyPath) + " IS NULL AND " +
		aggJT(m, aggReadTimePath) + " IS NULL)"
}

func aggIsDirtySQL(m string) string { return "(" + aggJX(m, aggDirtyPath) + " IS 1)" }

// aggHasKeysSQL is any stamp key or state on the row.
func aggHasKeysSQL(m string) string {
	terms := make([]string, 0, len(aggKeyPaths)+1)
	for _, path := range append([]string{aggStatePath}, aggKeyPaths...) {
		terms = append(terms, aggJT(m, path)+" IS NOT NULL")
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// aggKeysDifferSQL compares two metas' stamp keys and state.
func aggKeysDifferSQL(a, b string) string {
	terms := make([]string, 0, len(aggKeyPaths)+1)
	for _, path := range append([]string{aggStatePath}, aggKeyPaths...) {
		terms = append(terms, aggJX(a, path)+" IS NOT "+aggJX(b, path))
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

func aggPosSQL(a string) string { return "(" + a + "turn_index, " + a + "item_index)" }

func aggStatePosSQL(m, path string) string {
	return "(" + aggJX(m, path+"[0]") + ", " + aggJX(m, path+"[1]") + ")"
}

// aggAtSQL is "the position stored at path is ref's", NULL-safe.
func aggAtSQL(m, path, ref string) string {
	return "((" + aggStatePosSQL(m, path) + " = " + aggPosSQL(ref) + ") IS 1)"
}

// aggAfterSQL is "the position stored at path is after ref's", NULL-safe.
func aggAfterSQL(m, path, ref string) string {
	return "((" + aggStatePosSQL(m, path) + " > " + aggPosSQL(ref) + ") IS 1)"
}

// aggNewerSQL is "ref is newer than the position stored at path, or
// nothing is stored there".
func aggNewerSQL(m, path, ref string) string {
	return "(" + aggJX(m, path) + " IS NULL OR " + aggPosSQL(ref) + " > " + aggStatePosSQL(m, path) + ")"
}

// aggBetterPickSQL is betterSubagentPreview(ref, stored pick).
func aggBetterPickSQL(m, ref string) string {
	return "(" + aggPreviewableSQL(ref) + " AND (" + aggJX(m, aggPickPath) + " IS NULL OR " +
		aggPosSQL(ref) + " > " + aggStatePosSQL(m, aggPickPath) + " OR (" +
		aggPosSQL(ref) + " = " + aggStatePosSQL(m, aggPickPath) + " AND " + ref + "id < " +
		aggJX(m, aggPickPath+"[2]") + ")))"
}

// aggBetterToolSQL is the tray rule: ref is newer than the stored tool,
// ties by smallest id.
func aggBetterToolSQL(m, ref string) string {
	stored := "(" + aggJX(m, aggToolTurnPath) + ", " + aggJX(m, aggToolItemPath) + ")"
	return "(" + aggToolableSQL(ref) + " AND (" + aggJX(m, aggToolTurnPath) + " IS NULL OR " +
		aggPosSQL(ref) + " > " + stored + " OR (" + aggPosSQL(ref) + " = " + stored + " AND " +
		ref + "id < " + aggJX(m, aggToolPickPath) + ")))"
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

// aggChainCTE walks ref's ancestors by primary key. walk is the row's
// visibility: a walk from an ancestor reaches ref only through visible
// rows, so the chain stops above a hidden one.
func aggChainCTE(name, ref string) string {
	return name + `(thread_id, id, parent_id, kind, tool_name, meta, depth, walk) AS (
	    SELECT thread_id, id, parent_id, kind, tool_name, meta, 1, ` + visibleItemsFilterFor("") + `
	      FROM items
	     WHERE thread_id = ` + ref + `.thread_id AND id = ` + ref + `.parent_id
	       AND ` + ref + `.parent_id <> '' AND ` + visibleItemsFilterFor(ref+".") + `
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
// counts toward its card.
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
	return name + `_prompt(thread_id, id, meta, depth, prompt_id) AS MATERIALIZED (
	    SELECT ` + chain + `.thread_id, ` + chain + `.id, ` + chain + `.meta, ` + chain + `.depth,
	           (SELECT p.id FROM items p
	             WHERE p.thread_id = ` + chain + `.thread_id AND p.parent_id = ` + chain + `.id
	               AND ` + aggPromptSQL("p.") + `
	               AND ` + aggPosSQL("p.") + ` <= ` + aggPosSQL(ref+".") + `
	             ORDER BY p.turn_index DESC, p.item_index DESC LIMIT 1)
	      FROM ` + chain + `
	     WHERE ` + aggAnchorableSQL(chain+".") + ` AND NOT ` + aggCarrierSQL(chain+".") + `
	),
	` + name + `(thread_id, id, meta, depth, prompt_id, carrier_id, carrier_ok) AS MATERIALIZED (
	    SELECT a.thread_id, a.id, a.meta, a.depth, a.prompt_id, ` + aggPromptCarrierSQL("p.") + `,
	           CASE WHEN c.id IS NULL THEN 1 ELSE ` + aggTranscriptRootSQL("c.") + ` IS a.id END
	      FROM ` + name + `_prompt a
	      LEFT JOIN items p ON p.thread_id = a.thread_id AND p.id = a.prompt_id
	      LEFT JOIN items c ON c.thread_id = a.thread_id AND c.id = ` + aggPromptCarrierSQL("p.") + ` AND c.id <> ''
	            AND ` + aggAnchorableSQL("c.") + `
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

func aggDirtyMetaSQL(m string) string {
	return "json_set(json_remove(" + m + ", " + aggQuotedPaths(aggKeyPaths...) + "), '" + aggStatePath +
		"', json_object('gen', COALESCE(" + aggJX(m, aggGenPath) + ", 0), 'dirty', json('true')))"
}

func aggStripMetaSQL(m string) string {
	return "json_remove(" + m + ", " + aggQuotedPaths(append(aggKeyPaths, aggStatePath)...) + ")"
}

func aggClearedMetaSQL(m string) string { return aggStripMetaSQL(m) }

const aggStampRevSQL = "(SELECT history_rev FROM threads WHERE id = items.thread_id)"

// subagentAggregateInsertSQL is the insert trigger's aggregate statement.
//
// Roles, per target row:
//   - NEW itself, when it can carry a stamp: a copied stamp is stripped (a
//     clone or transfer rebuilds it from the rows it inserts), and a row
//     that adopts existing children is dirty;
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
	        WHEN NOT ` + aggStampedSQL("r.meta") + ` THEN
	          CASE WHEN r.depth = 1 AND (r.prompt_id IS NULL OR r.prompt_id = NEW.id)
	                    AND NOT ` + aggHasChildSQL("r.thread_id", "r.id", "NEW.id") + `
	               THEN CASE WHEN r.prompt_id IS NULL THEN 'init'
	                         WHEN r.carrier_id <> r.id AND r.carrier_ok THEN 'initRoot'
	                         ELSE 'dirty' END
	               ELSE 'dirty' END
	        WHEN NOT ` + aggCleanSQL("r.meta") + ` THEN NULL
	        WHEN NOT r.carrier_ok THEN 'dirty'
	        WHEN r.prompt_id IS NULL THEN 'round'
	        WHEN r.prompt_id = NEW.id THEN
	          CASE WHEN r.carrier_id = r.id
	                 OR ` + aggAfterSQL("r.meta", aggNewestPath, "NEW.") + `
	                 OR ` + aggAfterSQL("r.meta", aggTranscriptNewestPath, "NEW.") + `
	                 OR EXISTS (SELECT 1 FROM items c WHERE c.thread_id = r.thread_id AND c.id = r.carrier_id
	                              AND ` + aggStampedSQL("c.meta") + `)
	               THEN 'dirty' ELSE 'promptRoot' END
	        WHEN ` + aggJT("r.meta", aggTranscriptPath) + ` IS NULL THEN 'dirty'
	        ELSE 'transcript'
	      END
	      FROM agg_rounds r
	  )`
	ops := `agg_ops(thread_id, id, role) AS MATERIALIZED (
	    SELECT NEW.thread_id, NEW.id, CASE WHEN ` + adopts + ` THEN 'dirty' ELSE 'strip' END
	     WHERE ` + aggAnchorableSQL("NEW.") + ` AND (` + adopts + ` OR ` + aggHasKeysSQL("NEW.meta") + `)
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
	        WHEN ` + aggCleanSQL("c.meta") + ` THEN 'round'
	        WHEN ` + aggStampedSQL("c.meta") + ` THEN NULL
	        ELSE 'dirty'
	      END
	      FROM agg_roles r CROSS JOIN items c
	     WHERE r.carrier_id <> '' AND r.carrier_id <> r.id AND r.carrier_ok
	       AND c.thread_id = r.thread_id AND c.id = r.carrier_id AND ` + aggAnchorableSQL("c.") + `
	    UNION ALL
	    ` + aggFamilyDirtySQL("agg_roles") + `
	    UNION ALL
	    SELECT ch.thread_id, ch.id, 'tray'
	      FROM agg_chain ch
	     WHERE ch.depth = 1 AND ` + aggToolableSQL("NEW.") + ` AND ` + aggAnchorableSQL("ch.") + `
	       AND ` + aggCleanSQL("ch.meta") + `
	  )`
	aggregate := `SELECT thread_id, id,
	         CASE WHEN SUM(role = 'dirty') > 0
	                OR SUM(role IN ('init', 'initRoot', 'carrierInit', 'round', 'transcript', 'promptRoot')) > 1
	                OR SUM(role = 'tray') > 1
	                OR (SUM(role = 'strip') > 0 AND COUNT(*) > 1)
	              THEN 'dirty'
	              ELSE COALESCE(MAX(CASE WHEN role <> 'tray' THEN role END), 'none') END AS op,
	         SUM(role = 'tray') > 0 AS tray
	    FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	   GROUP BY thread_id, id`

	m := "items.meta"
	round := "agg.op = 'round'"
	hasTranscript := "(" + aggJT(m, aggTranscriptPath) + " IS NOT NULL)"
	better := aggBetterPickSQL(m, "NEW.")
	tool := "(agg.tray AND " + aggBetterToolSQL(m, "NEW.") + ")"
	newPos := "json_array(NEW.turn_index, NEW.item_index)"
	patch := `json_patch(` + m + `, json_object(
	    '` + metaKeySubagentDescendantCount + `', CASE agg.op
	        WHEN 'round' THEN COALESCE(` + aggJX(m, aggCountPath) + `, 0) + 1
	        WHEN 'promptRoot' THEN COALESCE(` + aggJX(m, aggCountPath) + `, 0)
	        ELSE ` + aggJX(m, aggCountPath) + ` END,
	    '` + metaKeySubagentLatestChildSummary + `', CASE WHEN ` + round + ` AND ` + better + `
	        THEN NEW.summary ELSE ` + aggJX(m, aggSummaryPath) + ` END,
	    '` + metaKeySubagentTranscriptDescendantCount + `', CASE
	        WHEN agg.op = 'transcript' OR (` + round + ` AND ` + hasTranscript + `) THEN ` + aggJX(m, aggTranscriptPath) + ` + 1
	        WHEN agg.op = 'promptRoot' THEN COALESCE(` + aggJX(m, aggTranscriptPath) + `, ` + aggJX(m, aggCountPath) + `, 0) + 1
	        ELSE ` + aggJX(m, aggTranscriptPath) + ` END,
	    '` + metaKeySubagentLatestToolSummary + `', CASE WHEN ` + tool + `
	        THEN trim(NEW.summary, ` + aggBlankSQL + `) ELSE ` + aggJX(m, aggToolSummaryPath) + ` END,
	    '` + metaKeySubagentLatestToolTurn + `', CASE WHEN ` + tool + ` THEN NEW.turn_index ELSE ` + aggJX(m, aggToolTurnPath) + ` END,
	    '` + metaKeySubagentLatestToolItem + `', CASE WHEN ` + tool + ` THEN NEW.item_index ELSE ` + aggJX(m, aggToolItemPath) + ` END,
	    '` + metaKeySubagentAggregateState + `', json_object(
	        'pick', json(CASE WHEN ` + round + ` AND ` + better + `
	            THEN json_array(NEW.turn_index, NEW.item_index, NEW.id) ELSE ` + aggJX(m, aggPickPath) + ` END),
	        'newest', json(CASE WHEN ` + round + ` AND ` + aggNewerSQL(m, aggNewestPath, "NEW.") + `
	            THEN ` + newPos + ` ELSE ` + aggJX(m, aggNewestPath) + ` END),
	        'transcriptNewest', json(CASE
	            WHEN (agg.op IN ('transcript', 'promptRoot') OR (` + round + ` AND ` + hasTranscript + `))
	             AND ` + aggNewerSQL(m, aggTranscriptNewestPath, "NEW.") + `
	            THEN ` + newPos + ` ELSE ` + aggJX(m, aggTranscriptNewestPath) + ` END),
	        'toolPick', CASE WHEN ` + tool + ` THEN NEW.id ELSE ` + aggJX(m, aggToolPickPath) + ` END)))`
	initMeta := `json_patch(` + aggClearedMetaSQL(m) + `, json_object(
	    '` + metaKeySubagentDescendantCount + `', 1,
	    '` + metaKeySubagentLatestChildSummary + `', CASE WHEN ` + aggPreviewableSQL("NEW.") + ` THEN NEW.summary END,
	    '` + metaKeySubagentLatestToolSummary + `', CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN trim(NEW.summary, ` + aggBlankSQL + `) END,
	    '` + metaKeySubagentLatestToolTurn + `', CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN NEW.turn_index END,
	    '` + metaKeySubagentLatestToolItem + `', CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN NEW.item_index END,
	    '` + metaKeySubagentAggregateState + `', json_object('gen', 0, 'newest', ` + newPos + `,
	        'pick', json(CASE WHEN ` + aggPreviewableSQL("NEW.") + ` THEN json_array(NEW.turn_index, NEW.item_index, NEW.id) END),
	        'toolPick', CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN NEW.id END)))`
	initRootMeta := `json_patch(` + aggClearedMetaSQL(m) + `, json_object(
	    '` + metaKeySubagentDescendantCount + `', 0,
	    '` + metaKeySubagentTranscriptDescendantCount + `', 1,
	    '` + metaKeySubagentAggregateState + `', json_object('gen', 0, 'transcriptNewest', ` + newPos + `)))`
	carrierInitMeta := `json_patch(` + aggClearedMetaSQL(m) + `, json_object(
	    '` + metaKeySubagentDescendantCount + `', 1,
	    '` + metaKeySubagentAggregateState + `', json_object('gen', 0, 'newest', ` + newPos + `)))`

	return `UPDATE items SET
	    meta = CASE agg.op
	      WHEN 'dirty' THEN ` + aggDirtyMetaSQL(m) + `
	      WHEN 'strip' THEN ` + aggStripMetaSQL(m) + `
	      WHEN 'init' THEN ` + initMeta + `
	      WHEN 'initRoot' THEN ` + initRootMeta + `
	      WHEN 'carrierInit' THEN ` + carrierInitMeta + `
	      ELSE ` + patch + `
	    END,
	    rev = ` + aggStampRevSQL + `
	  FROM (
	    WITH RECURSIVE
	    ` + aggChainCTE("agg_chain", "NEW") + `,
	    ` + aggRoundsCTE("agg_rounds", "agg_chain", "NEW") + `,
	    agg_orphan(adopts) AS MATERIALIZED (SELECT ` + aggHasChildSQL("NEW.thread_id", "NEW.id", "") + `),
	    ` + roles + `,
	    ` + ops + `
	    ` + aggregate + `
	  ) AS agg
	 WHERE (SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id) = 0
	   AND ((NEW.parent_id <> '' AND ` + visibleItemsFilterFor("NEW.") + `) OR ` + aggAnchorableSQL("NEW.") + `)
	   AND items.thread_id = agg.thread_id AND items.id = agg.id
	   AND (` + aggAnchorableSQL("items.") + ` OR agg.op = 'strip')
	   AND NOT (agg.op = 'dirty' AND ` + aggIsDirtySQL(m) + `)
	   AND NOT (agg.op = 'none' AND NOT agg.tray);`
}

// subagentAggregateDeleteSQL is the delete trigger's aggregate statement.
// A deleted row leaves its round (`unround`: count minus one) and, in a
// carrier's round, its root's whole-transcript count (`untranscript`).
// Deleting the row a stamp names as its preview, newest or tray row, a
// prompt (two rounds merge), or a row with children (its subtree leaves
// every walk above it) makes the affected anchors dirty.
func subagentAggregateDeleteSQL() string {
	adopts := "(SELECT adopts FROM agg_orphan)"
	unroundDirty := func(m string) string {
		return "(" + aggJX(m, aggPickPath+"[2]") + " IS OLD.id OR " +
			aggAtSQL(m, aggNewestPath, "OLD.") + " OR " + aggAtSQL(m, aggTranscriptNewestPath, "OLD.") + ")"
	}
	roles := `agg_roles(thread_id, id, depth, prompt_id, carrier_id, carrier_ok, role) AS MATERIALIZED (
	    SELECT r.thread_id, r.id, r.depth, r.prompt_id, r.carrier_id, r.carrier_ok,
	      CASE
	        WHEN NOT ` + aggStampedSQL("r.meta") + ` THEN 'dirty'
	        WHEN NOT ` + aggCleanSQL("r.meta") + ` THEN NULL
	        WHEN ` + adopts + ` OR NOT r.carrier_ok OR (r.depth = 1 AND ` + aggIsPromptSQL("OLD.") + `) THEN 'dirty'
	        WHEN r.prompt_id IS NULL THEN CASE WHEN ` + unroundDirty("r.meta") + ` THEN 'dirty' ELSE 'unround' END
	        WHEN ` + aggJT("r.meta", aggTranscriptPath) + ` IS NULL
	          OR ` + aggAtSQL("r.meta", aggTranscriptNewestPath, "OLD.") + ` THEN 'dirty'
	        ELSE 'untranscript'
	      END
	      FROM agg_rounds r
	  )`
	ops := `agg_ops(thread_id, id, role) AS MATERIALIZED (
	    SELECT thread_id, id, role FROM agg_roles WHERE role IS NOT NULL
	    UNION ALL
	    SELECT r.thread_id, c.id,
	      CASE
	        WHEN ` + adopts + ` OR NOT ` + aggStampedSQL("c.meta") + ` THEN 'dirty'
	        WHEN NOT ` + aggCleanSQL("c.meta") + ` THEN NULL
	        WHEN ` + unroundDirty("c.meta") + ` THEN 'dirty'
	        ELSE 'unround'
	      END
	      FROM agg_roles r CROSS JOIN items c
	     WHERE r.carrier_id <> '' AND r.carrier_id <> r.id AND r.carrier_ok
	       AND c.thread_id = r.thread_id AND c.id = r.carrier_id AND ` + aggAnchorableSQL("c.") + `
	    UNION ALL
	    ` + aggFamilyDirtySQL("agg_roles") + `
	    UNION ALL
	    SELECT OLD.thread_id, ` + aggPromptCarrierSQL("OLD.") + `, 'dirty' WHERE ` + aggIsPromptSQL("OLD.") + `
	    UNION ALL
	    SELECT ch.thread_id, ch.id, 'dirty'
	      FROM agg_chain ch
	     WHERE ch.depth = 1 AND ` + aggAnchorableSQL("ch.") + ` AND ` + aggCleanSQL("ch.meta") + `
	       AND ` + aggJX("ch.meta", aggToolPickPath) + ` IS OLD.id
	  )`
	aggregate := `SELECT thread_id, id,
	         CASE WHEN SUM(role = 'dirty') > 0 OR COUNT(*) > 1 THEN 'dirty' ELSE MAX(role) END AS op
	    FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	   GROUP BY thread_id, id`
	m := "items.meta"
	patch := `json_patch(` + m + `, json_object(
	    '` + metaKeySubagentDescendantCount + `', CASE agg.op WHEN 'unround' THEN ` + aggJX(m, aggCountPath) + ` - 1
	        ELSE ` + aggJX(m, aggCountPath) + ` END,
	    '` + metaKeySubagentTranscriptDescendantCount + `', ` + aggJX(m, aggTranscriptPath) + ` - 1))`
	return `UPDATE items SET
	    meta = CASE agg.op WHEN 'dirty' THEN ` + aggDirtyMetaSQL(m) + ` ELSE ` + patch + ` END,
	    rev = ` + aggStampRevSQL + `
	  FROM (
	    WITH RECURSIVE
	    ` + aggChainCTE("agg_chain", "OLD") + `,
	    ` + aggRoundsCTE("agg_rounds", "agg_chain", "OLD") + `,
	    agg_orphan(adopts) AS MATERIALIZED (SELECT ` + aggHasChildSQL("OLD.thread_id", "OLD.id", "") + `),
	    ` + roles + `,
	    ` + ops + `
	    ` + aggregate + `
	  ) AS agg
	 WHERE (SELECT history_bulk_load FROM threads WHERE id = OLD.thread_id) = 0
	   AND OLD.parent_id <> '' AND ` + visibleItemsFilterFor("OLD.") + `
	   AND items.thread_id = agg.thread_id AND items.id = agg.id
	   AND ` + aggAnchorableSQL("items.") + `
	   AND NOT (agg.op = 'dirty' AND ` + aggIsDirtySQL(m) + `);`
}

// subagentAggregateUpdateSQL is the update trigger's aggregate statement.
//
//   - A structural change (the row's parent, position, thread, visibility
//     or prompt identity) makes every stamped anchor on both the old and
//     the new chain dirty, with their carriers.
//   - A summary or kind change re-stamps the preview of NEW's round anchor
//     when NEW is its pick or now newer than it, and the tray of NEW's
//     parent likewise; a pick that stops qualifying makes the anchor dirty.
//   - On the row itself: becoming or ceasing to be an anchor, or a changed
//     carrier root, forces a recompute; a whole-meta write that changed the
//     stamp without being a stamp write (gen + 1) gets the previous stamp
//     back, so a writer holding stale meta cannot erase it.
//
// Status, updated_at, payload and rev-only writes match none of these and
// leave every stamp alone.
//
// The three change classes are spelled twice: once as the statement's
// gate, and once in agg_change, which every branch reads. Inlining them
// in each branch multiplied the text the update program is compiled
// from, and every INSERT, UPDATE and DELETE on items compiles it.
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
	rootChanged := "(" + aggJX("OLD.meta", "$."+metaKeyTranscriptRootID) + " IS NOT " + aggJX("NEW.meta", "$."+metaKeyTranscriptRootID) + ")"
	genBump := "(" + aggJX("NEW.meta", aggGenPath) + " IS COALESCE(" + aggJX("OLD.meta", aggGenPath) + ", 0) + 1)"
	selfTerms := `((` + aggAnchorableSQL("OLD.") + ` OR ` + aggAnchorableSQL("NEW.") + `) AND (
	      ` + aggAnchorableSQL("OLD.") + ` IS NOT ` + aggAnchorableSQL("NEW.") + `
	   OR ` + rootChanged + ` OR ` + aggKeysDifferSQL("OLD.meta", "NEW.meta") + `))`
	change := `agg_change(structural, content, self) AS MATERIALIZED (
	    SELECT ` + structuralTerms + `, ` + contentTerms + `, ` + selfTerms + `)`
	structural := "(SELECT structural FROM agg_change)"
	content := "(SELECT content AND NOT structural FROM agg_change)"
	self := "(SELECT self FROM agg_change)"

	chainDirty := func(chain string) string {
		return `SELECT ch.thread_id, ch.id, 'dirty' FROM ` + chain + ` ch
	     WHERE ` + structural + ` AND ` + aggAnchorableSQL("ch.") + `
	       AND (` + aggCleanSQL("ch.meta") + ` OR (NOT ` + aggStampedSQL("ch.meta") + ` AND NOT ` + aggCarrierSQL("ch.") + `))
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
	    SELECT x.thread_id, x.id,
	      CASE WHEN ` + aggJX("x.meta", aggPickPath+"[2]") + ` IS NEW.id
	             THEN CASE WHEN ` + aggPreviewableSQL("NEW.") + ` THEN 'pick' ELSE 'dirty' END
	           WHEN ` + aggBetterPickSQL("x.meta", "NEW.") + ` THEN 'pick'
	      END
	      FROM agg_rounds r CROSS JOIN items x
	     WHERE ` + content + `
	       AND (r.prompt_id IS NULL OR r.carrier_ok)
	       AND x.thread_id = r.thread_id AND x.id = CASE WHEN r.prompt_id IS NULL THEN r.id ELSE r.carrier_id END
	       AND ` + aggAnchorableSQL("x.") + ` AND ` + aggCleanSQL("x.meta") + `
	    UNION ALL
	    SELECT ch.thread_id, ch.id,
	      CASE WHEN ` + aggJX("ch.meta", aggToolPickPath) + ` IS NEW.id
	             THEN CASE WHEN ` + aggToolableSQL("NEW.") + ` THEN 'tool' ELSE 'dirty' END
	           WHEN ` + aggBetterToolSQL("ch.meta", "NEW.") + ` THEN 'tool'
	      END
	      FROM agg_chain_new ch
	     WHERE ` + content + ` AND ch.depth = 1 AND ` + aggAnchorableSQL("ch.") + ` AND ` + aggCleanSQL("ch.meta") + `
	    UNION ALL
	    SELECT NEW.thread_id, NEW.id,
	      CASE
	        WHEN ` + aggAnchorableSQL("OLD.") + ` IS NOT ` + aggAnchorableSQL("NEW.") + ` THEN
	          CASE WHEN ` + aggAnchorableSQL("NEW.") + ` THEN 'dirty' WHEN ` + aggStampedSQL("NEW.meta") + ` THEN 'strip' END
	        WHEN ` + rootChanged + ` AND (` + aggStampedSQL("NEW.meta") + ` OR ` + aggHasChildSQL("NEW.thread_id", "NEW.id", "") + `)
	          THEN 'dirty'
	        WHEN ` + aggKeysDifferSQL("OLD.meta", "NEW.meta") + ` AND NOT ` + genBump + ` THEN 'restore'
	      END
	     WHERE ` + self + `
	  )`
	aggregate := `SELECT thread_id, id,
	         CASE WHEN SUM(role = 'dirty') > 0 OR SUM(role = 'pick') > 1 OR SUM(role = 'tool') > 1
	                OR (SUM(role IN ('restore', 'strip')) > 0 AND COUNT(*) > 1)
	              THEN 'dirty'
	              WHEN SUM(role = 'restore') > 0 THEN 'restore'
	              WHEN SUM(role = 'strip') > 0 THEN 'strip'
	              ELSE 'patch' END AS op,
	         SUM(role = 'pick') > 0 AS pick,
	         SUM(role = 'tool') > 0 AS tool
	    FROM agg_ops WHERE role IS NOT NULL AND id <> ''
	   GROUP BY thread_id, id`

	m := "items.meta"
	restoreValues := make([]string, 0, len(aggKeyPaths)+1)
	for _, path := range aggKeyPaths {
		restoreValues = append(restoreValues, "'"+strings.TrimPrefix(path, "$.")+"', "+aggJX("OLD.meta", path))
	}
	restoreValues = append(restoreValues, "'"+metaKeySubagentAggregateState+"', json("+aggJX("OLD.meta", aggStatePath)+")")
	restore := `json_patch(` + aggClearedMetaSQL(m) + `, json_object(` + strings.Join(restoreValues, ", ") + `))`
	patch := `json_patch(` + m + `, json_object(
	    '` + metaKeySubagentLatestChildSummary + `', CASE WHEN agg.pick THEN NEW.summary ELSE ` + aggJX(m, aggSummaryPath) + ` END,
	    '` + metaKeySubagentLatestToolSummary + `', CASE WHEN agg.tool
	        THEN trim(NEW.summary, ` + aggBlankSQL + `) ELSE ` + aggJX(m, aggToolSummaryPath) + ` END,
	    '` + metaKeySubagentLatestToolTurn + `', CASE WHEN agg.tool THEN NEW.turn_index ELSE ` + aggJX(m, aggToolTurnPath) + ` END,
	    '` + metaKeySubagentLatestToolItem + `', CASE WHEN agg.tool THEN NEW.item_index ELSE ` + aggJX(m, aggToolItemPath) + ` END,
	    '` + metaKeySubagentAggregateState + `', json_object(
	        'pick', json(CASE WHEN agg.pick THEN json_array(NEW.turn_index, NEW.item_index, NEW.id)
	            ELSE ` + aggJX(m, aggPickPath) + ` END),
	        'toolPick', CASE WHEN agg.tool THEN NEW.id ELSE ` + aggJX(m, aggToolPickPath) + ` END)))`
	return `UPDATE items SET
	    meta = CASE agg.op
	      WHEN 'dirty' THEN ` + aggDirtyMetaSQL(m) + `
	      WHEN 'strip' THEN ` + aggStripMetaSQL(m) + `
	      WHEN 'restore' THEN ` + restore + `
	      ELSE ` + patch + `
	    END,
	    rev = ` + aggStampRevSQL + `
	  FROM (
	    WITH RECURSIVE
	    ` + change + `,
	    ` + aggChainCTE("agg_chain_new", "NEW") + `,
	    ` + aggChainCTE("agg_chain_old", "OLD") + `,
	    ` + aggRoundsCTE("agg_rounds", "agg_chain_new", "NEW") + `,
	    ` + ops + `
	    ` + aggregate + `
	  ) AS agg
	 WHERE NOT EXISTS (SELECT 1 FROM threads WHERE id IN (OLD.thread_id, NEW.thread_id) AND history_bulk_load <> 0)
	   AND (` + structuralTerms + ` OR ` + contentTerms + ` OR ` + selfTerms + `)
	   AND items.thread_id = agg.thread_id AND items.id = agg.id
	   AND (` + aggAnchorableSQL("items.") + ` OR agg.op = 'strip')
	   AND NOT (agg.op = 'dirty' AND ` + aggIsDirtySQL(m) + `);`
}

// The statements are built once: the trigger DDL embeds them, and the
// plan tests prepare the same text.
var (
	subagentAggregateInsertStmt = subagentAggregateInsertSQL()
	subagentAggregateUpdateStmt = subagentAggregateUpdateSQL()
	subagentAggregateDeleteStmt = subagentAggregateDeleteSQL()
)
