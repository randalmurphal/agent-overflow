package store

// threadRequestPollingV104SQL retires a settled request from the poller and
// gives a cross-computer request the two things a restart would otherwise
// lose: the name of the thread that answered, and how many times its wake
// has been attempted.
//
// `polling` is the poller's own gate. A settled row is visited for one thing
// only, a late `thread_reply`, and the destination holds that for a day past
// the answer (`expires_at`). Once the window passes or the late reply lands
// there is nothing left to ask, and the row leaves the due index for good
// rather than costing a call every few seconds until the retention floor.
// It is a column rather than a far-future `next_check` so the partial index
// drops the row instead of carrying it at the head of the scan.
//
// `target_thread_title` is the answering thread's name as its own computer
// reported it. A thread on another computer has no row here, and a wake
// rendered after a restart named nothing at all.
//
// `wake_attempts`, `wake_next_check` and `wake_issue` bound the undelivered
// wake retry. A caller that cannot take a wake (its thread lock is never
// free, its execution access is revoked) was retried every thirty seconds
// forever; now the retry backs off and ends with the reason recorded.
const threadRequestPollingV104SQL = `
ALTER TABLE thread_requests ADD COLUMN polling INTEGER NOT NULL DEFAULT 1
    CHECK(polling IN (0,1));
ALTER TABLE thread_requests ADD COLUMN target_thread_title TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN wake_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN wake_next_check INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN wake_issue TEXT NOT NULL DEFAULT '';

-- The poller's due-row scan, now gated on "polling". 'finished' stays in the
-- predicate because a late thread_reply is a new revision on a request that
-- already answered; "polling = 0" is what ends that watch.
DROP INDEX IF EXISTS idx_thread_requests_poll;
CREATE INDEX idx_thread_requests_poll
    ON thread_requests(next_check)
 WHERE polling = 1
   AND target_computer_id <> ''
   AND state IN ('unconfirmed','accepted','running','finished');

-- The undelivered-wake retry, and the sweep's read of when it is next due.
-- Partial on "notify", which is armed only while a wake is owed, so the scan
-- is over the rows that can be in the set rather than the whole ledger.
CREATE INDEX idx_thread_requests_wake
    ON thread_requests(wake_next_check)
 WHERE notify = 1;

-- Rows this store settled before the retirement rule existed: one that has
-- its late reply, or whose answer hold has run out, has nothing left to
-- report. The hold falls back to a day past the settlement for a row whose
-- destination reported no deadline, which is what the poller assumes too.
UPDATE thread_requests SET polling = 0
 WHERE target_computer_id <> ''
   AND settled_at IS NOT NULL
   AND (late_reply IS NOT NULL
     OR COALESCE(expires_at, settled_at + 86400000) <= CAST(strftime('%s','now') AS INTEGER) * 1000);
`
