package store

// Migration v155 keeps what a rest or an archive did to a remote command it
// stopped. It cancels the commands the thread started on paired computers and
// dismisses their completions; when the destination cannot be reached the
// cancel is still owed, and the watcher sends it again until the destination
// answers or ends the command itself.
//
// `cancel_owed_at` is when the cancel was first owed, 0 when none is;
// `cancel_next_at` is when the watcher sends it next. The partial index serves
// the watcher's due list. `stopped_at` is when a rest or an archive dismissed
// the completion, 0 when none did: such a dismissal is final, where the
// dismissal of a refused start gives way to a later proof the destination
// accepted it.
const remoteCancelOwedMigrationVersion = 155

const remoteCancelOwedV155SQL = `
ALTER TABLE remote_watches ADD COLUMN cancel_owed_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE remote_watches ADD COLUMN cancel_next_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE remote_watches ADD COLUMN stopped_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_remote_watches_cancel_owed
    ON remote_watches(cancel_next_at)
 WHERE cancel_owed_at <> 0;
`
