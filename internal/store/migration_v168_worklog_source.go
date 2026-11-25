package store

// Migration v168 ties a Jira connection's logged time to its time source:
// Tempo while its Tempo connection is turned on, Jira's own worklogs
// otherwise. The source changes when a Tempo row is added turned on,
// deleted while on, or turned on or off; adding or deleting one that is
// off, or any other write to it, changes nothing. On each change its
// trigger advances the Jira row's time_source_gen and deletes the
// worklogs and read days of its mirror root, in the connection write's
// transaction, so no row or read day of the earlier source is shown,
// edited or deleted, and every day reads as unread until the next
// refresh. A replace read under an earlier generation is refused by
// ReplaceMirrorWorklogs, however many changes came between.
//
// The worklogs and read days stored before v168 are deleted, since
// nothing records which source read them.
const worklogSourceV168SQL = `
ALTER TABLE tracker_connections ADD COLUMN time_source_gen INTEGER NOT NULL DEFAULT 0 CHECK(time_source_gen >= 0);
DELETE FROM tracker_cache_worklogs;
DELETE FROM tracker_cache_worklog_days;
CREATE TRIGGER tracker_tempo_added_drops_worklogs AFTER INSERT ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled = 1
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = NEW.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_enabled_drops_worklogs AFTER UPDATE OF enabled ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled IS NOT OLD.enabled
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = NEW.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_deleted_drops_worklogs AFTER DELETE ON tracker_connections
WHEN OLD.kind = 'tempo' AND OLD.enabled = 1
BEGIN
 UPDATE tracker_connections SET time_source_gen = time_source_gen + 1 WHERE id = OLD.parent_id;
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
END;
`

const worklogSourceMigrationVersion = 168
