package store

import "fmt"

// A snapshot borrows an unchanged physical payload until its first mutation.
// The BEFORE triggers preserve the old graph in that same transaction. Forks
// of forks reference the same snapshot, so reads never walk an alias chain.
var payloadSnapshotsV107SQL = `
CREATE TABLE payload_snapshots (
    id TEXT PRIMARY KEY,
    payload_id TEXT NOT NULL,
    source_thread_id TEXT,
    chunk_id TEXT REFERENCES import_history_chunks(id) ON DELETE RESTRICT,
    data BLOB,
    CHECK ((source_thread_id IS NOT NULL) + (chunk_id IS NOT NULL) + (data IS NOT NULL) = 1)
);
CREATE UNIQUE INDEX idx_payload_snapshots_source
 ON payload_snapshots(source_thread_id, payload_id) WHERE source_thread_id IS NOT NULL;
CREATE UNIQUE INDEX idx_payload_snapshots_import
 ON payload_snapshots(chunk_id, payload_id) WHERE chunk_id IS NOT NULL;
CREATE TABLE payload_snapshot_refs (
    thread_id TEXT NOT NULL,
    payload_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL REFERENCES payload_snapshots(id) ON DELETE RESTRICT,
    PRIMARY KEY(thread_id, payload_id),
    FOREIGN KEY(thread_id, payload_id) REFERENCES payloads(thread_id,id) ON DELETE CASCADE
);
CREATE INDEX idx_payload_snapshot_refs_snapshot ON payload_snapshot_refs(snapshot_id);
CREATE TABLE payload_snapshot_chunks (
    snapshot_id TEXT NOT NULL REFERENCES payload_snapshots(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    start_offset INTEGER NOT NULL,
    data BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(snapshot_id,chunk_index)
);
CREATE TABLE payload_snapshot_edits (
    snapshot_id TEXT NOT NULL REFERENCES payload_snapshots(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    content BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(snapshot_id,path)
);
` + payloadSnapshotGCTriggersSQL + `
DROP TRIGGER trg_thread_import_chunks_gc;
CREATE TRIGGER trg_thread_import_chunks_gc AFTER DELETE ON thread_import_chunks BEGIN
    DELETE FROM import_history_chunks WHERE id = OLD.chunk_id
      AND NOT EXISTS (SELECT 1 FROM thread_import_chunks WHERE chunk_id = OLD.chunk_id)
      AND NOT EXISTS (SELECT 1 FROM payload_snapshots WHERE chunk_id = OLD.chunk_id);
END;

-- A single-table outer SELECT can flatten under LEFT JOIN. Resolving the
-- shared bytes in a correlated lookup avoids materializing all payloads.
-- Keep length(column) beside the physical blob: length(resolved data) would
-- evaluate the whole blob expression just to answer a preview's byte count.
CREATE VIEW resolved_payloads AS
SELECT p.thread_id, p.id, p.kind, p.meta,
       COALESCE((
         SELECT COALESCE(s.data, original.data, imported.data)
           FROM payload_snapshot_refs r
           JOIN payload_snapshots s ON s.id = r.snapshot_id
           LEFT JOIN payloads original ON original.thread_id = s.source_thread_id AND original.id = s.payload_id
           LEFT JOIN import_history_payloads imported ON imported.chunk_id = s.chunk_id AND imported.id = s.payload_id
          WHERE r.thread_id = p.thread_id AND r.payload_id = p.id
       ), p.data) AS data,
       COALESCE((
         SELECT COALESCE(length(s.data), length(original.data), length(imported.data))
           FROM payload_snapshot_refs r
           JOIN payload_snapshots s ON s.id = r.snapshot_id
           LEFT JOIN payloads original ON original.thread_id = s.source_thread_id AND original.id = s.payload_id
           LEFT JOIN import_history_payloads imported ON imported.chunk_id = s.chunk_id AND imported.id = s.payload_id
          WHERE r.thread_id = p.thread_id AND r.payload_id = p.id
       ), length(p.data)) AS data_length,
       p.created_at, p.preview_spans, p.spans
 FROM payloads p;

DROP VIEW timeline_payloads;
CREATE VIEW timeline_payloads AS
SELECT thread_id, id, kind, meta, data, created_at, preview_spans, spans, data_length FROM resolved_payloads
UNION ALL
SELECT refs.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans, length(p.data)
 FROM thread_import_chunks refs JOIN import_history_payloads p ON p.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id);

CREATE VIEW timeline_payload_chunks AS
SELECT thread_id, payload_id, chunk_index, start_offset, data, created_at, length(data) AS data_length FROM payload_chunks
UNION ALL
SELECT r.thread_id, r.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at, length(c.data)
 FROM payload_snapshot_refs r JOIN payload_snapshots s ON s.id = r.snapshot_id
 JOIN payload_chunks c ON c.thread_id = s.source_thread_id AND c.payload_id = s.payload_id
UNION ALL
SELECT r.thread_id, r.payload_id, c.chunk_index, c.start_offset, c.data, c.created_at, length(c.data)
 FROM payload_snapshot_refs r JOIN payload_snapshot_chunks c ON c.snapshot_id = r.snapshot_id;

CREATE VIEW timeline_edit_file_snapshots AS
SELECT thread_id, payload_id, path, content, created_at FROM edit_file_snapshots
UNION ALL
SELECT r.thread_id, r.payload_id, e.path, e.content, e.created_at
 FROM payload_snapshot_refs r JOIN payload_snapshots s ON s.id = r.snapshot_id
 JOIN edit_file_snapshots e ON e.thread_id = s.source_thread_id AND e.payload_id = s.payload_id
UNION ALL
SELECT r.thread_id, r.payload_id, e.path, e.content, e.created_at
 FROM payload_snapshot_refs r JOIN payload_snapshot_edits e ON e.snapshot_id = r.snapshot_id;
` + payloadSnapshotTriggersSQL()

const payloadSnapshotGCTriggersSQL = `CREATE TRIGGER trg_payload_snapshot_refs_gc AFTER DELETE ON payload_snapshot_refs BEGIN
    DELETE FROM payload_snapshots WHERE id = OLD.snapshot_id
      AND NOT EXISTS (SELECT 1 FROM payload_snapshot_refs WHERE snapshot_id = OLD.snapshot_id);
END;
CREATE TRIGGER trg_payload_snapshots_import_gc AFTER DELETE ON payload_snapshots
WHEN OLD.chunk_id IS NOT NULL BEGIN
    DELETE FROM import_history_chunks WHERE id = OLD.chunk_id
      AND NOT EXISTS (SELECT 1 FROM thread_import_chunks WHERE chunk_id = OLD.chunk_id)
      AND NOT EXISTS (SELECT 1 FROM payload_snapshots WHERE chunk_id = OLD.chunk_id);
END;
`

func payloadSnapshotTriggersSQL() string {
	var sql string
	for _, table := range []string{"payloads", "payload_chunks", "edit_file_snapshots"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			if table == "payloads" && event == "INSERT" {
				continue
			}
			ref, id := "OLD", "payload_id"
			if event == "INSERT" {
				ref = "NEW"
			}
			condition := ""
			if table == "payloads" {
				id = "id"
				if event == "UPDATE" {
					condition = " AND OLD.data IS NOT NEW.data"
				}
			}
			thread, payload := ref+".thread_id", ref+"."+id
			sql += fmt.Sprintf(`
CREATE TRIGGER trg_snapshot_%s_%s BEFORE %s ON %s
WHEN EXISTS (SELECT 1 FROM payload_snapshots WHERE source_thread_id = %s AND payload_id = %s)%s
BEGIN
 INSERT INTO payload_snapshot_chunks(snapshot_id,chunk_index,start_offset,data,created_at)
 SELECT s.id,c.chunk_index,c.start_offset,c.data,c.created_at
 FROM payload_snapshots s JOIN payload_chunks c ON c.thread_id = s.source_thread_id AND c.payload_id = s.payload_id
 WHERE s.source_thread_id = %s AND s.payload_id = %s;
 INSERT INTO payload_snapshot_edits(snapshot_id,path,content,created_at)
 SELECT s.id,e.path,e.content,e.created_at
 FROM payload_snapshots s JOIN edit_file_snapshots e ON e.thread_id = s.source_thread_id AND e.payload_id = s.payload_id
 WHERE s.source_thread_id = %s AND s.payload_id = %s;
 UPDATE payload_snapshots SET data = (SELECT data FROM payloads WHERE thread_id = %s AND id = %s), source_thread_id = NULL
 WHERE source_thread_id = %s AND payload_id = %s;
END;
`, table, event, event, table, thread, payload, condition, thread, payload, thread, payload, thread, payload, thread, payload)
		}
	}
	return sql
}

func dropPayloadSnapshotTriggersSQL() string {
	sql := `DROP TRIGGER trg_payload_snapshot_refs_gc; DROP TRIGGER trg_payload_snapshots_import_gc;`
	for _, table := range []string{"payloads", "payload_chunks", "edit_file_snapshots"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			if table == "payloads" && event == "INSERT" {
				continue
			}
			sql += fmt.Sprintf("DROP TRIGGER trg_snapshot_%s_%s;", table, event)
		}
	}
	return sql
}
