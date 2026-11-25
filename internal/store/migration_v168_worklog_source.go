package store

// Migration v168 ties a Jira connection's logged time to its time source:
// Tempo while its Tempo connection is turned on, Jira's own worklogs
// otherwise. A Tempo row added, turned on or off, or deleted changes that
// source, and its trigger deletes the worklogs and read days of its Jira
// connection's mirror root in the same transaction, so no row or read day
// of the earlier source is shown, edited or deleted; every day reads as
// unread until the next refresh. A replace made with the earlier source
// that commits after it is refused by ReplaceMirrorWorklogs.
//
// The worklogs and read days stored before v168 are deleted, since
// nothing records which source read them.
const worklogSourceV168SQL = `
DELETE FROM tracker_cache_worklogs;
DELETE FROM tracker_cache_worklog_days;
CREATE TRIGGER tracker_tempo_added_drops_worklogs AFTER INSERT ON tracker_connections
WHEN NEW.kind = 'tempo'
BEGIN
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_enabled_drops_worklogs AFTER UPDATE OF enabled ON tracker_connections
WHEN NEW.kind = 'tempo' AND NEW.enabled IS NOT OLD.enabled
BEGIN
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = NEW.parent_id);
END;
CREATE TRIGGER tracker_tempo_deleted_drops_worklogs AFTER DELETE ON tracker_connections
WHEN OLD.kind = 'tempo'
BEGIN
 DELETE FROM tracker_cache_worklogs WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
 DELETE FROM tracker_cache_worklog_days WHERE root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.parent_id);
END;
`

const worklogSourceMigrationVersion = 168
