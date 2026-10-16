package store

// Migration v155 keeps the request ledger, the wake rows and the remote job
// watches cheap to read and to sweep as they grow.
//
// thread_requests. The Agents segment's read (threadAgentRequestsSQL) asks
// each thread on a spawn walk for its open requests, its requests settled
// in the last moments, its local spawns and its nested remote spawns.
// idx_thread_requests_caller orders a caller's whole history by creation,
// so each of those probes read every row the caller ever made. Each now
// has an index of its own: idx_thread_requests_caller_settled serves the
// open probe (settled_at IS NULL) and the recently settled range,
// idx_thread_requests_spawn_caller the walk's step and the spawns kept for
// open work under them, idx_thread_requests_nested_caller the remote spawns
// whose computer reported open work. All three leave out lineage-only rows,
// which no ledger read lists.
//
// The retention sweep measured every row's age against the floor.
// idx_thread_requests_age and idx_thread_request_receipts_age index that
// age for the rows the floor can remove, so the sweep reads the oldest one
// and writes nothing when none is past it; idx_thread_requests_lineage
// lists the lineage rows, which the sweep deletes once their spawned thread
// is gone.
//
// agent_wakes. idx_agent_wakes_thread serves every per-thread read: the live
// count and label lookup of a registration, the thread-wide cancel, the
// listing and the thread's own cascade. A cancelled row is kept only so its
// label revives with its id, so it keeps nothing a wake carried
// (last_output, last_stderr), and a thread keeps its
// MaxCancelledAgentWakesPerThread newest cancelled rows.
//
// remote_watches. A watch belongs to the thread that receives its
// completion, and nothing deleted it with that thread: a deleted thread's
// pending watch stayed in the watcher's due poll. The table is rebuilt
// with a foreign key that cascades from the thread, a watch whose thread
// is already gone is deleted, and idx_remote_watches_done serves the
// sweep that deletes finished watches past the retention floor.
const ledgerIndexesMigrationVersion = 155

const ledgerIndexesV155SQL = `
CREATE INDEX idx_thread_requests_caller_settled
    ON thread_requests(caller_thread_id, settled_at)
 WHERE lineage_only = 0;
CREATE INDEX idx_thread_requests_spawn_caller
    ON thread_requests(caller_thread_id, target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND lineage_only = 0;
CREATE INDEX idx_thread_requests_nested_caller
    ON thread_requests(caller_thread_id)
 WHERE nested_open = 1 AND kind = 'spawn' AND target_computer_id <> '' AND lineage_only = 0;
CREATE INDEX idx_thread_requests_age
    ON thread_requests(COALESCE(settled_at, due_at, created_at))
 WHERE lineage_only = 0 AND state NOT IN ('unconfirmed','accepted','running');
CREATE INDEX idx_thread_requests_lineage
    ON thread_requests(target_thread_id)
 WHERE lineage_only = 1;
CREATE INDEX idx_thread_request_receipts_age
    ON thread_request_receipts(COALESCE(settled_at, created_at))
 WHERE state NOT IN ('accepted','running');

CREATE INDEX idx_agent_wakes_thread ON agent_wakes(thread_id, state);
UPDATE agent_wakes SET last_output = '', last_stderr = ''
 WHERE state = 'cancelled' AND (last_output <> '' OR last_stderr <> '');
DELETE FROM agent_wakes
 WHERE state = 'cancelled'
   AND id NOT IN (SELECT id FROM (
         SELECT id, row_number() OVER (PARTITION BY thread_id ORDER BY created_at DESC, id DESC) AS newest
           FROM agent_wakes WHERE state = 'cancelled')
        WHERE newest <= 16);

CREATE TABLE remote_watches_new (
 computer_id TEXT NOT NULL, request_id TEXT NOT NULL,
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL, label TEXT NOT NULL, receipt TEXT NOT NULL DEFAULT '{}',
 error TEXT NOT NULL DEFAULT '', notification TEXT NOT NULL DEFAULT 'pending'
 CHECK(notification IN ('pending','queued','dismissed')),
 next_check INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, command TEXT NOT NULL DEFAULT '',
 confirmed_at INTEGER, wake_attempts INTEGER NOT NULL DEFAULT 0, wake_issue TEXT NOT NULL DEFAULT '',
 launched_in_thread_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(computer_id,request_id)
);
INSERT INTO remote_watches_new (computer_id, request_id, thread_id, fingerprint, label, receipt, error,
    notification, next_check, created_at, command, confirmed_at, wake_attempts, wake_issue, launched_in_thread_id)
SELECT computer_id, request_id, thread_id, fingerprint, label, receipt, error,
    notification, next_check, created_at, command, confirmed_at, wake_attempts, wake_issue, launched_in_thread_id
  FROM remote_watches WHERE thread_id IN (SELECT id FROM threads);
DROP TABLE remote_watches;
ALTER TABLE remote_watches_new RENAME TO remote_watches;
CREATE INDEX idx_remote_watches_pending ON remote_watches(next_check) WHERE notification='pending';
CREATE INDEX idx_remote_watches_thread ON remote_watches(thread_id,created_at DESC);
CREATE INDEX idx_remote_watches_done ON remote_watches(created_at)
 WHERE notification = 'dismissed' OR (notification = 'queued' AND confirmed_at IS NOT NULL);
`
