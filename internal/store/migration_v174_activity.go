package store

// Migration v174 adds what the activity recorder measured of the person's
// day, for the time draft.
//
// activity_spans is the person's input on this computer: from start_ms to
// end_ms (Unix milliseconds) they gave input at least every 3 minutes, to
// thread_id (empty when no thread was focused) on a screen in the compact
// layout when phone is 1. Only the recorder writes it (internal/activity):
// a span is inserted at its end or at the first flush that sees it, and a
// later flush moves its end. thread_days names each thread that started a
// turn on day (this computer's local date), written in the turn insert's
// transaction (InsertTurn) only while a Jira connection is on. Neither has
// a foreign key: a deleted thread's span still counts toward the day's
// active minutes, and its rows stay until the recorder's prune, which
// deletes rows of both tables older than 92 days and nothing else. A
// restore keeps both tables (restoreSkipsTable).
const activityV174SQL = `
CREATE TABLE activity_spans (
 id INTEGER PRIMARY KEY,
 thread_id TEXT NOT NULL,
 phone INTEGER NOT NULL CHECK(phone IN (0, 1)),
 start_ms INTEGER NOT NULL CHECK(start_ms > 0),
 end_ms INTEGER NOT NULL CHECK(end_ms >= start_ms)
);
CREATE INDEX idx_activity_spans_start ON activity_spans(start_ms);
CREATE TABLE thread_days (
 day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'),
 thread_id TEXT NOT NULL CHECK(thread_id <> ''),
 PRIMARY KEY (day, thread_id)
) WITHOUT ROWID;
`

const activityMigrationVersion = 174
