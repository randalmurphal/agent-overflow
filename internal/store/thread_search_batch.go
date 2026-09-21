package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// indexSettledItemsTx applies the ordinary settle hook in bounded batches.
// Mapping rows retain their insertion order and identity, including on an
// upsert, so ranking ties and per-thread search semantics do not change.
func indexSettledItemsTx(tx *sql.Tx, items []Item, source string) error {
	for start := 0; start < len(items); start += 128 {
		batch := items[start:min(start+128, len(items))]
		args := make([]any, 0, len(batch)*4)
		texts := make(map[[2]string]string, len(batch))
		for _, item := range batch {
			kind, indexed := threadSearchItemKinds[item.Kind]
			if !indexed || !settledItemStatus(item.Status) {
				continue
			}
			args = append(args, item.ThreadID, item.ID, source, kind)
			texts[[2]string{item.ThreadID, item.ID}] = item.Summary
		}
		if len(args) == 0 {
			continue
		}
		rows, err := tx.Query(`INSERT INTO thread_search_rows(thread_id,item_id,source,kind) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(args)/4), ",")+`
 ON CONFLICT(thread_id,item_id,source) DO UPDATE SET kind=excluded.kind
 RETURNING rowid,thread_id,item_id`, args...)
		if err != nil {
			return fmt.Errorf("store: index search batch: %w", err)
		}
		var rowIDs, values []any
		for rows.Next() {
			var rowID int64
			var threadID, itemID string
			if err := rows.Scan(&rowID, &threadID, &itemID); err != nil {
				return errors.Join(fmt.Errorf("store: read search batch row: %w", err), rows.Close())
			}
			rowIDs = append(rowIDs, rowID)
			values = append(values, rowID, texts[[2]string{threadID, itemID}])
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("store: read search batch: %w", err)
		}
		// Even a newly allocated mapping can reuse an old rowid. Clear its
		// previous FTS text, as the single-row hook does, before inserting.
		if _, err := tx.Exec(`DELETE FROM thread_search WHERE rowid IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(rowIDs)), ",")+`)`, rowIDs...); err != nil {
			return fmt.Errorf("store: clear search batch text: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO thread_search(rowid,text) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?,?),", len(rowIDs)), ","), values...); err != nil {
			return fmt.Errorf("store: write search batch text: %w", err)
		}
	}
	return nil
}
