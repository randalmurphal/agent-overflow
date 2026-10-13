package store

// Migration v148 makes the delivery of a request's answer and a remote job's
// completion two-phase (docs/specs/standing-agents.md, "Wake supervisor").
// The queue write marks the owner delivered, as before; `confirmed_at`
// (`late_confirmed_at` for a late reply) records the provider's echo of
// the message. An owner marked delivered and not confirmed has a message
// the agent may never have taken in, and the boot sweep gives it back.
//
// `wake_attempts` and `wake_issue` pace and bound the redelivery of a
// completion that a session end gave back to its watch, on the ladder the
// thread request wake retry uses, and record why it was given up.
//
// Every row delivered before this migration is recorded as confirmed: the
// rows still queued then are given back by their queue rows, and the rest
// reached the agent under the old single mark.
const deliveryConfirmationsMigrationVersion = 148

const deliveryConfirmationsV148SQL = `
ALTER TABLE thread_requests ADD COLUMN confirmed_at INTEGER;
ALTER TABLE thread_requests ADD COLUMN late_confirmed_at INTEGER;
ALTER TABLE remote_watches ADD COLUMN confirmed_at INTEGER;
ALTER TABLE remote_watches ADD COLUMN wake_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE remote_watches ADD COLUMN wake_issue TEXT NOT NULL DEFAULT '';
UPDATE thread_requests SET confirmed_at = delivered_at WHERE delivered_at IS NOT NULL;
UPDATE thread_requests SET late_confirmed_at = late_delivered_at WHERE late_delivered_at IS NOT NULL;
UPDATE remote_watches SET confirmed_at = created_at WHERE notification = 'queued';
`
