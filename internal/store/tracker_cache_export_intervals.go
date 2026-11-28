package store

import (
	"database/sql"
	"strconv"
)

// The export's intervals: what a ticket held over time, rebuilt from its
// stored history and its card (status_intervals, sprint_intervals).
//
// What must hold:
//   - They reach back to the earliest instant a count can ask of them
//     (mirrorExportEarliest): the start of the mirror's history window, or
//     of a sprint the board keeps that began before it. No range's period
//     and no sprint a count shows begins earlier. An interval that ends at
//     or before it has no row, and a ticket's history is read only when
//     what it held may have changed since then, so a fill follows what
//     changed in the window, not the board's size.
//   - Every interval is evidence of the card's version (its update time),
//     the version the board shows: the history is read only up to it, so
//     a change the tracker made after the card was read (the history read
//     later holds it) makes no stretch, and a stretch the card holds now
//     carries the card's value. After any order of card and history
//     writes the intervals hold what the ticket held up to the card's
//     version (TestTheIntervalsHoldWhatTheIssueHeldInAnyOrderOfReads).
//   - A status interval is one change to the next, or what the first
//     change left from the history's start (history_from, else creation),
//     each closed no later than the status now began; the status now is
//     open from status_since, the store's one rule for when it began
//     (MirrorStatusSince), when that is known; and when it is not known
//     but the history covers the card (mirrorHistoryCovers) with no status
//     change up to its version (Linear's fold hides when the status began,
//     not what it was), it is open from the history's start. began is a
//     stretch's start when that is when the status began: a change, or a
//     history complete from creation; NULL otherwise. A ticket whose status
//     now began at or before the earliest instant, or that was last updated
//     then (mirrorExportUpdatedSince), has its interval now alone, which
//     covers every instant a count can ask; its history is not read.
//   - A sprint interval is a stretch a ticket spent in one of the board's
//     sprints with one estimate of the board, of a ticket whose history
//     covers its card in the board's estimate definition
//     (mirrorExportHistoryCurrent); a ticket whose history does not has no
//     sprint interval, and tickets.history_behind says so: its history
//     says nothing of the stretch since its latest change, which may be
//     newer than the history. The sprint sets the history holds (the first
//     change's from-set from the history's start, each change's to-set to
//     the next) and the card's set from the latest change, or the
//     history's start for none, are split where the estimate changed, the
//     same way. A stretch may be cut in rows where the ticket's other
//     sprints or its estimate changed. A ticket's history is read only
//     when it is in one of the board's sprints now, or it was updated
//     after the earliest instant and its latest sprint change is after it:
//     one in none now that changed none since was in none of them at any
//     instant a count can ask. A history is read for the card's update
//     time (mirrorHistoryDue), so a sprint change the store holds moved the
//     card's update time; the update time picks those issues by
//     idx_tracker_cache_issues_updated, never visiting the board's others.
//   - A fill reads an issue's changes by its key, from the file or, when
//     the same export copies the changes, from the copy
//     (mirrorExportFill.with and without), so a count that reads both
//     does not read the file's history twice. Each change probes its next
//     by key, the first and latest of a kind are probes of their own, and
//     the status fill probes, for each ticket whose status time is not
//     known and whose history covers its card, whether it changed status.

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

// latestBy is the time of issue on's latest change of kind at or before
// upTo, an SQL expression; NULL for none.
func (src mirrorExportChangeSource) latestBy(on, kind, upTo string) string {
	return `(SELECT x.at FROM ` + src.ofKind(on, "x", kind) + ` AND x.at <= ` + upTo + ` ORDER BY x.seq DESC LIMIT 1)`
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
	return mirrorUpdatedSinceSQL(alias, ">", mirrorExportIntervalsFrom())
}

// mirrorUpdatedSinceSQL is the condition that issue alias of root ?1 was
// updated cmp (">" or ">=") from, an SQL expression never below zero,
// which walks the issues by their update time
// (idx_tracker_cache_issues_updated) from from on. It states the index's
// own condition, an update time above zero, so the planner can take the
// index, after the bound: of two lower bounds on the column, the planner
// starts the walk at the first, and from 0 it reads every issue of the
// root (TestAnUpdateWalkStartsAtItsBound).
func mirrorUpdatedSinceSQL(alias, cmp, from string) string {
	return alias + `.root_id = CAST(?1 AS INTEGER) AND ` + alias + `.updated_at ` + cmp + ` ` + from + ` AND ` + alias + `.updated_at > 0`
}

// mirrorExportBoardIssues reads the board's issues as b and i, by key.
const mirrorExportBoardIssues = `tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id`

// mirrorExportOnBoard is the condition that limits b to the board.
const mirrorExportOnBoard = `b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`

// mirrorExportHistoryStart is where issue i's history begins, NULL while
// it is not read: as tickets.history_from.
const mirrorExportHistoryStart = `iif(i.history_for > 0, iif(i.history_from > 0, i.history_from, nullif(i.created_at, 0)), NULL)`

// mirrorExportHistoryBegan is when what issue i held where its history
// begins began: that start when the history is complete from creation,
// else NULL (not known).
const mirrorExportHistoryBegan = `iif(i.history_from > 0, NULL, ` + mirrorExportHistoryStart + `)`

// mirrorExportHistoryCurrent holds for an issue i, with its board row b,
// whose history covers its card (mirrorHistoryCovers) in the board's
// estimate definition (mirrorRowEstimateRead, ?4 the board's reset): its
// sprint stretches are known from the history's start to now. The sprint
// intervals and tickets.history_behind state it.
var mirrorExportHistoryCurrent = `(` + mirrorHistoryCovers("i") + ` AND (CAST(?5 AS INTEGER) = 0 OR ` + mirrorRowEstimateRead("CAST(?4 AS INTEGER)") + `))`

// mirrorExportStatusOpen is whether the board counts status, an SQL
// expression, not done, as tickets.open (mirrorStatusNotDone, ?3 the
// regrouping).
func mirrorExportStatusOpen(status string) string {
	return `iif(` + mirrorStatusNotDone(status, "?3") + `, 1, 0)`
}

// mirrorExportNever stands for an open end where the intervals' fills
// compare ends: no time the mirror holds reaches it.
const mirrorExportNever = `9223372036854775807`

// statusIntervalsSQL fills status_intervals from src: each status change
// to the next and what the first left from the history's start, up to
// the stretch the card holds; the status now from status_since; and the
// status of a card its history covers with no change, from the history's
// start. ri is the issues whose history is read, each with its history's
// start, when what it held there began, and upTo, where the stretches
// the history gives end: the status now's start when known, else the
// card's version.
func (src mirrorExportChangeSource) statusIntervalsSQL() string {
	from := mirrorExportIntervalsFrom()
	return `WITH ri AS MATERIALIZED (
	    SELECT i.root_id, i.issue_id, i.history_gen, ` + mirrorExportHistoryStart + ` AS start, ` + mirrorExportHistoryBegan + ` AS began,
	      iif(i.status_since > 0, i.status_since, i.updated_at) AS up_to
	    FROM tracker_cache_issues i
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.issue_id = i.issue_id AND b.board_id = ?2
	    WHERE ` + mirrorExportUpdatedSince("i") + ` AND NOT (i.status_since > 0 AND i.status_since <= ` + from + `))
	  INSERT INTO main.status_intervals(ticket, status, from_at, to_at, began, open)
	  SELECT x.ticket, x.status, x.from_at, x.to_at, x.began, ` + mirrorExportStatusOpen("x.status") + ` FROM (
	  SELECT ri.issue_id AS ticket, c.t AS status, c.at AS from_at, n.at AS to_at, c.at AS began
	  FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeStatus, false) + ` CROSS JOIN ` + src.next("ri", MirrorChangeStatus) + `
	  WHERE n.at > c.at AND n.at > ` + from + ` AND n.at <= ri.up_to AND c.t IS NOT NULL
	  UNION ALL
	  SELECT ri.issue_id, c.f, ri.start, c.at, ri.began
	  FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeStatus, true) + `
	  WHERE c.f IS NOT NULL AND ri.start < c.at AND c.at > ` + from + ` AND c.at <= ri.up_to
	  UNION ALL
	  SELECT i.issue_id, i.status_id, i.status_since, NULL, i.status_since
	  FROM ` + mirrorExportBoardIssues + `
	  WHERE ` + mirrorExportOnBoard + ` AND i.status_since > 0 AND i.status_id <> ''
	  UNION ALL
	  SELECT i.issue_id, i.status_id, ` + mirrorExportHistoryStart + `, NULL, ` + mirrorExportHistoryBegan + `
	  FROM ` + mirrorExportBoardIssues + `
	  WHERE ` + mirrorExportOnBoard + ` AND i.status_since = 0 AND i.status_id <> '' AND ` + mirrorHistoryCovers("i") + `
	    AND ` + mirrorExportHistoryStart + ` IS NOT NULL AND ` + src.latestBy("i", MirrorChangeStatus, "i.updated_at") + ` IS NULL) x`
}

// sprintIntervalsSQL fills sprint_intervals from src: ?5 whether the board
// has an estimate, ?4 its reset (mirrorRowEstimate).
func (src mirrorExportChangeSource) sprintIntervalsSQL() string {
	from := mirrorExportIntervalsFrom()
	never := mirrorExportNever
	// pick is the root's issues in one of the board's sprints now, by the
	// sprints' members, and those updated after the earliest instant, by
	// their update time. ri is those of them on the board whose history is
	// current (mirrorExportHistoryCurrent), each with its history's start,
	// its card's version (up_to), estimate and sprints: one of the first,
	// or one of the second whose latest sprint change is after the
	// earliest instant. v is each version of their sprint sets, from and
	// to: what the first change left from the history's start, each
	// change's to-set to the next, both up to the card's version, and the
	// card's set from the latest change, or the history's start for none.
	// p is each stretch of a version in a sprint of the board that ends
	// after the earliest instant. e is each version of their estimates,
	// built the same way.
	return `WITH pick AS MATERIALIZED (
	    SELECT m.issue_id, 1 AS member FROM main.sprints sp
	    CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = sp.id
	    UNION
	    SELECT u.issue_id, 0 FROM tracker_cache_issues u WHERE ` + mirrorExportUpdatedSince("u") + `),
	  ri AS MATERIALIZED (
	    SELECT i.root_id, i.issue_id, i.history_gen, ` + mirrorExportHistoryStart + ` AS start, i.updated_at AS up_to,
	      iif(CAST(?5 AS INTEGER) = 1, ` + mirrorRowEstimate("CAST(?4 AS INTEGER)") + `, NULL) AS estimate,
	      (SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id) AS sprints
	    FROM (SELECT issue_id, max(member) AS member FROM pick GROUP BY issue_id) pk
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND b.issue_id = pk.issue_id
	    CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	    WHERE ` + mirrorExportHistoryCurrent + ` AND (pk.member = 1 OR coalesce(` + src.latest("i", MirrorChangeSprint, "at") + `, 0) > ` + from + `)),
	  v AS (
	    SELECT ri.issue_id AS ticket, ri.start AS from_at, c.at AS to_at, c.f AS sprints
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeSprint, true) + ` WHERE ri.start < c.at AND c.at <= ri.up_to
	    UNION ALL
	    SELECT ri.issue_id, c.at, n.at, c.t
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeSprint, false) + ` CROSS JOIN ` + src.next("ri", MirrorChangeSprint) + `
	    WHERE n.at <= ri.up_to
	    UNION ALL
	    SELECT ri.issue_id, coalesce(` + src.latestBy("ri", MirrorChangeSprint, "ri.up_to") + `, ri.start), NULL, ri.sprints FROM ri),
	  p AS MATERIALIZED (
	    SELECT v.ticket, j.value AS sprint, v.from_at, v.to_at FROM v CROSS JOIN json_each(v.sprints) j
	    WHERE j.value IN (SELECT id FROM main.sprints) AND coalesce(v.to_at, ` + never + `) > max(v.from_at, ` + from + `)),
	  e AS MATERIALIZED (
	    SELECT ri.issue_id AS ticket, ri.start AS from_at, c.at AS to_at, c.fn AS points
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeEstimate, true) + ` WHERE ri.start < c.at AND c.at <= ri.up_to
	    UNION ALL
	    SELECT ri.issue_id, c.at, n.at, c.tn
	    FROM ri CROSS JOIN ` + src.changes("ri", MirrorChangeEstimate, false) + ` CROSS JOIN ` + src.next("ri", MirrorChangeEstimate) + `
	    WHERE n.at <= ri.up_to
	    UNION ALL
	    SELECT ri.issue_id, coalesce(` + src.latestBy("ri", MirrorChangeEstimate, "ri.up_to") + `, ri.start), NULL, ri.estimate FROM ri)
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
