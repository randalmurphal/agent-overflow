package store

// Migration v159 creates every table of the issue tracker integration
// (docs/architecture/tracker-backend.md#tables). While the feature is
// unshipped the tracker schema stays in this one migration, so a later
// tracker table amends trackerV159SQL here, with its frozen hash, instead
// of adding a version.
//
// The durable records:
//
//   - tracker_connections: one credential for one site. A Tempo connection
//     belongs to a Jira connection (parent_id) and goes with it; the parent
//     being a Jira connection is checked by CreateTrackerConnection, since
//     a CHECK cannot read another row. A Jira or Linear connection is
//     unique by (kind, site, account) and a Tempo connection by its parent:
//     two Jira sites can share one Tempo region.
//   - project_tracker_boards: the boards an app project lists, in picker
//     order, with at most one that opens first.
//   - thread_tickets: one ticket per thread. It names the tracker kind and
//     site, not a connection, so a link survives a connection removed and
//     added again.
const trackerMigrationVersion = 159

const trackerV159SQL = trackerDurableTablesV159SQL

const trackerDurableTablesV159SQL = `
CREATE TABLE tracker_connections (
 id TEXT PRIMARY KEY,
 kind TEXT NOT NULL CHECK(kind IN ('jira', 'tempo', 'linear')),
 site TEXT NOT NULL CHECK(site <> ''),
 account TEXT NOT NULL DEFAULT '',
 parent_id TEXT REFERENCES tracker_connections(id) ON DELETE CASCADE,
 label TEXT NOT NULL DEFAULT '',
 enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
 verified_account_id TEXT NOT NULL DEFAULT '',
 verified_name TEXT NOT NULL DEFAULT '',
 verified_at INTEGER NOT NULL DEFAULT 0 CHECK(verified_at >= 0),
 token_expires_at INTEGER NOT NULL DEFAULT 0 CHECK(token_expires_at >= 0),
 created_at INTEGER NOT NULL CHECK(created_at > 0),
 CHECK((kind = 'tempo') = (parent_id IS NOT NULL)),
 CHECK((kind = 'jira') = (account <> ''))
);
CREATE UNIQUE INDEX idx_tracker_connections_site ON tracker_connections(kind, site, account) WHERE parent_id IS NULL;
CREATE UNIQUE INDEX idx_tracker_connections_parent ON tracker_connections(parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE project_tracker_boards (
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 connection_id TEXT NOT NULL REFERENCES tracker_connections(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL CHECK(board_id <> ''),
 position INTEGER NOT NULL CHECK(position >= 0),
 opens_first INTEGER NOT NULL DEFAULT 0 CHECK(opens_first IN (0, 1)),
 PRIMARY KEY (project_id, connection_id, board_id),
 UNIQUE (project_id, position)
);
CREATE UNIQUE INDEX idx_project_tracker_boards_opens_first ON project_tracker_boards(project_id) WHERE opens_first = 1;
CREATE INDEX idx_project_tracker_boards_board ON project_tracker_boards(connection_id, board_id);

CREATE TABLE thread_tickets (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('jira', 'linear')),
 site TEXT NOT NULL CHECK(site <> ''),
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 issue_key TEXT NOT NULL CHECK(issue_key <> ''),
 issue_title TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL CHECK(origin IN ('start_work', 'ask', 'manual', 'agent')),
 linked_at INTEGER NOT NULL CHECK(linked_at > 0)
);
CREATE INDEX idx_thread_tickets_issue ON thread_tickets(kind, site, issue_id);
`
