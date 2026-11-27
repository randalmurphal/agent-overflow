package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
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
//     board), and the statuses (and the board's columns'), people and epics
//     those name. A board the mirror does not show (mirrorBoardShown)
//     exports nothing.
//   - It fills the relations the caller asks for, what their fills read of
//     the copy, and the board and its sprints; the rest stay empty. So a
//     count whose metrics read no changes copies none, and its people are
//     the assignees alone. A column of MirrorExportColumns is filled only
//     when asked for by name, and is NULL otherwise: its value is decoded
//     from each ticket's card blob, in the shape the caller gives
//     (MirrorCardShape), which only the caller knows.
//   - Each statement searches the mirror by the board's keys, root and
//     board first, then each row's issue: its cost follows the board, never
//     the mirror's or the file's size (TestMirrorExportSearchesByTheBoard),
//     and what it reads per board row does not grow with the board's
//     sprints (TestMirrorExportReadsEachRowOnce).
//   - Status groups are the project's regrouping over the root's catalog,
//     as the board's sections count them, and a sprint member counted done
//     at the sprint's completion by the pace's rule (mirrorStatusAtCompletionSQL,
//     mirrorDoneStatusesSQL), so the pace a dashboard shows is the Backlog's.
//   - The whole fill is one read transaction of the file: one state of the
//     mirror, whose first-read fact (MirrorExport.FirstReadDone) it reads
//     in the same transaction. The file is detached before the export
//     returns, whatever it returns; a detach that fails is the export's
//     error.

// MirrorExportTables are the relations the export writes, the only tables
// of the database it fills.
var MirrorExportTables = []string{"board", "tickets", "statuses", "people", "epics", "sprints", "sprint_tickets", "changes", "sprint_finished", "labels",
	"status_intervals", "sprint_intervals", "threads", "worklogs", "worklog_days"}

// mirrorExportCardColumns are the tickets' columns decoded from the card
// blob, in the tickets fill's order: each one's name in the export
// (relation.column) and the member of the blob that holds it. An export
// fills one only when asked for it by that name, as it is asked for a
// relation, and selects NULL for it otherwise (mirrorExportFill.text),
// so a count that reads none of them decodes no blob.
var mirrorExportCardColumns = []struct {
	name   string
	member func(MirrorCardShape) string
}{
	{"tickets.type", func(c MirrorCardShape) string { return c.Type }},
	{"tickets.priority", func(c MirrorCardShape) string { return c.Priority }},
}

// MirrorExportColumns are the names of mirrorExportCardColumns, the
// columns an export fills only when asked for them.
var MirrorExportColumns = func() []string {
	names := make([]string, len(mirrorExportCardColumns))
	for i, c := range mirrorExportCardColumns {
		names[i] = c.name
	}
	return names
}()

// MirrorCardShape is where a card blob (MirrorCard.Data) holds what the
// export decodes from it, as the blob's owner writes it: the version a
// blob of this shape has and the member holding it, the members holding
// the type's and the priority's names and the labels, and each label's
// ID and name members. A blob of another version gives no names and no
// labels.
type MirrorCardShape struct {
	Version                               int
	VersionMember, Type, Priority, Labels string
	LabelID, LabelName                    string
}

// mirrorExportCardArg is the parameter of the first column of
// mirrorExportCardColumns, the others following in order (args).
const mirrorExportCardArg = 11

// mirrorExportWindowArg is the parameter of the earliest instant a count
// can ask of the intervals (mirrorExportEarliest), after the card blob's
// shape.
var mirrorExportWindowArg = mirrorExportCardArg + len(mirrorExportCardColumns)

// mirrorExportLoggedArg is the parameter of the first day the logged
// time holds (MirrorExport.LoggedFrom), after mirrorExportWindowArg.
var mirrorExportLoggedArg = mirrorExportWindowArg + 1

// args is the shape as the fills bind it, ?6 to ?10 and then a member
// path for each column of mirrorExportCardColumns (mirrorExportFills).
func (c MirrorCardShape) args() []any {
	path := func(member string) string { return "$." + member }
	args := []any{c.Version, path(c.VersionMember), path(c.Labels), path(c.LabelID), path(c.LabelName)}
	for _, column := range mirrorExportCardColumns {
		args = append(args, path(column.member(c)))
	}
	return args
}

// MirrorExportSchema is the export's relations, whose comments say what
// each column holds; an agent writing a widget reads them as written
// (trackerdash.Rules). How the fill derives them: started_known is
// mirrorExportStartKnownSQL, the status a ticket's history begins with
// being what its first status change left, else its status now; open is
// mirrorRegroupedNotDone (an unclassified status open) and counts 0 is
// mirrorLeftOut; a status group is named by mirrorRegroupedGroupNameSQL
// over the root's catalog; a closed sprint's done is the pace's rule
// (mirrorStatusAtCompletionSQL); a ticket's epic is found through
// tracker_cache_parents, its parent's parent read only when the mirror
// keeps the parent.
const MirrorExportSchema = `
-- Times are milliseconds since 1970 UTC; a time the app does not hold is NULL.
-- estimates: points, time (seconds) or none.
CREATE TABLE main.board(name TEXT NOT NULL, plans_sprints INTEGER NOT NULL, estimates TEXT NOT NULL);
-- status_group: backlog, todo, in_progress, done, canceled or left_out, as the project groups statuses; NULL when not known.
-- A status is open (tickets.open) unless done, canceled or left_out.
CREATE TABLE main.statuses(id TEXT PRIMARY KEY, name TEXT NOT NULL, status_group TEXT) WITHOUT ROWID;
-- type_level: 1 an epic, 0 a story, task or bug, -1 a subtask. assignee: a people.id. epic: an epics.id.
-- points: the board's estimate, in board.estimates. history_from: changes are complete from then; NULL while not read.
-- history_behind 1: it changed after its history was read; its sprint_intervals are left out until that is read again.
-- started_known 1: the history shows when it started, started_at its first move into in_progress.
-- done_at: its last move into done, for a ticket done now. open 1: the board counts it not done.
-- counts 0: its status is left out of counts.
-- type: the issue type's name (Bug); NULL on Linear, which has none: use labels. priority: its name (High), NULL for none; no order.
CREATE TABLE main.tickets(id TEXT PRIMARY KEY, key TEXT NOT NULL, title TEXT NOT NULL, type_level INTEGER NOT NULL,
  status TEXT, status_group TEXT, assignee TEXT, parent TEXT, epic TEXT, created_at INTEGER, updated_at INTEGER, resolved_at INTEGER,
  status_since INTEGER, points REAL, history_from INTEGER, history_behind INTEGER NOT NULL, started_known INTEGER NOT NULL, started_at INTEGER, done_at INTEGER,
  open INTEGER NOT NULL, counts INTEGER NOT NULL, type TEXT, priority TEXT) WITHOUT ROWID;
-- A ticket's labels by name, an unnamed one by its ID; a ticket with none has no row.
CREATE TABLE main.labels(ticket TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY (ticket, name)) WITHOUT ROWID;
CREATE TABLE main.epics(id TEXT PRIMARY KEY, key TEXT NOT NULL, title TEXT NOT NULL) WITHOUT ROWID;
-- state: future, active or closed. position: the board's order.
CREATE TABLE main.sprints(id TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL, start_at INTEGER, end_at INTEGER,
  completed_at INTEGER, position INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE main.sprint_tickets(sprint TEXT NOT NULL, ticket TEXT NOT NULL, PRIMARY KEY (sprint, ticket)) WITHOUT ROWID;
-- kind: status (from_value and to_value are statuses.id), assignee (people.id), sprint (each the ticket's whole set of
-- sprints.id as a JSON array, such as ["14","15"]) or estimate (from_number and to_number). author: a people.id.
CREATE TABLE main.changes(ticket TEXT NOT NULL, kind TEXT NOT NULL, seq INTEGER NOT NULL, at INTEGER NOT NULL, author TEXT,
  from_value TEXT, to_value TEXT, from_number REAL, to_number REAL, PRIMARY KEY (ticket, kind, seq)) WITHOUT ROWID;
-- Each member of a closed or active sprint; done 1 when it counted done at the sprint's completion, or for an active sprint now;
-- NULL when its status at the completion is not known.
CREATE TABLE main.sprint_finished(sprint TEXT NOT NULL, ticket TEXT NOT NULL, done INTEGER, PRIMARY KEY (sprint, ticket)) WITHOUT ROWID;
CREATE TABLE main.people(id TEXT PRIMARY KEY, name TEXT NOT NULL) WITHOUT ROWID;
-- status_intervals: a ticket's status from from_at until to_at (NULL: now), back to 180 days ago or an older sprint's start.
-- Where no row covers an instant, the status then is not known. began: when the status began; NULL when before from_at,
-- at a time not known. open 1: the board counts the status not done, as tickets.open.
CREATE TABLE main.status_intervals(ticket TEXT NOT NULL, status TEXT NOT NULL, from_at INTEGER NOT NULL, to_at INTEGER,
  began INTEGER, open INTEGER NOT NULL, PRIMARY KEY (ticket, from_at)) WITHOUT ROWID;
-- sprint_intervals: a ticket in a sprint at one estimate (points), likewise; none for a ticket whose history_behind is 1.
CREATE TABLE main.sprint_intervals(sprint TEXT NOT NULL, ticket TEXT NOT NULL, points REAL, from_at INTEGER NOT NULL, to_at INTEGER,
  PRIMARY KEY (sprint, ticket, from_at)) WITHOUT ROWID;
-- threads: the threads on this board's computer linked to a ticket. origin: start_work, ask, manual or agent. last_turn_at: its newest turn's start.
CREATE TABLE main.threads(id TEXT PRIMARY KEY, ticket TEXT NOT NULL, origin TEXT NOT NULL, linked_at INTEGER NOT NULL, created_at INTEGER NOT NULL,
  archived INTEGER NOT NULL, last_turn_at INTEGER) WITHOUT ROWID;
-- worklogs: your own logged time on this board's tickets, by day (YYYY-MM-DD), over the last 182 days. Only the days in worklog_days were read: a number counted from worklogs is not known until every one of those days up to today is, and says how many are not.
CREATE TABLE main.worklogs(id TEXT PRIMARY KEY, ticket TEXT NOT NULL, day TEXT NOT NULL, seconds INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE main.worklog_days(day TEXT PRIMARY KEY) WITHOUT ROWID;
`

// mirrorExportSchemaName is the attached file's schema name.
const mirrorExportSchemaName = "mirror_file"

// mirrorExportBoardSQL reads the board's state: ?1 root, ?2 board. Its
// first read is done once a reconcile of it completed, which follows the
// last page of the read before it; unlisting the board deletes its sync
// row, so a board listed again reads first again.
var mirrorExportBoardSQL = `SELECT b.name, b.plans_sprints, b.estimate_unit, b.estimate_reset_at,
	  coalesce((SELECT sy.reconciled_at FROM tracker_cache_sync sy WHERE sy.root_id = b.root_id AND sy.board_id = b.board_id), 0) > 0
	FROM tracker_cache_boards b
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

// mirrorExportStatusesSQL copies the statuses of root ?1 that the list x
// (a subquery of one column, id) names, each with its group under the
// regrouping ?3; or, as the statement does, ignores one already copied.
func mirrorExportStatusesSQL(insert, x string) string {
	return insert + ` INTO main.statuses(id, name, status_group)
	  SELECT st.status_id, st.name, ` + mirrorRegroupedGroupNameSQL(`coalesce((SELECT o.value FROM json_each(?3) o WHERE o.key = st.status_id), st.status_group)`) + `
	  FROM (` + x + `) x
	  CROSS JOIN tracker_cache_statuses st ON st.root_id = CAST(?1 AS INTEGER) AND st.status_id = x.id`
}

// mirrorExportCardSQL is the member at path p of issue i's card blob,
// NULL when it is empty or the blob is not of the shape's version (?6 at
// ?7). The blob is read as text: SQLite reads a BLOB argument as JSONB.
func mirrorExportCardSQL(p string) string {
	return `iif(json_extract(CAST(i.data AS TEXT), ?7) = CAST(?6 AS INTEGER), nullif(json_extract(CAST(i.data AS TEXT), ` + p + `), ''), NULL)`
}

// mirrorExportCardNames is the tickets fill's insert list's names of
// mirrorExportCardColumns, each after a comma.
func mirrorExportCardNames() string {
	var b strings.Builder
	for _, c := range mirrorExportCardColumns {
		_, column, _ := strings.Cut(c.name, ".")
		b.WriteString(", " + column)
	}
	return b.String()
}

// mirrorExportFill is one statement of the export: the relation it fills
// (a fill of another relation's rows is that relation's, as the statuses
// and people its changes name are the changes'), and the relations of the
// copy it reads, which the export fills first. A fill whose select list
// ends with mirrorExportCardColumns has the text after them in afterCard.
//
// A fill with with runs only when the export fills that relation too, and
// one with without only when it does not; neither makes the export fill
// it. They choose where a fill reads (the file or the copy) and skip rows
// of a relation no count reads.
type mirrorExportFill struct {
	relation, name, sql, afterCard string
	needs                          []string
	with, without                  string
}

// runs is whether an export of relations runs f.
func (f mirrorExportFill) runs(relations map[string]bool) bool {
	return relations[f.relation] && (f.with == "" || relations[f.with]) && (f.without == "" || !relations[f.without])
}

// text is the fill's statement in an export of relations: sql, and for a
// fill with card columns each column of mirrorExportCardColumns decoded
// when relations names it and NULL otherwise, then afterCard.
func (f mirrorExportFill) text(relations map[string]bool) string {
	if f.afterCard == "" {
		return f.sql
	}
	var b strings.Builder
	b.WriteString(f.sql)
	for i, c := range mirrorExportCardColumns {
		b.WriteString(", ")
		if relations[c.name] {
			b.WriteString(mirrorExportCardSQL("?" + strconv.Itoa(mirrorExportCardArg+i)))
		} else {
			b.WriteString("NULL")
		}
	}
	b.WriteString(f.afterCard)
	return b.String()
}

// mirrorExportFills are the fill statements in order: ?1 root, ?2 board,
// ?3 the regrouping (a JSON object, '{}' for none), ?4 the board's
// estimate reset, ?5 whether the board has an estimate (0 or 1), from
// ?6 the card blob's shape (MirrorCardShape.args), then the earliest
// instant a count can ask (mirrorExportWindowArg) and the first day of
// the logged time (mirrorExportLoggedArg). Each
// names the snapshot's tables by main and the file's unqualified, so a
// file table the snapshot has a relation of the same name as (threads) is
// read only through a view of the file's (mirrorExportRelationFills). An export runs those of the
// relations asked for and the relations they need (mirrorExportRelations).
var mirrorExportFills = append([]mirrorExportFill{
	{relation: "board", name: "board", sql: `INSERT INTO main.board(name, plans_sprints, estimates)
	  SELECT b.name, b.plans_sprints, CASE b.estimate_unit WHEN ` + strconv.Itoa(MirrorEstimatePoints) + ` THEN 'points' WHEN ` +
		strconv.Itoa(MirrorEstimateTime) + ` THEN 'time' ELSE 'none' END
	  FROM tracker_cache_boards b WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`},
	{relation: "tickets", name: "tickets", sql: `INSERT INTO main.tickets(id, key, title, type_level, status, status_group, assignee, parent, epic,
	    created_at, updated_at, resolved_at, status_since, points, history_from, history_behind, started_known, started_at, done_at, open, counts` + mirrorExportCardNames() + `)
	  SELECT i.issue_id, i.issue_key, i.title, i.type_level, nullif(i.status_id, ''),
	    ` + mirrorRegroupedGroupNameSQL(`coalesce((SELECT o.value FROM json_each(?3) o WHERE o.key = i.status_id),
	      (SELECT st.status_group FROM tracker_cache_statuses st WHERE st.root_id = i.root_id AND st.status_id = i.status_id))`) + `,
	    nullif(i.assignee_id, ''), nullif(i.parent_id, ''),
	    CASE WHEN n.type_level = 1 THEN i.parent_id WHEN gn.type_level = 1 THEN p.parent_id END,
	    nullif(i.created_at, 0), i.updated_at, nullif(i.resolved_at, 0), nullif(i.status_since, 0),
	    iif(CAST(?5 AS INTEGER) = 1, ` + mirrorRowEstimate("CAST(?4 AS INTEGER)") + `, NULL),
	    iif(i.history_for > 0, iif(i.history_from > 0, i.history_from, i.created_at), NULL),
	    iif(i.history_for > 0 AND NOT ` + mirrorExportHistoryCurrent + `, 1, 0),
	    iif(` + mirrorExportStartKnownSQL + `, 1, 0),
	    iif(` + mirrorExportStartKnownSQL + `, ` + mirrorExportStatusChangeSQL("min", mirrorGroupStatusesSQL("?3", MirrorGroupInProgress)) + `, NULL),
	    iif(i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `), ` + mirrorExportStatusChangeSQL("max", mirrorDoneStatusesSQL("?3")) + `, NULL),
	    iif(` + mirrorRegroupedNotDone("?3") + `, 1, 0), iif(` + mirrorLeftOut("?3") + `, 0, 1)`, afterCard: `
	  FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  LEFT JOIN tracker_cache_parents n ON n.root_id = i.root_id AND n.issue_id = i.parent_id AND i.parent_id <> ''
	  LEFT JOIN tracker_cache_issues p ON p.root_id = i.root_id AND p.issue_id = i.parent_id AND i.parent_id <> '' AND n.type_level IS NOT 1
	    AND ` + mirrorIssueKept("p.root_id", "p.issue_id") + `
	  LEFT JOIN tracker_cache_parents gn ON gn.root_id = p.root_id AND gn.issue_id = p.parent_id AND p.parent_id <> ''
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`},
	{relation: "statuses", name: "statuses", needs: []string{"tickets"}, sql: mirrorExportStatusesSQL("INSERT",
		`SELECT c.status_id AS id FROM tracker_cache_board_column_statuses c WHERE c.root_id = CAST(?1 AS INTEGER) AND c.board_id = ?2
		 UNION SELECT status FROM main.tickets WHERE status IS NOT NULL`)},
	{relation: "epics", name: "epics", needs: []string{"tickets"}, sql: `INSERT INTO main.epics(id, key, title)
	  SELECT e.issue_id, e.issue_key, e.title FROM (SELECT DISTINCT epic FROM main.tickets WHERE epic IS NOT NULL) x
	  CROSS JOIN tracker_cache_parents e ON e.root_id = CAST(?1 AS INTEGER) AND e.issue_id = x.epic`},
	{relation: "sprints", name: "sprints", sql: `INSERT INTO main.sprints(id, name, state, start_at, end_at, completed_at, position)
	  SELECT sp.sprint_id, sp.name, CASE sp.state WHEN ` + strconv.Itoa(MirrorSprintFuture) + ` THEN 'future' WHEN ` +
		strconv.Itoa(MirrorSprintActive) + ` THEN 'active' ELSE 'closed' END,
	    nullif(sp.start_at, 0), nullif(sp.end_at, 0), nullif(sp.completed_at, 0), sp.position
	  FROM tracker_cache_sprints sp WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2`},
	// The membership is read by each ticket's key; +m.sprint_id keeps the
	// planner from probing it once per board sprint for every ticket.
	{relation: "sprint_tickets", name: "sprint_tickets", needs: []string{"sprints"}, sql: `INSERT INTO main.sprint_tickets(sprint, ticket)
	  SELECT m.sprint_id, m.issue_id FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = b.root_id AND m.issue_id = b.issue_id
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND +m.sprint_id IN (SELECT id FROM main.sprints)`},
	{relation: "changes", name: "changes", sql: `INSERT INTO main.changes(ticket, kind, seq, at, author, from_value, to_value, from_number, to_number)
	  SELECT c.issue_id, CASE c.kind WHEN 1 THEN 'status' WHEN 2 THEN 'sprint' WHEN 3 THEN 'estimate' ELSE 'assignee' END,
	    c.seq, c.at, nullif(c.author_id, ''), nullif(c.from_value, ''), nullif(c.to_value, ''), c.from_number, c.to_number
	  FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  CROSS JOIN tracker_cache_changes c ON c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND (c.kind <> 3 OR c.board_id = ?2)`},
	{relation: "changes", name: "statuses its changes name", needs: []string{"statuses"}, sql: mirrorExportStatusesSQL("INSERT OR IGNORE",
		`SELECT to_value AS id FROM main.changes WHERE kind = 'status' AND to_value IS NOT NULL
		 UNION SELECT from_value FROM main.changes WHERE kind = 'status' AND from_value IS NOT NULL`)},
	{relation: "sprint_finished", name: "closed sprints' members", needs: []string{"sprints"}, sql: `WITH l AS MATERIALIZED (
	    SELECT sprint_id, completed_at FROM tracker_cache_sprints
	     WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at > 0)
	  INSERT INTO main.sprint_finished(sprint, ticket, done)
	  SELECT l.sprint_id, i.issue_id, ` + mirrorStatusAtCompletionSQL + ` IN (` + mirrorDoneStatusesSQL("?3") + `)
	  FROM l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id`},
	{relation: "sprint_finished", name: "active sprints' members", needs: []string{"sprints", "sprint_tickets", "tickets"}, sql: `INSERT INTO main.sprint_finished(sprint, ticket, done)
	  SELECT sprint_tickets.sprint, sprint_tickets.ticket, coalesce(tickets.status_group = 'done', 0)
	  FROM main.sprints JOIN main.sprint_tickets ON sprint_tickets.sprint = sprints.id
	  JOIN main.tickets ON tickets.id = sprint_tickets.ticket WHERE sprints.state = 'active'`},
	// A label is named by its name, else its ID; OR IGNORE keeps one row of
	// a name a ticket has twice and skips a label with neither.
	{relation: "labels", name: "labels", sql: `INSERT OR IGNORE INTO main.labels(ticket, name)
	  SELECT i.issue_id, coalesce(nullif(json_extract(l.value, ?10), ''), nullif(json_extract(l.value, ?9), ''))
	  FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  CROSS JOIN json_each(CAST(i.data AS TEXT), ?8) l
	  WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND json_extract(CAST(i.data AS TEXT), ?7) = CAST(?6 AS INTEGER)`},
	{relation: "people", name: "people", needs: []string{"tickets"}, sql: `INSERT INTO main.people(id, name)
	  SELECT u.user_id, u.name FROM (SELECT DISTINCT assignee AS id FROM main.tickets WHERE assignee IS NOT NULL) x
	  CROSS JOIN tracker_cache_users u ON u.root_id = CAST(?1 AS INTEGER) AND u.user_id = x.id`},
	{relation: "changes", name: "people its changes name", needs: []string{"people"}, sql: `INSERT OR IGNORE INTO main.people(id, name)
	  SELECT u.user_id, u.name FROM (
	    SELECT author AS id FROM main.changes WHERE author IS NOT NULL
	    UNION SELECT from_value FROM main.changes WHERE kind = 'assignee' AND from_value IS NOT NULL
	    UNION SELECT to_value FROM main.changes WHERE kind = 'assignee' AND to_value IS NOT NULL) x
	  CROSS JOIN tracker_cache_users u ON u.root_id = CAST(?1 AS INTEGER) AND u.user_id = x.id`},
}, slices.Concat(mirrorExportIntervalFills(), mirrorExportRelationFills())...)

// mirrorExportAlways are the relations every export fills: what a range
// is resolved against.
var mirrorExportAlways = []string{"board", "sprints"}

// mirrorExportRelations is the set of relations an export of relations
// fills: those, the ones every export fills, and what each of their fills
// needs, through every fill, with each column of MirrorExportColumns
// named and its relation. A name that is neither an export relation nor
// one of those columns is the caller's error.
func mirrorExportRelations(relations []string) (map[string]bool, error) {
	set := map[string]bool{}
	queue := slices.Concat(mirrorExportAlways, relations)
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		if set[r] {
			continue
		}
		if slices.Contains(MirrorExportColumns, r) {
			set[r] = true
			relation, _, _ := strings.Cut(r, ".")
			queue = append(queue, relation)
			continue
		}
		if !slices.Contains(MirrorExportTables, r) {
			return nil, fmt.Errorf("store: %q is not one of the export's relations (%s) or columns (%s)", r,
				strings.Join(MirrorExportTables, ", "), strings.Join(MirrorExportColumns, ", "))
		}
		set[r] = true
		for _, f := range mirrorExportFills {
			if f.relation == r {
				queue = append(queue, f.needs...)
			}
		}
	}
	return set, nil
}

// MirrorExport is what ExportMirrorBoard says of the board it exported.
type MirrorExport struct {
	// Name is the board's name, EstimateUnit its estimate's (MirrorEstimate*)
	// and PlansSprints whether it plans sprints.
	Name         string
	EstimateUnit int
	PlansSprints bool
	// FirstReadDone is whether the board's first read has finished: until
	// then the export holds the tickets read so far.
	FirstReadDone bool
	// Earliest is the earliest instant a count can ask of the history
	// (mirrorExportEarliest): no interval and no day of logged time before
	// it is in the export.
	Earliest int64
	// LoggedFrom is the first day (YYYY-MM-DD) of the logged time the
	// export holds, as the export was given it: worklogs and worklog_days
	// hold no day before it (trackerdash worklogs.go).
	LoggedFrom string
	// Viewer is the board's connection's verified account, whose logged
	// time worklogs and worklog_days hold, read in the export's snapshot;
	// "" before one is verified.
	Viewer string
}

// ErrMirrorExportUnsupported is an export from a store with no database
// file to attach (an in-memory store).
var ErrMirrorExportUnsupported = errors.New("store: this store has no database file to export a board from")

// ExportMirrorBoard fills the main database of conn, which must be empty,
// with board boardID of root rootID as the export's relations hold it
// (MirrorExportTables): relations, what their fills need and the ones
// every export fills (mirrorExportRelations), the others left empty, and
// the columns of MirrorExportColumns relations names, decoded from card
// blobs of shape card, the intervals from window, the start of the
// mirror's history window in milliseconds (mirrorExportIntervalsFrom), and
// the logged time from day loggedFrom. It reads with regroup in one read transaction of this
// store's file, attached to conn read-only for the fill and detached
// before it returns. ok is false, with nothing filled, for a board the
// mirror does not show. It takes a history read slot, as every read that
// walks a board does; ctx bounds the wait for it and every statement.
func (s *Store) ExportMirrorBoard(ctx context.Context, conn *sql.Conn, rootID int64, boardID string, regroup MirrorRegroup, relations []string, window int64, loggedFrom string, card MirrorCardShape) (export MirrorExport, ok bool, err error) {
	if s.path == "" || s.path == ":memory:" || strings.Contains(s.path, "mode=memory") {
		return MirrorExport{}, false, ErrMirrorExportUnsupported
	}
	set, err := mirrorExportRelations(relations)
	if err != nil {
		return MirrorExport{}, false, err
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
	export, ok, err = exportMirrorBoardTx(ctx, conn, rootID, boardID, regroup, set, window, loggedFrom, card)
	return export, ok, err
}

// exportMirrorBoardTx runs the fill in one transaction on conn, the file
// attached.
func exportMirrorBoardTx(ctx context.Context, conn *sql.Conn, rootID int64, boardID string, regroup MirrorRegroup, relations map[string]bool, window int64, loggedFrom string, card MirrorCardShape) (MirrorExport, bool, error) {
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
	err = tx.QueryRowContext(ctx, mirrorExportBoardSQL, rootID, boardID).Scan(&export.Name, &export.PlansSprints, &export.EstimateUnit, &resetAt, &export.FirstReadDone)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorExport{}, false, nil
	}
	if err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: read board %s to export it: %w", boardID, err)
	}
	var earliestSprint sql.NullInt64
	if err := tx.QueryRowContext(ctx, mirrorExportEarliestSprintSQL, rootID, boardID).Scan(&earliestSprint); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: read board %s's earliest sprint to export it: %w", boardID, err)
	}
	export.Earliest = mirrorExportEarliest(window, earliestSprint)
	export.LoggedFrom = loggedFrom
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(`+mirrorExportViewerSQL+`, '')`, rootID).Scan(&export.Viewer); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: read board %s's viewer to export it: %w", boardID, err)
	}
	args := mirrorExportArgs(rootID, boardID, regroup, resetAt, export.EstimateUnit != MirrorEstimateNone, export.Earliest, loggedFrom, card)
	for _, fill := range mirrorExportFills {
		if !fill.runs(relations) {
			continue
		}
		if _, err := tx.ExecContext(ctx, fill.text(relations), args...); err != nil {
			return MirrorExport{}, false, fmt.Errorf("store: export board %s's %s: %w", boardID, fill.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MirrorExport{}, false, fmt.Errorf("store: commit the export of board %s: %w", boardID, err)
	}
	commit = true
	return export, true, nil
}

// mirrorExportArgs is what the fills bind (mirrorExportFills).
// from is the earliest instant a count can ask of the intervals
// (mirrorExportEarliest), loggedFrom the first day of the logged time.
func mirrorExportArgs(rootID int64, boardID string, regroup MirrorRegroup, resetAt int64, estimated bool, from int64, loggedFrom string, card MirrorCardShape) []any {
	groups := regroup.statuses
	if groups == "" {
		groups = "{}"
	}
	hasEstimate := 0
	if estimated {
		hasEstimate = 1
	}
	return append(append([]any{rootID, boardID, groups, resetAt, hasEstimate}, card.args()...), from, loggedFrom)
}

// mirrorExportURI is the read-only URI of the database file at path.
func mirrorExportURI(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	return u.String()
}
