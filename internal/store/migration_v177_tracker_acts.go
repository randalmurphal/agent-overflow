package store

// Migration v177 adds the evidence of who worked on an issue, which
// credit counts (ReplaceMirrorActs, ListMirrorActsDue):
//
//   - tracker_cache_issue_acts: one row per comment (source 1) and
//     worklog (source 2) of an issue, its author's account (person), when
//     it was made (at: a comment's creation, a worklog's start, Unix ms)
//     and a worklog's time spent in seconds (0 for a comment). No text.
//     Source 3 is a pull request approval, which has no time (at 0) and
//     names its reviewer by account (person) or, when no single account
//     has the name the development data gives, by that name alone
//     (unmatched, person empty); no writer stores one yet. Each source's
//     shape is checked on its own. idx_tracker_cache_issue_acts_worklogs
//     holds a root's worklogs by their start, which credit walks for the
//     support time of a sprint's dates (mirrorCreditRowsSQL).
//   - tracker_cache_issue_act_reads: one row per issue and source, the
//     receipt of the last read that stored its acts and the last failed
//     attempt since. The receipt: the issue's update time the read
//     returned (read_for, 0 once a changed worklog made it due again),
//     the read's order time (read_at) and wall time (read_wall), and the
//     start of the worklogs it read (read_from, 0 for every one; a
//     comment's is 0). An issue's acts of a source are what the read
//     found from read_from exactly when a receipt exists (read_at > 0).
//     The attempt: when it failed on the wall clock (tried_at) and why
//     (tried_reason), both cleared by a read that stores. A row holds a
//     receipt, an attempt or both.
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
 source INTEGER NOT NULL,
 act_id TEXT NOT NULL CHECK(act_id <> ''),
 person TEXT NOT NULL,
 unmatched TEXT NOT NULL DEFAULT '',
 at INTEGER NOT NULL,
 seconds INTEGER NOT NULL,
 CHECK(CASE source
  WHEN 1 THEN person <> '' AND unmatched = '' AND at > 0 AND seconds = 0
  WHEN 2 THEN person <> '' AND unmatched = '' AND at > 0 AND seconds >= 0
  WHEN 3 THEN (person = '') <> (unmatched = '') AND at = 0 AND seconds = 0
  ELSE 0 END),
 PRIMARY KEY (root_id, issue_id, source, act_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_issue_acts_worklogs ON tracker_cache_issue_acts(root_id, at) WHERE source = 2;
CREATE TABLE tracker_cache_issue_act_reads (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 source INTEGER NOT NULL CHECK(source IN (1, 2, 3)),
 read_for INTEGER NOT NULL DEFAULT 0 CHECK(read_for >= 0),
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 read_wall INTEGER NOT NULL DEFAULT 0,
 read_from INTEGER NOT NULL DEFAULT 0 CHECK(read_from >= 0 AND (source = 2 OR read_from = 0)),
 tried_at INTEGER NOT NULL DEFAULT 0 CHECK(tried_at >= 0),
 tried_reason TEXT NOT NULL DEFAULT '',
 CHECK((read_at > 0) = (read_wall > 0)),
 CHECK(read_at > 0 OR read_for = 0 AND read_from = 0),
 CHECK((tried_at > 0) = (tried_reason <> '')),
 CHECK(read_at > 0 OR tried_at > 0),
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
