package store

// Migration v163 adds tracker_cache_open_tickets: the tickets a person has
// open in this process, each one row per ticket pump (trackerapp), so the
// mirror keeps the issue of every open ticket (mirrorIssueKept) for as
// long as its pump lives, whatever else keeps it or not. session names
// the process that wrote the row and pump the pump within it; the pump
// writes its row when it starts and deletes it when it ends, and the
// rows of other sessions, left by a process that ended without deleting
// them, are deleted at the next boot (DropMirrorTicketsOfOtherSessions).
// The table is of the mirror: it cascades from its root and a restore
// empties it. idx_tracker_cache_open_tickets_issue serves the keep rule's
// probe and a root's drain.
//
// The table is new: no row is rewritten.
const trackerOpenTicketsMigrationVersion = 163

const trackerOpenTicketsV163SQL = `
CREATE TABLE tracker_cache_open_tickets (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 session INTEGER NOT NULL,
 pump INTEGER NOT NULL CHECK(pump > 0),
 PRIMARY KEY (session, pump)
);
CREATE INDEX idx_tracker_cache_open_tickets_issue ON tracker_cache_open_tickets(root_id, issue_id);
`
