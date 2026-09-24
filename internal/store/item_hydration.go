package store

import "fmt"

// itemHydrationColumns is the canonical frontend-bound Item projection with
// caller-supplied expressions for the logical thread id, the three payload
// fields carried alongside a timeline row, the row meta and the row
// revision. Keeping the variable expressions here lets local and imported
// physical branches share one scanner contract without routing either
// branch through the compound timeline_payloads view. A read passes
// servedItemMetaFor(rev); a copy of the stored rows passes items.meta.
func itemHydrationColumns(threadID, payloadKind, payloadMeta, previewSpans, meta, rev string) string {
	return fmt.Sprintf(`items.id, %s, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''), %s, %s, %s,
    COALESCE(items.input_payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, %s, items.created_at, items.updated_at,
    %s`,
		threadID, payloadKind, payloadMeta, previewSpans, meta, rev)
}

// localItemHydrationColumns is the projection of a local row, own or a
// fork's inherited one. An own row serves its merged meta over the join
// resolveSelectedTimelineSQL renders when the caller passes servedItemJoin;
// an inherited row reads revision -1 and serves its stored meta.
func localItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(payloads.kind, '')",
		"COALESCE(payloads.meta, '')",
		"COALESCE(payloads.preview_spans, '')",
		servedItemMetaFor(rev),
		rev,
	)
}

func importedItemHydrationColumns(thread, rev string) string {
	return itemHydrationColumns(
		thread,
		"COALESCE(local_payloads.kind, imported_payloads.kind, '')",
		"COALESCE(local_payloads.meta, imported_payloads.meta, '')",
		"COALESCE(local_payloads.preview_spans, imported_payloads.preview_spans, '')",
		servedItemMetaFor(rev),
		rev,
	)
}

// resolveSelectedTimelineSQL renders the branches that resolve a
// MATERIALIZED `selected(id)` set to rows of one thread, each through the
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
//
// local and imported render a branch's projection from its logical thread
// id and row revision expressions. localJoin is a join the own local
// branch's projection reads (servedItemJoin for served meta), or "". It
// returns the branches and their bind values, which follow the
// selection's.
func resolveSelectedTimelineSQL(q sqlQueryer, threadID string, local, imported func(thread, rev string) string, localJoin, orderBy string) (string, []any, error) {
	depth, err := forkLineageDepth(q, threadID)
	if err != nil {
		return "", nil, err
	}
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
	args := []any{threadID, threadID}
	if depth > 0 {
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
		args = append(args, threadID, threadID)
	}
	return sql + `
		 ORDER BY ` + orderBy, args, nil
}

// queryHydratedTimelineItems selects logical item ids first, then resolves each
// id through the physical branch that owns it. This shape is deliberately not
// expressed as `timeline_items LEFT JOIN timeline_payloads`: SQLite materializes
// that compound payload view, scans every payload in the database, and builds
// an automatic index before returning even a one-row lookup.
//
// selectedSQL must return one `id` column. It runs once (MATERIALIZED matters
// because every physical branch consumes it), then each id resolves through
// the branch that owns it (resolveSelectedTimelineSQL).
//
// The caller chooses q, so SyncThreadWindow can keep selection, hydration,
// decoration, and its history stamps in one WAL snapshot.
func queryHydratedTimelineItems(
	q sqlQueryer,
	threadID string,
	selectedSQL string,
	selectedArgs ...any,
) ([]Item, error) {
	branches, branchArgs, err := resolveSelectedTimelineSQL(q, threadID, localItemHydrationColumns, importedItemHydrationColumns, servedItemJoin, "3, 4")
	if err != nil {
		return nil, fmt.Errorf("store: query hydrated timeline items for %s: %w", threadID, err)
	}
	args := append(append([]any{}, selectedArgs...), branchArgs...)
	rows, err := q.Query(`
		WITH selected(id) AS MATERIALIZED (
			`+selectedSQL+`
		)`+branches,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query hydrated timeline items for %s: %w", threadID, err)
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		item, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan hydrated timeline item row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate hydrated timeline items for %s: %w", threadID, err)
	}
	return items, nil
}

func queryOneHydratedTimelineItem(
	q sqlQueryer,
	threadID string,
	selectedSQL string,
	selectedArgs ...any,
) (Item, bool, error) {
	items, err := queryHydratedTimelineItems(q, threadID, selectedSQL, selectedArgs...)
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
