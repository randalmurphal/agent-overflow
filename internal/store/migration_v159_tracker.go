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
//     order, with at most one that opens first. The delete of a board's
//     last listing retires the board in its mirror
//     (trg_project_tracker_boards_retire).
//   - thread_tickets: one ticket per thread. It names the tracker kind and
//     site, not a connection, so a link survives a connection removed and
//     added again.
//
// The mirror (trackerCacheTablesV159SQL) is every table named
// tracker_cache_*: a cache of what the trackers hold, filled by sync and
// emptied by a restore. Each row reaches a tracker_cache_roots row through
// ON DELETE CASCADE keys, the root's link to its connection is the only
// SET NULL, and no other table references the mirror
// (TestTrackerCacheTablesCascadeFromTheRoot). A root's ID is an
// AUTOINCREMENT integer, so it is never reused and costs a byte or two in
// every row and index entry that names it. Times are Unix milliseconds,
// zero for none. Columns hold what a query filters, joins, sorts or orders
// writes by; data is the rest of a record, in a format only the caller
// knows. A read_at column is when the read that stored the row's content
// began: a later write of other content from a read that began earlier is
// refused (superseded), and an identical write changes nothing.
//
//   - tracker_cache_roots: one per mirror. connection_id is NULL once the
//     root is detached, by DetachMirror or by the connection's removal (ON
//     DELETE SET NULL); FinishMirrorCleanup then drains it in batches.
//   - tracker_cache_boards: revision advances, through the
//     trg_tracker_cache_revision_* triggers, with every change that can
//     add, remove or move a row in one of the board's sections, so a
//     section page continued at another revision is refused. retired is
//     set when the board's last listing goes; a retired board is absent to
//     reads and writes and FinishMirrorCleanup drains it.
//     rows_removed_at is the latest read that removed rows: a read that
//     began before it adds no row (a row it would add was removed or is
//     read again).
//   - tracker_cache_board_column_statuses: a status sits in at most one
//     column of a board; rank is its place within the column.
//   - tracker_cache_sprints: a sprint as one board shows it; a Jira sprint
//     can show on several boards and names the board it was made on
//     (origin_board_id). position is the order the structure read gave.
//   - tracker_cache_issues: status_since is when the card's status began,
//     set from a stored history and zeroed when the card's status changes.
//     history_from and history_to are the times the stored changes are
//     complete from (zero: from creation) and final through (zero: none).
//     board_rows counts the issue's board rows, kept by the
//     trg_tracker_cache_board_rows_* triggers (a row's keys are never
//     updated); idx_tracker_cache_issues_boardless holds the issues only a
//     thread's link can keep, which the cleanup sweep reads.
//   - tracker_cache_board_issues: keyed by issue first, so the primary key
//     serves the keep rule and an issue's rows; idx_..._order serves a
//     section page, the counts, a reconcile and a board's drain. It covers
//     every column: SQLite plans a lookup through an index that does not
//     cover a WITHOUT ROWID table's row as dearer than a walk of the
//     primary key's root_id prefix, which reads every row of the root.
//   - tracker_cache_issue_sprints: the sprints an issue's card names. A
//     sprint the mirror does not keep can be named, so it has no key to
//     tracker_cache_sprints.
//   - tracker_cache_changes: kind is 1 status, 2 sprint, 3 estimate, 4
//     assignee; seq orders one kind's changes of one issue. A status or
//     assignee change holds IDs in from_value and to_value, a sprint change
//     the sorted sprint IDs as a JSON array, an estimate change its numbers
//     and the board it was made on.
//   - tracker_cache_issue_details: opened_at is when the issue was last
//     opened, which orders eviction; data is the document, NULL until one
//     is stored, and read_at when its read began.
//   - tracker_cache_worklogs: logged time; issue_id has no key, since a
//     worklog's issue need not be in the mirror.
//     tracker_cache_worklog_days holds, per author and day, the read that
//     last changed that day's logs.
//   - tracker_cache_sync: one row per scope, an empty board_id being the
//     root's linked issues. A board scope's row goes when the board's last
//     listing goes. outcome_at is the time of the last failure or clear.
const trackerMigrationVersion = 159

const trackerV159SQL = trackerDurableTablesV159SQL + trackerCacheTablesV159SQL

const trackerCacheTablesV159SQL = `
CREATE TABLE tracker_cache_roots (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 connection_id TEXT UNIQUE REFERENCES tracker_connections(id) ON DELETE SET NULL,
 created_at INTEGER NOT NULL CHECK(created_at > 0)
);

CREATE TABLE tracker_cache_users (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL CHECK(user_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 data BLOB NOT NULL DEFAULT x'',
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, user_id)
) WITHOUT ROWID;

CREATE TABLE tracker_cache_statuses (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 status_id TEXT NOT NULL CHECK(status_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 status_group INTEGER NOT NULL CHECK(status_group BETWEEN 1 AND 5),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, status_id)
) WITHOUT ROWID;

CREATE TABLE tracker_cache_boards (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL CHECK(board_id <> ''),
 project_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 plans_sprints INTEGER NOT NULL DEFAULT 0 CHECK(plans_sprints IN (0, 1)),
 estimate_unit INTEGER NOT NULL DEFAULT 0 CHECK(estimate_unit IN (0, 1, 2)),
 data BLOB NOT NULL DEFAULT x'',
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 rows_removed_at INTEGER NOT NULL DEFAULT 0 CHECK(rows_removed_at >= 0),
 revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
 retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0, 1)),
 PRIMARY KEY (root_id, board_id)
);
CREATE INDEX idx_tracker_cache_boards_retired ON tracker_cache_boards(root_id, board_id) WHERE retired = 1;

CREATE TABLE tracker_cache_board_columns (
 root_id INTEGER NOT NULL,
 board_id TEXT NOT NULL,
 position INTEGER NOT NULL CHECK(position >= 0),
 name TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (root_id, board_id, position),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TABLE tracker_cache_board_column_statuses (
 root_id INTEGER NOT NULL,
 board_id TEXT NOT NULL,
 position INTEGER NOT NULL,
 status_id TEXT NOT NULL CHECK(status_id <> ''),
 rank INTEGER NOT NULL CHECK(rank >= 0),
 PRIMARY KEY (root_id, board_id, status_id),
 FOREIGN KEY (root_id, board_id, position) REFERENCES tracker_cache_board_columns(root_id, board_id, position) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_board_column_statuses_column ON tracker_cache_board_column_statuses(root_id, board_id, position, rank);

CREATE TABLE tracker_cache_sprints (
 root_id INTEGER NOT NULL,
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
) WITHOUT ROWID;

CREATE TABLE tracker_cache_issues (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
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
 status_since INTEGER NOT NULL DEFAULT 0 CHECK(status_since >= 0),
 history_from INTEGER NOT NULL DEFAULT 0 CHECK(history_from >= 0),
 history_to INTEGER NOT NULL DEFAULT 0 CHECK(history_to >= 0),
 board_rows INTEGER NOT NULL DEFAULT 0 CHECK(board_rows >= 0),
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, issue_id)
);
CREATE INDEX idx_tracker_cache_issues_history_due ON tracker_cache_issues(root_id, issue_id) WHERE history_to < updated_at;
CREATE INDEX idx_tracker_cache_issues_boardless ON tracker_cache_issues(root_id, issue_id) WHERE board_rows = 0;

CREATE TABLE tracker_cache_board_issues (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 order_key TEXT NOT NULL DEFAULT '',
 estimate REAL,
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, issue_id, board_id),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE,
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_board_issues_order ON tracker_cache_board_issues(root_id, board_id, order_key, issue_id, estimate, read_at);

CREATE TABLE tracker_cache_issue_sprints (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 sprint_id TEXT NOT NULL CHECK(sprint_id <> ''),
 PRIMARY KEY (root_id, issue_id, sprint_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TABLE tracker_cache_changes (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 kind INTEGER NOT NULL CHECK(kind BETWEEN 1 AND 4),
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
 CHECK((kind = 3) = (board_id <> '')),
 CHECK(kind = 3 OR (from_number IS NULL AND to_number IS NULL)),
 CHECK(kind <> 3 OR (from_value = '' AND to_value = ''))
) WITHOUT ROWID;

CREATE TABLE tracker_cache_issue_details (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 opened_at INTEGER NOT NULL CHECK(opened_at > 0),
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 data BLOB,
 PRIMARY KEY (root_id, issue_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE,
 CHECK((read_at = 0) = (data IS NULL))
);
CREATE INDEX idx_tracker_cache_issue_details_opened ON tracker_cache_issue_details(root_id, opened_at, issue_id);

CREATE TABLE tracker_cache_worklogs (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
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

CREATE TABLE tracker_cache_worklog_days (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 author_id TEXT NOT NULL CHECK(author_id <> ''),
 day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 PRIMARY KEY (root_id, author_id, day)
) WITHOUT ROWID;

CREATE TABLE tracker_cache_sync (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL,
 cursor BLOB,
 cursor_read_at INTEGER NOT NULL DEFAULT 0 CHECK(cursor_read_at >= 0),
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 reconciled_at INTEGER NOT NULL DEFAULT 0 CHECK(reconciled_at >= 0),
 failed_at INTEGER NOT NULL DEFAULT 0 CHECK(failed_at >= 0),
 failure_kind TEXT NOT NULL DEFAULT '' CHECK(failure_kind IN ('', 'transient', 'rate_limited', 'setup', 'forbidden', 'tracker')),
 failure_message TEXT NOT NULL DEFAULT '',
 outcome_at INTEGER NOT NULL DEFAULT 0 CHECK(outcome_at >= failed_at),
 PRIMARY KEY (root_id, board_id),
 CHECK((failure_kind = '') = (failed_at = 0)),
 CHECK(failure_kind <> '' OR failure_message = '')
);

CREATE TRIGGER trg_tracker_cache_board_rows_insert AFTER INSERT ON tracker_cache_board_issues BEGIN
 UPDATE tracker_cache_issues SET board_rows = board_rows + 1 WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id;
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_board_rows_delete AFTER DELETE ON tracker_cache_board_issues BEGIN
 UPDATE tracker_cache_issues SET board_rows = board_rows - 1 WHERE root_id = OLD.root_id AND issue_id = OLD.issue_id;
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_order AFTER UPDATE OF order_key ON tracker_cache_board_issues
 WHEN OLD.order_key IS NOT NEW.order_key BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_status AFTER UPDATE OF status_id ON tracker_cache_issues
 WHEN OLD.status_id IS NOT NEW.status_id BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_join AFTER INSERT ON tracker_cache_issue_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_leave AFTER DELETE ON tracker_cache_issue_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = OLD.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = OLD.root_id AND issue_id = OLD.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_insert AFTER INSERT ON tracker_cache_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_delete AFTER DELETE ON tracker_cache_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_state AFTER UPDATE OF state ON tracker_cache_sprints
 WHEN OLD.state IS NOT NEW.state BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_insert AFTER INSERT ON tracker_cache_board_column_statuses BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_delete AFTER DELETE ON tracker_cache_board_column_statuses BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_move AFTER UPDATE OF position ON tracker_cache_board_column_statuses
 WHEN OLD.position IS NOT NEW.position BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_group_insert AFTER INSERT ON tracker_cache_statuses BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_group AFTER UPDATE OF status_group ON tracker_cache_statuses
 WHEN OLD.status_group IS NOT NEW.status_group BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id;
END;
CREATE TRIGGER trg_project_tracker_boards_retire AFTER DELETE ON project_tracker_boards
 WHEN NOT EXISTS (SELECT 1 FROM project_tracker_boards WHERE connection_id = OLD.connection_id AND board_id = OLD.board_id) BEGIN
 UPDATE tracker_cache_boards SET retired = 1 WHERE retired = 0 AND board_id = OLD.board_id
  AND root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.connection_id);
 DELETE FROM tracker_cache_sync WHERE board_id = OLD.board_id
  AND root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.connection_id);
END;
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
