package store

// Migration v157 makes a worker a paired computer spawned owned by its
// spawner there (docs/specs/standing-agents.md, "Owned workers").
//
// On the computer that holds the worker (D), the spawn's receipt is the
// link: `spawn_link` marks it, at most one per thread
// (idx_thread_request_receipts_spawn_link); `owner_seen_at` is when the
// owner's computer last reached it, which the retention sweep reads to
// release a link whose owner has been silent past the floor
// (idx_thread_request_receipts_owner_seen); `owner_op_seq` is the newest
// owner operation the link admitted. trg_thread_placement_link_gone and
// trg_thread_placement_link_deleted return an owned thread of a project to
// standalone when its link ends, as trg_thread_placement_spawn_gone does
// for a local spawn.
//
// On the computer that holds the spawner (S), a remote spawn's
// `owner_op_seq` numbers the owner operations owed for it, and
// `thread_owner_ops` keeps what S still owes D, one row per link, past the
// spawn row's own delete (no foreign key); its `unanswered_archive_seq` is
// the newest archive sent for the link that no answer has overtaken, which
// a reopen withdraws with a supersede. idx_thread_requests_remote_spawn_target
// finds the remote spawn that names a thread there.
//
// `threads.archive_cascade` marks an archive that has not finished reaching
// what its thread owns.
const remoteOwnershipMigrationVersion = 157

const remoteOwnershipV157SQL = `
ALTER TABLE threads ADD COLUMN archive_cascade INTEGER NOT NULL DEFAULT 0 CHECK(archive_cascade IN (0,1));
CREATE INDEX idx_threads_archive_cascade ON threads(id) WHERE archive_cascade = 1;

ALTER TABLE thread_request_receipts ADD COLUMN spawn_link INTEGER NOT NULL DEFAULT 0 CHECK(spawn_link IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN owner_seen_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_request_receipts ADD COLUMN owner_op_seq INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_thread_request_receipts_spawn_link
    ON thread_request_receipts(target_thread_id)
 WHERE spawn_link = 1 AND target_thread_id IS NOT NULL;
CREATE INDEX idx_thread_request_receipts_owner_seen
    ON thread_request_receipts(owner_seen_at) WHERE spawn_link = 1;

ALTER TABLE thread_requests ADD COLUMN owner_op_seq INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_thread_requests_remote_spawn_target
    ON thread_requests(target_computer_id, target_thread_id)
 WHERE kind = 'spawn' AND target_computer_id <> '';

CREATE TABLE thread_owner_ops (
  token            TEXT PRIMARY KEY,
  computer_id      TEXT NOT NULL,
  source_thread_id TEXT NOT NULL,
  op               TEXT NOT NULL CHECK(op IN ('archive','delete','release','supersede')),
  seq              INTEGER NOT NULL,
  sent             INTEGER NOT NULL DEFAULT 0 CHECK(sent IN (0,1)),
  unanswered_archive_seq INTEGER NOT NULL DEFAULT 0,
  owed_at          INTEGER NOT NULL,
  next_at          INTEGER NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0,
  last_error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_thread_owner_ops_due ON thread_owner_ops(next_at);
CREATE INDEX idx_thread_owner_ops_computer ON thread_owner_ops(computer_id);

CREATE TRIGGER trg_thread_placement_link_gone
AFTER UPDATE OF spawn_link, target_thread_id ON thread_request_receipts
WHEN OLD.spawn_link = 1 AND OLD.target_thread_id IS NOT NULL
 AND (NEW.spawn_link = 0 OR NEW.target_thread_id IS NOT OLD.target_thread_id)
BEGIN
  UPDATE threads SET placement = 'standalone'
   WHERE id = OLD.target_thread_id AND placement = 'owned' AND project_id IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM thread_requests
                      WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0
                        AND target_thread_id = OLD.target_thread_id);
END;

CREATE TRIGGER trg_thread_placement_link_deleted
AFTER DELETE ON thread_request_receipts
WHEN OLD.spawn_link = 1 AND OLD.target_thread_id IS NOT NULL
BEGIN
  UPDATE threads SET placement = 'standalone'
   WHERE id = OLD.target_thread_id AND placement = 'owned' AND project_id IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM thread_requests
                      WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0
                        AND target_thread_id = OLD.target_thread_id);
END;
`
