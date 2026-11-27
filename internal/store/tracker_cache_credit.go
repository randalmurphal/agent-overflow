package store

import (
	"fmt"
	"strconv"
)

// Credit: what each person is credited with on a board, by its project's
// credit rules (MirrorRegroup.WithCredit). One fragment computes it
// (mirrorCreditRowsSQL), which the export's credit relation and the
// Backlog's pace (readMirrorPaceTx) both read, so a dashboard and the
// Backlog count credit alike. What must hold:
//   - A finished ticket credits the person it was assigned to when it
//     moved to done: on a board that plans sprints, a closed sprint's
//     member done at its completion (mirrorStatusAtCompletionSQL; its
//     status not known is listed, not counted) or an active sprint's done
//     now, at its last move into done at or before the completion
//     (mirrorExportStatusChangeSQL); on one that does not, a ticket done
//     now and updated since the window, at its last move into done, else
//     its resolution. The assignee then is the last assignee change at or
//     before it, else what the first after it is from, else the assignee
//     now.
//   - Each other person who acted on a finished ticket is a reviewer: one
//     row with the kinds of action done, counted ReviewShare percent of
//     its points when it did ReviewNeed of the kinds the rule counts, and
//     0 otherwise. A closed sprint counts what was done by its
//     completion; an active sprint and a board without sprints, up to now.
//     While the evidence a rule counts (comments, worklogs) is not read for
//     the ticket's card, the ticket has one review row not counted, saying
//     why, and none of its own. Approvals are not read: they never keep a
//     ticket's evidence unread, and a rule that counts them says so on
//     each review row (mirrorCreditApprovalsSQL).
//   - Time logged on a support ticket (mirrorSupportSQL) credits its
//     author, a point for every hours_per_point hours, or the seconds on a
//     board estimated in time: on a board that plans sprints, by the sprint
//     whose dates hold its start, the first to end of several, else the
//     next to start (mirrorCreditCountsInSQL), one row per sprint, ticket
//     and person; on one that does not, one row per worklog. A support
//     ticket whose worklogs are not read for its card has one row not
//     counted.
//   - A closed sprint counts by the rule snapshot in force at its
//     completion, an active sprint by the newest, and a board without
//     sprints by the one in force when the ticket finished, or when the
//     support work was done (mirrorCreditRuleSQL).
//   - Each source is read when its receipt is for the issue's card now,
//     and a finished ticket's worklogs when they were read whole: the due
//     rule's terms (ListMirrorActsDue), so a ticket is unread exactly
//     while the evidence unit has it to read.

// MirrorCreditRule is one snapshot of a project's credit rules: From,
// when it took effect (Unix ms, the first 0); ReviewShare, the percent of
// a finished ticket's points a reviewer gets who did ReviewNeed of the
// kinds of action it counts (Commented, Approved, LoggedTime); and
// HoursPerPoint, the hours of support work that count as a point.
type MirrorCreditRule struct {
	From                            int64
	ReviewShare, ReviewNeed         int
	Commented, Approved, LoggedTime bool
	HoursPerPoint                   int
}

// mirrorCreditRule is a MirrorCreditRule as the fragment reads it.
type mirrorCreditRule struct {
	From       int64 `json:"from"`
	Share      int   `json:"share"`
	Need       int   `json:"need"`
	Commented  int   `json:"commented"`
	Approved   int   `json:"approved"`
	LoggedTime int   `json:"logged_time"`
	HPP        int   `json:"hpp"`
}

// WithCredit is r with rules as the credit its project counts, none for a
// project that counts none. The snapshots are in order of From, the first
// from 0, each with a share of 0 to 100, a need of 1 to the kinds it
// counts, and at least an hour a point.
func (r MirrorRegroup) WithCredit(rules []MirrorCreditRule) (MirrorRegroup, error) {
	r.credit = ""
	if len(rules) == 0 {
		return r, nil
	}
	out := make([]mirrorCreditRule, len(rules))
	bit := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	for i, c := range rules {
		kinds := bit(c.Commented) + bit(c.Approved) + bit(c.LoggedTime)
		switch {
		case i == 0 && c.From != 0, i > 0 && c.From <= rules[i-1].From:
			return MirrorRegroup{}, fmt.Errorf("%w: credit rule %d takes effect at %d, not after the one before or the first at 0", ErrInvalidTrackerCache, i+1, c.From)
		case c.ReviewShare < 0 || c.ReviewShare > 100 || c.ReviewNeed < 1 || c.ReviewNeed > kinds || c.HoursPerPoint < 1:
			return MirrorRegroup{}, fmt.Errorf("%w: credit rule %d gives %d%% for %d of %d kinds of review action, a point for %d hours",
				ErrInvalidTrackerCache, i+1, c.ReviewShare, c.ReviewNeed, kinds, c.HoursPerPoint)
		}
		out[i] = mirrorCreditRule{From: c.From, Share: c.ReviewShare, Need: c.ReviewNeed, Commented: bit(c.Commented), Approved: bit(c.Approved),
			LoggedTime: bit(c.LoggedTime), HPP: c.HoursPerPoint}
	}
	encoded, err := encodeMirrorJSON(out)
	if err != nil {
		return MirrorRegroup{}, err
	}
	r.credit = encoded
	return r, nil
}

// creditArg is the credit rules as a statement binds them: NULL for none.
func (r MirrorRegroup) creditArg() any {
	if r.credit == "" {
		return nil
	}
	return r.credit
}

// mirrorCreditParams are the parameters a credit fragment reads, beside
// ?1 root and ?2 board: the regrouping (a JSON object), the board's
// estimate reset, the support rule, the credit rules, and the earliest
// instant its worklogs and support tickets are read from (an SQL
// expression each).
type mirrorCreditParams struct {
	groups, reset, support, rules, from string
}

// mirrorCreditRuleSQL is the snapshot of rules in force at instant at:
// the last whose from is at or before it, a JSON object.
func mirrorCreditRuleSQL(rules, at string) string {
	return `json_extract(` + rules + `, '$[' || (SELECT max(CAST(cr.key AS INTEGER)) FROM json_each(` + rules + `) cr
	     WHERE cr.value ->> 'from' <= ` + at + `) || ']')`
}

// mirrorCreditSprintsSQL is the CTE cr_sp of the board's sprints credit
// counts by, closed (completed) and active: each one's end hi (its
// completion, the open end for an active one), the rule it counts by,
// the board's estimate unit, lo, where its support work can begin: the
// latest completion at or before its start, no earlier than from, and
// alone, whether no other sprint's dates meet [start, hi), which holds
// [lo, hi) but for its start before lo (every other sprint that ends
// after lo ends after start): then every instant of [lo, hi) counts in
// the sprint (mirrorCreditCountsInSQL). lo and alone read the sprints
// credit counts (cr_sp0), not the board's future ones. It holds no row
// without rules.
func mirrorCreditSprintsSQL(p mirrorCreditParams) string {
	closed := strconv.Itoa(MirrorSprintClosed)
	hi := `iif(sp.state = ` + closed + `, sp.completed_at, ` + mirrorExportNever + `)`
	lo := `max(` + p.from + `, coalesce((SELECT max(o.completed_at) FROM cr_sp0 o WHERE o.state = ` + closed + ` AND o.completed_at <= sp.start_at), 0))`
	return `cr_sp0 AS MATERIALIZED (
	  SELECT sp.sprint_id, sp.state, sp.start_at, sp.completed_at, ` + hi + ` AS hi, ` + mirrorCreditRuleSQL(p.rules, hi) + ` AS rule,
	    (SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = sp.root_id AND bd.board_id = sp.board_id) AS unit
	  FROM tracker_cache_sprints sp
	  WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND ` + p.rules + ` IS NOT NULL
	    AND (sp.state = ` + closed + ` AND sp.completed_at > 0 OR sp.state = ` + strconv.Itoa(MirrorSprintActive) + `)),
	  cr_sp AS MATERIALIZED (
	  SELECT sp.sprint_id, sp.state, sp.start_at, sp.completed_at, sp.hi, sp.rule, sp.unit, ` + lo + ` AS lo,
	    NOT EXISTS (SELECT 1 FROM cr_sp0 h WHERE h.sprint_id <> sp.sprint_id AND h.start_at < sp.hi AND h.hi > sp.start_at) AS alone
	  FROM cr_sp0 sp)`
}

// mirrorCreditCountsInSQL holds for a support worklog starting at at, in
// sprint l's [lo, hi), that counts in l: of the sprints of cr_sp, l's
// dates hold it and no other's that ends first does, or none holds it and
// l is the next to start. Both hold for every instant of a sprint no other
// sprint's dates meet (l.alone), so only the others read cr_sp, a board's
// sprints, with no sort.
func mirrorCreditCountsInSQL(at string) string {
	holds := `h.start_at <= ` + at + ` AND ` + at + ` < h.hi`
	return `(l.alone OR l.start_at <= ` + at + ` AND NOT EXISTS (SELECT 1 FROM cr_sp h WHERE ` + holds + `
	      AND (h.hi < l.hi OR h.hi = l.hi AND h.sprint_id < l.sprint_id))
	    OR l.start_at > ` + at + ` AND NOT EXISTS (SELECT 1 FROM cr_sp h WHERE ` + holds + ` OR h.start_at > ` + at + `
	      AND (h.start_at < l.start_at OR h.start_at = l.start_at AND h.sprint_id < l.sprint_id)))`
}

// mirrorCreditMemberColumns are the columns of a row of cr_m, a ticket
// whose finish credit counts, as each arm of mirrorCreditMembersSQL
// selects them: its sprint (NULL on a board without sprints), whether the
// sprint is closed (its acts cut at hi) and its end, the sprint's rule
// (NULL: the one in force at the finish), the board's unit, the issue, its points, whether it
// counted done (NULL: not known) and the moment it did, and its receipts.
const mirrorCreditMemberColumns = `sprint_id, closed, hi, rule, unit, root_id, issue_id, history_gen, updated_at, assignee_id, points, done, moment,
	    c_at, c_for, c_why, w_at, w_for, w_from, w_why`

// mirrorCreditReceiptsSQL are an issue i's receipts as cr_m holds them.
const mirrorCreditReceiptsSQL = `r1.read_at, r1.read_for, r1.tried_reason, r2.read_at, r2.read_for, r2.read_from, r2.tried_reason`

// mirrorCreditMembersSQL is the CTE cr_m of the tickets whose finish
// credit counts: the members of the sprints of cr_sp chosen holds for
// (a condition on its row l), and with weeks the done tickets of a board
// that plans no sprints updated since from.
func mirrorCreditMembersSQL(p mirrorCreditParams, chosen string, weeks bool) string {
	done := mirrorDoneStatusesSQL(p.groups)
	points := `iif(l.unit <> ` + strconv.Itoa(MirrorEstimateNone) + `, ` + mirrorRowEstimate(p.reset) + `, NULL)`
	sprints := `SELECT l.sprint_id, l.state = ` + strconv.Itoa(MirrorSprintClosed) + `, l.hi, l.rule, l.unit, i.root_id, i.issue_id, i.history_gen, i.updated_at,
	    i.assignee_id, ` + points + `, iif(l.state = ` + strconv.Itoa(MirrorSprintClosed) + `, ` + mirrorStatusAtCompletionSQL + ` IN (` + done + `),
	    i.status_id IN (` + done + `)), NULL, ` + mirrorCreditReceiptsSQL + `
	  FROM cr_sp l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id` + mirrorActsReceiptsSQL + `
	  WHERE ` + chosen
	if weeks {
		unit := `(SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = i.root_id AND bd.board_id = ?2)`
		sprints += `
	  UNION ALL
	  SELECT NULL, 0, NULL, NULL, ` + unit + `, i.root_id, i.issue_id, i.history_gen, i.updated_at, i.assignee_id,
	    iif(` + unit + ` <> ` + strconv.Itoa(MirrorEstimateNone) + `, ` + mirrorRowEstimate(p.reset) + `, NULL), 1,
	    coalesce(` + mirrorExportStatusChangeSQL("i", "max", done, "") + `, nullif(i.resolved_at, 0)), ` + mirrorCreditReceiptsSQL + `
	  FROM tracker_cache_issues i
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id` + mirrorActsReceiptsSQL + `
	  WHERE ` + mirrorUpdatedSinceSQL("i", ">=", p.from) + ` AND ` + p.rules + ` IS NOT NULL
	    AND i.status_id IN (` + done + `) AND ` + mirrorCreditWeeksSQL
	}
	return `cr_m(` + mirrorCreditMemberColumns + `) AS MATERIALIZED (` + sprints + `)`
}

// mirrorCreditWeeksSQL holds for a board ?2 of root ?1 that plans no
// sprints, which credit counts by the week.
const mirrorCreditWeeksSQL = `EXISTS (SELECT 1 FROM tracker_cache_boards bd WHERE bd.root_id = CAST(?1 AS INTEGER) AND bd.board_id = ?2 AND bd.plans_sprints = 0)`

// mirrorCreditCurrentSQL holds for a receipt (its read_at, read_for and,
// for a finished ticket's worklogs, read_from) of issue's card now
// (updated): the due rule's (mirrorCommentsDueSQL, mirrorWorklogsDueSQL),
// so it holds exactly when the source is not due for its card.
func mirrorCreditCurrentSQL(readAt, readFor, readFrom, updated string) string {
	whole := ""
	if readFrom != "" {
		whole = ` AND ` + readFrom + ` = 0`
	}
	return `(coalesce(` + readAt + `, 0) > 0 AND ` + readFor + ` > 0 AND ` + readFor + ` >= ` + updated + whole + `)`
}

// mirrorCreditWhySQL is why a source of issue issue whose receipt (its
// read_at) is not current is not read: the reason its last read failed
// for the issue alone (stored as "issue ID: ..."), else that it has
// never been read, else that the card changed since.
func mirrorCreditWhySQL(issue, readAt, reason string) string {
	prefix := `'issue ' || ` + issue + ` || ': '`
	return `CASE WHEN ` + reason + ` <> '' THEN 'could not be read: ' || iif(substr(` + reason + `, 1, length(` + prefix + `)) = ` + prefix + `,
	      substr(` + reason + `, length(` + prefix + `) + 1), ` + reason + `)
	    WHEN coalesce(` + readAt + `, 0) = 0 THEN 'who worked on it is not read yet'
	    ELSE 'it changed since who worked on it was read' END`
}

// mirrorCreditFinishedSQL is the CTEs cr_d and cr_f of cr_m's tickets
// that counted done, or whose status then is not known: cr_d gives each
// the moment a sprint member moved to done (its last move into done by
// the sprint's end) and the rule it counts by (its sprint's, else the one
// in force when it finished), once each, since cr_f reads them in several
// columns; cr_f the person it was assigned to at that moment, and
// why its evidence is not read for its card (NULL when it is): the first
// source the rule counts that is not current.
func mirrorCreditFinishedSQL(p mirrorCreditParams) string {
	assignee := func(cmp, value, order string) string {
		return `(SELECT c.` + value + ` FROM tracker_cache_changes c WHERE c.root_id = cm.root_id AND c.issue_id = cm.issue_id AND c.gen = cm.history_gen
	      AND c.kind = 4 AND c.at ` + cmp + ` cm.moment ORDER BY c.seq` + order + ` LIMIT 1)`
	}
	comments := `(cm.rule ->> 'commented' = 1 AND NOT ` + mirrorCreditCurrentSQL("cm.c_at", "cm.c_for", "", "cm.updated_at") + `)`
	worklogs := `(cm.rule ->> 'logged_time' = 1 AND NOT ` + mirrorCreditCurrentSQL("cm.w_at", "cm.w_for", "cm.w_from", "cm.updated_at") + `)`
	return `cr_d AS MATERIALIZED (
	  SELECT cm.sprint_id, cm.closed, cm.hi, cm.root_id, cm.issue_id, cm.history_gen, cm.updated_at, cm.assignee_id, cm.points, cm.done,
	    iif(cm.sprint_id IS NULL, cm.moment, ` + mirrorExportStatusChangeSQL("cm", "max", mirrorDoneStatusesSQL(p.groups), "cm.hi") + `) AS moment,
	    coalesce(cm.rule, ` + mirrorCreditRuleSQL(p.rules, `coalesce(cm.moment, `+mirrorExportNever+`)`) + `) AS rule,
	    cm.c_at, cm.c_for, cm.c_why, cm.w_at, cm.w_for, cm.w_from, cm.w_why
	  FROM cr_m cm WHERE cm.done IS NOT 0),
	  cr_f AS MATERIALIZED (
	  SELECT cm.sprint_id, cm.closed, cm.hi, cm.rule, cm.issue_id, cm.points, cm.done, cm.moment,
	    nullif(coalesce(` + assignee("<=", "to_value", " DESC") + `, ` + assignee(">", "from_value", "") + `, cm.assignee_id), '') AS person,
	    CASE WHEN ` + comments + ` THEN ` + mirrorCreditWhySQL("cm.issue_id", "cm.c_at", "cm.c_why") + `
	         WHEN ` + worklogs + ` THEN ` + mirrorCreditWhySQL("cm.issue_id", "cm.w_at", "cm.w_why") + ` END AS unread
	  FROM cr_d cm)`
}

// mirrorCreditApprovalsSQL is a review row's note while approvals are not
// read, for a review row of rule g.rule that counts them: the one place
// that says so.
const mirrorCreditApprovalsSQL = `iif(g.rule ->> 'approved' = 1, 'approvals are not read yet', NULL)`

// MirrorCreditColumns are the columns of a credit row, as
// mirrorCreditRowsSQL selects them and the export's credit relation
// holds them (MirrorExportSchema).
const mirrorCreditColumns = `sprint, at, ticket, kind, person, value, share, points, review_share, review_need, did, qualified, hours, hours_per_point, note`

// mirrorCreditRowsSQL is the credit rows of cr_f and of the support
// tickets (mirrorCreditColumns): of the sprints of cr_sp chosen holds
// for, and with weeks of a board that plans no sprints.
func mirrorCreditRowsSQL(p mirrorCreditParams, chosen string, weeks bool) string {
	support := mirrorSupportSQL(p.support, "?2", mirrorRowEstimate(p.reset))
	worklogs := strconv.Itoa(MirrorActWorklog)
	current := mirrorCreditCurrentSQL("r2.read_at", "r2.read_for", "", "i.updated_at")
	// The support value of seconds of rule r on a board of unit u.
	supportValue := func(seconds, r, u string) string {
		return `iif(` + u + ` = ` + strconv.Itoa(MirrorEstimateTime) + `, ` + seconds + `, ` + seconds + ` / 3600.0 / (` + r + ` ->> 'hpp'))`
	}
	rows := `SELECT f.sprint_id, f.moment, f.issue_id, 'finished', f.person, iif(f.done IS NULL OR f.moment IS NULL AND f.sprint_id IS NULL, NULL, coalesce(f.points, 0)),
	    iif(f.done IS NULL OR f.moment IS NULL AND f.sprint_id IS NULL, NULL, 1), f.points, NULL, NULL, NULL, NULL, NULL, NULL,
	    CASE WHEN f.done IS NULL THEN 'its status when the sprint ended is not known'
	         WHEN f.moment IS NULL AND f.sprint_id IS NULL THEN 'when it finished is not known' END
	  FROM cr_f f
	  UNION ALL
	  SELECT f.sprint_id, f.moment, f.issue_id, 'review', NULL, NULL, NULL, f.points, f.rule ->> 'share', f.rule ->> 'need', NULL, NULL, NULL, NULL, f.unread
	  FROM cr_f f WHERE f.done = 1 AND f.unread IS NOT NULL
	  UNION ALL
	  SELECT g.sprint_id, g.moment, g.issue_id, 'review', g.person, iif(g.qualified, coalesce(g.points, 0) * (g.rule ->> 'share') / 100.0, 0),
	    iif(g.qualified, (g.rule ->> 'share') / 100.0, 0), g.points, g.rule ->> 'share', g.rule ->> 'need',
	    nullif(rtrim(iif(g.commented, 'commented, ', '') || iif(g.approved, 'approved, ', '') || iif(g.logged, 'logged time, ', ''), ', '), ''),
	    g.qualified, NULL, NULL, ` + mirrorCreditApprovalsSQL + `
	  FROM (SELECT d.sprint_id, d.moment, d.issue_id, d.points, d.rule, d.person, d.commented, d.approved, d.logged,
	      (d.rule ->> 'commented') * d.commented + (d.rule ->> 'approved') * d.approved + (d.rule ->> 'logged_time') * d.logged
	      >= (d.rule ->> 'need') AS qualified FROM (
	    SELECT f.sprint_id, f.moment, f.issue_id, f.points, f.rule, a.person, max(a.source = ` + strconv.Itoa(MirrorActComment) + `) AS commented,
	      max(a.source = 3) AS approved, max(a.source = ` + worklogs + `) AS logged
	    FROM cr_f f
	    CROSS JOIN tracker_cache_issue_acts a ON a.root_id = CAST(?1 AS INTEGER) AND a.issue_id = f.issue_id
	    WHERE f.done = 1 AND f.unread IS NULL AND a.person <> '' AND a.person IS NOT f.person AND (NOT f.closed OR a.at <= f.hi)
	    GROUP BY f.sprint_id, f.issue_id, a.person) d) g
	  UNION ALL
	  SELECT s.sprint_id, NULL, s.issue_id, 'support', s.person, ` + supportValue("s.seconds", "s.rule", "s.unit") + `, s.seconds / 3600.0 / (s.rule ->> 'hpp'),
	    NULL, NULL, NULL, NULL, NULL, s.seconds / 3600.0, s.rule ->> 'hpp', NULL
	  FROM (SELECT l.sprint_id, l.rule, l.unit, a.issue_id, a.person, total(a.seconds) AS seconds
	    FROM cr_sp l
	    CROSS JOIN tracker_cache_issue_acts a ON a.root_id = CAST(?1 AS INTEGER) AND a.source = ` + worklogs + ` AND a.at >= l.lo AND a.at < l.hi
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = a.root_id AND b.board_id = ?2 AND b.issue_id = a.issue_id
	    CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	    CROSS JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	    WHERE ` + chosen + ` AND ` + current + ` AND ` + support + ` AND ` + mirrorCreditCountsInSQL("a.at") + `
	    GROUP BY l.sprint_id, a.issue_id, a.person) s
	  UNION ALL
	  SELECT l.sprint_id, i.updated_at, i.issue_id, 'support', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
	    ` + mirrorCreditWhySQL("i.issue_id", "r2.read_at", "r2.tried_reason") + `
	  FROM cr_sp l
	  CROSS JOIN tracker_cache_issues i ON ` + mirrorUpdatedSinceSQL("i", ">=", "l.lo") + ` AND i.updated_at < l.hi
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id
	  LEFT JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	  WHERE ` + chosen + ` AND ` + support + ` AND NOT ` + current + ` AND ` + mirrorCreditCountsInSQL("i.updated_at")
	if weeks {
		unit := `(SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = CAST(?1 AS INTEGER) AND bd.board_id = ?2)`
		rule := mirrorCreditRuleSQL(p.rules, "a.at")
		rows += `
	  UNION ALL
	  SELECT NULL, a.at, a.issue_id, 'support', a.person, ` + supportValue("a.seconds", rule, unit) + `, a.seconds / 3600.0 / (` + rule + ` ->> 'hpp'),
	    NULL, NULL, NULL, NULL, NULL, a.seconds / 3600.0, ` + rule + ` ->> 'hpp', NULL
	  FROM tracker_cache_issue_acts a
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = a.root_id AND b.board_id = ?2 AND b.issue_id = a.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	  CROSS JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	  WHERE a.root_id = CAST(?1 AS INTEGER) AND a.source = ` + worklogs + ` AND a.at >= ` + p.from + ` AND ` + p.rules + ` IS NOT NULL
	    AND ` + mirrorCreditWeeksSQL + ` AND ` + current + ` AND ` + support + `
	  UNION ALL
	  SELECT NULL, i.updated_at, i.issue_id, 'support', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
	    ` + mirrorCreditWhySQL("i.issue_id", "r2.read_at", "r2.tried_reason") + `
	  FROM tracker_cache_issues i
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id
	  LEFT JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	  WHERE ` + mirrorUpdatedSinceSQL("i", ">=", p.from) + ` AND ` + p.rules + ` IS NOT NULL
	    AND ` + mirrorCreditWeeksSQL + ` AND NOT ` + current + ` AND ` + support
	}
	return rows
}

// mirrorCreditSQL is the WITH list of the credit fragment (cr_sp0,
// cr_sp, cr_m, cr_d, cr_f) of the sprints of cr_sp chosen holds for, and with weeks of a
// board that plans no sprints, after the caller's own CTEs (before, ""
// for none), and its rows (mirrorCreditRowsSQL).
func mirrorCreditSQL(p mirrorCreditParams, before, chosen string, weeks bool) (with, rows string) {
	with = `WITH `
	if before != "" {
		with += before + `,
	  `
	}
	with += mirrorCreditSprintsSQL(p) + `,
	  ` + mirrorCreditMembersSQL(p, chosen, weeks) + `,
	  ` + mirrorCreditFinishedSQL(p)
	return with, mirrorCreditRowsSQL(p, chosen, weeks)
}
