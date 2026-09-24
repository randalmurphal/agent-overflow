package store

// pendingThreadDeletesV122SQL records a thread whose delete has begun.
//
// DeleteThreadPaced sets threads.deleting in its first transaction, before
// it drains any item chunk, and removes the row in its last. owned_threads,
// the view every listing, search, catalog, transfer and fork admission reads
// threads through, leaves a marked thread out, so a delete that a crash or
// an error stopped partway leaves a thread nothing can list, open or fork.
// The partial index finds those threads at boot, where the app completes
// their deletes (ListPendingThreadDeletes).
//
// The view is v101's with the deleting term added.
const pendingThreadDeletesV122SQL = `
ALTER TABLE threads ADD COLUMN deleting INTEGER NOT NULL DEFAULT 0 CHECK(deleting IN (0,1));

CREATE INDEX idx_threads_deleting ON threads(id) WHERE deleting = 1;

DROP VIEW owned_threads;

CREATE VIEW owned_threads AS
SELECT threads.*, COALESCE((SELECT MAX(ownership_epoch) FROM thread_transfers
    WHERE thread_id = threads.id AND direction = 'incoming' AND phase = 'complete'),0) AS ownership_epoch
FROM threads
WHERE threads.deleting = 0 AND COALESCE((
    SELECT CASE
        WHEN direction = 'incoming' THEN phase = 'complete'
        WHEN kind = 'copy' THEN 1
        WHEN phase IN ('committed', 'complete') THEN 0
        ELSE 1 END
    FROM thread_transfers WHERE thread_id = threads.id AND phase <> 'canceled'
    ORDER BY rowid DESC LIMIT 1
), 1) = 1;
`
