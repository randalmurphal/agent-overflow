package store

// Migration v167 is what a thread's link shows of its ticket
// (docs/architecture/tracker-backend.md#thread-links):
//
//   - thread_tickets.status_id, status_name and status_group: the link's
//     last-seen status beside its key and title, its group in the mirror's
//     numbering (1 to 5), 0 while unknown. The link shows its copy only
//     while the site's link reader's mirror holds no card of the issue.
//   - tracker_cache_issues.title: the card's title, which the card blob no
//     longer carries, read by the link projection.
//   - tracker_cache_issue_missing: a read of a root's tracker that did not
//     return the issue, at read_at, while no card of that root is from a
//     later read (the writers keep that true: the missing mark skips an
//     issue with a later card, and a card write deletes the absences its
//     read supersedes).
//
// The mirror is emptied as a restore leaves it (restoreEmptiesTable): a
// card stored before v167 has no title column and a blob of the version
// before, and a delta sync never reads an unchanged card again, so every
// card is read again whole. The mirror is a cache of the trackers; the
// roots' AUTOINCREMENT sequence is kept, so no ID is reused. Children go
// first, so no cascade walks a parent's rows.
const threadTicketCopyV167SQL = `
ALTER TABLE thread_tickets ADD COLUMN status_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_tickets ADD COLUMN status_name TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_tickets ADD COLUMN status_group INTEGER NOT NULL DEFAULT 0 CHECK(status_group BETWEEN 0 AND 5);
DELETE FROM tracker_cache_issue_bodies;
DELETE FROM tracker_cache_issue_sprints;
DELETE FROM tracker_cache_board_issues;
DELETE FROM tracker_cache_changes;
DELETE FROM tracker_cache_open_tickets;
DELETE FROM tracker_cache_issues;
DELETE FROM tracker_cache_board_column_statuses;
DELETE FROM tracker_cache_board_columns;
DELETE FROM tracker_cache_board_unexplained;
DELETE FROM tracker_cache_sprints;
DELETE FROM tracker_cache_sync;
DELETE FROM tracker_cache_boards;
DELETE FROM tracker_cache_worklogs;
DELETE FROM tracker_cache_worklog_days;
DELETE FROM tracker_cache_statuses;
DELETE FROM tracker_cache_users;
DELETE FROM tracker_cache_roots;
ALTER TABLE tracker_cache_issues ADD COLUMN title TEXT NOT NULL DEFAULT '';
CREATE TABLE tracker_cache_issue_missing (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, issue_id)
) WITHOUT ROWID;
`

const threadTicketCopyMigrationVersion = 167
