package store

import "strconv"

// The export's intervals: what a ticket held over time, rebuilt from its
// stored history (status_intervals, sprint_intervals).
//
// What must hold:
//   - A status interval is known at both ends: from the ticket's history
//     (one change to the next, and what the first change left from the
//     history's start, history_from or else creation), or the status now
//     from status_since, the store's one rule for when it began
//     (MirrorStatusSince), when that is known and no earlier than the
//     latest status change. Where the history and the card disagree (a
//     history read behind the card), the stretch after the latest change
//     has no interval: it is not known.
//   - A sprint interval is a stretch a ticket spent in one of the board's
//     sprints with one estimate of the board: the sprint sets the history
//     holds (the first change's from-set from the history's start, each
//     change's to-set from its time; a ticket with no sprint change its
//     sprints now from the start), split where the estimate changed (the
//     first estimate change's from-value from the start; a ticket with no
//     estimate change its estimate now). A stretch still open at the
//     latest change is open only when the ticket is in the sprint now;
//     otherwise its end is not known and it has no row.
//   - Each fill reads the change rows once: from the file by each board
//     issue's key, or from the copy's changes when the same export fills
//     them (mirrorExportFill.with and without), so a count that reads both
//     scans the history once.

// mirrorExportChangeSource is where the intervals' fills read a board
// issue's changes: the file's rows by key, or the copy's. Either holds
// each kind's changes of an issue numbered from 0 in their order, oldest
// first (MirrorHistory), so a change's next is the one numbered after it:
// one lookup by key, as the copy holds them too, but for the estimates,
// numbered over every board's (the next of this board's is found by
// seeking past it).
type mirrorExportChangeSource struct {
	fromCopy bool
}

// changes is the join that reads issue i's changes of kind as c (f and t
// their values, NULL for none), from the board issues b and i the join
// reads first (mirrorExportBoardIssues). first limits them to the first.
func (src mirrorExportChangeSource) changes(kind string, first bool) string {
	only := ``
	if first {
		only = ` AND c.seq = (SELECT min(x.seq) FROM ` + src.ofKind("x", kind) + `)`
	}
	if src.fromCopy {
		return `(SELECT ticket, kind, seq, at, from_value AS f, to_value AS t, from_number AS fn, to_number AS tn FROM main.changes) c
	    ON c.ticket = i.issue_id AND c.kind = '` + kind + `'` + only
	}
	board := ``
	if kind == MirrorChangeEstimate {
		board = ` AND c.board_id = ?2`
	}
	return `(SELECT root_id, issue_id, gen, kind, seq, at, nullif(from_value, '') AS f, nullif(to_value, '') AS t, from_number AS fn,
	      to_number AS tn, board_id FROM tracker_cache_changes) c
	    ON c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = ` +
		strconv.Itoa(mirrorChangeKinds[kind]) + board + only
}

// ofKind is issue i's changes of kind as alias, for a subquery's FROM and
// WHERE.
func (src mirrorExportChangeSource) ofKind(alias, kind string) string {
	if src.fromCopy {
		return `main.changes ` + alias + ` WHERE ` + alias + `.ticket = i.issue_id AND ` + alias + `.kind = '` + kind + `'`
	}
	board := ``
	if kind == MirrorChangeEstimate {
		board = ` AND ` + alias + `.board_id = ?2`
	}
	return `tracker_cache_changes ` + alias + ` WHERE ` + alias + `.root_id = i.root_id AND ` + alias + `.issue_id = i.issue_id
	      AND ` + alias + `.gen = i.history_gen AND ` + alias + `.kind = ` + strconv.Itoa(mirrorChangeKinds[kind]) + board
}

// next joins the change of issue i of kind after c as n, by key: the
// estimates' by seeking past c, the others' by its number.
func (src mirrorExportChangeSource) next(kind string) string {
	if kind == MirrorChangeEstimate {
		after := `(SELECT min(x.seq) FROM ` + src.ofKind("x", kind) + ` AND x.seq > c.seq)`
		if src.fromCopy {
			return `main.changes n ON n.ticket = i.issue_id AND n.kind = '` + kind + `' AND n.seq = ` + after
		}
		return `tracker_cache_changes n ON n.root_id = i.root_id AND n.issue_id = i.issue_id AND n.gen = i.history_gen AND n.kind = ` +
			strconv.Itoa(mirrorChangeKinds[kind]) + ` AND n.seq = ` + after
	}
	if src.fromCopy {
		return `main.changes n ON n.ticket = i.issue_id AND n.kind = '` + kind + `' AND n.seq = c.seq + 1`
	}
	return `tracker_cache_changes n ON n.root_id = i.root_id AND n.issue_id = i.issue_id AND n.gen = i.history_gen AND n.kind = ` +
		strconv.Itoa(mirrorChangeKinds[kind]) + ` AND n.seq = c.seq + 1`
}

// latest is issue i's latest change of kind: column of it, by key.
func (src mirrorExportChangeSource) latest(kind, column string) string {
	return `(SELECT x.` + column + ` FROM ` + src.ofKind("x", kind) + ` ORDER BY x.seq DESC LIMIT 1)`
}

// mirrorExportBoardIssues reads the board's issues as b and i, by key.
const mirrorExportBoardIssues = `tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id`

// mirrorExportOnBoard is the condition that limits b to the board.
const mirrorExportOnBoard = `b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`

// mirrorExportHistoryStart is where issue i's history begins, NULL while
// it is not read: as tickets.history_from.
const mirrorExportHistoryStart = `iif(i.history_for > 0, iif(i.history_from > 0, i.history_from, nullif(i.created_at, 0)), NULL)`

// statusIntervalsSQL fills status_intervals from src: each status change
// to the next, what the first left from the history's start, and the
// status now from status_since.
func (src mirrorExportChangeSource) statusIntervalsSQL() string {
	return `INSERT INTO main.status_intervals(ticket, status, from_at, to_at)
	  SELECT i.issue_id, c.t, c.at, n.at
	  FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeStatus, false) + ` CROSS JOIN ` + src.next(MirrorChangeStatus) + `
	  WHERE ` + mirrorExportOnBoard + ` AND n.at > c.at AND c.t IS NOT NULL
	  UNION ALL
	  SELECT i.issue_id, c.f, ` + mirrorExportHistoryStart + `, c.at
	  FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeStatus, true) + `
	  WHERE ` + mirrorExportOnBoard + ` AND c.f IS NOT NULL AND ` + mirrorExportHistoryStart + ` < c.at
	  UNION ALL
	  SELECT i.issue_id, i.status_id, i.status_since, NULL
	  FROM ` + mirrorExportBoardIssues + `
	  WHERE ` + mirrorExportOnBoard + ` AND i.status_since > 0 AND i.status_id <> ''
	    AND i.status_since >= coalesce(` + src.latest(MirrorChangeStatus, "at") + `, 0)`
}

// mirrorExportNever stands for an open end where the intervals' fill
// compares ends: no time the mirror holds reaches it.
const mirrorExportNever = `9223372036854775807`

// sprintIntervalsSQL fills sprint_intervals from src: ?5 whether the board
// has an estimate, ?4 its reset (mirrorRowEstimate).
func (src mirrorExportChangeSource) sprintIntervalsSQL() string {
	// v is each version of a covered ticket's sprint set, from and to: what
	// the first change left from the history's start, each change's to-set
	// to the next change, and the set now from the start for a ticket with
	// no change. p is each stretch of a version in a sprint of the board, an
	// open one only while the ticket is in the sprint now. e is each
	// version of the ticket's estimate, built the same way.
	estimate := `iif(CAST(?5 AS INTEGER) = 1, ` + mirrorRowEstimate("CAST(?4 AS INTEGER)") + `, NULL)`
	return `WITH v AS (
	    SELECT i.issue_id AS ticket, ` + mirrorExportHistoryStart + ` AS from_at, c.at AS to_at, c.f AS sprints
	    FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeSprint, true) + `
	    WHERE ` + mirrorExportOnBoard + ` AND ` + mirrorExportHistoryStart + ` < c.at
	    UNION ALL
	    SELECT i.issue_id, c.at, n.at, c.t
	    FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeSprint, false) + ` LEFT JOIN ` + src.next(MirrorChangeSprint) + `
	    WHERE ` + mirrorExportOnBoard + `
	    UNION ALL
	    SELECT i.issue_id, ` + mirrorExportHistoryStart + `, NULL, (SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m
	        WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id)
	    FROM ` + mirrorExportBoardIssues + `
	    WHERE ` + mirrorExportOnBoard + ` AND i.history_for > 0 AND NOT EXISTS (SELECT 1 FROM ` + src.ofKind("x", MirrorChangeSprint) + `)),
	  p AS MATERIALIZED (
	    SELECT v.ticket, j.value AS sprint, v.from_at, v.to_at FROM v CROSS JOIN json_each(v.sprints) j
	    WHERE j.value IN (SELECT id FROM main.sprints) AND coalesce(v.to_at, ` + mirrorExportNever + `) > v.from_at
	      AND (v.to_at IS NOT NULL OR EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	        WHERE m.root_id = CAST(?1 AS INTEGER) AND m.issue_id = v.ticket AND m.sprint_id = j.value))),
	  e AS MATERIALIZED (
	    SELECT i.issue_id AS ticket, ` + mirrorExportHistoryStart + ` AS from_at, c.at AS to_at, c.fn AS points
	    FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeEstimate, true) + `
	    WHERE ` + mirrorExportOnBoard + ` AND ` + mirrorExportHistoryStart + ` < c.at AND i.issue_id IN (SELECT ticket FROM p)
	    UNION ALL
	    SELECT i.issue_id, c.at, n.at, c.tn
	    FROM ` + mirrorExportBoardIssues + ` CROSS JOIN ` + src.changes(MirrorChangeEstimate, false) + ` LEFT JOIN ` + src.next(MirrorChangeEstimate) + `
	    WHERE ` + mirrorExportOnBoard + ` AND i.issue_id IN (SELECT ticket FROM p)
	    UNION ALL
	    SELECT i.issue_id, ` + mirrorExportHistoryStart + `, NULL, ` + estimate + `
	    FROM ` + mirrorExportBoardIssues + `
	    WHERE ` + mirrorExportOnBoard + ` AND i.history_for > 0 AND i.issue_id IN (SELECT ticket FROM p)
	      AND NOT EXISTS (SELECT 1 FROM ` + src.ofKind("x", MirrorChangeEstimate) + `))
	  INSERT INTO main.sprint_intervals(sprint, ticket, points, from_at, to_at)
	  SELECT p.sprint, p.ticket, iif(CAST(?5 AS INTEGER) = 1, e.points, NULL), max(p.from_at, e.from_at),
	    nullif(min(coalesce(p.to_at, ` + mirrorExportNever + `), coalesce(e.to_at, ` + mirrorExportNever + `)), ` + mirrorExportNever + `)
	  FROM p JOIN e ON e.ticket = p.ticket
	  WHERE max(p.from_at, e.from_at) < min(coalesce(p.to_at, ` + mirrorExportNever + `), coalesce(e.to_at, ` + mirrorExportNever + `))`
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
		{relation: "sprint_intervals", name: "sprint_intervals from the copy's changes", needs: []string{"sprints"}, with: "changes", sql: copied.sprintIntervalsSQL()},
	}
}
