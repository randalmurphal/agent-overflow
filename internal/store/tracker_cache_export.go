package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The board export: what a dashboard's metrics read. ExportMirrorBoard
// fills the main database of a connection the caller opened (an empty,
// private, in-memory database: trackerdash's) with one board's rows, read
// from this store's file attached read-only, and detaches the file again.
// The caller never learns the file's path; it gets a database holding the
// board and nothing else.
//
// What must hold:
//   - Every row the export writes is the board's: its board rows, their
//     issues, memberships of the board's sprints, the changes of the
//     issues' current history generation (an estimate change only of this
//     board), and the statuses, people and epics those name. A board the
//     mirror does not show (mirrorBoardShown) exports nothing.
//   - Each statement searches the mirror by the board's keys, root and
//     board first, then each row's issue: its cost follows the board, never
//     the mirror's or the file's size (TestMirrorExportSearchesByTheBoard).
//   - Status groups are the project's regrouping over the root's catalog,
//     as the board's sections count them, and a sprint member counted done
//     at the sprint's completion by the pace's rule (mirrorStatusAtCompletionSQL,
//     mirrorDoneStatusesSQL), so the pace a dashboard shows is the Backlog's.
//   - The whole fill is one read transaction of the file: one state of the
//     mirror. The file is detached before the export returns, whatever it
//     returns; a detach that fails is the export's error.

// MirrorExportTables are the relations the export writes, the only tables
// of the database it fills.
var MirrorExportTables = []string{"board", "tickets", "statuses", "people", "epics", "sprints", "sprint_tickets", "changes", "sprint_finished"}

// MirrorExportSchema is the export's relations. Times are milliseconds;
// a time the mirror does not hold is NULL. tickets.history_from is when
// the ticket's changes are complete from, NULL while its history is not
// read. started_known is 1 when its history shows when it started: the
// status it had when its history begins (what its first status change
// left, else its status now) was in neither an in-progress nor a done
// group, so a move into progress is in the history (mirrorExportStartKnownSQL).
// started_at is then its first change into an in-progress status, and
// NULL otherwise. done_at, for a ticket whose status counts as done, is
// its last change into a done status, NULL when its changes hold none.
// Groups are the regrouping's over the root's catalog. tickets.open is 1
// for a ticket the board's sections count as not done (mirrorRegroupedNotDone:
// not done, canceled or left out, an unclassified status open), and
// tickets.counts 0 for one whose status the regrouping leaves out of
// counts (mirrorLeftOut). sprint_finished holds each member of a closed
// sprint, done when its status at the sprint's completion counted as done
// (the pace's rule), and each member of an active sprint, done when its
// status counts as done now. A status group is named as the project setup
// names it (mirrorRegroupedGroupNameSQL), NULL for a status the mirror does
// not know.
const MirrorExportSchema = `
CREATE TABLE main.board(name TEXT NOT NULL, plans_sprints INTEGER NOT NULL, estimates TEXT NOT NULL);
CREATE TABLE main.statuses(id TEXT PRIMARY KEY, name TEXT NOT NULL, status_group TEXT) WITHOUT ROWID;
CREATE TABLE main.tickets(id TEXT PRIMARY KEY, key TEXT NOT NULL, title TEXT NOT NULL, type_level INTEGER NOT NULL,
  status TEXT, status_group TEXT, assignee TEXT, parent TEXT, epic TEXT, created_at INTEGER, updated_at INTEGER, resolved_at INTEGER,
  status_since INTEGER, points REAL, history_from INTEGER, started_known INTEGER NOT NULL, started_at INTEGER, done_at INTEGER,
  open INTEGER NOT NULL, counts INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE main.epics(id TEXT PRIMARY KEY, key TEXT NOT NULL, title TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE main.sprints(id TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL, start_at INTEGER, end_at INTEGER,
  completed_at INTEGER, position INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE main.sprint_tickets(sprint TEXT NOT NULL, ticket TEXT NOT NULL, PRIMARY KEY (sprint, ticket)) WITHOUT ROWID;
CREATE TABLE main.changes(ticket TEXT NOT NULL, kind TEXT NOT NULL, seq INTEGER NOT NULL, at INTEGER NOT NULL, author TEXT,
  from_value TEXT, to_value TEXT, from_number REAL, to_number REAL, PRIMARY KEY (ticket, kind, seq)) WITHOUT ROWID;
CREATE TABLE main.sprint_finished(sprint TEXT NOT NULL, ticket TEXT NOT NULL, done INTEGER NOT NULL, PRIMARY KEY (sprint, ticket)) WITHOUT ROWID;
CREATE TABLE main.people(id TEXT PRIMARY KEY, name TEXT NOT NULL) WITHOUT ROWID;
`

// mirrorExportSchemaName is the attached file's schema name.
const mirrorExportSchemaName = "mirror_file"

// mirrorExportBoardSQL reads the board's state: ?1 root, ?2 board.
var mirrorExportBoardSQL = `SELECT b.name, b.plans_sprints, b.estimate_unit, b.estimate_reset_at FROM tracker_cache_boards b
	WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ` + mirrorBoardShown(intParam("?1"), "?2")

// mirrorExportStatusChangeSQL is the changes into a status of list of
// issue i's current history generation, by key: agg(c.at) of them.
func mirrorExportStatusChangeSQL(agg, list string) string {
	return `(SELECT ` + agg + `(c.at) FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1
	        AND c.to_value IN (` + list + `))`
}

// mirrorExportStartKnownSQL holds for issue i when its read history shows
// its start: the status it had when the history begins, what its first
// status change left or else its status now, was in neither an
// in-progress nor a done group under the regrouping ?3. A Jira history
// begins at creation; a Linear one minutes after (its creation fold), so
// an issue made in progress then is not one whose start it shows.
var mirrorExportStartKnownSQL = `(i.history_for > 0 AND coalesce(
	    (SELECT c.from_value FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1 ORDER BY c.seq LIMIT 1),
	    i.status_id) NOT IN (` + mirrorGroupStatusesSQL("?3", MirrorGroupInProgress) + ` UNION ALL ` + mirrorDoneStatusesSQL("?3") + `))`

// mirrorExportFills are the fill statements in order: ?1 root, ?2 board,
// ?3 the regrouping (a JSON object, '{}' for none), ?4 the board's
// estimate reset, ?5 whether the board has an estimate (0 or 1). Each
// names the snapshot's tables by main and the file's unqualified: the
// snapshot names none of the file's tables.
var mirrorExportFills = []struct{ table, sql string }{
	{"board", `INSERT INTO main.board(name, plans_sprints, estimates)
	  SELECT b.name, b.plans_sprints, CASE b.estimate_unit WHEN ` + strconv.Itoa(MirrorEstimatePoints) + ` THEN 'points' WHEN ` +
		strconv.Itoa(MirrorEstimateTime) + ` THEN 'time' ELSE 'none' END
	  FROM tracker_cache_boards b WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`},
	{"statuses", `INSERT INTO main.statuses(id, name, status_group)
	  SELECT st.status_id, st.name, ` + mirrorRegroupedGroupNameSQL(`coalesce((SELECT o.value FROM json_each(?3) o WHERE o.key = st.status_id), st.status_group)`) + `
	  FROM tracker_cache_statuses st WHERE st.root_id = CAST(?1 AS INTEGER)`},
	{"tickets", `INSERT INTO main.tickets(id, key, title, type_level, status, status_group, assignee, parent, epic,
	    created_at, updated_at, resolved_at, status_since, points, history_from, started_known, started_at, done_at, open, counts)
	  SELECT i.issue_id, i.issue_key, i.title, i.type_level, nullif(i.status_id, ''),
	    coalesce((SELECT ms.status_group FROM main.statuses ms WHERE ms.id = i.status_id),
	      ` + mirrorRegroupedGroupNameSQL(`(SELECT o.value FROM json_each(?3) o WHERE o.key = i.status_id)`) + `),
	    nullif(i.assignee_id, ''), nullif(i.parent_id, ''),
	    CASE WHEN p.type_level = 1 THEN p.issue_id WHEN gp.type_level = 1 THEN gp.issue_id END,
	    nullif(i.created_at, 0), i.updated_at, nullif(i.resolved_at, 0), nullif(i.status_since, 0),
	    iif(CAST(?5 AS INTEGER) = 1, ` + mirrorRowEstimate("CAST(?4 AS INTEGER)") + `, NULL),
	    iif(i.history_for > 0, iif(i.history_from > 0, i.history_from, i.created_at), NULL),
	    iif(` + mirrorExportStartKnownSQL + `, 1, 0),
	    iif(` + mirrorExportStartKnownSQL + `, ` + mirrorExportStatusChangeSQL("min", mirrorGroupStatusesSQL("?3", MirrorGroupInProgress)) + `, NULL),
	    iif(i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `), ` + mirrorExportStatusChangeSQL("max", mirrorDoneStatusesSQL("?3")) + `, NULL),
	    iif(` + mirrorRegroupedNotDone("?3") + `, 1, 0), iif(` + mirrorLeftOut("?3") + `, 0, 1)
	  FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  LEFT JOIN tracker_cache_issues p ON p.root_id = i.root_id AND p.issue_id = i.parent_id AND i.parent_id <> ''
	    AND ` + mirrorIssueKept("p.root_id", "p.issue_id") + `
	  LEFT JOIN tracker_cache_issues gp ON gp.root_id = p.root_id AND gp.issue_id = p.parent_id AND p.parent_id <> ''
	    AND ` + mirrorIssueKept("gp.root_id", "gp.issue_id") + `
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`},
	{"epics", `INSERT INTO main.epics(id, key, title)
	  SELECT e.issue_id, e.issue_key, e.title FROM (SELECT DISTINCT epic FROM main.tickets WHERE epic IS NOT NULL) x
	  CROSS JOIN tracker_cache_issues e ON e.root_id = CAST(?1 AS INTEGER) AND e.issue_id = x.epic`},
	{"sprints", `INSERT INTO main.sprints(id, name, state, start_at, end_at, completed_at, position)
	  SELECT sp.sprint_id, sp.name, CASE sp.state WHEN ` + strconv.Itoa(MirrorSprintFuture) + ` THEN 'future' WHEN ` +
		strconv.Itoa(MirrorSprintActive) + ` THEN 'active' ELSE 'closed' END,
	    nullif(sp.start_at, 0), nullif(sp.end_at, 0), nullif(sp.completed_at, 0), sp.position
	  FROM tracker_cache_sprints sp WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2`},
	{"sprint_tickets", `INSERT INTO main.sprint_tickets(sprint, ticket)
	  SELECT m.sprint_id, m.issue_id FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = b.root_id AND m.issue_id = b.issue_id
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND m.sprint_id IN (SELECT id FROM main.sprints)`},
	{"changes", `INSERT INTO main.changes(ticket, kind, seq, at, author, from_value, to_value, from_number, to_number)
	  SELECT c.issue_id, CASE c.kind WHEN 1 THEN 'status' WHEN 2 THEN 'sprint' WHEN 3 THEN 'estimate' ELSE 'assignee' END,
	    c.seq, c.at, nullif(c.author_id, ''), nullif(c.from_value, ''), nullif(c.to_value, ''), c.from_number, c.to_number
	  FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  CROSS JOIN tracker_cache_changes c ON c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND (c.kind <> 3 OR c.board_id = ?2)`},
	{"sprint_finished", `WITH l AS MATERIALIZED (
	    SELECT sprint_id, completed_at FROM tracker_cache_sprints
	     WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at > 0)
	  INSERT INTO main.sprint_finished(sprint, ticket, done)
	  SELECT l.sprint_id, i.issue_id, coalesce(` + mirrorStatusAtCompletionSQL + ` IN (` + mirrorDoneStatusesSQL("?3") + `), 0)
	  FROM l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id`},
	{"active sprints' members", `INSERT INTO main.sprint_finished(sprint, ticket, done)
	  SELECT sprint_tickets.sprint, sprint_tickets.ticket, coalesce(tickets.status_group = 'done', 0)
	  FROM main.sprints JOIN main.sprint_tickets ON sprint_tickets.sprint = sprints.id
	  JOIN main.tickets ON tickets.id = sprint_tickets.ticket WHERE sprints.state = 'active'`},
	{"people", `INSERT INTO main.people(id, name)
	  SELECT u.user_id, u.name FROM (
	    SELECT assignee AS id FROM main.tickets WHERE assignee IS NOT NULL
	    UNION SELECT author FROM main.changes WHERE author IS NOT NULL
	    UNION SELECT from_value FROM main.changes WHERE kind = 'assignee' AND from_value IS NOT NULL
	    UNION SELECT to_value FROM main.changes WHERE kind = 'assignee' AND to_value IS NOT NULL) x
	  CROSS JOIN tracker_cache_users u ON u.root_id = CAST(?1 AS INTEGER) AND u.user_id = x.id`},
}

// MirrorExport is what ExportMirrorBoard says of the board it exported.
type MirrorExport struct {
	// Name is the board's name, EstimateUnit its estimate's (MirrorEstimate*)
	// and PlansSprints whether it plans sprints.
	Name         string
	EstimateUnit int
	PlansSprints bool
}

// ErrMirrorExportUnsupported is an export from a store with no database
// file to attach (an in-memory store).
var ErrMirrorExportUnsupported = errors.New("store: this store has no database file to export a board from")

// ExportMirrorBoard fills the main database of conn, which must be empty,
// with board boardID of root rootID as the export's relations hold it
// (MirrorExportTables), read with regroup in one read transaction of this
// store's file, attached to conn read-only for the fill and detached
// before it returns. ok is false, with nothing filled, for a board the
// mirror does not show. It takes a history read slot, as every read that
// walks a board does; ctx bounds the wait for it and every statement.
func (s *Store) ExportMirrorBoard(ctx context.Context, conn *sql.Conn, rootID int64, boardID string, regroup MirrorRegroup) (export MirrorExport, ok bool, err error) {
	if s.path == "" || s.path == ":memory:" || strings.Contains(s.path, "mode=memory") {
		return MirrorExport{}, false, ErrMirrorExportUnsupported
	}
	release, err := s.historyReads.acquire(ctx)
	if err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: wait for a history read slot to export board %s: %w", boardID, err)
	}
	defer release()
	if _, err := conn.ExecContext(ctx, MirrorExportSchema); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: create the export's relations: %w", err)
	}
	// busy_timeout is the read pool's: a read of the WAL waits out a
	// checkpoint's brief exclusive moment instead of failing.
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: set the export's busy timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `ATTACH ? AS `+mirrorExportSchemaName, mirrorExportURI(s.path)); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: attach the database read-only to export board %s: %w", boardID, err)
	}
	defer func() {
		// The detach runs on a context of its own: a canceled export must
		// still leave the connection holding the snapshot alone.
		if _, detachErr := conn.ExecContext(context.WithoutCancel(ctx), `DETACH `+mirrorExportSchemaName); detachErr != nil {
			err = errors.Join(err, fmt.Errorf("store: detach the database after exporting board %s: %w", boardID, detachErr))
			ok = false
		}
	}()
	export, ok, err = exportMirrorBoardTx(ctx, conn, rootID, boardID, regroup)
	return export, ok, err
}

// exportMirrorBoardTx runs the fill in one transaction on conn, the file
// attached.
func exportMirrorBoardTx(ctx context.Context, conn *sql.Conn, rootID int64, boardID string, regroup MirrorRegroup) (MirrorExport, bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: begin the export of board %s: %w", boardID, err)
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback() // the transaction wrote nothing that outlives it; its error is the one returned
		}
	}()
	var export MirrorExport
	var resetAt int64
	err = tx.QueryRowContext(ctx, mirrorExportBoardSQL, rootID, boardID).Scan(&export.Name, &export.PlansSprints, &export.EstimateUnit, &resetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorExport{}, false, nil
	}
	if err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: read board %s to export it: %w", boardID, err)
	}
	groups := regroup.statuses
	if groups == "" {
		groups = "{}"
	}
	estimated := 0
	if export.EstimateUnit != MirrorEstimateNone {
		estimated = 1
	}
	for _, fill := range mirrorExportFills {
		if _, err := tx.ExecContext(ctx, fill.sql, rootID, boardID, groups, resetAt, estimated); err != nil {
			return MirrorExport{}, false, fmt.Errorf("store: export board %s's %s: %w", boardID, fill.table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: commit the export of board %s: %w", boardID, err)
	}
	commit = true
	return export, true, nil
}

// mirrorExportURI is the read-only URI of the database file at path.
func mirrorExportURI(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	return u.String()
}
