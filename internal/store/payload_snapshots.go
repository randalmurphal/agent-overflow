package store

import (
	"database/sql"
	"fmt"
)

// clonePayloadSnapshotsTx shares the exact payload graph visible to this
// transaction. Source writes preserve it through schema-owned copy-on-write;
// source deletion and forking a fork cannot invalidate the reference.
func clonePayloadSnapshotsTx(tx *sql.Tx, source, target string, ids []string) error {
	for start := 0; start < len(ids); start += 256 {
		batch := ids[start:min(start+256, len(ids))]
		clause, tail := inClause("p.id", batch)
		args := append([]any{source}, tail...)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO payload_snapshots(id,payload_id,source_thread_id)
 SELECT lower(hex(randomblob(16))),p.id,p.thread_id FROM payloads p
 WHERE p.thread_id = ? AND `+clause+`
 AND NOT EXISTS (SELECT 1 FROM payload_snapshot_refs r WHERE r.thread_id = p.thread_id AND r.payload_id = p.id)`, args...); err != nil {
			return fmt.Errorf("store: snapshot fork payloads: %w", err)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO payload_snapshots(id,payload_id,chunk_id)
 SELECT lower(hex(randomblob(16))),p.id,p.chunk_id
 FROM thread_import_chunks refs JOIN import_history_payloads p ON p.chunk_id = refs.chunk_id
 WHERE refs.thread_id = ? AND `+clause+`
 AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)`, args...); err != nil {
			return fmt.Errorf("store: snapshot imported fork payloads: %w", err)
		}
		args = append([]any{target, source}, tail...)
		result, err := tx.Exec(`INSERT INTO payloads(thread_id,id,kind,meta,data,created_at,preview_spans,spans)
 SELECT ?,p.id,p.kind,p.meta,x'',p.created_at,p.preview_spans,p.spans
 FROM timeline_payloads p WHERE p.thread_id = ? AND `+clause, args...)
		if err != nil {
			return fmt.Errorf("store: copy fork payload metadata: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: count fork payload metadata: %w", err)
		}
		if count != int64(len(batch)) {
			return fmt.Errorf("store: clone fork payloads: copied %d of %d payloads", count, len(batch))
		}
		if _, err := tx.Exec(`INSERT INTO payload_snapshot_refs(thread_id,payload_id,snapshot_id)
 SELECT ?,p.id,COALESCE(r.snapshot_id,s.id)
 FROM payloads p
 LEFT JOIN payload_snapshot_refs r ON r.thread_id = p.thread_id AND r.payload_id = p.id
 LEFT JOIN payload_snapshots s ON s.source_thread_id = p.thread_id AND s.payload_id = p.id
 WHERE p.thread_id = ? AND `+clause, args...); err != nil {
			return fmt.Errorf("store: attach fork payload snapshots: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO payload_snapshot_refs(thread_id,payload_id,snapshot_id)
 SELECT ?,p.id,s.id
 FROM thread_import_chunks refs JOIN import_history_payloads p ON p.chunk_id = refs.chunk_id
 JOIN payload_snapshots s ON s.chunk_id = p.chunk_id AND s.payload_id = p.id
 WHERE refs.thread_id = ? AND `+clause+`
 AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = refs.thread_id AND local.id = p.id)`, args...); err != nil {
			return fmt.Errorf("store: attach imported fork payload snapshots: %w", err)
		}
	}
	return nil
}

// materializePayloadSnapshotTx detaches a fork's payload before an append or
// edit-snapshot write. A full upsert, which also replaces edit snapshots,
// can simply drop its reference.
func materializePayloadSnapshotTx(tx *sql.Tx, threadID, payloadID string) error {
	var shared bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM payload_snapshot_refs WHERE thread_id = ? AND payload_id = ?)`, threadID, payloadID).Scan(&shared); err != nil {
		return fmt.Errorf("store: inspect payload snapshot: %w", err)
	}
	if !shared {
		return nil
	}
	if _, err := tx.Exec(`UPDATE payloads SET data = (SELECT data FROM resolved_payloads WHERE thread_id = ? AND id = ?) WHERE thread_id = ? AND id = ?`, threadID, payloadID, threadID, payloadID); err != nil {
		return fmt.Errorf("store: detach payload snapshot data: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO payload_chunks(thread_id,payload_id,chunk_index,start_offset,data,created_at)
 SELECT thread_id,payload_id,chunk_index,start_offset,data,created_at FROM timeline_payload_chunks WHERE thread_id = ? AND payload_id = ?`, threadID, payloadID); err != nil {
		return fmt.Errorf("store: detach payload snapshot chunks: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO edit_file_snapshots(thread_id,payload_id,path,content,created_at)
 SELECT thread_id,payload_id,path,content,created_at FROM timeline_edit_file_snapshots WHERE thread_id = ? AND payload_id = ?`, threadID, payloadID); err != nil {
		return fmt.Errorf("store: detach payload snapshot edits: %w", err)
	}
	return dropPayloadSnapshotRefTx(tx, threadID, payloadID)
}

func dropPayloadSnapshotRefTx(exec sqlExecutor, threadID, payloadID string) error {
	_, err := exec.Exec(`DELETE FROM payload_snapshot_refs WHERE thread_id = ? AND payload_id = ?`, threadID, payloadID)
	if err != nil {
		return fmt.Errorf("store: release payload snapshot: %w", err)
	}
	return nil
}
