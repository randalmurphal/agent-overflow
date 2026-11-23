package store

// Migration v162 creates every table of the issue tracker integration
// (docs/architecture/tracker-backend.md#tables). It is shipped and
// frozen: a later change to these tables is a later migration.
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
//     order, with at most one that opens first. A listing gives the board
//     a row in its connection's mirror (trg_project_tracker_boards_list),
//     and the delete of a board's last listing retires it
//     (trg_project_tracker_boards_retire).
//   - thread_tickets: one ticket per thread. It names the tracker kind and
//     site, not a connection, so a link survives a connection removed and
//     added again.
//
// The mirror (trackerCacheTablesV162SQL) is every table named
// tracker_cache_*: a cache of what the trackers hold, filled by sync and
// emptied by a restore. Each row reaches a tracker_cache_roots row through
// ON DELETE CASCADE keys, the root's link to its connection is the only
// SET NULL, and no other table references the mirror
// (TestTrackerCacheTablesCascadeFromTheRoot). A root's ID is an
// AUTOINCREMENT integer, so it is never reused and costs a byte or two in
// every row and index entry that names it. Times are Unix milliseconds,
// zero for none. Columns hold what a query filters, joins, sorts or orders
// writes by; data is the rest of a record, in a format only the caller
// knows. A read_at column is when the latest read admitted for the row
// began, whether or not it changed the content: a write from a read that
// began earlier is refused (superseded), an identical write from a later
// read moves read_at alone, and one from the same read changes nothing.
//
//   - tracker_cache_roots: one per mirror. connection_id is NULL once the
//     root is detached, by DetachMirror or by the connection's removal (ON
//     DELETE SET NULL); FinishMirrorCleanup then drains it in batches.
//     sweep advances with every sweep transaction that deletes an issue or
//     its history, so it is never repeated: a card, detail or history
//     written outside a board names the value its caller captured before
//     the read, and one naming an earlier value is refused, so content
//     read before an issue was swept never lands after it is kept again.
//   - tracker_cache_users, tracker_cache_statuses: changed_at is the read
//     that last changed the content, and done_changed_at the read that
//     last moved a status into or out of the done and canceled groups, so
//     a write's RETURNING tells a change from a newer identical read.
//   - tracker_cache_boards: one row per lifetime of a board in a root,
//     from its listing (or the root's creation while it is listed) to its
//     drain. lifetime is an AUTOINCREMENT ID, never reused: every board
//     write and section continuation names the lifetime its caller
//     captured, so nothing captured before the board was unlisted and
//     listed again is admitted or continued. read_at is zero until the
//     structure is stored. revision advances, through the
//     trg_tracker_cache_revision_* triggers and a status group change,
//     with every change that can add, remove or move a row in one of the
//     board's sections, so a section page continued at another revision is
//     refused. totals_revision advances, through the same triggers but the
//     order one, trg_tracker_cache_totals_estimate, the estimate reread and
//     an estimate definition change, with every change that can change a
//     section's total, so a patch read for a caller holding the totals
//     at it reads none. retired is set when the board's last listing goes; a retired
//     board is absent to reads and writes, FinishMirrorCleanup drains it,
//     and a board listed again gets a new lifetime then.
//     The estimate definition is estimate_unit and estimate_field, the
//     tracker's identity of what the estimates are of (a Jira field ID).
//     estimate_reset_at is the read of the latest structure that changed
//     the board's estimate definition: a row read no later than it may hold
//     a number in the old definition, which reads show as unknown, a row
//     read again after it advances its issue's boards_rev
//     (trg_tracker_cache_board_rows_estimate_reread), and a row page
//     stores its cursor only when it names the value its walk began under.
//     rows_removed_at
//     is the latest read that found issues gone from the board: a read
//     that began before it adds no row. listing_read_at is the read of the
//     newest complete listing reconciled: an older listing, and a read by
//     ID that began before it, removes no row.
//   - tracker_cache_board_unexplained: a status (kind 1), user (2) or
//     sprint (3) a page of the board's rows named that the mirror lacked.
//     named_at is the read of the page that named it while pending; the
//     board is due a structure read while a pending mark is no earlier
//     than the structure's read_at (idx_tracker_cache_board_unexplained_pending
//     serves that probe). A complete structure read turns the pending
//     marks no later than it into unexplained ones (named_at zero), a
//     chunk per transaction; a row naming an unexplained reference no
//     longer makes the board due. A structure read that supplies a marked
//     sprint removes its mark. A mark no row names any more stays until
//     its board goes.
//   - tracker_cache_board_column_statuses: a status sits in at most one
//     column of a board; rank is its place within the column.
//   - tracker_cache_sprints: a sprint as one board shows it; a Jira sprint
//     can show on several boards and names the board it was made on
//     (origin_board_id). position is the order the structure read gave.
//   - tracker_cache_issues: read_at is when the latest read admitted for
//     the card began, identical or not, and orders the card and its
//     sprints: a card with the same update time from a read that began
//     earlier is superseded. No index or trigger reads it, so an identical
//     read moves it alone. status_since is when the card's status began,
//     derived from the card and the stored history whenever either is
//     written (mirrorStatusSinceSQL); while the history does not cover
//     the card it stands until the card's status changes, then is zero. history_from is the time the stored changes are complete
//     from (zero: from creation). history_for is the update time the read
//     was made for (zero: none read), history_answered_at when its answer
//     arrived by the reader's steady clock, and history_wait how long
//     after that the issue is due again, zero when the read is final for
//     history_for: an issue updated later than history_for is due at once,
//     one waiting is due once its wait is over (mirrorHistoryDue), and the
//     history covers the card when it is final for its update time.
//     idx_tracker_cache_issues_history_waiting keys a waiting issue by the
//     end of its wait and idx_tracker_cache_issues_history_answered by its
//     answer, so a due read seeks those past their wait and those answered
//     after the reader's now, and never visits one inside its wait.
//     history_receipt counts the history writes admitted and the cleanup's
//     resets: a write names the count its read was listed at and is
//     superseded once it moved.
//     boards_rev advances whenever the issue gains a
//     board row or a row of it is read again after its board's estimate
//     changed (estimate_reset_at), since estimate changes are read per
//     board;
//     history_boards_rev is the boards_rev the stored history was read
//     for, so an issue is history-due while it is behind;
//     idx_tracker_cache_issues_history_due holds the issues due for a new
//     version or boards. history_gen is the generation of changes that is the
//     issue's history; history_dirty marks an issue whose other
//     generations, staged or replaced, the history write or
//     FinishMirrorCleanup has yet to delete
//     (idx_tracker_cache_issues_history_dirty).
//     board_rows counts the issue's board rows, kept by the
//     trg_tracker_cache_board_rows_* triggers (a row's keys are never
//     updated); idx_tracker_cache_issues_boardless holds the issues only a
//     thread's link can keep, which the cleanup sweep reads;
//     idx_tracker_cache_issues_parent finds the children of a changed
//     issue, whose rows show its key and title.
//     idx_tracker_cache_issues_status holds each issue's status, so a walk
//     of a board's rows that needs only the status (the counts) probes it
//     and never the table, whose rows hold the card's data.
//   - tracker_cache_board_issues: keyed by issue first, so the primary key
//     serves the keep rule, an issue's rows and the cascade of an issue's
//     delete; idx_..._order serves a section page, the counts, a
//     reconcile, a board's drain and the foreign key work of a board's
//     start and delete. It covers every column of the row: SQLite plans a
//     lookup through an index that does not cover a WITHOUT ROWID table's
//     row as dearer than a walk of the primary key's root_id prefix, which
//     reads every row of the root, and a cascade reads the whole row. So
//     moving read_at or listed writes the index too. listed is 1 once a
//     listing of the board named the issue: a delta page (a search of the
//     same index) or a listing a reconcile was given. A complete listing
//     removes only listed rows; an unlisted row it leaves out has only a
//     read by ID's word that the board holds it, so a read by ID judges it.
//   - tracker_cache_issue_sprints: the sprints an issue's card names. A
//     sprint the mirror does not keep can be named, so it has no key to
//     tracker_cache_sprints. idx_tracker_cache_issue_sprints_sprint holds
//     a sprint's members, so a sprint's page and the counts read them
//     from the board's sprints, not per board row.
//   - tracker_cache_changes: gen is the generation the change belongs to
//     (the issue's history_gen is its history); kind is 1 status, 2
//     sprint, 3 estimate, 4 assignee; seq orders one kind's changes of one
//     issue. A status or
//     assignee change holds IDs in from_value and to_value, a sprint change
//     the sorted sprint IDs as a JSON array, an estimate change its numbers
//     and the board it was made on.
//   - tracker_cache_issue_details: opened_at is when the issue was last
//     opened, which orders eviction; data is the document, NULL until one
//     is stored, and read_at when its read began, or the opening that
//     created the row while no document from a read no earlier is stored:
//     a detail the cap dropped starts again at its next opening, so no
//     document read before that lands.
//   - tracker_cache_worklogs: logged time; issue_id has no key, since a
//     worklog's issue need not be in the mirror.
//     tracker_cache_worklog_days holds, per author and day, the latest read
//     that replaced the day, empty or not.
//   - tracker_cache_sync: one row per scope, an empty board_id being the
//     root's linked issues. A board scope's row goes when the board's last
//     listing goes. outcome_at is the time of the last failure or clear.
const trackerMigrationVersion = 162

const trackerV162SQL = trackerDurableTablesV162SQL + trackerCacheTablesV162SQL

const trackerCacheTablesV162SQL = `
CREATE TABLE tracker_cache_roots (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 connection_id TEXT UNIQUE REFERENCES tracker_connections(id) ON DELETE SET NULL,
 created_at INTEGER NOT NULL CHECK(created_at > 0),
 sweep INTEGER NOT NULL DEFAULT 1 CHECK(sweep >= 1)
);

CREATE TABLE tracker_cache_users (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL CHECK(user_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 data BLOB NOT NULL DEFAULT x'',
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 changed_at INTEGER NOT NULL CHECK(changed_at BETWEEN 1 AND read_at),
 PRIMARY KEY (root_id, user_id)
) WITHOUT ROWID;

CREATE TABLE tracker_cache_statuses (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 status_id TEXT NOT NULL CHECK(status_id <> ''),
 name TEXT NOT NULL DEFAULT '',
 status_group INTEGER NOT NULL CHECK(status_group BETWEEN 1 AND 5),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 changed_at INTEGER NOT NULL CHECK(changed_at BETWEEN 1 AND read_at),
 done_changed_at INTEGER NOT NULL CHECK(done_changed_at BETWEEN 0 AND changed_at),
 PRIMARY KEY (root_id, status_id)
) WITHOUT ROWID;

CREATE TABLE tracker_cache_boards (
 lifetime INTEGER PRIMARY KEY AUTOINCREMENT,
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 board_id TEXT NOT NULL CHECK(board_id <> ''),
 project_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '',
 plans_sprints INTEGER NOT NULL DEFAULT 0 CHECK(plans_sprints IN (0, 1)),
 estimate_unit INTEGER NOT NULL DEFAULT 0 CHECK(estimate_unit IN (0, 1, 2)),
 estimate_field TEXT NOT NULL DEFAULT '',
 data BLOB NOT NULL DEFAULT x'',
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 rows_removed_at INTEGER NOT NULL DEFAULT 0 CHECK(rows_removed_at >= 0),
 listing_read_at INTEGER NOT NULL DEFAULT 0 CHECK(listing_read_at >= 0),
 estimate_reset_at INTEGER NOT NULL DEFAULT 0 CHECK(estimate_reset_at >= 0),
 revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
 totals_revision INTEGER NOT NULL DEFAULT 0 CHECK(totals_revision >= 0),
 retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0, 1)),
 UNIQUE (root_id, board_id)
);
CREATE INDEX idx_tracker_cache_boards_retired ON tracker_cache_boards(root_id, board_id) WHERE retired = 1;

CREATE TABLE tracker_cache_board_unexplained (
 root_id INTEGER NOT NULL,
 board_id TEXT NOT NULL,
 kind INTEGER NOT NULL CHECK(kind IN (1, 2, 3)),
 ref_id TEXT NOT NULL CHECK(ref_id <> ''),
 named_at INTEGER NOT NULL CHECK(named_at >= 0),
 PRIMARY KEY (root_id, board_id, kind, ref_id),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_board_unexplained_pending ON tracker_cache_board_unexplained(root_id, board_id, named_at) WHERE named_at > 0;

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
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 status_since INTEGER NOT NULL DEFAULT 0 CHECK(status_since >= 0),
 history_from INTEGER NOT NULL DEFAULT 0 CHECK(history_from >= 0),
 history_for INTEGER NOT NULL DEFAULT 0 CHECK(history_for >= 0),
 history_answered_at INTEGER NOT NULL DEFAULT 0 CHECK(history_answered_at >= 0),
 history_wait INTEGER NOT NULL DEFAULT 0 CHECK(history_wait >= 0),
 history_receipt INTEGER NOT NULL DEFAULT 0 CHECK(history_receipt >= 0),
 history_gen INTEGER NOT NULL DEFAULT 0 CHECK(history_gen >= 0),
 history_dirty INTEGER NOT NULL DEFAULT 0 CHECK(history_dirty IN (0, 1)),
 boards_rev INTEGER NOT NULL DEFAULT 0 CHECK(boards_rev >= 0),
 history_boards_rev INTEGER NOT NULL DEFAULT 0 CHECK(history_boards_rev >= 0),
 board_rows INTEGER NOT NULL DEFAULT 0 CHECK(board_rows >= 0),
 data BLOB NOT NULL DEFAULT x'',
 PRIMARY KEY (root_id, issue_id)
);
CREATE INDEX idx_tracker_cache_issues_history_due ON tracker_cache_issues(root_id, issue_id)
 WHERE history_for < updated_at OR history_boards_rev < boards_rev;
CREATE INDEX idx_tracker_cache_issues_history_waiting ON tracker_cache_issues(root_id, history_answered_at + history_wait)
 WHERE history_wait > 0;
CREATE INDEX idx_tracker_cache_issues_history_answered ON tracker_cache_issues(root_id, history_answered_at)
 WHERE history_wait > 0;
CREATE INDEX idx_tracker_cache_issues_boardless ON tracker_cache_issues(root_id, issue_id) WHERE board_rows = 0;
CREATE INDEX idx_tracker_cache_issues_history_dirty ON tracker_cache_issues(root_id, issue_id) WHERE history_dirty = 1;
CREATE INDEX idx_tracker_cache_issues_parent ON tracker_cache_issues(root_id, parent_id) WHERE parent_id <> '';
CREATE INDEX idx_tracker_cache_issues_status ON tracker_cache_issues(root_id, issue_id, status_id);

CREATE TABLE tracker_cache_board_issues (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 board_id TEXT NOT NULL,
 order_key TEXT NOT NULL DEFAULT '',
 estimate REAL,
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 listed INTEGER NOT NULL DEFAULT 0 CHECK(listed IN (0, 1)),
 PRIMARY KEY (root_id, issue_id, board_id),
 FOREIGN KEY (root_id, board_id) REFERENCES tracker_cache_boards(root_id, board_id) ON DELETE CASCADE,
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_board_issues_order ON tracker_cache_board_issues(root_id, board_id, order_key, issue_id, estimate, read_at, listed);

CREATE TABLE tracker_cache_issue_sprints (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 sprint_id TEXT NOT NULL CHECK(sprint_id <> ''),
 PRIMARY KEY (root_id, issue_id, sprint_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_tracker_cache_issue_sprints_sprint ON tracker_cache_issue_sprints(root_id, sprint_id, issue_id);

CREATE TABLE tracker_cache_changes (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 gen INTEGER NOT NULL CHECK(gen >= 0),
 kind INTEGER NOT NULL CHECK(kind BETWEEN 1 AND 4),
 seq INTEGER NOT NULL CHECK(seq >= 0),
 at INTEGER NOT NULL CHECK(at > 0),
 author_id TEXT NOT NULL DEFAULT '',
 board_id TEXT NOT NULL DEFAULT '',
 from_value TEXT NOT NULL DEFAULT '',
 to_value TEXT NOT NULL DEFAULT '',
 from_number REAL,
 to_number REAL,
 PRIMARY KEY (root_id, issue_id, gen, kind, seq),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE,
 CHECK((kind = 3) = (board_id <> '')),
 CHECK(kind = 3 OR (from_number IS NULL AND to_number IS NULL)),
 CHECK(kind <> 3 OR (from_value = '' AND to_value = ''))
) WITHOUT ROWID;

CREATE TABLE tracker_cache_issue_details (
 root_id INTEGER NOT NULL,
 issue_id TEXT NOT NULL,
 opened_at INTEGER NOT NULL CHECK(opened_at > 0),
 read_at INTEGER NOT NULL CHECK(read_at > 0),
 data BLOB,
 PRIMARY KEY (root_id, issue_id),
 FOREIGN KEY (root_id, issue_id) REFERENCES tracker_cache_issues(root_id, issue_id) ON DELETE CASCADE
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
 UPDATE tracker_cache_issues SET board_rows = board_rows + 1, boards_rev = boards_rev + 1
 WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id;
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_board_rows_delete AFTER DELETE ON tracker_cache_board_issues BEGIN
 UPDATE tracker_cache_issues SET board_rows = board_rows - 1 WHERE root_id = OLD.root_id AND issue_id = OLD.issue_id;
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_board_rows_estimate_reread AFTER UPDATE OF read_at ON tracker_cache_board_issues
 WHEN OLD.read_at <= (SELECT bl.estimate_reset_at FROM tracker_cache_boards bl WHERE bl.root_id = NEW.root_id AND bl.board_id = NEW.board_id)
  AND NEW.read_at > (SELECT bl.estimate_reset_at FROM tracker_cache_boards bl WHERE bl.root_id = NEW.root_id AND bl.board_id = NEW.board_id) BEGIN
 UPDATE tracker_cache_issues SET boards_rev = boards_rev + 1 WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id;
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_totals_estimate AFTER UPDATE OF estimate ON tracker_cache_board_issues
 WHEN OLD.estimate IS NOT NEW.estimate BEGIN
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_order AFTER UPDATE OF order_key ON tracker_cache_board_issues
 WHEN OLD.order_key IS NOT NEW.order_key BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_status AFTER UPDATE OF status_id ON tracker_cache_issues
 WHEN OLD.status_id IS NOT NEW.status_id BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_join AFTER INSERT ON tracker_cache_issue_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_leave AFTER DELETE ON tracker_cache_issue_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = OLD.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = OLD.root_id AND issue_id = OLD.issue_id);
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_insert AFTER INSERT ON tracker_cache_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_delete AFTER DELETE ON tracker_cache_sprints BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_sprint_state AFTER UPDATE OF state ON tracker_cache_sprints
 WHEN OLD.state IS NOT NEW.state BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_insert AFTER INSERT ON tracker_cache_board_column_statuses BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_delete AFTER DELETE ON tracker_cache_board_column_statuses BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = OLD.root_id AND board_id = OLD.board_id;
END;
CREATE TRIGGER trg_tracker_cache_revision_column_move AFTER UPDATE OF position ON tracker_cache_board_column_statuses
 WHEN OLD.position IS NOT NEW.position BEGIN
 UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id AND board_id = NEW.board_id;
END;
CREATE TRIGGER trg_project_tracker_boards_list AFTER INSERT ON project_tracker_boards BEGIN
 INSERT INTO tracker_cache_boards(root_id, board_id) SELECT id, NEW.board_id FROM tracker_cache_roots
  WHERE connection_id = NEW.connection_id ON CONFLICT DO NOTHING;
END;
CREATE TRIGGER trg_project_tracker_boards_retire AFTER DELETE ON project_tracker_boards
 WHEN NOT EXISTS (SELECT 1 FROM project_tracker_boards WHERE connection_id = OLD.connection_id AND board_id = OLD.board_id) BEGIN
 UPDATE tracker_cache_boards SET retired = 1 WHERE retired = 0 AND board_id = OLD.board_id
  AND root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.connection_id);
 DELETE FROM tracker_cache_sync WHERE board_id = OLD.board_id
  AND root_id = (SELECT id FROM tracker_cache_roots WHERE connection_id = OLD.connection_id);
END;
`

const trackerDurableTablesV162SQL = `
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
