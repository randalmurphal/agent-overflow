package store

import (
	"errors"
	"fmt"
)

// itemHydrationColumns is the canonical frontend-bound Item projection with
// caller-supplied expressions for the logical thread id, the three payload
// fields carried alongside a timeline row, the row meta and the row
// revision. Keeping the variable expressions here lets local and imported
// physical branches share one scanner contract without routing either
// branch through the compound timeline_payloads view. A read passes
// servedItemMetaFor(rev); a copy of the stored rows passes items.meta.
func itemHydrationColumns(threadID, payloadKind, payloadMeta, previewSpans, meta, streamEnd, rev string) string {
	return fmt.Sprintf(`items.id, %s, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''), %s, %s, %s,
    COALESCE(items.input_payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, %s, items.created_at, items.updated_at,
    %s,
    %s`,
		threadID, payloadKind, payloadMeta, previewSpans, meta, streamEnd, rev)
}

// localItemHydrationColumns is the projection of a local row, own or a
// fork's inherited one. An own row serves its merged meta over the join
// buildSelectedTimelineBranches renders when the caller passes servedItemJoin;
// an inherited row reads revision -1 and serves its stored meta.
func localItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(payloads.kind, '')",
		"COALESCE(payloads.meta, '')",
		"COALESCE(payloads.preview_spans, '')",
		servedItemMetaFor(rev),
		streamEndSQL,
		rev,
	)
}

// importedItemHydrationColumns reads no stream end: imported history
// never streams.
func importedItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(local_payloads.kind, imported_payloads.kind, '')",
		"COALESCE(local_payloads.meta, imported_payloads.meta, '')",
		"COALESCE(local_payloads.preview_spans, imported_payloads.preview_spans, '')",
		servedItemMetaFor(rev),
		"NULL",
		rev,
	)
}

// localStoredItemHydrationColumns is localItemHydrationColumns without
// subagentServedMetaSQL, for callers that read a row to write it back.
func localStoredItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(payloads.kind, '')",
		"COALESCE(payloads.meta, '')",
		"COALESCE(payloads.preview_spans, '')",
		"items.meta",
		streamEndSQL,
		rev,
	)
}

// importedStoredItemHydrationColumns mirrors importedItemHydrationColumns with
// raw stored meta.
func importedStoredItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(local_payloads.kind, imported_payloads.kind, '')",
		"COALESCE(local_payloads.meta, imported_payloads.meta, '')",
		"COALESCE(local_payloads.preview_spans, imported_payloads.preview_spans, '')",
		"items.meta",
		"NULL",
		rev,
	)
}

// buildSelectedTimelineBranches renders the branches that resolve the
// `selected(id)` set of selectedIDsSQL to rows of one thread, each through the
// physical source that owns the id:
//
//   - local rows probe items(thread_id,id) and payloads(thread_id,id);
//   - imported rows probe the item ID index, check the owning chunk's thread
//     membership, then resolve the chunk-scoped payload;
//   - a local payload overlay wins over its immutable imported payload, which
//     preserves timeline_payloads' copy-on-write shadowing contract;
//   - a pointer fork's inherited rows repeat the first two branches against
//     each lineage level's owner, under the same visibility rule as the
//     timeline_items lineage arms. A payload resolves in the thread that owns
//     the row, which is where the fork's copy-on-write put it. Only a fork
//     renders these branches: on every other thread they would only make
//     each read's statement several times as costly to prepare.
func buildSelectedTimelineBranches(local, imported func(thread, rev string) string, localJoin, orderBy string, forked bool) string {
	sql := `
		SELECT ` + local("items.thread_id", "items.rev") + `
		  FROM selected
		  CROSS JOIN items AS items
		    ON items.thread_id = ? AND items.id = selected.id
		  LEFT JOIN payloads AS payloads
		    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id` + localJoin + `
		UNION ALL
		SELECT ` + imported("refs.thread_id", importedItemRevExpr) + `
		  FROM selected
		  CROSS JOIN import_history_items AS items ON items.id = selected.id
		  CROSS JOIN thread_import_chunks AS refs ON refs.chunk_id = items.chunk_id
		  LEFT JOIN payloads AS local_payloads
		    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
		  LEFT JOIN import_history_payloads AS imported_payloads
		    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
		  LEFT JOIN thread_import_item_overrides AS overrides
		    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
		 WHERE refs.thread_id = ? AND overrides.item_id IS NULL`
	if forked {
		sql += `
		UNION ALL
		SELECT ` + local("l.thread_id", importedItemRevExpr) + `
		  FROM selected
		  CROSS JOIN thread_fork_lineage AS l
		  CROSS JOIN items AS items
		    ON items.thread_id = l.ancestor_id AND items.id = selected.id
		  LEFT JOIN payloads AS payloads
		    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		 WHERE l.thread_id = ?
		   AND ` + inheritedItemVisibleSQL + `
		UNION ALL
		SELECT ` + imported("l.thread_id", importedItemRevExpr) + `
		  FROM selected
		  CROSS JOIN thread_fork_lineage AS l
		  CROSS JOIN import_history_items AS items ON items.id = selected.id
		  CROSS JOIN thread_import_chunks AS refs
		    ON refs.chunk_id = items.chunk_id AND refs.thread_id = l.ancestor_id
		  LEFT JOIN payloads AS local_payloads
		    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
		  LEFT JOIN import_history_payloads AS imported_payloads
		    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
		  LEFT JOIN thread_import_item_overrides AS overrides
		    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
		 WHERE l.thread_id = ? AND overrides.item_id IS NULL
		   AND ` + inheritedItemVisibleSQL
	}
	return sql + `
		 ORDER BY ` + orderBy
}

// Precomputed timeline hydration branches eliminate per-query fmt.Sprintf and
// SQL string concatenation across the hot read and write-read paths.
var (
	hydratedTimelineBranchesNoFork = buildSelectedTimelineBranches(localItemHydrationColumns, importedItemHydrationColumns, servedItemJoin, "3, 4", false)
	hydratedTimelineBranchesFork   = buildSelectedTimelineBranches(localItemHydrationColumns, importedItemHydrationColumns, servedItemJoin, "3, 4", true)
	storedTimelineBranchesNoFork   = buildSelectedTimelineBranches(localStoredItemHydrationColumns, importedStoredItemHydrationColumns, "", "3, 4", false)
	storedTimelineBranchesFork     = buildSelectedTimelineBranches(localStoredItemHydrationColumns, importedStoredItemHydrationColumns, "", "3, 4", true)
)

func resolvePrecomputedTimelineSQL(q sqlQueryer, threadID, noForkSQL, forkSQL string) (string, []any, error) {
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return "", nil, err
	}
	if depth > 0 {
		return forkSQL, []any{threadID, threadID, threadID, threadID}, nil
	}
	return noForkSQL, []any{threadID, threadID}, nil
}

// idSelection is SQL that returns one `id` column, and its bind values.
// perLevel marks a selection rendered with a pair of arms per lineage
// level (renderTimelineArms), whose statement grows with the fork's depth.
type idSelection struct {
	sql      string
	args     []any
	perLevel bool
}

// fixedIDSelection is a selection whose text does not depend on the thread.
func fixedIDSelection(sql string, args ...any) idSelection {
	return idSelection{sql: sql, args: args}
}

// selectedIDsSQL is the `selected(id)` set of buildSelectedTimelineBranches'
// branches as a list of ids bound as one JSON array.
const selectedIDsSQL = `
		WITH selected(id) AS MATERIALIZED (SELECT value FROM json_each(?))`

// selectedSet renders the `selected(id)` set buildSelectedTimelineBranches'
// branches read from sel, and its bind values; found is false when sel
// selected nothing. The set is MATERIALIZED: every branch reads it.
//
// sel is the set's body unless it is perLevel. SQLite copies a CTE's body
// into the statement once for every reference, and from 3.54 refuses to
// expand a CTE reference once the statement holds 1000 cursors; a
// per-level selection grows with depth, so it runs as its own statement
// and the set is the ids it returned.
func selectedSet(q sqlQueryer, sel idSelection) (set string, args []any, found bool, err error) {
	if !sel.perLevel {
		return `
		WITH selected(id) AS MATERIALIZED (
			` + sel.sql + `
		)`, sel.args, true, nil
	}
	ids, err := queryIDs(q, sel.sql, sel.args...)
	if err != nil {
		return "", nil, false, err
	}
	return selectedIDList(ids)
}

// selectedIDList renders ids as the `selected(id)` set.
func selectedIDList(ids []string) (string, []any, bool, error) {
	if len(ids) == 0 {
		return "", nil, false, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return "", nil, false, err
	}
	return selectedIDsSQL, []any{list}, true, nil
}

// queryHydratedTimelineItems resolves each id sel selects through the
// physical branch that owns it, in timeline order. This shape is
// deliberately not expressed as `timeline_items LEFT JOIN
// timeline_payloads`: SQLite materializes that compound payload view,
// scans every payload in the database, and builds an automatic index
// before returning even a one-row lookup.
//
// The caller chooses q, so SyncThreadWindow can keep selection, hydration,
// decoration, and its history stamps in one WAL snapshot.
func queryHydratedTimelineItems(q sqlQueryer, threadID string, sel idSelection) ([]Item, error) {
	set, args, found, err := selectedSet(q, sel)
	if err != nil {
		return nil, fmt.Errorf("store: select hydrated timeline items for %s: %w", threadID, err)
	}
	return hydrateSelectedSetWithBranches(q, threadID, hydratedTimelineBranchesNoFork, hydratedTimelineBranchesFork, set, args, found)
}

// hydrateTimelineIDs is queryHydratedTimelineItems for ids a caller holds.
func hydrateTimelineIDs(q sqlQueryer, threadID string, ids []string) ([]Item, error) {
	set, args, found, err := selectedIDList(ids)
	if err != nil {
		return nil, err
	}
	return hydrateSelectedSetWithBranches(q, threadID, hydratedTimelineBranchesNoFork, hydratedTimelineBranchesFork, set, args, found)
}

func hydrateSelectedSetWithBranches(q sqlQueryer, threadID, noForkSQL, forkSQL, set string, setArgs []any, found bool) ([]Item, error) {
	if !found {
		return []Item{}, nil
	}
	branches, branchArgs, err := resolvePrecomputedTimelineSQL(q, threadID, noForkSQL, forkSQL)
	if err != nil {
		return nil, fmt.Errorf("store: query hydrated timeline items for %s: %w", threadID, err)
	}
	args := make([]any, 0, len(setArgs)+len(branchArgs))
	args = append(args, setArgs...)
	args = append(args, branchArgs...)
	rows, err := q.Query(set+branches, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query hydrated timeline items for %s: %w", threadID, err)
	}
	items, scanErr := scanItemRows(rows, 0)
	if err := errors.Join(scanErr, rows.Close()); err != nil {
		return nil, fmt.Errorf("store: scan hydrated timeline item row for %s: %w", threadID, err)
	}
	return items, nil
}

func queryOneHydratedTimelineItem(q sqlQueryer, threadID string, sel idSelection) (Item, bool, error) {
	return queryOneTimelineItemWithBranches(q, threadID, hydratedTimelineBranchesNoFork, hydratedTimelineBranchesFork, sel)
}

func queryOneStoredTimelineItem(q sqlQueryer, threadID string, sel idSelection) (Item, bool, error) {
	return queryOneTimelineItemWithBranches(q, threadID, storedTimelineBranchesNoFork, storedTimelineBranchesFork, sel)
}

func queryOneTimelineItemWithBranches(q sqlQueryer, threadID, noForkSQL, forkSQL string, sel idSelection) (Item, bool, error) {
	set, args, found, err := selectedSet(q, sel)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: select hydrated timeline items for %s: %w", threadID, err)
	}
	items, err := hydrateSelectedSetWithBranches(q, threadID, noForkSQL, forkSQL, set, args, found)
	if err != nil {
		return Item{}, false, err
	}
	if len(items) == 0 {
		return Item{}, false, nil
	}
	if len(items) != 1 {
		return Item{}, false, fmt.Errorf(
			"store: selected one hydrated timeline item for %s, got %d",
			threadID, len(items),
		)
	}
	return items[0], true, nil
}
