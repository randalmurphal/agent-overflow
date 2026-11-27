package store

// Migration v177 adds the evidence of who worked on an issue, which
// credit counts (ReplaceMirrorActs, ListMirrorActsDue):
//
//   - tracker_cache_issue_acts: one row per comment (source 1) and
//     worklog (source 2) of an issue, its author's account (person), when
//     it was made (at: a comment's creation, a worklog's start, Unix ms)
//     and a worklog's time spent in seconds (0 for a comment). No text.
//   - tracker_cache_issue_act_reads: one receipt per issue and source, the
//     issue's update time the read returned (read_for) and the read's
//     order time (read_at). An issue's acts of a source are what its read
//     found exactly when a receipt exists; one with none was never read
//     whole, and one whose receipt is older than its card is due.
//
// Both belong to their issue (ON DELETE CASCADE), so the cleanup sweep and
// a root's drain take them with it. The three Tempo triggers of v168 are
// made again to delete the root's worklog acts and receipts too when the
// time source changes, so no worklog of the earlier source is counted and
// every issue's logged time reads as not read until the next read. A
// replace read under an earlier generation is refused by
// ReplaceMirrorActs, as ReplaceMirrorWorklogs refuses one.
const trackerActsV177SQL = `
CREATE TABLE tracker_cache_issue_acts (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 source INTEGER NOT NULL CHECK(source IN (1, 2)),
 act_id TEXT NOT NULL CHECK(act_id <> ''),
 person TEXT NOT NULL CHECK(person <> ''),
 at INTEGER NOT NULL CHECK(at > 0),
 seconds INTEGER NOT NULL CHECK(seconds >= 0 AND (source = 2 OR seconds = 0)),
 PRIMARY KEY (root_id, issue_id, source, act_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE tracker_cache_issue_act_reads (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 source INTEGER NOT NULL CHECK(source IN (1, 2)),
 read_for INTEGER NOT NULL CHECK(read_for > 0),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, issue_id, source),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
DROP TRIGGER tracker_tempo_added_drops_worklogs;
DROP TRIGGER tracker_tempo_enabled_drops_worklogs;
DROP TRIGGER tracker_tempo_deleted_drops_worklogs;
CREATE TRIGGER tracker_tempo_added_drops_worklogs AFTER INSERT ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled = 1
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = NEW.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_issue_act_reads WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
 DELETE FROM tracker_cache_issue_acts WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
END;
CREATE TRIGGER tracker_tempo_enabled_drops_worklogs AFTER UPDATE OF enabled ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled IS NOT OLD.enabled
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = NEW.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_issue_act_reads WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
 DELETE FROM tracker_cache_issue_acts WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
END;
CREATE TRIGGER tracker_tempo_deleted_drops_worklogs AFTER DELETE ON tracker_connections
WHEN OLD.kind = 'tempo' AND OLD.enabled = 1
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = OLD.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_issue_act_reads WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id) AND source = 2;
 DELETE FROM tracker_cache_issue_acts WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id) AND source = 2;
END;
`

const trackerActsMigrationVersion = 177
