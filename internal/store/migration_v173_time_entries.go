package store

// Migration v173 adds tracker_time_entries: the person's time draft, one
// row per entry, by Jira connection, author (the connection's verified
// account it was drafted for) and day. An entry names a ticket, or none
// for a question the person answers; source says whether an agent or the
// person last wrote it, thread_id and call_id the agent call that drafted
// it (kept when the person changes it). state is drafted, sent (marked in
// its own transaction before its request, so a crash leaves it to be
// checked, never sent blind), posted, unknown (sent, outcome not known) or
// left_out (a question the person closed). sent_to is the time source it
// was sent to: the Tempo connection's ID, or the Jira connection's for
// Jira's own worklogs. revision moves on every change; a screen sends it
// back. A ticket has at most one entry a day that is not posted or left
// out (idx_tracker_time_entries_open), so an entry sent back to drafted
// never meets another. The ID is AUTOINCREMENT, never reused, so an ID a screen holds
// never names another entry. No foreign key to threads: a deleted
// thread's entries stay. A restore keeps the table (restoreSkipsTable)
// and deletes the entries of a connection the snapshot lacks.
const timeEntriesV173SQL = `
CREATE TABLE tracker_time_entries (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 connection_id TEXT NOT NULL REFERENCES tracker_connections(id) ON DELETE CASCADE,
 author_id TEXT NOT NULL CHECK(author_id <> ''),
 day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'),
 issue_id TEXT NOT NULL DEFAULT '',
 issue_key TEXT NOT NULL DEFAULT '',
 issue_title TEXT NOT NULL DEFAULT '',
 seconds INTEGER NOT NULL CHECK(seconds > 0 AND seconds % 60 = 0 AND seconds <= 86400),
 reason TEXT NOT NULL CHECK(reason <> ''),
 offers TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(offers) AND json_type(offers) = 'array'),
 source TEXT NOT NULL CHECK(source IN ('agent', 'person')),
 thread_id TEXT NOT NULL DEFAULT '',
 call_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('drafted', 'sent', 'posted', 'unknown', 'left_out')),
 revision INTEGER NOT NULL CHECK(revision > 0),
 refusal TEXT NOT NULL DEFAULT '',
 sent_ms INTEGER NOT NULL DEFAULT 0,
 sent_to TEXT NOT NULL DEFAULT '',
 worklog_id TEXT NOT NULL DEFAULT '',
 posted_ms INTEGER NOT NULL DEFAULT 0,
 created_ms INTEGER NOT NULL CHECK(created_ms > 0),
 updated_ms INTEGER NOT NULL CHECK(updated_ms > 0),
 CHECK((issue_id = '') = (issue_key = '')),
 CHECK(issue_id <> '' OR state IN ('drafted', 'left_out')),
 CHECK(issue_id = '' OR offers = '[]'),
 CHECK((state IN ('sent', 'posted', 'unknown')) = (sent_ms > 0 AND sent_to <> '')),
 CHECK((state = 'posted') = (posted_ms > 0)),
 CHECK(worklog_id = '' OR state = 'posted'),
 CHECK(source = 'person' OR thread_id <> '')
);
CREATE INDEX idx_tracker_time_entries_day ON tracker_time_entries(connection_id, author_id, day, id);
CREATE INDEX idx_tracker_time_entries_thread ON tracker_time_entries(thread_id, id) WHERE thread_id <> '';
CREATE UNIQUE INDEX idx_tracker_time_entries_open ON tracker_time_entries(connection_id, author_id, day, issue_id)
 WHERE state IN ('drafted', 'sent', 'unknown') AND issue_id <> '';
`

const timeEntriesMigrationVersion = 173
