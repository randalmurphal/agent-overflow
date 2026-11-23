package store

// Migration v164 holds every change of the mirror after v162, which is
// shipped: the tables it adds are new, and the tables it changes are a
// cache nothing in a shipped build writes.
//
//   - tracker_cache_open_tickets: the tickets a person has open in this
//     process, each one row per ticket pump (trackerapp), so the mirror
//     keeps the issue of every open ticket (mirrorIssueKept) for as long
//     as its pump lives, whatever else keeps it or not. session names the
//     process that wrote the row and pump the pump within it; the pump
//     writes its row when it starts and deletes it when it ends, and the
//     rows of other sessions, left by a process that ended without
//     deleting them, are deleted at the next boot
//     (DropMirrorTicketsOfOtherSessions). The table cascades from its
//     root and a restore empties it. idx_tracker_cache_open_tickets_issue
//     serves the keep rule's probe and a root's drain.
//   - The wall time of the reads whose interval decides the next one:
//     tracker_cache_boards.structure_wall (the board's structure read),
//     tracker_cache_sync.reconciled_wall (its last reconcile) and
//     tracker_cache_issue_details.read_wall (a document's read), each the
//     MirrorRead.Wall of the read whose order time the row already stores,
//     written by the same statement. A row stored before them holds 0,
//     which is due: each scope is read once.
//   - tracker_cache_issues.sprint_set, the digest of the sprints an issue's
//     card names (mirrorSprintSet), part of the card's content, so a card
//     that names the same sprints costs one comparison and no membership
//     read or write. The digest is computed in Go, so an issue that holds
//     memberships when the column lands is given mirrorSprintSetPartial,
//     which no set digests to: the next write that admits its card
//     replaces its sprints. An issue with none holds the empty digest,
//     which is right for it.
//
// A root's read clock is seeded from the read times the root stores
// (BeginMirrorRead): no table holds it.
const trackerMirrorMigrationVersion = 164

const trackerMirrorV164SQL = `
CREATE TABLE tracker_cache_open_tickets (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 session INTEGER NOT NULL,
 pump INTEGER NOT NULL CHECK(pump > 0),
 PRIMARY KEY (session, pump)
);
CREATE INDEX idx_tracker_cache_open_tickets_issue ON tracker_cache_open_tickets(root_id, issue_id);
ALTER TABLE tracker_cache_boards ADD COLUMN structure_wall INTEGER NOT NULL DEFAULT 0 CHECK(structure_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconciled_wall INTEGER NOT NULL DEFAULT 0 CHECK(reconciled_wall >= 0);
ALTER TABLE tracker_cache_issue_details ADD COLUMN read_wall INTEGER NOT NULL DEFAULT 0 CHECK(read_wall >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN sprint_set BLOB NOT NULL DEFAULT x'';
UPDATE tracker_cache_issues SET sprint_set = x'` + mirrorSprintSetPartial + `'
 WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
                WHERE m.root_id = tracker_cache_issues.root_id AND m.issue_id = tracker_cache_issues.issue_id);
`
