package store

import "fmt"

// itemHydrationColumns is the canonical frontend-bound Item projection with
// caller-supplied expressions for the logical thread id and the three payload
// fields carried alongside a timeline row. Keeping the variable expressions
// here lets local and imported physical branches share one scanner contract
// without routing either branch through the compound timeline_payloads view.
func itemHydrationColumns(threadID, payloadKind, payloadMeta, previewSpans string) string {
	return fmt.Sprintf(`items.id, %s, items.turn_index, items.item_index,
    items.kind, items.role, items.status, items.summary,
    COALESCE(items.payload_id, ''), %s, %s, %s,
    COALESCE(items.input_payload_id, ''),
    items.parent_id, items.is_background, items.completion_of,
    items.tool_name, items.decision, items.meta, items.created_at, items.updated_at`,
		threadID, payloadKind, payloadMeta, previewSpans)
}

var localItemHydrationColumns = itemHydrationColumns(
	"items.thread_id",
	"COALESCE(payloads.kind, '')",
	"COALESCE(payloads.meta, '')",
	"COALESCE(payloads.preview_spans, '')",
)

var importedItemHydrationColumns = itemHydrationColumns(
	"refs.thread_id",
	"COALESCE(local_payloads.kind, imported_payloads.kind, '')",
	"COALESCE(local_payloads.meta, imported_payloads.meta, '')",
	"COALESCE(local_payloads.preview_spans, imported_payloads.preview_spans, '')",
)

// queryHydratedTimelineItems selects logical item ids first, then resolves each
// id through the physical branch that owns it. This shape is deliberately not
// expressed as `timeline_items LEFT JOIN timeline_payloads`: SQLite materializes
// that compound payload view, scans every payload in the database, and builds
// an automatic index before returning even a one-row lookup.
//
// selectedSQL must return one `id` column. It runs once (MATERIALIZED matters
// because both physical branches consume it), then:
//
//   - local rows probe items(thread_id,id) and payloads(thread_id,id);
//   - imported rows probe the thread's chunk refs and the chunk-scoped item /
//     payload primary keys;
//   - a local payload overlay wins over its immutable imported payload, which
//     preserves timeline_payloads' copy-on-write shadowing contract.
//
// The caller chooses q, so SyncThreadWindow can keep selection, hydration,
// decoration, and its history stamps in one WAL snapshot.
func queryHydratedTimelineItems(
	q sqlQueryer,
	threadID string,
	selectedSQL string,
	selectedArgs ...any,
) ([]Item, error) {
	args := append([]any{}, selectedArgs...)
	args = append(args, threadID, threadID)
	rows, err := q.Query(`
		WITH selected(id) AS MATERIALIZED (
			`+selectedSQL+`
		)
		SELECT `+localItemHydrationColumns+`
		  FROM selected
		  CROSS JOIN items AS items
		    ON items.thread_id = ? AND items.id = selected.id
		  LEFT JOIN payloads AS payloads
		    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		UNION ALL
		SELECT `+importedItemHydrationColumns+`
		  FROM selected
		  CROSS JOIN thread_import_chunks AS refs
		  JOIN import_history_items AS items
		    ON items.chunk_id = refs.chunk_id AND items.id = selected.id
		  LEFT JOIN payloads AS local_payloads
		    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
		  LEFT JOIN import_history_payloads AS imported_payloads
		    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
		  LEFT JOIN thread_import_item_overrides AS overrides
		    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
		 WHERE refs.thread_id = ? AND overrides.item_id IS NULL
		 ORDER BY 3, 4`,
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
