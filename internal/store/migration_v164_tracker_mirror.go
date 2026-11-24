package store

// Migration v164 holds every change of the mirror after v162, which is
// shipped: the tables it adds are new, and the tables it changes are a
// cache nothing in a shipped build writes.
//
//   - tracker_cache_open_tickets: the tickets a person has open in this
//     process, each one row per ticket pump (trackerapp), so the mirror
//     keeps the issue of every open ticket (mirrorIssueKept) for as long
//     as its pump lives, whatever else keeps it or not. session names the
//     process that wrote the row and pump the pump within it, and
//     opened_at is the order time of the root's read clock when the pump
//     kept it, which seeds the clock. The pump writes its row before its
//     first read and deletes it when it ends; which reads write a body is
//     the bind's rule, in memory (MirrorBind), not the row's. An issue may
//     have more than one row (a closed ticket's drop owed beside the
//     reopened ticket's keep). The rows of other sessions, left by a
//     process that ended without deleting them, are deleted by the first
//     operation of the next process (DropMirrorTicketsOfOtherSessions).
//     The table cascades from its root and a restore empties it.
//     idx_tracker_cache_open_tickets_issue serves the keep rule's probe
//     and a root's drain.
//   - The wall time of the reads whose interval decides the next one or
//     that a person is shown: tracker_cache_boards.structure_wall (the
//     board's structure read) and tracker_cache_sync.read_wall,
//     reconciled_wall and failed_wall (the scope's last read, reconcile
//     and failure), each the MirrorRead.Wall of the read whose order time
//     the row already stores, written by the same statement. A row stored
//     before them holds 0, which is due: each scope is read once.
//   - A board's reconcile failure, beside the scope's other failure:
//     tracker_cache_sync.reconcile_failed_at (the order time of the read
//     its failing call began, which seeds the clock), reconcile_failed_wall
//     (when that call returned), reconcile_failure_kind and _message, and
//     reconcile_refused (the tracker refused the listing's rule, which
//     starts the reconcile's wait). Only a completed reconcile clears it,
//     and the other failure is cleared only by a read
//     (RecordMirrorSyncAttempt). A failure stored before v164 stays in the
//     other slot, which the next read clears.
//   - The issue record is the one record of an issue (StoreMirrorTickets):
//     tracker_cache_issues.status_final_for is the update time whose
//     status time a final read decided (MirrorStatusSince), backfilled
//     for the rows whose stored history was final for their card, the
//     rule v162 derived the time by. status_floor_at and status_read_at
//     are the read orders of its evidence (mirrorStatusWriteSQL): the read
//     of the card write that gave the issue its version or status, and
//     the read behind a provisional time, never below the floor; a row
//     stored before them holds 0, which refuses no read. body_updated_at, body_read_at,
//     body_wall, body_opened_at and body_digest are the bookkeeping of the
//     ticket body the issue holds in tracker_cache_issue_bodies, a body
//     existing exactly when body_read_at and body_updated_at are over zero;
//     one function writes a body and one evicts it, each moving both
//     tables in one transaction. idx_tracker_cache_issues_body_opened
//     orders the bodies for the cap's eviction; its predicate names
//     body_updated_at, which moves with a version, not body_read_at, which
//     moves with every read. tracker_cache_issue_details is dropped with
//     the documents it held: the next opening reads each body again.
//   - tracker_cache_issues.sprint_set, the digest of the sprints an issue's
//     card names (mirrorSprintSet), part of the card's content, so a card
//     that names the same sprints costs one comparison and no membership
//     read or write. The digest is computed in Go, so an issue that holds
//     memberships when the column lands is given mirrorSprintSetPartial,
//     which no set digests to: the next write that admits its card
//     replaces its sprints. An issue with none holds the empty digest,
//     which is right for it.
//   - tracker_cache_board_issues.listed is dropped, with it from
//     idx_tracker_cache_board_issues_order, which is made again over the
//     rest of the row: a reconcile removes no row on a listing's word, so
//     nothing reads which listing named a row.
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
 opened_at INTEGER NOT NULL CHECK(opened_at > 0),
 PRIMARY KEY (session, pump)
);
CREATE INDEX idx_tracker_cache_open_tickets_issue ON tracker_cache_open_tickets(root_id, issue_id);
ALTER TABLE tracker_cache_boards ADD COLUMN structure_wall INTEGER NOT NULL DEFAULT 0 CHECK(structure_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN read_wall INTEGER NOT NULL DEFAULT 0 CHECK(read_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconciled_wall INTEGER NOT NULL DEFAULT 0 CHECK(reconciled_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN failed_wall INTEGER NOT NULL DEFAULT 0 CHECK(failed_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconcile_failed_at INTEGER NOT NULL DEFAULT 0 CHECK(reconcile_failed_at >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconcile_failed_wall INTEGER NOT NULL DEFAULT 0 CHECK(reconcile_failed_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconcile_failure_kind TEXT NOT NULL DEFAULT ''
 CHECK(reconcile_failure_kind IN ('', 'transient', 'rate_limited', 'setup', 'forbidden', 'tracker')
   AND (reconcile_failure_kind = '') = (reconcile_failed_at = 0));
ALTER TABLE tracker_cache_sync ADD COLUMN reconcile_failure_message TEXT NOT NULL DEFAULT ''
 CHECK(reconcile_failure_kind <> '' OR reconcile_failure_message = '');
ALTER TABLE tracker_cache_sync ADD COLUMN reconcile_refused INTEGER NOT NULL DEFAULT 0
 CHECK(reconcile_refused IN (0, 1) AND (reconcile_refused = 0 OR reconcile_failure_kind <> ''));
ALTER TABLE tracker_cache_issues ADD COLUMN sprint_set BLOB NOT NULL DEFAULT x'';
DROP INDEX idx_tracker_cache_board_issues_order;
ALTER TABLE tracker_cache_board_issues DROP COLUMN listed;
CREATE INDEX idx_tracker_cache_board_issues_order ON tracker_cache_board_issues(root_id, board_id, order_key, issue_id, estimate, read_at);
UPDATE tracker_cache_issues SET sprint_set = x'` + mirrorSprintSetPartial + `'
 WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
                WHERE m.root_id = tracker_cache_issues.root_id AND m.issue_id = tracker_cache_issues.issue_id);
DROP TABLE tracker_cache_issue_details;
ALTER TABLE tracker_cache_issues ADD COLUMN status_final_for INTEGER NOT NULL DEFAULT 0 CHECK(status_final_for >= 0);
UPDATE tracker_cache_issues SET status_final_for = updated_at WHERE history_for >= updated_at AND history_wait = 0;
ALTER TABLE tracker_cache_issues ADD COLUMN status_read_at INTEGER NOT NULL DEFAULT 0 CHECK(status_read_at >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN status_floor_at INTEGER NOT NULL DEFAULT 0 CHECK(status_floor_at BETWEEN 0 AND status_read_at);
ALTER TABLE tracker_cache_issues ADD COLUMN body_updated_at INTEGER NOT NULL DEFAULT 0 CHECK(body_updated_at >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN body_read_at INTEGER NOT NULL DEFAULT 0 CHECK(body_read_at >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN body_wall INTEGER NOT NULL DEFAULT 0 CHECK(body_wall >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN body_opened_at INTEGER NOT NULL DEFAULT 0 CHECK(body_opened_at >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN body_digest BLOB NOT NULL DEFAULT x'';
CREATE INDEX idx_tracker_cache_issues_body_opened ON tracker_cache_issues(root_id, body_opened_at, issue_id) WHERE body_updated_at > 0;
CREATE TABLE tracker_cache_issue_bodies (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 data BLOB NOT NULL,
 PRIMARY KEY (root_id, issue_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
);
`
