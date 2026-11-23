package store

// Migration v164 changes three things of the mirror:
//
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
// No table is added: a root's read clock is seeded from the read times
// the root stores (BeginMirrorRead).
const trackerMirrorClockMigrationVersion = 164

const trackerMirrorClockV164SQL = `
ALTER TABLE tracker_cache_boards ADD COLUMN structure_wall INTEGER NOT NULL DEFAULT 0 CHECK(structure_wall >= 0);
ALTER TABLE tracker_cache_sync ADD COLUMN reconciled_wall INTEGER NOT NULL DEFAULT 0 CHECK(reconciled_wall >= 0);
ALTER TABLE tracker_cache_issue_details ADD COLUMN read_wall INTEGER NOT NULL DEFAULT 0 CHECK(read_wall >= 0);
ALTER TABLE tracker_cache_issues ADD COLUMN sprint_set BLOB NOT NULL DEFAULT x'';
UPDATE tracker_cache_issues SET sprint_set = x'` + mirrorSprintSetPartial + `'
 WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
                WHERE m.root_id = tracker_cache_issues.root_id AND m.issue_id = tracker_cache_issues.issue_id);
`
