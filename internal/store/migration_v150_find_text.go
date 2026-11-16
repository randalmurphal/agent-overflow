package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Migration v150 stores each row's thread-find candidate text
// (find_candidate.go) and indexes a thread's rows by revision.
//
// `find_text` is derived data: NULL until the row is filled, then the
// row's candidate text. The column is written only by the triggers below,
// by the writers that supply it inline in the same statement (which skips
// the triggers' second write), and by the deferred backfill. No read
// serves it, so the revision triggers, which list the columns a read
// returns, and the shown-history guards, which list the columns a fork
// reads, leave it out: filling it neither moves a thread's revision nor
// counts as changing history a fork shows.
//
// The insert trigger fills a row inserted without it; the update trigger
// refills a row whose text, meta, kind, tool or payload changed without
// it. A reasoning row's candidate text is its payload's content
// (findReasoningContentSQL); a chunk appended to that payload appends its
// loose form, so a stream costs its chunks, not its length. While the row
// streams, the update trigger leaves the summary tail the stream rewrites
// to the chunks, since the tail is the content's end; a write that settles
// the row's text refills it once. A tool row's candidate text also holds
// the strings of its payload's meta. A payload written whole, or its meta,
// refills the reasoning and tool rows that show it.
//
// `idx_items_thread_rev` serves a find refresh, which re-reads the rows of
// one thread changed since its last answer (SearchThreadItems). Its
// predicate holds every row (rev is NOT NULL), but keeps the planner from
// taking it for a read that only narrows by thread: only a read that
// compares rev implies it. The pending indexes list the rows the backfill
// has not reached; the backfill drops them when it finishes.
const findTextMigrationVersion = 150

var findTextV150SQL = `
ALTER TABLE items ADD COLUMN find_text TEXT;
ALTER TABLE import_history_items ADD COLUMN find_text TEXT;
CREATE INDEX idx_items_thread_rev ON items(thread_id, rev) WHERE rev IS NOT NULL;
` + findTextPendingIndexesSQL + findTextTriggersSQL

// findTextPendingIndexesSQL lists the rows whose candidate text is unset,
// in the backfill's cursor order.
const findTextPendingIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_items_find_text_pending
    ON items(thread_id) WHERE find_text IS NULL;
CREATE INDEX IF NOT EXISTS idx_import_history_items_find_text_pending
    ON import_history_items(chunk_id) WHERE find_text IS NULL;
`

const dropFindTextPendingIndexesSQL = `
DROP INDEX IF EXISTS idx_items_find_text_pending;
DROP INDEX IF EXISTS idx_import_history_items_find_text_pending;
`

// findReasoningContentSQL is the content of the local payload a reasoning
// row of alias names: its blob and its append chunks in order, or NULL.
func findReasoningContentSQL(alias string) string {
	return `(SELECT p.data || COALESCE(
	    (SELECT group_concat(c.data, '' ORDER BY c.chunk_index)
	       FROM payload_chunks c
	      WHERE c.thread_id = p.thread_id AND c.payload_id = p.id), '')
	   FROM payloads p
	  WHERE p.thread_id = ` + alias + `.thread_id AND p.id = ` + alias + `.payload_id)`
}

// findTextSQL is the candidate text of a local row of alias.
func findTextSQL(alias string) string {
	return `find_candidate(` + alias + `.kind, ` + alias + `.summary, ` + alias + `.meta, ` + alias + `.tool_name,
	    CASE WHEN ` + alias + `.payload_id IS NULL THEN NULL
	         WHEN ` + alias + `.kind IN ('thinking', 'compaction_reasoning') THEN ` + findReasoningContentSQL(alias) + `
	         WHEN ` + alias + `.kind IN ('tool_call', 'tool_completion')
	         THEN (SELECT p.meta FROM payloads p WHERE p.thread_id = ` + alias + `.thread_id AND p.id = ` + alias + `.payload_id) END)`
}

// The triggers that keep find_text current. The restore drops them for the
// copy and installs them again (snapshot.go), from this one definition.
var findTextTriggersSQL = `
CREATE TRIGGER trg_items_find_text_insert AFTER INSERT ON items
WHEN NEW.find_text IS NULL
BEGIN
  UPDATE items SET find_text = ` + findTextSQL("NEW") + `
   WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER trg_items_find_text_update
AFTER UPDATE OF kind, summary, meta, tool_name, payload_id ON items
WHEN NEW.find_text IS OLD.find_text
 AND NOT (NEW.kind IN ('thinking', 'compaction_reasoning') AND NEW.payload_id IS NOT NULL
          AND OLD.find_text IS NOT NULL AND OLD.status = 'streaming' AND NEW.status = 'streaming'
          AND NEW.kind IS OLD.kind AND NEW.payload_id IS OLD.payload_id
          AND NEW.meta IS OLD.meta AND NEW.tool_name IS OLD.tool_name)
BEGIN
  UPDATE items SET find_text = ` + findTextSQL("NEW") + `
   WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER trg_payloads_find_text_insert AFTER INSERT ON payloads
BEGIN
  UPDATE items SET find_text = ` + findTextSQL("items") + `
   WHERE thread_id = NEW.thread_id AND payload_id IS NOT NULL AND payload_id = NEW.id
     AND kind IN ('thinking', 'compaction_reasoning', 'tool_call', 'tool_completion');
END;

CREATE TRIGGER trg_payloads_find_text_update AFTER UPDATE OF data, meta ON payloads
BEGIN
  UPDATE items SET find_text = ` + findTextSQL("items") + `
   WHERE thread_id = NEW.thread_id AND payload_id IS NOT NULL AND payload_id = NEW.id
     AND kind IN ('thinking', 'compaction_reasoning', 'tool_call', 'tool_completion');
END;

CREATE TRIGGER trg_payload_chunks_find_text AFTER INSERT ON payload_chunks
BEGIN
  UPDATE items SET find_text = find_text || find_loose(NEW.data)
   WHERE thread_id = NEW.thread_id AND payload_id IS NOT NULL AND payload_id = NEW.payload_id
     AND kind IN ('thinking', 'compaction_reasoning') AND find_text IS NOT NULL;
END;
`

const dropFindTextTriggersSQL = `
DROP TRIGGER IF EXISTS trg_items_find_text_insert;
DROP TRIGGER IF EXISTS trg_items_find_text_update;
DROP TRIGGER IF EXISTS trg_payloads_find_text_insert;
DROP TRIGGER IF EXISTS trg_payloads_find_text_update;
DROP TRIGGER IF EXISTS trg_payload_chunks_find_text;
`

// findTextBackfillBatch is how many rows one backfill transaction fills.
const findTextBackfillBatch = 500

// findTextTable is one table the backfill fills, walked by its pending
// index: (key, rowid) is the cursor.
type findTextTable struct {
	name, key string
	// text is the candidate text of a row aliased `items`.
	text string
}

var findTextTables = []findTextTable{
	{name: "items", key: "thread_id", text: findTextSQL("items")},
	{name: "import_history_items", key: "chunk_id", text: `find_candidate(items.kind, items.summary, items.meta, items.tool_name,
	    CASE WHEN items.payload_id IS NULL THEN NULL
	         WHEN items.kind IN ('thinking', 'compaction_reasoning')
	         THEN (SELECT p.data FROM import_history_payloads p
	                WHERE p.chunk_id = items.chunk_id AND p.id = items.payload_id)
	         WHEN items.kind IN ('tool_call', 'tool_completion')
	         THEN (SELECT p.meta FROM import_history_payloads p
	                WHERE p.chunk_id = items.chunk_id AND p.id = items.payload_id) END)`},
}

// pendingSQL lists the next batch of unfilled rows after the cursor (key,
// rowid), walking the table's pending index.
func (table findTextTable) pendingSQL() string {
	return `SELECT ` + table.key + `, rowid FROM ` + table.name + `
	  WHERE find_text IS NULL AND (` + table.key + `, rowid) > (?, ?)
	  ORDER BY ` + table.key + `, rowid
	  LIMIT ` + strconv.Itoa(findTextBackfillBatch)
}

// backfillFindText is v150's deferred phase: it fills the candidate text
// of every row written before v150, a batch per transaction, in each
// table's pending-index order, then drops the pending indexes. A find
// meanwhile computes an unfilled row's candidate text as it reads it
// (findMatchSQL). A batch that fails is filled a row at a time; a row that
// still fails is reported and left unfilled for the next run, which finds
// it through the pending index again.
func backfillFindText(ctx context.Context, s *Store, run *deferredRun) error {
	// A restore copies rows the backfill has not reached into a schema
	// whose pending indexes it already dropped.
	if _, err := s.db.ExecContext(ctx, findTextPendingIndexesSQL); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("create the pending indexes: %w", err)
	}
	failed := false
	for _, table := range findTextTables {
		ok, err := backfillFindTextTable(ctx, s, run, table)
		if err != nil || ctx.Err() != nil {
			return err
		}
		failed = failed || !ok
	}
	if failed {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, dropFindTextPendingIndexesSQL); err != nil && ctx.Err() == nil {
		return fmt.Errorf("drop the pending indexes: %w", err)
	}
	return nil
}

// backfillFindTextTable fills one table. It reports false when a row failed.
func backfillFindTextTable(ctx context.Context, s *Store, run *deferredRun, table findTextTable) (bool, error) {
	nextSQL := table.pendingSQL()
	fillSQL := `UPDATE ` + table.name + ` AS items SET find_text = ` + table.text + `
	  WHERE items.rowid IN (SELECT value FROM json_each(?)) AND items.find_text IS NULL`
	fillOneSQL := `UPDATE ` + table.name + ` AS items SET find_text = ` + table.text + `
	  WHERE items.rowid = ? AND items.find_text IS NULL`
	afterKey, afterRow := "", int64(-1)
	ok := true
	for batches := 0; ; batches++ {
		if batches > 0 {
			run.pause()
		}
		if ctx.Err() != nil {
			return ok, nil
		}
		rowIDs, lastKey, err := findTextPendingRows(ctx, s, nextSQL, afterKey, afterRow)
		if err != nil {
			if ctx.Err() != nil {
				return ok, nil
			}
			return ok, fmt.Errorf("list the %s rows to fill: %w", table.name, err)
		}
		if len(rowIDs) == 0 {
			return ok, nil
		}
		afterKey, afterRow = lastKey, rowIDs[len(rowIDs)-1]
		list, err := json.Marshal(rowIDs)
		if err != nil {
			return ok, fmt.Errorf("encode the %s batch: %w", table.name, err)
		}
		if _, err := s.db.ExecContext(ctx, fillSQL, string(list)); err == nil {
			continue
		} else if ctx.Err() != nil {
			return ok, nil
		}
		for _, rowID := range rowIDs {
			if _, err := s.db.ExecContext(ctx, fillOneSQL, rowID); err != nil {
				if ctx.Err() != nil {
					return ok, nil
				}
				ok = false
				run.fail(fmt.Errorf("fill the find text of %s row %d: %w", table.name, rowID, err))
			}
		}
	}
}

func findTextPendingRows(ctx context.Context, s *Store, query, afterKey string, afterRow int64) ([]int64, string, error) {
	rows, err := s.db.QueryContext(ctx, query, afterKey, afterRow)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	var key string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	return ids, key, rows.Err()
}
