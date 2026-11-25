package store

// Migration v166 adds to thread_tickets what a thread's link shows of its
// ticket besides the key and title, and whose copy it is
// (docs/architecture/tracker-backend.md#thread-links):
//
//   - status_id, status_name and status_group: the ticket's status as last
//     seen, its group in the mirror's numbering (1 to 5), 0 while unknown.
//   - seen_root: the mirror root (tracker_cache_roots.id) whose card the
//     key, title and status equal, 0 for none. A plain integer, since no
//     table outside the mirror references it: a root's ID is never reused,
//     so a link copied under a root that was detached or emptied names no
//     live root again.
//   - missing: the last read of the issue under seen_root did not return
//     it.
//
// A link stored before v166 has no status and names no root, which reads
// as not checked on this computer until a read stores the card.
const threadTicketSeenV166SQL = `
ALTER TABLE thread_tickets ADD COLUMN status_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_tickets ADD COLUMN status_name TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_tickets ADD COLUMN status_group INTEGER NOT NULL DEFAULT 0 CHECK(status_group BETWEEN 0 AND 5);
ALTER TABLE thread_tickets ADD COLUMN seen_root INTEGER NOT NULL DEFAULT 0 CHECK(seen_root >= 0);
ALTER TABLE thread_tickets ADD COLUMN missing INTEGER NOT NULL DEFAULT 0 CHECK(missing IN (0, 1));
`

const threadTicketSeenMigrationVersion = 166
