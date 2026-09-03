package store

// flushQueueV85SQL makes the per-thread flush queue survive the process that
// holds it.
//
// The queue is a message the user has already sent — the composer cleared
// their draft the moment they pressed Send — waiting out an active turn
// before it reaches the provider. It lived only in `triage`'s per-thread
// state, so a backend crash or an ungraceful restart lost it with no trace
// anywhere: no timeline row, no draft, nothing on screen afterwards to say a
// message had ever existed. This row is that trace.
//
// Not cache content, and the one thing that separates it from the rest of
// this database's history-cache posture: nothing can recompute a message
// nobody kept a copy of. It is dropped only when the message reaches a
// durable endpoint (the dispatcher's persisted `user_text` row, or a
// session-death restore into the composer draft), and every row still here
// at boot is restored into its thread's draft and then deleted.
//
// Three decisions worth stating:
//
//   - `id` is the PRIMARY KEY on its own, not `(thread_id, id)`. Queue ids
//     are minted as `queue:<uuid>` (`internal/flushqueue`), so they are
//     already unique across threads, and the app deletes one by id from a
//     dispatcher that knows the item and not always the thread.
//   - `send_id` is the client-minted per-send idempotency id, and it is
//     deliberately NOT unique: an empty string is legal (every app-internal
//     injector, and any client bundle older than the field), and a UNIQUE
//     index over a column whose common value is the empty string would refuse
//     a thread's second injected message. Uniqueness is a lookup the app
//     does in Go over a bounded set, not a constraint.
//   - The FK cascades from `threads`, so deleting a thread takes its queued
//     messages with it and no sweep has to remember this table.
//
// `enqueued_at` is Unix milliseconds, matching every other table, and it is
// the queue's order — with `rowid` as the tiebreak, because two messages
// queued in the same millisecond still have a first one.
const flushQueueV85SQL = `
CREATE TABLE flush_queue_items (
    id          TEXT PRIMARY KEY,
    thread_id   TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    send_id     TEXT NOT NULL DEFAULT '',
    message     TEXT NOT NULL,
    payload     BLOB,
    enqueued_at INTEGER NOT NULL
);

CREATE INDEX idx_flush_queue_items_thread
    ON flush_queue_items(thread_id, enqueued_at);
`
