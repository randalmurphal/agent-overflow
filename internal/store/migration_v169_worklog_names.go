package store

// Migration v169 stores each worklog's issue key and title as the read
// that stored it found them, empty for an issue the tracker did not
// return: a worklog names its issue by ID alone, and a Tempo worklog's ID
// is the only thing a screen could show without them.
//
// The worklogs and read days stored before v169 are deleted, since none
// holds a name: every day reads as unread until the next refresh.
const worklogNamesV169SQL = `
ALTER TABLE tracker_cache_worklogs ADD COLUMN issue_key TEXT NOT NULL DEFAULT '';
ALTER TABLE tracker_cache_worklogs ADD COLUMN issue_title TEXT NOT NULL DEFAULT '';
DELETE FROM tracker_cache_worklogs;
DELETE FROM tracker_cache_worklog_days;
`

const worklogNamesMigrationVersion = 169
