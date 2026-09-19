package store

// threadRequestsV102SQL adds the two sides of the agent thread-tools request
// ledger (docs/specs/agent-thread-tools.md, "Request identity and durability").
//
// `thread_requests` is what this computer's threads asked for; a row exists
// before any dispatch, so a crash can never leave work running with no record
// of who asked for it. `thread_request_receipts` is what this computer's
// threads were asked, including by their own computer: the token is the
// idempotency key on both sides, so the code that detects settlement and the
// code that wakes the caller are each written once and run identically for a
// local and a remote peer.
//
// Answers are BLOB and never truncated: a wake carries a preview, and
// `thread_status` pages the whole thing out of these rows, which stay
// readable after the thread that wrote the answer is deleted.
//
// Both tables cascade from their thread: requests belong to the caller,
// receipts to the thread that answers. Both are in the RestoreFrom keep-local
// set (snapshot.go), so a history restore can neither revive a settled request
// nor make a retry run twice.
const threadRequestsV102SQL = `
CREATE TABLE thread_requests (
    token              TEXT    PRIMARY KEY,
    caller_thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    kind               TEXT    NOT NULL CHECK(kind IN ('spawn','send','ask','remind')),
    due_at             INTEGER,
    target_computer_id TEXT    NOT NULL DEFAULT '',
    target_thread_id   TEXT    NOT NULL DEFAULT '',
    origin_thread_id   TEXT    NOT NULL DEFAULT '',
    notify             INTEGER NOT NULL DEFAULT 0 CHECK(notify IN (0,1)),
    state              TEXT    NOT NULL CHECK(state IN (
        'unconfirmed','accepted','running','replied','finished','errored',
        'cancelled','interrupted','expired','refused')),
    answer             BLOB,
    answer_kind        TEXT    NOT NULL DEFAULT ''
        CHECK(answer_kind IN ('','reply','final','error','note')),
    late_reply         BLOB,
    late_reply_at      INTEGER,
    revision           INTEGER NOT NULL DEFAULT 0,
    settled_at         INTEGER,
    delivered_at       INTEGER,
    delivered_how      TEXT    NOT NULL DEFAULT ''
        CHECK(delivered_how IN ('','inline','queued','draft')),
    late_delivered_at  INTEGER,
    expires_at         INTEGER,
    next_check         INTEGER NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    issue              TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX idx_thread_requests_caller
    ON thread_requests(caller_thread_id, created_at DESC);

-- The poller's due-row scan. 'finished' stays polled until the source has
-- acknowledged the settlement, because a late thread_reply is a new revision
-- on a request that already answered.
CREATE INDEX idx_thread_requests_poll
    ON thread_requests(next_check)
 WHERE target_computer_id <> ''
   AND state IN ('unconfirmed','accepted','running','finished');

CREATE INDEX idx_thread_requests_due
    ON thread_requests(due_at)
 WHERE kind = 'remind' AND state = 'accepted';

CREATE TABLE thread_request_receipts (
    token                TEXT    PRIMARY KEY,
    owner_device_id      TEXT    NOT NULL,
    source_computer_id   TEXT    NOT NULL DEFAULT '',
    source_computer_name TEXT    NOT NULL DEFAULT '',
    source_thread_id     TEXT    NOT NULL DEFAULT '',
    source_thread_title  TEXT    NOT NULL DEFAULT '',
    kind                 TEXT    NOT NULL CHECK(kind IN ('spawn','send','ask','remind')),
    -- NULL until the destination has a thread to name: a spawn's new thread
    -- and an ask's scratch fork are created AFTER the receipt, which is what
    -- makes a retried call find the acceptance instead of forking twice.
    target_thread_id     TEXT    REFERENCES threads(id) ON DELETE CASCADE,
    message_item_id      TEXT    NOT NULL DEFAULT '',
    turn_id              TEXT    NOT NULL DEFAULT '',
    state                TEXT    NOT NULL CHECK(state IN (
        'accepted','running','replied','finished','errored','cancelled',
        'interrupted','expired')),
    answer               BLOB,
    answer_kind          TEXT    NOT NULL DEFAULT ''
        CHECK(answer_kind IN ('','reply','final','error','note')),
    late_reply           BLOB,
    late_reply_at        INTEGER,
    revision             INTEGER NOT NULL DEFAULT 0,
    settled_at           INTEGER,
    collected_revision   INTEGER NOT NULL DEFAULT 0,
    collected_at         INTEGER,
    expires_at           INTEGER,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

-- The settlement observer and the responder-enable rule both ask "what is
-- open against this thread". A receipt whose target is still undecided is in
-- the index as NULL and matches no thread, which is what it should do.
CREATE INDEX idx_thread_request_receipts_target
    ON thread_request_receipts(target_thread_id);

-- The expiry sweep: an answer nobody collected is dropped a day after it
-- settled, and the row stays until the retention floor so the token keeps
-- answering.
CREATE INDEX idx_thread_request_receipts_expiry
    ON thread_request_receipts(expires_at);
`
