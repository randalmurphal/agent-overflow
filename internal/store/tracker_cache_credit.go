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
//     now, read only from a history that covers the card's version
//     (mirrorHistoryReadForCardSQL): until it does, the ticket names no
//     person, and its reviews are one row not counted, as who had it
//     decides who reviewed it.
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
//     next to start (mirrorCreditCountsInSQL), of the closed and active
//     sprints, one row per sprint, ticket and person; on one that does
//     not, one row per worklog. Work after the last completion while no
//     sprint is active counts in none until the next one starts. A support
//     ticket whose worklogs are not read for its card has one row not
//     counted.
//   - A closed sprint counts by the rule snapshot in force at its
//     completion, an active sprint by the newest, and a board without
//     sprints by the one in force when the ticket finished, or when the
//     support work was done (mirrorCreditRuleSQL).
//   - Each source is read when its receipt is current for the issue's
//     card now, a finished ticket's worklogs read whole
//     (mirrorActsCurrentSQL), the rule the due listing negates
//     (ListMirrorActsDue), so a ticket is unread exactly while the
//     evidence unit has it to read.

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
// estimate reset, the support rule ("" for a statement that reads none:
// no ticket is support), the credit rules, the earliest update a board
// without sprints reads its tickets from (its weeks), and until, the
// instant after which no act counts: the evaluation's clock, or
// mirrorExportNever for a read of closed sprints alone (an SQL expression
// each).
type mirrorCreditParams struct {
	groups, reset, support, rules, from, until string
}

// mirrorCreditRuleSQL is the snapshot of rules in force at instant at:
// the last whose from is at or before it, a JSON object.
func mirrorCreditRuleSQL(rules, at string) string {
	return `json_extract(` + rules + `, '$[' || (SELECT max(CAST(cr.key AS INTEGER)) FROM json_each(` + rules + `) cr
	     WHERE cr.value ->> 'from' <= ` + at + `) || ']')`
}

// mirrorCreditSprintsSQL is the CTEs cr_sp0 and cr_sp of the board's
// sprints credit places by, closed (completed) and active: no range
// shows a sprint that has not started. cr_sp0 is every one of them with
// its end hi (its completion; until for an active one), which support
// placement weighs one against another (mirrorCreditCountsInSQL). cr_sp
// is those chosen holds for (a condition on the alias it is given), the
// ones a reader counts, each with the rule it counts by, the board's
// estimate unit, lo, where its support work can begin: the latest
// completion at or before its start, else its start (work before the
// board's first sprint the mirror holds is an earlier sprint's, which it
// does not), and alone, whether no other sprint's dates meet [start,
// hi), which holds [lo, hi) but for its start before lo (every other
// sprint that ends after lo ends after start): then every instant of
// [lo, hi) counts in the sprint. Neither holds a row without rules, nor
// of a board that plans no sprints, which counts by the week.
func mirrorCreditSprintsSQL(p mirrorCreditParams, chosen func(alias string) string) string {
	closed := strconv.Itoa(MirrorSprintClosed)
	hi := `iif(sp.state = ` + closed + `, sp.completed_at, ` + p.until + `)`
	lo := `coalesce((SELECT max(o.completed_at) FROM cr_sp0 o WHERE o.state = ` + closed + ` AND o.completed_at <= sp.start_at), sp.start_at)`
	return `cr_sp0 AS MATERIALIZED (
	  SELECT sp.sprint_id, sp.state, sp.start_at, sp.completed_at, ` + hi + ` AS hi
	  FROM tracker_cache_sprints sp
	  WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND ` + p.rules + ` IS NOT NULL AND NOT ` + mirrorCreditWeeksSQL + `
	    AND (sp.state = ` + closed + ` AND sp.completed_at > 0 OR sp.state = ` + strconv.Itoa(MirrorSprintActive) + `)),
	  cr_sp AS MATERIALIZED (
	  SELECT sp.sprint_id, sp.state, sp.start_at, sp.completed_at, sp.hi, ` + mirrorCreditRuleSQL(p.rules, "sp.hi") + ` AS rule,
	    (SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = CAST(?1 AS INTEGER) AND bd.board_id = ?2) AS unit, ` + lo + ` AS lo,
	    NOT EXISTS (SELECT 1 FROM cr_sp0 h WHERE h.sprint_id <> sp.sprint_id AND h.start_at < sp.hi AND h.hi > sp.start_at) AS alone
	  FROM cr_sp0 sp WHERE ` + chosen("sp") + `)`
}

// mirrorCreditCountsInSQL holds for a support worklog starting at at, in
// sprint l's [lo, hi), that counts in l: of the sprints of cr_sp0, l's
// dates hold it and no other's that ends first does, or none holds it and
// l is the next to start. Both hold for every instant of a sprint no other
// sprint's dates meet (l.alone), so only the others read cr_sp0, a
// board's sprints, with no sort.
func mirrorCreditCountsInSQL(at string) string {
	holds := `h.start_at <= ` + at + ` AND ` + at + ` < h.hi`
	return `(l.alone OR l.start_at <= ` + at + ` AND NOT EXISTS (SELECT 1 FROM cr_sp0 h WHERE ` + holds + `
	      AND (h.hi < l.hi OR h.hi = l.hi AND h.sprint_id < l.sprint_id))
	    OR l.start_at > ` + at + ` AND NOT EXISTS (SELECT 1 FROM cr_sp0 h WHERE ` + holds + ` OR h.start_at > ` + at + `
	      AND (h.start_at < l.start_at OR h.start_at = l.start_at AND h.sprint_id < l.sprint_id)))`
}

// mirrorCreditMemberColumns are the columns of a row of cr_m, a ticket
// whose finish credit counts, as each arm of mirrorCreditMembersSQL
// selects them: its sprint (NULL on a board without sprints), whether the
// sprint is closed and its end (until for an open sprint and a week: acts
// count up to it), the sprint's rule (NULL: the one in force at the
// finish), the board's unit, the issue, whether its history covers its
// card (mirrorHistoryReadForCardSQL), its points, whether it counted done
// (NULL: not known), whether it is a support ticket (credited by its
// time alone), and its receipts, c_ its comments' and w_ its worklogs'.
const mirrorCreditMemberColumns = `sprint_id, closed, hi, rule, unit, root_id, issue_id, history_gen, covered, updated_at, resolved_at, assignee_id, points,
	    done, support, c_read_at, c_read_for, c_tried_at, c_tried_reason, w_read_at, w_read_for, w_read_from, w_tried_at, w_tried_reason`

// mirrorCreditIssueSQL are an issue i's columns of cr_m from its history
// to its receipts.
const mirrorCreditIssueSQL = `i.history_gen, ` + `(` + mirrorHistoryReadForCardSQL + `), i.updated_at, i.resolved_at, i.assignee_id`

// mirrorCreditReceiptsSQL are an issue i's receipts as cr_m holds them.
const mirrorCreditReceiptsSQL = `r1.read_at, r1.read_for, r1.tried_at, r1.tried_reason, r2.read_at, r2.read_for, r2.read_from, r2.tried_at, r2.tried_reason`

// mirrorCreditSupportSQL is whether issue i (its row b) of the board is a
// support ticket, by p's support rule: never without one.
func mirrorCreditSupportSQL(p mirrorCreditParams) string {
	if p.support == "" {
		return `0`
	}
	return mirrorSupportSQL(p.support, "?2", mirrorRowEstimate(p.reset))
}

// mirrorCreditMembersSQL is the CTE cr_m of the tickets whose finish
// credit counts: the members of the closed and active sprints of cr_sp,
// and with weeks the done tickets of a board that plans no sprints
// updated since from.
func mirrorCreditMembersSQL(p mirrorCreditParams, weeks bool) string {
	done := mirrorDoneStatusesSQL(p.groups)
	estimate := mirrorRowEstimate(p.reset)
	points := `iif(l.unit <> ` + strconv.Itoa(MirrorEstimateNone) + `, ` + estimate + `, NULL)`
	support := mirrorCreditSupportSQL(p)
	sprints := `SELECT l.sprint_id, l.state = ` + strconv.Itoa(MirrorSprintClosed) + `, l.hi, l.rule, l.unit, i.root_id, i.issue_id, ` + mirrorCreditIssueSQL + `,
	    ` + points + `, iif(l.state = ` + strconv.Itoa(MirrorSprintClosed) + `, ` + mirrorStatusAtCompletionSQL + ` IN (` + done + `),
	    i.status_id IN (` + done + `)), ` + support + `, ` + mirrorCreditReceiptsSQL + `
	  FROM cr_sp l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id` + mirrorActsReceiptsSQL
	if weeks {
		unit := `(SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = i.root_id AND bd.board_id = ?2)`
		sprints += `
	  UNION ALL
	  SELECT NULL, 0, ` + p.until + `, NULL, ` + unit + `, i.root_id, i.issue_id, ` + mirrorCreditIssueSQL + `,
	    iif(` + unit + ` <> ` + strconv.Itoa(MirrorEstimateNone) + `, ` + estimate + `, NULL), 1, ` + support + `, ` + mirrorCreditReceiptsSQL + `
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

// mirrorCreditFinishedSQL is the CTEs cr_d0, cr_d1 and cr_f of cr_m's
// tickets that counted done, or whose status then is not known, and are
// not support tickets: cr_d0 gives each the moment it moved to done (its
// last move into done by its sprint's end or until, else its resolution
// by then; NULL: not known), cr_d1 the rule it counts by (its sprint's,
// else the one in force when it finished) and whether who had it then is
// not known (unowned: the moment is not known and its history holds an
// assignee change); cr_f the rule's terms, the person it was assigned to
// at that moment (NULL when unowned, or while its history does not cover
// its card, covered), and why its reviews are not read for its card (NULL when
// they are): its history not covering it, else the first source the rule
// counts that is not current (mirrorActsCurrentSQL).
func mirrorCreditFinishedSQL(p mirrorCreditParams) string {
	assignee := func(cmp, value, order string) string {
		return `(SELECT c.` + value + ` FROM tracker_cache_changes c WHERE c.root_id = cm.root_id AND c.issue_id = cm.issue_id AND c.gen = cm.history_gen
	      AND c.kind = 4 AND c.at ` + cmp + ` cm.moment ORDER BY c.seq` + order + ` LIMIT 1)`
	}
	reassigned := `EXISTS (SELECT 1 FROM tracker_cache_changes c WHERE c.root_id = cm.root_id AND c.issue_id = cm.issue_id AND c.gen = cm.history_gen
	      AND c.kind = 4)`
	comments := `(cm.rule_now ->> 'commented' = 1 AND NOT ` + mirrorActsCurrentSQL(receipt("cm.c_"), "cm.updated_at", "") + `)`
	worklogs := `(cm.rule_now ->> 'logged_time' = 1 AND NOT ` + mirrorActsCurrentSQL(receipt("cm.w_"), "cm.updated_at", "0") + `)`
	return `cr_d0 AS MATERIALIZED (
	  SELECT cm.*, coalesce(` + mirrorExportStatusChangeSQL("cm", "max", mirrorDoneStatusesSQL(p.groups), "cm.hi") + `,
	      iif(cm.resolved_at > 0 AND cm.resolved_at <= cm.hi, cm.resolved_at, NULL)) AS moment
	  FROM cr_m cm WHERE cm.done IS NOT 0 AND NOT cm.support),
	  cr_d1 AS MATERIALIZED (
	  SELECT cm.*, coalesce(cm.rule, ` + mirrorCreditRuleSQL(p.rules, `coalesce(cm.moment, `+mirrorExportNever+`)`) + `) AS rule_now,
	    cm.moment IS NULL AND ` + reassigned + ` AS unowned
	  FROM cr_d0 cm),
	  cr_f AS MATERIALIZED (
	  SELECT cm.sprint_id, cm.closed, cm.hi, cm.rule_now ->> 'share' AS rule_share, cm.rule_now ->> 'need' AS rule_need,
	    cm.rule_now ->> 'commented' AS rule_commented, cm.rule_now ->> 'approved' AS rule_approved, cm.rule_now ->> 'logged_time' AS rule_logged,
	    cm.root_id, cm.issue_id, cm.history_gen, cm.covered, cm.assignee_id, cm.points, cm.done, cm.moment, cm.unowned,
	    iif(cm.covered, nullif(coalesce(` + assignee("<=", "to_value", " DESC") + `, ` + assignee(">", "from_value", "") + `,
	      iif(cm.unowned, '', cm.assignee_id)), ''), NULL) AS person,
	    CASE WHEN NOT cm.covered THEN '` + mirrorCreditOwnerUnread + `'
	         WHEN ` + comments + ` THEN ` + mirrorCreditWhySQL("cm.issue_id", "cm.c_read_at", "cm.c_tried_reason") + `
	         WHEN ` + worklogs + ` THEN ` + mirrorCreditWhySQL("cm.issue_id", "cm.w_read_at", "cm.w_tried_reason") + ` END AS unread
	  FROM cr_d1 cm)`
}

// mirrorCreditOwnerUnread is why a finished ticket names no person and
// counts none of its reviews while its history does not cover its card.
const mirrorCreditOwnerUnread = `who had it when it finished is not read yet`

// mirrorCreditKindsSQL names the kinds of review action whose conditions
// hold, as a review row's did and review_kinds list them: "commented,
// approved, logged time", NULL for none.
func mirrorCreditKindsSQL(commented, approved, logged string) string {
	return `nullif(rtrim(iif(` + commented + `, 'commented, ', '') || iif(` + approved + `, 'approved, ', '') || iif(` + logged + `, 'logged time, ', ''), ', '), '')`
}

// mirrorCreditApprovalsSQL is a review row's note while approvals are not
// read, for a review row whose rule counts them: the one place that says
// so.
const mirrorCreditApprovalsSQL = `iif(g.rule_approved = 1, 'approvals are not read yet', NULL)`

// MirrorCreditColumns are the columns of a credit row, as
// mirrorCreditRowsSQL selects them and the export's credit relation
// holds them (MirrorExportSchema).
const mirrorCreditColumns = `sprint, at, ticket, kind, person, value, share, points, review_share, review_need, review_kinds, did, qualified, hours, hours_per_point,
	note`

// mirrorCreditRowsSQL is the credit rows of cr_f and of the support
// tickets (mirrorCreditColumns): of the sprints of cr_sp, and with weeks
// of a board that plans no sprints; with no support rule, none of
// support.
func mirrorCreditRowsSQL(p mirrorCreditParams, weeks bool) string {
	support := mirrorCreditSupportSQL(p)
	worklogs := strconv.Itoa(MirrorActWorklog)
	current := mirrorActsCurrentSQL(receipt("r2."), "i.updated_at", "")
	// The support value of seconds of hpp hours a point on a board of unit u.
	supportValue := func(seconds, hpp, u string) string {
		return `iif(` + u + ` = ` + strconv.Itoa(MirrorEstimateTime) + `, ` + seconds + `, ` + seconds + ` / 3600.0 / ` + hpp + `)`
	}
	// The kinds of action the rule of a row of alias a counts.
	kinds := func(a string) string {
		return mirrorCreditKindsSQL(a+".rule_commented = 1", a+".rule_approved = 1", a+".rule_logged = 1")
	}
	// A ticket whose owner at its finish is not known (cr_f.unowned) has
	// as reviewers only the people who never had it.
	owner := `(NOT f.unowned OR a.person IS NOT f.assignee_id AND NOT EXISTS (SELECT 1 FROM tracker_cache_changes c
	        WHERE c.root_id = f.root_id AND c.issue_id = f.issue_id AND c.gen = f.history_gen AND c.kind = 4 AND a.person IN (c.from_value, c.to_value)))`
	rows := `SELECT f.sprint_id, f.moment, f.issue_id, 'finished', f.person, iif(f.done IS NULL OR f.moment IS NULL AND f.sprint_id IS NULL, NULL, coalesce(f.points, 0)),
	    iif(f.done IS NULL OR f.moment IS NULL AND f.sprint_id IS NULL, NULL, 1), f.points, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
	    CASE WHEN f.done IS NULL THEN 'its status when the sprint ended is not known'
	         WHEN f.moment IS NULL AND f.sprint_id IS NULL THEN 'when it finished is not known'
	         WHEN NOT f.covered THEN '` + mirrorCreditOwnerUnread + `'
	         WHEN f.unowned THEN 'who had it when it finished is not known' END
	  FROM cr_f f
	  UNION ALL
	  SELECT f.sprint_id, f.moment, f.issue_id, 'review', NULL, NULL, NULL, f.points, f.rule_share, f.rule_need, ` + kinds("f") + `, NULL, NULL, NULL, NULL, f.unread
	  FROM cr_f f WHERE f.done = 1 AND f.unread IS NOT NULL
	  UNION ALL
	  SELECT g.sprint_id, g.moment, g.issue_id, 'review', g.person, iif(g.qualified, coalesce(g.points, 0) * g.rule_share / 100.0, 0),
	    iif(g.qualified, g.rule_share / 100.0, 0), g.points, g.rule_share, g.rule_need, ` + kinds("g") + `,
	    ` + mirrorCreditKindsSQL("g.commented", "g.approved", "g.logged") + `,
	    g.qualified, NULL, NULL, ` + mirrorCreditApprovalsSQL + `
	  FROM (SELECT d.*, d.rule_commented * d.commented + d.rule_approved * d.approved + d.rule_logged * d.logged >= d.rule_need AS qualified FROM (
	    SELECT f.sprint_id, f.moment, f.issue_id, f.points, f.rule_share, f.rule_need, f.rule_commented, f.rule_approved, f.rule_logged, a.person,
	      max(a.source = ` + strconv.Itoa(MirrorActComment) + `) AS commented, max(a.source = 3) AS approved, max(a.source = ` + worklogs + `) AS logged
	    FROM cr_f f
	    CROSS JOIN tracker_cache_issue_acts a ON a.root_id = CAST(?1 AS INTEGER) AND a.issue_id = f.issue_id
	    WHERE f.done = 1 AND f.unread IS NULL AND a.person <> '' AND a.person IS NOT f.person AND a.at <= f.hi AND ` + owner + `
	    GROUP BY f.sprint_id, f.issue_id, a.person) d) g`
	if p.support == "" {
		return rows
	}
	rows += `
	  UNION ALL
	  SELECT s.sprint_id, NULL, s.issue_id, 'support', s.person, ` + supportValue("s.seconds", "s.hpp", "s.unit") + `, s.seconds / 3600.0 / s.hpp,
	    NULL, NULL, NULL, NULL, NULL, NULL, s.seconds / 3600.0, s.hpp,
	    iif(s.read_from > s.lo, 'time logged before ' || strftime('%Y-%m-%d', s.read_from / 1000, 'unixepoch') || ' is not read', NULL)
	  FROM (SELECT l.sprint_id, l.rule ->> 'hpp' AS hpp, l.unit, l.lo, a.issue_id, a.person, total(a.seconds) AS seconds, max(r2.read_from) AS read_from
	    FROM cr_sp l
	    CROSS JOIN tracker_cache_issue_acts a ON a.root_id = CAST(?1 AS INTEGER) AND a.source = ` + worklogs + ` AND a.at >= l.lo AND a.at < l.hi
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = a.root_id AND b.board_id = ?2 AND b.issue_id = a.issue_id
	    CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	    CROSS JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	    WHERE ` + current + ` AND ` + support + ` AND ` + mirrorCreditCountsInSQL("a.at") + `
	    GROUP BY l.sprint_id, a.issue_id, a.person) s
	  UNION ALL
	  SELECT l.sprint_id, i.updated_at, i.issue_id, 'support', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
	    ` + mirrorCreditWhySQL("i.issue_id", "r2.read_at", "r2.tried_reason") + `
	  FROM cr_sp l
	  CROSS JOIN tracker_cache_issues i ON ` + mirrorUpdatedSinceSQL("i", ">=", "l.lo") + ` AND i.updated_at < l.hi
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id
	  LEFT JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	  WHERE ` + support + ` AND NOT ` + current + `
	    AND ` + mirrorCreditCountsInSQL("i.updated_at")
	if weeks {
		unit := `(SELECT bd.estimate_unit FROM tracker_cache_boards bd WHERE bd.root_id = CAST(?1 AS INTEGER) AND bd.board_id = ?2)`
		hpp := `(` + mirrorCreditRuleSQL(p.rules, "a.at") + ` ->> 'hpp')`
		rows += `
	  UNION ALL
	  SELECT NULL, w.at, w.issue_id, 'support', w.person, ` + supportValue("w.seconds", "w.hpp", unit) + `, w.seconds / 3600.0 / w.hpp,
	    NULL, NULL, NULL, NULL, NULL, NULL, w.seconds / 3600.0, w.hpp, NULL
	  FROM (SELECT a.at, a.issue_id, a.person, a.seconds, ` + hpp + ` AS hpp
	    FROM tracker_cache_issue_acts a
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = a.root_id AND b.board_id = ?2 AND b.issue_id = a.issue_id
	    CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	    CROSS JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	    WHERE a.root_id = CAST(?1 AS INTEGER) AND a.source = ` + worklogs + ` AND a.at >= ` + p.from + ` AND a.at < ` + p.until + ` AND ` + p.rules + ` IS NOT NULL
	      AND ` + mirrorCreditWeeksSQL + ` AND ` + current + ` AND ` + support + `) w
	  UNION ALL
	  SELECT NULL, i.updated_at, i.issue_id, 'support', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
	    ` + mirrorCreditWhySQL("i.issue_id", "r2.read_at", "r2.tried_reason") + `
	  FROM tracker_cache_issues i
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id
	  LEFT JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + worklogs + `
	  WHERE ` + mirrorUpdatedSinceSQL("i", ">=", p.from) + ` AND ` + p.rules + ` IS NOT NULL
	    AND ` + mirrorCreditWeeksSQL + ` AND NOT ` + current + ` AND ` + support
	}
	return rows
}

// mirrorCreditSQL is the WITH list of the credit fragment (cr_sp0, cr_sp,
// cr_m, cr_d0, cr_d1, cr_f) of the sprints chosen holds for (a
// condition on the alias it is given), and with weeks of a board that
// plans no sprints, after the caller's own CTEs (before, "" for none),
// and its rows (mirrorCreditRowsSQL).
func mirrorCreditSQL(p mirrorCreditParams, before string, chosen func(alias string) string, weeks bool) (with, rows string) {
	with = `WITH `
	if before != "" {
		with += before + `,
	  `
	}
	with += mirrorCreditSprintsSQL(p, chosen) + `,
	  ` + mirrorCreditMembersSQL(p, weeks) + `,
	  ` + mirrorCreditFinishedSQL(p)
	return with, mirrorCreditRowsSQL(p, weeks)
}
