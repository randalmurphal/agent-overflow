package store

// Migration v158 makes a worker a paired computer spawned owned by its
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
// spawn row's own delete (no foreign key). idx_thread_requests_remote_spawn_target
// finds the remote spawn that names a thread there.
//
// S also keeps what it owes D for each request there. `ack_owed` is the
// revision of D's receipt S holds and has not yet told D it holds, 0 when
// none: it is written with the collection that took the revision and
// cleared when D takes the acknowledgement, and `next_check` paces its
// retry (idx_thread_requests_ack_owed). An earlier build kept no such
// debt, so an acknowledgement it sent may have been lost: the migration
// owes one again for each answer S collected from D (its hold recorded)
// that D could still be holding, the hold of the answer or of its late
// reply not yet run out, at the revision S holds. `late_reply_expired`
// marks a request whose late reply D dropped, its hold run out, before S
// collected it: the first answer stays, and `late_reply_at` says when the
// reply arrived there.
//
// S records the pairing each request and remote command was admitted
// under (`pairing`, its generation), so the end of one pairing with D ends
// only what that pairing admitted, and the name the person knew D by when
// it was admitted (`target_computer_name`, `computer_name`), which names
// ended work. A row from before names none; the boot binds it to the
// pairing held then, found by idx_thread_requests_unbound_pairing and
// idx_remote_watches_unbound_pairing, which hold nothing afterwards.
const remoteOwnershipMigrationVersion = 158

const remoteOwnershipV158SQL = `
ALTER TABLE thread_request_receipts ADD COLUMN spawn_link INTEGER NOT NULL DEFAULT 0 CHECK(spawn_link IN (0,1));
ALTER TABLE thread_request_receipts ADD COLUMN owner_seen_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_request_receipts ADD COLUMN owner_op_seq INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_thread_request_receipts_spawn_link
    ON thread_request_receipts(target_thread_id)
 WHERE spawn_link = 1 AND target_thread_id IS NOT NULL;
CREATE INDEX idx_thread_request_receipts_owner_seen
    ON thread_request_receipts(owner_seen_at) WHERE spawn_link = 1;

ALTER TABLE thread_requests ADD COLUMN owner_op_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN ack_owed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thread_requests ADD COLUMN late_reply_expired INTEGER NOT NULL DEFAULT 0 CHECK(late_reply_expired IN (0,1));
ALTER TABLE thread_requests ADD COLUMN pairing TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN target_computer_name TEXT NOT NULL DEFAULT '';
ALTER TABLE remote_watches ADD COLUMN pairing TEXT NOT NULL DEFAULT '';
ALTER TABLE remote_watches ADD COLUMN computer_name TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_thread_requests_unbound_pairing
    ON thread_requests(target_computer_id) WHERE target_computer_id <> '' AND pairing = '';
CREATE INDEX idx_remote_watches_unbound_pairing
    ON remote_watches(computer_id) WHERE pairing = '';
CREATE INDEX idx_thread_requests_ack_owed
    ON thread_requests(next_check) WHERE ack_owed > 0;
UPDATE thread_requests
   SET ack_owed = revision,
       next_check = MIN(next_check, CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER))
 WHERE target_computer_id <> '' AND lineage_only = 0 AND settled_at IS NOT NULL AND revision > 0
   AND expires_at IS NOT NULL
   AND MAX(expires_at, COALESCE(late_reply_at, 0) + 86400000) > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER);
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
  owed_at          INTEGER NOT NULL,
  next_at          INTEGER NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0
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
