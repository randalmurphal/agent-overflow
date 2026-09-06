package store

// Transfer coordination is authoritative: restoring a history cache must never
// make a retired source runnable again. No foreign key cascades from threads;
// the ownership tombstone outlives deletion and retention of display history.
const threadTransfersV86SQL = `
CREATE TABLE thread_transfers (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL,
    target_thread_id TEXT NOT NULL,
    peer_backend_id TEXT NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('move', 'copy')),
    direction TEXT NOT NULL CHECK (direction IN ('incoming', 'outgoing')),
    phase TEXT NOT NULL CHECK (phase IN ('preparing', 'prepared', 'committed', 'complete', 'canceled')),
    manifest_hash TEXT NOT NULL DEFAULT '',
    archive_size INTEGER NOT NULL DEFAULT 0 CHECK(archive_size >= 0),
    activation_hash TEXT NOT NULL,
    private_state BLOB NOT NULL,
    peer_state BLOB,
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0,1)),
    ownership_epoch INTEGER NOT NULL DEFAULT 0 CHECK(ownership_epoch >= 0),
    error TEXT NOT NULL DEFAULT '',
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK(retry_count >= 0),
    cleanup_pending INTEGER NOT NULL DEFAULT 1 CHECK(cleanup_pending IN (0,1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_thread_transfers_pending
    ON thread_transfers(thread_id)
    WHERE phase NOT IN ('complete', 'canceled') AND (direction = 'incoming' OR kind = 'move' OR archive_size = 0);
CREATE INDEX idx_thread_transfers_owner
    ON thread_transfers(thread_id);
CREATE INDEX idx_thread_transfers_retry ON thread_transfers(next_attempt_at, created_at, id)
    WHERE phase NOT IN ('complete', 'canceled') OR cleanup_pending = 1;
CREATE INDEX idx_thread_transfers_project ON thread_transfers(project_id)
    WHERE direction = 'incoming' AND phase NOT IN ('complete', 'canceled');

CREATE TABLE thread_transfer_sessions (
    transfer_id TEXT NOT NULL REFERENCES thread_transfers(id),
    provider TEXT NOT NULL CHECK(provider IN ('claude','codex')),
    session_ref TEXT NOT NULL,
    PRIMARY KEY(transfer_id, provider, session_ref)
);
CREATE INDEX idx_thread_transfer_sessions_ref ON thread_transfer_sessions(provider, session_ref);

CREATE VIEW owned_threads AS
SELECT threads.*, COALESCE((SELECT MAX(ownership_epoch) FROM thread_transfers
    WHERE thread_id = threads.id AND direction = 'incoming' AND phase = 'complete'),0) AS ownership_epoch
FROM threads
WHERE COALESCE((
    SELECT CASE
        WHEN direction = 'incoming' THEN phase = 'complete'
        WHEN kind = 'copy' THEN 1
        WHEN phase IN ('committed', 'complete') THEN 0
        ELSE 1 END
    FROM thread_transfers WHERE thread_id = threads.id AND phase <> 'canceled'
    ORDER BY rowid DESC LIMIT 1
), 1) = 1;
`
