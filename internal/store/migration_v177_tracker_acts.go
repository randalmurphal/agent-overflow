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
// every issue's logged time reads as not read until the next read, and to
// advance the totals revision of each of the root's boards, whose credit
// that changes. A replace read under an earlier generation is refused by
// ReplaceMirrorActs, as ReplaceMirrorWorklogs refuses one.
//
// trg_tracker_cache_credit_card_moved advances the totals revision of the
// boards holding an issue whose update time moves past what credit read
// of it: a receipt that was for it, whose source is not read from then
// until a read for the new update (mirrorActsCurrentSQL), or, for a
// member of a closed sprint (the pace's), a history final for it, whose
// person is not known from then until the history is read again
// (mirrorHistoryReadForCard; that history write advances them again,
// advanceMirrorPaceTx). A card write that moves neither out of date, the
// steady state of a board read, runs one probe of the issue's receipts by
// key.
//
// tracker_cache_roots.evidence_failure is why the evidence unit's last
// read of the root's connection failed as a whole; empty while its last
// read did not (RecordMirrorEvidenceOutcome).
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
ALTER TABLE tracker_cache_roots ADD COLUMN evidence_failure TEXT NOT NULL DEFAULT '';
CREATE TRIGGER trg_tracker_cache_credit_card_moved AFTER UPDATE OF updated_at ON tracker_cache_issues
WHEN EXISTS (SELECT 1 FROM tracker_cache_issue_act_reads r WHERE r.root_id = NEW.root_id AND r.issue_id = NEW.issue_id
  AND r.read_at > 0 AND r.read_for >= OLD.updated_at AND r.read_for < NEW.updated_at)
 OR NEW.history_for > 0 AND NEW.history_wait = 0 AND NEW.history_for >= OLD.updated_at AND NEW.history_for < NEW.updated_at
  AND EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
   CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.issue_id = m.issue_id
   CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
   WHERE m.root_id = NEW.root_id AND m.issue_id = NEW.issue_id AND sp.state = 3)
BEGIN
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
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
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_enabled_drops_worklogs AFTER UPDATE OF enabled ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled IS NOT OLD.enabled
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = NEW.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_issue_act_reads WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
 DELETE FROM tracker_cache_issue_acts WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id) AND source = 2;
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_deleted_drops_worklogs AFTER DELETE ON tracker_connections
WHEN OLD.kind = 'tempo' AND OLD.enabled = 1
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = OLD.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_issue_act_reads WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id) AND source = 2;
 DELETE FROM tracker_cache_issue_acts WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id) AND source = 2;
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
END;
`

const trackerActsMigrationVersion = 177
