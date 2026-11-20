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
//
// The mirror (trackerCacheTablesV159SQL) is every table named
// tracker_cache_*: a cache of what the trackers hold, filled by sync and
// recreated empty by a restore. Each row reaches a tracker_cache_roots row
// through ON DELETE CASCADE keys, the root's link to its connection is the
// only SET NULL, and no other table references the mirror
// (TestTrackerCacheTablesCascadeFromTheRoot), so a write naming a root that
// is gone is refused by the schema. Times
// are Unix milliseconds, zero for none. Columns hold what a query filters,
// joins or sorts by; data is the rest of a record, in a format only the
// caller knows.
//
//   - tracker_cache_roots: one per mirror. connection_id is NULL once the
//     root is detached from its connection, by a clear or by the
//     connection's removal (ON DELETE SET NULL), so neither deletes a whole
//     mirror in one transaction; the root's rows are then drained in
//     batches and the root deleted (drainMirrorRoot).
//   - tracker_cache_board_column_statuses: a status sits in at most one
//     column of a board; rank is its place within the column.
//   - tracker_cache_sprints: a sprint as one board shows it; a Jira sprint
//     can show on several boards and names the board it was made on
//     (origin_board_id). position is the order the structure read gave.
//   - tracker_cache_issue_sprints: the sprints an issue's card names. A
//     sprint the mirror does not keep can be named, so it has no key to
//     tracker_cache_sprints.
//   - tracker_cache_issues: history_from and history_to are the times the
//     stored changes are complete from (zero: from creation) and final
//     through (zero: no history stored). idx_tracker_cache_issues_history_due
//     holds the issues whose history is due again. board_rows counts the
//     issue's tracker_cache_board_issues rows, kept by the
//     trg_tracker_cache_board_rows_* triggers (a board row's keys are never
//     updated), so idx_tracker_cache_issues_boardless holds the issues only
//     a thread's link can keep (PruneUnkeptMirrorIssues).
//   - tracker_cache_changes: seq orders one kind's changes of one issue. A
//     status or assignee change holds IDs in from_value and to_value, a
//     sprint change the sorted sprint IDs as a JSON array, an estimate
//     change its numbers and the board it was made on.
//   - tracker_cache_worklogs: logged time; issue_id has no key, since a
//     worklog's issue need not be in the mirror.
//   - tracker_cache_sync: one row per scope, an empty board_id being the root's
//     linked issues; no key to the board, since a scope's failure can come
//     before its structure is stored.
const trackerMigrationVersion = 159

const trackerV159SQL = trackerDurableTablesV159SQL + trackerCacheTablesV159SQL

const trackerCacheTablesV159SQL = `
CREATE TABLE tracker_cache_roots (
 id TEXT PRIMARY KEY CHECK(id <> ''),
 connection_id TEXT UNIQUE REFERENCES tracker_connections(id) ON DELETE SET NULL,
 created_at INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TABLE tracker_cache_projects (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL CHECK(project_id <> ''),
 project_key TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, project_id)
);

CREATE TABLE tracker_cache_users (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL CHECK(user_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, user_id)
);

CREATE TABLE tracker_cache_statuses (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 status_id TEXT NOT NULL CHECK(status_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 status_group INTEGER NOT NULL CHECK(status_group BETWEEN 1 AND 5),
 PRIMARY KEY (root_id, status_id)
);

CREATE TABLE tracker_cache_boards (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL CHECK(board_id <> ''),
 project_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 plans_sprints INTEGER NOT NULL DEFAULT 0 CHECK(plans_sprints IN (0, 1)),
 estimate_unit INTEGER NOT NULL DEFAULT 0 CHECK(estimate_unit IN (0, 1, 2)),
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, board_id)
);

CREATE TABLE tracker_cache_board_columns (
 root_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK(position >= 0),
 name TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (root_id, board_id, position),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE
);

CREATE TABLE tracker_cache_board_column_statuses (
 root_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 position INTEGER NOT NULL,
 status_id TEXT NOT NULL CHECK(status_id <> ''),
 rank INTEGER NOT NULL CHECK(rank >= 0),
 PRIMARY KEY (root_id, board_id, status_id),
 FOREIGN KEY (root_id, board_id, position) REFERENCES tracker_cache_board_columns(root_id, board_id, position) ON DELETE CASCADE
);
CREATE INDEX idx_tracker_cache_board_column_statuses_column ON tracker_cache_board_column_statuses(root_id, board_id, position, rank);

CREATE TABLE tracker_cache_sprints (
 root_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 sprint_id TEXT NOT NULL CHECK(sprint_id <> ''),
 position INTEGER NOT NULL CHECK(position >= 0),
 origin_board_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 goal TEXT NOT NULL DEFAULT '',
 state INTEGER NOT NULL CHECK(state IN (1, 2, 3)),
 start_at INTEGER NOT NULL DEFAULT 0 CHECK(start_at >= 0),
 end_at INTEGER NOT NULL DEFAULT 0 CHECK(end_at >= 0),
 completed_at INTEGER NOT NULL DEFAULT 0 CHECK(completed_at >= 0),
 PRIMARY KEY (root_id, board_id, sprint_id),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE
);

CREATE TABLE tracker_cache_issues (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 issue_key TEXT NOT NULL CHECK(issue_key <> ''),
 project_id TEXT NOT NULL DEFAULT '',
 type_id TEXT NOT NULL DEFAULT '',
 type_level INTEGER NOT NULL DEFAULT 0,
 status_id TEXT NOT NULL DEFAULT '',
 assignee_id TEXT NOT NULL DEFAULT '',
 priority_id TEXT NOT NULL DEFAULT '',
 parent_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL DEFAULT 0 CHECK(created_at >= 0),
 updated_at INTEGER NOT NULL CHECK(updated_at > 0),
 resolved_at INTEGER NOT NULL DEFAULT 0 CHECK(resolved_at >= 0),
 history_from INTEGER NOT NULL DEFAULT 0 CHECK(history_from >= 0),
 history_to INTEGER NOT NULL DEFAULT 0 CHECK(history_to >= 0),
 board_rows INTEGER NOT NULL DEFAULT 0 CHECK(board_rows >= 0),
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, issue_id)
);
CREATE INDEX idx_tracker_cache_issues_history_due ON tracker_cache_issues(root_id, issue_id) WHERE history_to < updated_at;
CREATE INDEX idx_tracker_cache_issues_boardless ON tracker_cache_issues(root_id, issue_id) WHERE board_rows = 0;

CREATE TABLE tracker_cache_board_issues (
 root_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 issue_id TEXT NOT NULL,
 order_key TEXT NOT NULL DEFAULT '',
 estimate REAL,
 PRIMARY KEY (root_id, board_id, issue_id),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE,
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
);
CREATE INDEX idx_tracker_cache_board_issues_order ON tracker_cache_board_issues(root_id, board_id, order_key, issue_id);
CREATE INDEX idx_tracker_cache_board_issues_issue ON tracker_cache_board_issues(root_id, issue_id, board_id);
CREATE TRIGGER trg_tracker_cache_board_rows_insert AFTER INSERT ON tracker_cache_board_issues BEGIN
 UPDATE tracker_cache_issues SET board_rows = board_rows + 1 WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id;
END;
CREATE TRIGGER trg_tracker_cache_board_rows_delete AFTER DELETE ON tracker_cache_board_issues BEGIN
 UPDATE tracker_cache_issues SET board_rows = board_rows - 1 WHERE root_id = OLD.root_id AND issue_id = OLD.issue_id;
END;

CREATE TABLE tracker_cache_issue_sprints (
 root_id TEXT NOT NULL,
 issue_id TEXT NOT NULL,
 sprint_id TEXT NOT NULL CHECK(sprint_id <> ''),
 PRIMARY KEY (root_id, issue_id, sprint_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
);
CREATE INDEX idx_tracker_cache_issue_sprints_sprint ON tracker_cache_issue_sprints(root_id, sprint_id, issue_id);

CREATE TABLE tracker_cache_changes (
 root_id TEXT NOT NULL,
 issue_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('status', 'sprint', 'estimate', 'assignee')),
 seq INTEGER NOT NULL CHECK(seq >= 0),
 at INTEGER NOT NULL CHECK(at > 0),
 author_id TEXT NOT NULL DEFAULT '',
 board_id TEXT NOT NULL DEFAULT '',
 from_value TEXT NOT NULL DEFAULT '',
 to_value TEXT NOT NULL DEFAULT '',
 from_number REAL,
 to_number REAL,
 PRIMARY KEY (root_id, issue_id, kind, seq),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE,
 CHECK((kind = 'estimate') = (board_id <> '')),
 CHECK(kind = 'estimate' OR (from_number IS NULL AND to_number IS NULL)),
 CHECK(kind <> 'estimate' OR (from_value = '' AND to_value = ''))
);

CREATE TABLE tracker_cache_issue_details (
 root_id TEXT NOT NULL,
 issue_id TEXT NOT NULL,
 opened_at INTEGER NOT NULL CHECK(opened_at > 0),
 data BLOB NOT NULL,
 PRIMARY KEY (root_id, issue_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
);
CREATE INDEX idx_tracker_cache_issue_details_opened ON tracker_cache_issue_details(root_id, opened_at, issue_id);

CREATE TABLE tracker_cache_worklogs (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 worklog_id TEXT NOT NULL CHECK(worklog_id <> ''),
 issue_id TEXT NOT NULL CHECK(issue_id <> ''),
 author_id TEXT NOT NULL CHECK(author_id <> ''),
 day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'),
 spent_seconds INTEGER NOT NULL CHECK(spent_seconds > 0),
 description TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL DEFAULT 0 CHECK(updated_at >= 0),
 PRIMARY KEY (root_id, worklog_id)
);
CREATE INDEX idx_tracker_cache_worklogs_author_day ON tracker_cache_worklogs(root_id, author_id, day, worklog_id);
CREATE INDEX idx_tracker_cache_worklogs_issue ON tracker_cache_worklogs(root_id, issue_id, day, worklog_id);

CREATE TABLE tracker_cache_sync (
 root_id TEXT NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL,
 cursor BLOB,
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 reconciled_at INTEGER NOT NULL DEFAULT 0 CHECK(reconciled_at >= 0),
 failed_at INTEGER NOT NULL DEFAULT 0 CHECK(failed_at >= 0),
 failure_kind TEXT NOT NULL DEFAULT '' CHECK(failure_kind IN ('', 'transient', 'rate_limited', 'setup', 'forbidden', 'tracker')),
 failure_message TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (root_id, board_id),
 CHECK((failure_kind = '') = (failed_at = 0)),
 CHECK(failure_kind <> '' OR failure_message = '')
);
`

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
