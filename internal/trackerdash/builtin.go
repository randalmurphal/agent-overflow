package trackerdash

// The built-in dashboards' IDs: one for a board that plans sprints and
// one for a board that does not. A board's kind picks one; the list
// offers only that one.
const (
	builtinSprintsID = "overview_sprints"
	builtinWeeksID   = "overview_weeks"
)

// weightOf is an estimate points (an SQL expression) on a board with
// one, a ticket on a board without, so a metric of the estimate counts
// tickets there, as the Backlog's pace does. weight is a ticket t's.
func weightOf(points string) string {
	return `iif((SELECT estimates FROM board) = 'none', 1, coalesce(` + points + `, 0))`
}

var weight = weightOf("t.points")

// openStatus holds for a status row g (statuses) the board counts not
// done, as tickets.open does for a ticket's status now: not done,
// canceled or left out of counts, an unclassified status included.
func openStatus(g string) string {
	return `coalesce(` + g + `.status_group, '') NOT IN ('done', 'canceled', 'left_out')`
}

// never stands for an open end where a query compares ends.
const never = `9223372036854775807`

// statusUnknown is the CTE unknown(ticket, from_at, to_at) of the
// stretches no status interval covers, when a ticket's status is not
// known (status_intervals): from its creation (NULL: not known) to its
// first interval, between two intervals, after the last closed one, and
// a ticket with none from its creation on.
const statusUnknown = `unknown(ticket, from_at, to_at) AS (
  SELECT ticket, prev, from_at FROM (
    SELECT si.ticket, si.from_at, iif(row_number() OVER w = 1, t.created_at, lag(si.to_at) OVER w) AS prev
    FROM status_intervals si JOIN tickets t ON t.id = si.ticket WINDOW w AS (PARTITION BY si.ticket ORDER BY si.from_at))
  WHERE prev IS NULL OR prev < from_at
  UNION ALL
  SELECT si.ticket, si.to_at, NULL FROM status_intervals si
  WHERE si.to_at IS NOT NULL AND si.from_at = (SELECT max(x.from_at) FROM status_intervals x WHERE x.ticket = si.ticket)
  UNION ALL
  SELECT t.id, t.created_at, NULL FROM tickets t WHERE NOT EXISTS (SELECT 1 FROM status_intervals x WHERE x.ticket = t.id))`

// activeMembers is the tickets of the board's active sprints.
const activeMembers = `(SELECT m.ticket FROM sprint_tickets m JOIN sprints s ON s.id = m.sprint WHERE s.state = 'active')`

// finishedAt is when a done ticket was finished: its last move into done,
// else the tracker's resolution time.
const finishedAt = `coalesce(t.done_at, t.resolved_at)`

// finishedInRange is the done tickets that can have finished in the range
// or the one before. A ticket's finish is one of its updates, so a done
// ticket with no known finish time is one only when it was updated since
// the range began; it is listed as not counted, and an older one stays out.
const finishedInRange = `t.status_group = 'done' AND (` + finishedAt + ` > :since OR (` + finishedAt + ` IS NULL AND t.updated_at > :since))`

// finishUnknown is a done ticket's note when when it finished is not known.
const finishUnknown = `'when it finished is not known'`

var cycleMetric = Metric{
	Name:  "start_to_done",
	Title: "Days from start to done",
	Words: []string{
		"The working days from a ticket's first move into an in-progress status to its last move into a done status, for the tickets finished in the range.",
		"A ticket whose history does not show its start (it was in progress or done when its history begins), or whose finish time is not known, is listed and not counted.",
	},
	SQL: `SELECT t.id AS ticket,
  iif(t.history_from IS NULL OR t.started_known = 0 OR t.started_at IS NULL OR t.done_at IS NULL OR t.started_at > t.done_at,
      NULL, working_days(t.started_at, t.done_at)) AS value,
  ` + finishedAt + ` AS at,
  CASE WHEN ` + finishedAt + ` IS NULL THEN ` + finishUnknown + `
       WHEN t.history_from IS NULL THEN 'its history is not read yet'
       WHEN t.started_known = 0 THEN 'its history shows no start'
       WHEN t.started_at IS NULL THEN 'it was never in progress'
       WHEN t.done_at IS NULL THEN 'its move to done is not in its history'
       WHEN t.started_at > t.done_at THEN 'it moved to done before it was started' END AS note
FROM tickets t WHERE ` + finishedInRange,
	Reduce: ReduceMedian, Unit: UnitDays, Good: GoodDown, Bucket: BucketPeriod,
}

// stuckAfter is how long a ticket stays in an in-progress status before
// it is stuck: more than 3 days, in milliseconds.
const stuckAfter = `259200000`

var stuckMetric = Metric{
	Name:  "stuck",
	Title: "Stuck right now",
	Words: []string{
		"The tickets in an in-progress status that have been in that status for more than 3 days, now and where the range begins.",
		"A ticket whose time in its status is not known, or whose status then is not known, is listed and not counted.",
	},
	// Each stay in an in-progress status is stuck from 3 days in until it
	// ends; a stretch whose status is not known is not counted while it
	// lasts, and one that lasts until now only for a ticket in progress
	// now (its status now is known otherwise).
	SQL: `WITH ` + statusUnknown + `
SELECT si.ticket AS ticket, days(si.from_at, coalesce(si.to_at, :now)) AS value, si.from_at + ` + stuckAfter + ` + 1 AS at, si.to_at AS until,
  'in ' || coalesce(s.name, si.status) || ' for ' || CAST(days(si.from_at, coalesce(si.to_at, :now)) AS INTEGER) || ' days' AS note
FROM status_intervals si JOIN statuses s ON s.id = si.status
WHERE s.status_group = 'in_progress' AND coalesce(si.to_at, ` + never + `) > max(si.from_at + ` + stuckAfter + ` + 1, :since)
UNION ALL
SELECT u.ticket, NULL, u.from_at, iif(u.to_at IS NULL AND t.status_group IS NOT 'in_progress', :now, u.to_at),
  iif(u.to_at IS NULL AND t.status_group = 'in_progress', 'how long it has been in ' || coalesce(s.name, t.status) || ' is not known',
      'its status then is not known')
FROM unknown u JOIN tickets t ON t.id = u.ticket LEFT JOIN statuses s ON s.id = t.status
WHERE coalesce(u.to_at, ` + never + `) > :since`,
	Reduce: ReduceCount, Unit: UnitTickets, Good: GoodDown,
}

var createdFinishedMetric = Metric{
	Name:  "created_finished",
	Title: "Created and finished",
	Words: []string{
		"The tickets on this board created in each period, and those finished in it: moved into a done status for the last time, or resolved. A ticket in a status the project leaves out of counts is not counted.",
		"A done ticket whose finish time is not known is listed and not counted.",
	},
	SQL: `SELECT t.id AS ticket, 1 AS value, t.created_at AS at, 'Created' AS series, NULL AS note FROM tickets t WHERE t.created_at > :since AND t.counts = 1
UNION ALL
SELECT t.id, iif(` + finishedAt + ` IS NULL, NULL, 1), ` + finishedAt + `, 'Finished', iif(` + finishedAt + ` IS NULL, ` + finishUnknown + `, NULL)
FROM tickets t WHERE ` + finishedInRange,
	Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketPeriod, Series: []string{"Created", "Finished"},
}

var burndownMetric = Metric{
	Name:  "burndown",
	Title: "Burndown",
	Words: []string{
		"The estimate of the active sprint's tickets not yet done or canceled, at its start and at the end of each working day, each by its estimate then. The dashed line is an even pace to zero on its last day.",
		"A ticket whose status, sprint or estimate then is not known is listed and not counted.",
	},
	// Each stretch in an active sprint (sprint_intervals) is remaining
	// work while its ticket's status is open; while the status is not
	// known, or the history does not reach back to the sprint's start, it
	// is not counted.
	SQL: `WITH ` + statusUnknown + `,
a AS (SELECT si.ticket, si.points, si.from_at, si.to_at FROM sprint_intervals si JOIN sprints s ON s.id = si.sprint WHERE s.state = 'active')
SELECT a.ticket AS ticket, ` + weightOf("a.points") + ` AS value, max(a.from_at, o.from_at) AS at,
  nullif(min(coalesce(a.to_at, ` + never + `), coalesce(o.to_at, ` + never + `)), ` + never + `) AS until, NULL AS note
FROM a JOIN status_intervals o ON o.ticket = a.ticket LEFT JOIN statuses g ON g.id = o.status
WHERE ` + openStatus("g") + ` AND o.from_at < coalesce(a.to_at, ` + never + `) AND a.from_at < coalesce(o.to_at, ` + never + `)
UNION ALL
SELECT a.ticket, NULL, max(a.from_at, coalesce(u.from_at, 0)), nullif(min(coalesce(a.to_at, ` + never + `), coalesce(u.to_at, ` + never + `)), ` + never + `),
  'its status then is not known'
FROM a JOIN unknown u ON u.ticket = a.ticket
WHERE coalesce(u.from_at, 0) < coalesce(a.to_at, ` + never + `) AND a.from_at < coalesce(u.to_at, ` + never + `)
UNION ALL
SELECT m.ticket, NULL, iif(t.history_from IS NULL, NULL, s.start_at), t.history_from,
  iif(t.history_from IS NULL, 'its history is not read yet', 'its history begins after the sprint started')
FROM sprint_tickets m JOIN sprints s ON s.id = m.sprint JOIN tickets t ON t.id = m.ticket
WHERE s.state = 'active' AND (t.history_from IS NULL OR t.history_from > s.start_at)`,
	Reduce: ReduceSum, Unit: UnitEstimate, Good: GoodDown, Bucket: BucketDay,
}

// The series of the sprint scope table.
const (
	seriesCommitted = "Committed"
	seriesAdded     = "Added"
	seriesRemoved   = "Removed"
	seriesCompleted = "Completed"
)

var sprintScopeMetric = Metric{
	Name:  "sprint_scope",
	Title: "Committed, added, removed and completed",
	Words: []string{
		"Each sprint's estimate in it at its start (committed), of the tickets that entered it after (added) or left it before its end (removed), each at that moment, and of those done at its end by the pace's rule (completed).",
		"A ticket counts once in each. One whose history does not reach back to the sprint's start is listed and not counted in committed and added.",
	},
	// A sprint's stretches (sprint_intervals) end at its end: its
	// completion, or now for the active one. A ticket is committed when a
	// stretch holds at the start, added at its first entry after it when
	// not committed, and removed at its last exit before the end when no
	// stretch holds at the end.
	SQL: `WITH s AS (SELECT id, start_at, iif(state = 'active', :now, completed_at) AS end_at FROM sprints
    WHERE state IN ('active', 'closed') AND coalesce(iif(state = 'active', :now, completed_at), 0) > :since),
  x AS (SELECT si.sprint, si.ticket, si.points, si.from_at, coalesce(si.to_at, ` + never + `) AS to_at, s.start_at, s.end_at
    FROM sprint_intervals si JOIN s ON s.id = si.sprint),
  committed AS (SELECT * FROM x WHERE x.from_at <= x.start_at AND x.to_at > x.start_at),
  entered AS (SELECT sprint, ticket, min(from_at) AS at FROM x WHERE from_at > start_at AND from_at <= end_at GROUP BY sprint, ticket),
  exited AS (SELECT sprint, ticket, max(to_at) AS at FROM x WHERE to_at > coalesce(start_at, 0) AND to_at <= end_at GROUP BY sprint, ticket)
SELECT c.ticket AS ticket, ` + weightOf("c.points") + ` AS value, c.sprint AS bucket, '` + seriesCommitted + `' AS series, NULL AS note FROM committed c
UNION ALL
SELECT e.ticket, ` + weightOf("x.points") + `, e.sprint, '` + seriesAdded + `', NULL
FROM entered e JOIN x ON x.sprint = e.sprint AND x.ticket = e.ticket AND x.from_at = e.at JOIN tickets t ON t.id = e.ticket
WHERE t.history_from <= x.start_at AND NOT EXISTS (SELECT 1 FROM committed c WHERE c.sprint = e.sprint AND c.ticket = e.ticket)
UNION ALL
SELECT l.ticket, ` + weightOf("x.points") + `, l.sprint, '` + seriesRemoved + `', NULL
FROM exited l JOIN x ON x.sprint = l.sprint AND x.ticket = l.ticket AND x.to_at = l.at
WHERE NOT EXISTS (SELECT 1 FROM x h WHERE h.sprint = l.sprint AND h.ticket = l.ticket AND h.from_at <= h.end_at AND h.to_at > h.end_at)
UNION ALL
SELECT f.ticket, ` + weight + `, f.sprint, '` + seriesCompleted + `', NULL
FROM sprint_finished f JOIN s ON s.id = f.sprint JOIN tickets t ON t.id = f.ticket WHERE f.done = 1
UNION ALL
SELECT f.ticket, NULL, f.sprint, n.series,
  CASE WHEN s.start_at IS NULL THEN 'the sprint has no start date' WHEN t.history_from IS NULL THEN 'its history is not read yet'
       ELSE 'its history begins after the sprint started' END
FROM sprint_finished f JOIN s ON s.id = f.sprint JOIN tickets t ON t.id = f.ticket
  CROSS JOIN (SELECT '` + seriesCommitted + `' AS series UNION ALL SELECT '` + seriesAdded + `') n
WHERE s.start_at IS NULL OR t.history_from IS NULL OR t.history_from > s.start_at`,
	Reduce: ReduceSum, Unit: UnitEstimate, Bucket: BucketSprint, Series: []string{seriesCommitted, seriesAdded, seriesRemoved, seriesCompleted},
}

// builtin is the built-in dashboard of a board of kind k and its metrics
// by name.
func builtin(k Kind) (Dashboard, map[string]Metric) {
	var pace, carrying, epics Metric
	d := Dashboard{Title: "Overview"}
	if k.PlansSprints {
		d.ID = builtinSprintsID
		pace = Metric{
			Name:  "finished_by_sprint",
			Title: "Usual pace",
			Words: []string{
				"The estimate of the tickets whose status counted as done when their sprint was completed, by the board's estimates now; a board without estimates counts the tickets.",
				"The active sprint counts the tickets done so far.",
			},
			SQL: `SELECT f.ticket AS ticket, ` + weight + ` AS value, f.sprint AS bucket
FROM sprint_finished f JOIN tickets t ON t.id = f.ticket WHERE f.done = 1`,
			Reduce: ReduceAverageOfPeriodSums, Unit: UnitEstimate, Good: GoodUp, Bucket: BucketSprint,
		}
		carrying = Metric{
			Name:  "carrying",
			Title: "Who is carrying what",
			Words: []string{"The estimate of the active sprint's tickets by assignee, split into to do, doing and done: its open tickets (Backlog, To do and an unclassified status as To do) and its done ones."},
			SQL: `SELECT t.id AS ticket, ` + weight + ` AS value, t.assignee AS bucket,
  CASE WHEN t.open = 0 THEN 'Done' WHEN t.status_group = 'in_progress' THEN 'Doing' ELSE 'To do' END AS series
FROM tickets t WHERE t.id IN ` + activeMembers + ` AND (t.open = 1 OR t.status_group = 'done')`,
			Reduce: ReduceSum, Unit: UnitEstimate, Bucket: BucketPerson, Series: []string{"To do", "Doing", "Done"},
		}
		epics = Metric{
			Name:  "open_by_epic",
			Title: "Open work by epic",
			Words: []string{"The estimate of the active sprint's open tickets (not done, canceled or left out of counts), by their epic."},
			SQL: `SELECT t.id AS ticket, ` + weight + ` AS value, t.epic AS bucket
FROM tickets t WHERE t.id IN ` + activeMembers + ` AND t.open = 1`,
			Reduce: ReduceSum, Unit: UnitEstimate, Bucket: BucketEpic,
		}
	} else {
		d.ID = builtinWeeksID
		pace = Metric{
			Name:  "finished_by_week",
			Title: "Usual pace",
			Words: []string{
				"The estimate of the tickets finished each week (moved into a done status for the last time, or resolved), by the board's estimates now; a board without estimates counts the tickets.",
				"A done ticket whose finish time is not known is listed and not counted.",
			},
			SQL: `SELECT t.id AS ticket, iif(` + finishedAt + ` IS NULL, NULL, ` + weight + `) AS value, ` + finishedAt + ` AS at,
  iif(` + finishedAt + ` IS NULL, ` + finishUnknown + `, NULL) AS note
FROM tickets t WHERE ` + finishedInRange,
			Reduce: ReduceAverageOfPeriodSums, Unit: UnitEstimate, Good: GoodUp, Bucket: BucketPeriod,
		}
		carrying = Metric{
			Name:  "carrying",
			Title: "Who is carrying what",
			Words: []string{"The estimate of the open tickets by assignee (Backlog, To do and an unclassified status as To do), split into to do and doing."},
			SQL: `SELECT t.id AS ticket, ` + weight + ` AS value, t.assignee AS bucket,
  iif(t.status_group = 'in_progress', 'Doing', 'To do') AS series
FROM tickets t WHERE t.open = 1`,
			Reduce: ReduceSum, Unit: UnitEstimate, Bucket: BucketPerson, Series: []string{"To do", "Doing"},
		}
		epics = Metric{
			Name:  "open_by_epic",
			Title: "Open work by epic",
			Words: []string{"The estimate of the open tickets (not done, canceled or left out of counts), by their epic."},
			SQL: `SELECT t.id AS ticket, ` + weight + ` AS value, t.epic AS bucket
FROM tickets t WHERE t.open = 1`,
			Reduce: ReduceSum, Unit: UnitEstimate, Bucket: BucketEpic,
		}
	}
	by := "sprint"
	if !k.PlansSprints {
		by = "week"
	}
	d.Widgets = []Widget{
		{ID: "pace", Metric: pace.Name, Form: FormNumber, Width: 4},
		{ID: "start_to_done", Metric: cycleMetric.Name, Form: FormNumber, Width: 4},
		{ID: "stuck", Metric: stuckMetric.Name, Form: FormNumber, Width: 4},
		{ID: "finished", Title: "Finished per " + by, Metric: pace.Name, Form: FormBars, Width: 8},
		{ID: "carrying", Metric: carrying.Name, Form: FormTable, Width: 4},
	}
	metrics := map[string]Metric{}
	for _, m := range []Metric{pace, cycleMetric, stuckMetric, carrying, createdFinishedMetric} {
		metrics[m.Name] = m
	}
	if k.PlansSprints {
		d.Widgets = append(d.Widgets,
			Widget{ID: "burndown", Metric: burndownMetric.Name, Form: FormLine, Width: 6},
			Widget{ID: "sprint_scope", Metric: sprintScopeMetric.Name, Form: FormTable, Width: 6})
		metrics[burndownMetric.Name], metrics[sprintScopeMetric.Name] = burndownMetric, sprintScopeMetric
	}
	d.Widgets = append(d.Widgets,
		Widget{ID: "created_finished", Metric: createdFinishedMetric.Name, Form: FormBars, Width: 6},
		Widget{ID: "start_to_done_line", Title: "Days from start to done, by " + by, Metric: cycleMetric.Name, Form: FormLine, Width: 6},
		Widget{ID: "stuck_list", Title: "Stuck tickets", Metric: stuckMetric.Name, Form: FormList, Width: 12})
	if k.Epics {
		d.Widgets[len(d.Widgets)-1].Width = 6
		d.Widgets = append(d.Widgets, Widget{ID: "open_by_epic", Metric: epics.Name, Form: FormBars, Width: 6})
		metrics[epics.Name] = epics
	}
	return d, metrics
}
