package store

import (
	"errors"
	"fmt"
	"strings"
)

// Served keys.

// storedServedKeysSQL finds rows a read serves the subagent stamps to,
// anchors and borrowing completions, whose stored meta holds a served key
// (subagentServedKeys), in either arm. A Codex spawn's completion stores
// its card as a snapshot and is not one of them.
var storedServedKeysSQL = func() string {
	holds := func(a string) string {
		terms := make([]string, 0, len(subagentServedKeys))
		for _, served := range subagentServedKeys {
			terms = append(terms, "json_type("+a+"meta, '$."+served.key+"') IS NOT NULL")
		}
		return "(" + aggAnchorableSQL(a) + " OR " + aggBorrowsCardSQL(a) + ") AND json_valid(" + a + "meta) AND (" +
			strings.Join(terms, " OR ") + ")"
	}
	return `SELECT items.thread_id || '/' || items.id FROM items WHERE ` + holds("items.") + `
UNION ALL
SELECT refs.thread_id || '/' || imported.id FROM import_history_items imported
  JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
 WHERE ` + holds("imported.") + `
LIMIT ?`
}()

// GetThreadItemForWrite is GetThreadItem for a caller that writes the
// row back: its meta is the stored meta, without the keys a read serves
// from the subagent stamps.
func (s *Store) GetThreadItemForWrite(threadID, id string) (Item, bool, error) {
	type result struct {
		item  Item
		found bool
	}
	got, err := readSnapshot(s.reader(), "item for write", func(q sqlQueryer) (result, error) {
		item, found, err := s.getThreadItem(q, threadID, id)
		if err != nil || !found || item.Rev < 0 {
			// An imported row serves its stored meta.
			return result{item, found}, err
		}
		if err := q.QueryRow(`SELECT meta FROM items WHERE thread_id = ? AND id = ?`, threadID, id).Scan(&item.Meta); err != nil {
			return result{}, fmt.Errorf("store: read stored meta of %s/%s: %w", threadID, id, err)
		}
		return result{item, true}, nil
	})
	return got.item, got.found, err
}

// RowsStoringServedSubagentKeys lists up to limit rows, as thread/id,
// whose stored meta holds a key a read serves from the subagent stamps.
// There must be none: a writer that stores a meta it read back would
// freeze a card in the row. storetest asserts it after every test.
func (s *Store) RowsStoringServedSubagentKeys(limit int) ([]string, error) {
	rows, err := s.reader().Query(storedServedKeysSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("store: find stored served subagent keys: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan stored served subagent key row: %w", err), rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate stored served subagent key rows: %w", err)
	}
	return ids, nil
}
