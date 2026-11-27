package store

import (
	"database/sql"
	"strconv"
)

// The export's intervals: what a ticket held over time, rebuilt from its
// stored history (status_intervals, sprint_intervals).
//
// What must hold:
//   - They reach back to the earliest instant a count can ask of them
//     (mirrorExportEarliest): the start of the mirror's history window, or
//     of a sprint the board keeps that began before it. No range's period
//     and no sprint a count shows begins earlier. An interval that ends at
//     or before it has no row, and a ticket's history is read only when
//     what it held may have changed since then, so a fill follows what
//     changed in the window, not the board's size.
//   - A status interval is known at both ends: from the ticket's history
//     (one change to the next, and what the first change left from the
//     history's start, history_from or else creation), or the status now
//     from status_since, the store's one rule for when it began
//     (MirrorStatusSince), when that is known. status_since is derived
//     from the stored history whenever either is written, so it is never
//     before the latest status change. Where the history and the card
//     disagree (a history read behind the card), the stretch after the
//     latest change has no interval: it is not known. A ticket whose
//     status now began at or before the earliest instant, or that was
//     last updated then (mirrorExportUpdatedSince), has its interval now
//     alone, which covers every instant a count can ask; its history is
//     not read.
//   - A sprint interval is a stretch a ticket spent in one of the board's
//     sprints with one estimate of the board: the sprint sets the history
//     holds (the first change's from-set from the history's start, each
//     change's to-set from its time; a ticket with no sprint change its
//     sprints now from the start), split where the estimate changed (the
//     first estimate change's from-value from the start; a ticket with no
//     estimate change its estimate now). A stretch may be cut in rows
//     where the ticket's other sprints or its estimate changed. A stretch
//     still open at the latest change is open only when the ticket is in
//     the sprint now; otherwise its end is not known and it has no row. A
//     ticket's history is read only when it is in one of the board's
//     sprints now, or it was updated after the earliest instant and its
//     latest sprint change is after it: one in none now that changed none
//     since was in none of them at any instant a count can ask. A history
//     is read for the card's update time (mirrorHistoryDue), so a sprint
//     change the store holds moved the card's update time; the update time
//     picks those issues by idx_tracker_cache_issues_updated, never
//     visiting the board's others.
//   - Each fill reads the change rows once: from the file by each issue's
//     key, or from the copy's changes when the same export fills them
//     (mirrorExportFill.with and without), so a count that reads both
//     scans the history once.

// mirrorExportIntervalsFrom is the earliest instant a count can ask of the
// intervals, as the export binds it (mirrorExportWindowArg).
func mirrorExportIntervalsFrom() string {
	return `CAST(?` + strconv.Itoa(mirrorExportWindowArg) + ` AS INTEGER)`
}

// mirrorExportEarliestSprintSQL is the earliest start of a sprint board ?2
// of root ?1 keeps, NULL for none.
const mirrorExportEarliestSprintSQL = `SELECT min(start_at) FROM tracker_cache_sprints
	WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND start_at > 0`

// mirrorExportEarliest is the earliest instant a count can ask of the
// intervals: window, the history window's start, or earliestSprint when
// that is earlier.
func mirrorExportEarliest(window int64, earliestSprint sql.NullInt64) int64 {
	if earliestSprint.Valid && earliestSprint.Int64 < window {
		return earliestSprint.Int64
	}
	return window
}

// mirrorExportChangeSource is where the intervals' fills read an issue's
// changes: the file's rows by key, or the copy's. Either holds each kind's
// changes of an issue numbered from 0 in their order, oldest first
// (MirrorHistory), so a change's next is the one numbered after it: one
// lookup by key, as the copy holds them too, but for the estimates,
// numbered over every board's (the next of this board's is found by
// seeking past it).
type mirrorExportChangeSource struct {
	fromCopy bool
}

// changes is the join that reads issue on's changes of kind as c (f and t
// their values, NULL for none), on naming a row with the issue's root_id,
// issue_id and history_gen. first limits them to the first.
func (src mirrorExportChangeSource) changes(on, kind string, first bool) string {
	only := ``
	if first {
		only = ` AND c.seq = (SELECT min(x.seq) FROM ` + src.ofKind(on, "x", kind) + `)`
	}
	if src.fromCopy {
		return `(SELECT ticket, kind, seq, at, from_value AS f, to_value AS t, from_number AS fn, to_number AS tn FROM main.changes) c
	    ON c.ticket = ` + on + `.issue_id AND c.kind = '` + kind + `'` + only
	}
	board := ``
	if kind == MirrorChangeEstimate {
		board = ` AND c.board_id = ?2`
	}
	return `(SELECT root_id, issue_id, gen, kind, seq, at, nullif(from_value, '') AS f, nullif(to_value, '') AS t, from_number AS fn,
	      to_number AS tn, board_id FROM tracker_cache_changes) c
	    ON c.root_id = ` + on + `.root_id AND c.issue_id = ` + on + `.issue_id AND c.gen = ` + on + `.history_gen AND c.kind = ` +
		strconv.Itoa(mirrorChangeKinds[kind]) + board + only
}

// ofKind is issue on's changes of kind as alias, for a subquery's FROM and
// WHERE.
func (src mirrorExportChangeSource) ofKind(on, alias, kind string) string {
	if src.fromCopy {
		return `main.changes ` + alias + ` WHERE ` + alias + `.ticket = ` + on + `.issue_id AND ` + alias + `.kind = '` + kind + `'`
	}
	board := ``
	if kind == MirrorChangeEstimate {
		board = ` AND ` + alias + `.board_id = ?2`
	}
	return `tracker_cache_changes ` + alias + ` WHERE ` + alias + `.root_id = ` + on + `.root_id AND ` + alias + `.issue_id = ` + on + `.issue_id
	      AND ` + alias + `.gen = ` + on + `.history_gen AND ` + alias + `.kind = ` + strconv.Itoa(mirrorChangeKinds[kind]) + board
}

// next joins the change of issue on of kind after c as n, by key: the
// estimates' by seeking past c, the others' by its number.
func (src mirrorExportChangeSource) next(on, kind string) string {
	seq := `c.seq + 1`
	if kind == MirrorChangeEstimate {
		seq = `(SELECT min(x.seq) FROM ` + src.ofKind(on, "x", kind) + ` AND x.seq > c.seq)`
	}
	if src.fromCopy {
		return `main.changes n ON n.ticket = ` + on + `.issue_id AND n.kind = '` + kind + `' AND n.seq = ` + seq
	}
	return `tracker_cache_changes n ON n.root_id = ` + on + `.root_id AND n.issue_id = ` + on + `.issue_id AND n.gen = ` + on +
		`.history_gen AND n.kind = ` + strconv.Itoa(mirrorChangeKinds[kind]) + ` AND n.seq = ` + seq
}

// latest is issue on's latest change of kind: column of it, by key.
func (src mirrorExportChangeSource) latest(on, kind, column string) string {
	return `(SELECT x.` + column + ` FROM ` + src.ofKind(on, "x", kind) + ` ORDER BY x.seq DESC LIMIT 1)`
}

// mirrorExportUpdatedSince is the condition that issue alias of the root
// was updated after the earliest instant: by its update time
// (idx_tracker_cache_issues_updated). The intervals read the history of
// no other issue. A history is read for the card's update time
// (mirrorHistoryDue), so the changes it holds are at or before that
// time, but for one made between the card's read and its history's,
// which the card's next read moves the update time past; until then the
// export follows the card's version, as the board does.
func mirrorExportUpdatedSince(alias string) string {
	return alias + `.root_id = CAST(?1 AS INTEGER) AND ` + alias + `.updated_at > ` + mirrorExportIntervalsFrom()
}

// mirrorExportBoardIssues reads the board's issues as b and i, by key.
const mirrorExportBoardIssues = `tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id`

// mirrorExportOnBoard is the condition that limits b to the board.
const mirrorExportOnBoard = `b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`

// mirrorExportHistoryStart is where issue i's history begins, NULL while
// it is not read: as tickets.history_from.
const mirrorExportHistoryStart = `iif(i.history_for > 0, iif(i.history_from > 0, i.history_from, nullif(i.created_at, 0)), NULL)`

// mirrorExportNever stands for an open end where the intervals' fills
// compare ends: no time the mirror holds reaches it.
const mirrorExportNever = `9223372036854775807`

// statusIntervalsSQL fills status_intervals from src: each status change
// to the next, what the first left from the history's start, and the
// status now from status_since. ri is the issues whose history is read,
// each with its history's start.
func (src mirrorExportChangeSource) statusIntervalsSQL() string {
	from := mirrorExportIntervalsFrom()
	return `WITH ri AS MATERIALIZED (
	    SELECT i.root_id, i.issue_id, i.history_gen, ` + mirrorExportHistoryStart + ` AS start
	    FROM tracker_cache_issues i
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.issue_id = i.issue_id AND b.board_id = ?2
	    WHERE ` + mirrorExportUpdatedSince("i") + ` AND NOT (i.status_since > 0 AND i.status_since <= ` + from + `))
	  INSERT INTO main.status_intervals(ticket, status, from_at, to_at)
	  SELECT ri.issue_id, c.t, c.at, n.at
	  FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeStatus, false) + ` CROSS JOIN ` + src.next("ri", MirrorChangeStatus) + `
	  WHERE n.at > c.at AND n.at > ` + from + ` AND c.t IS NOT NULL
	  UNION ALL
	  SELECT ri.issue_id, c.f, ri.start, c.at
	  FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeStatus, true) + `
	  WHERE c.f IS NOT NULL AND ri.start < c.at AND c.at > ` + from + `
	  UNION ALL
	  SELECT i.issue_id, i.status_id, i.status_since, NULL
	  FROM ` + mirrorExportBoardIssues + `
	  WHERE ` + mirrorExportOnBoard + ` AND i.status_since > 0 AND i.status_id <> ''`
}

// sprintIntervalsSQL fills sprint_intervals from src: ?5 whether the board
// has an estimate, ?4 its reset (mirrorRowEstimate).
func (src mirrorExportChangeSource) sprintIntervalsSQL() string {
	from := mirrorExportIntervalsFrom()
	never := mirrorExportNever
	// pick is the root's issues in one of the board's sprints now, by the
	// sprints' members, and those updated after the earliest instant, by
	// their update time. ri is those of them on the board whose history is
	// read, each with its history's start and its estimate now: one of the
	// first, or one of the second whose latest sprint change is after the
	// earliest instant. v is each version of their sprint sets, from and
	// to: what the first change left from the history's start, each
	// change's to-set to the next change, and the set now from the start
	// for one with no change. p is each stretch of a version in a sprint of
	// the board that ends after the earliest instant, an open one only
	// while the ticket is in the sprint now. e is each version of their
	// estimates, built the same way.
	return `WITH pick AS MATERIALIZED (
	    SELECT m.issue_id, 1 AS member FROM main.sprints sp
	    CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = sp.id
	    UNION
	    SELECT u.issue_id, 0 FROM tracker_cache_issues u WHERE ` + mirrorExportUpdatedSince("u") + `),
	  ri AS MATERIALIZED (
	    SELECT i.root_id, i.issue_id, i.history_gen, ` + mirrorExportHistoryStart + ` AS start,
	      iif(CAST(?5 AS INTEGER) = 1, ` + mirrorRowEstimate("CAST(?4 AS INTEGER)") + `, NULL) AS estimate
	    FROM (SELECT issue_id, max(member) AS member FROM pick GROUP BY issue_id) pk
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND b.issue_id = pk.issue_id
	    CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	    WHERE i.history_for > 0 AND (pk.member = 1 OR coalesce(` + src.latest("i", MirrorChangeSprint, "at") + `, 0) > ` + from + `)),
	  v AS (
	    SELECT ri.issue_id AS ticket, ri.start AS from_at, c.at AS to_at, c.f AS sprints
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeSprint, true) + ` WHERE ri.start < c.at
	    UNION ALL
	    SELECT ri.issue_id, c.at, n.at, c.t
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeSprint, false) + ` LEFT JOIN ` + src.next("ri", MirrorChangeSprint) + `
	    UNION ALL
	    SELECT ri.issue_id, ri.start, NULL, (SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m
	        WHERE m.root_id = ri.root_id AND m.issue_id = ri.issue_id)
	    FROM ri WHERE NOT EXISTS (SELECT 1 FROM ` + src.ofKind("ri", "y", MirrorChangeSprint) + `)),
	  p AS MATERIALIZED (
	    SELECT v.ticket, j.value AS sprint, v.from_at, v.to_at FROM v CROSS JOIN json_each(v.sprints) j
	    WHERE j.value IN (SELECT id FROM main.sprints) AND coalesce(v.to_at, ` + never + `) > max(v.from_at, ` + from + `)
	      AND (v.to_at IS NOT NULL OR EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	        WHERE m.root_id = CAST(?1 AS INTEGER) AND m.issue_id = v.ticket AND m.sprint_id = j.value))),
	  e AS MATERIALIZED (
	    SELECT ri.issue_id AS ticket, ri.start AS from_at, c.at AS to_at, c.fn AS points
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeEstimate, true) + ` WHERE ri.start < c.at
	    UNION ALL
	    SELECT ri.issue_id, c.at, n.at, c.tn
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeEstimate, false) + ` LEFT JOIN ` + src.next("ri", MirrorChangeEstimate) + `
	    UNION ALL
	    SELECT ri.issue_id, ri.start, NULL, ri.estimate
	    FROM ri WHERE NOT EXISTS (SELECT 1 FROM ` + src.ofKind("ri", "y", MirrorChangeEstimate) + `))
	  INSERT INTO main.sprint_intervals(sprint, ticket, points, from_at, to_at)
	  SELECT p.sprint, p.ticket, iif(CAST(?5 AS INTEGER) = 1, e.points, NULL), max(p.from_at, e.from_at),
	    nullif(min(coalesce(p.to_at, ` + never + `), coalesce(e.to_at, ` + never + `)), ` + never + `)
	  FROM p JOIN e ON e.ticket = p.ticket
	  WHERE max(p.from_at, e.from_at) < min(coalesce(p.to_at, ` + never + `), coalesce(e.to_at, ` + never + `))`
}

// mirrorExportIntervalFills are the intervals' fills, each from the file
// or, when the export copies the changes, from the copy; and the statuses
// the status intervals name, when the export copies the statuses.
func mirrorExportIntervalFills() []mirrorExportFill {
	file, copied := mirrorExportChangeSource{}, mirrorExportChangeSource{fromCopy: true}
	return []mirrorExportFill{
		{relation: "status_intervals", name: "status_intervals", without: "changes", sql: file.statusIntervalsSQL()},
		{relation: "status_intervals", name: "status_intervals from the copy's changes", with: "changes", sql: copied.statusIntervalsSQL()},
		{relation: "status_intervals", name: "statuses its intervals name", with: "statuses", sql: mirrorExportStatusesSQL("INSERT OR IGNORE",
			`SELECT DISTINCT status AS id FROM main.status_intervals`)},
		{relation: "sprint_intervals", name: "sprint_intervals", needs: []string{"sprints"}, without: "changes", sql: file.sprintIntervalsSQL()},
		{relation: "sprint_intervals", name: "sprint_intervals from the copy's changes", needs: []string{"sprints"}, with: "changes",
			sql: copied.sprintIntervalsSQL()},
	}
}
