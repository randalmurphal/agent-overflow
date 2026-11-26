package trackerdash

// The built-in dashboards' IDs: one for a board that plans sprints and
// one for a board that does not. A board's kind picks one; the list
// offers only that one.
const (
	builtinSprintsID = "overview_sprints"
	builtinWeeksID   = "overview_weeks"
)

// weight is a ticket t's estimate on a board with one, a ticket on a
// board without, so a metric of the estimate counts tickets there, as the
// Backlog's pace does.
const weight = `iif((SELECT estimates FROM board) = 'none', 1, coalesce(t.points, 0))`

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
		"A ticket whose history does not show its start (it was in progress when its history begins), or whose finish time is not known, is listed and not counted.",
	},
	SQL: `SELECT t.id AS ticket,
  iif(t.history_from IS NULL OR t.started_known = 0 OR t.started_at IS NULL OR t.done_at IS NULL OR t.started_at > t.done_at,
      NULL, working_days(t.started_at, t.done_at)) AS value,
  ` + finishedAt + ` AS at,
  CASE WHEN ` + finishedAt + ` IS NULL THEN ` + finishUnknown + `
       WHEN t.history_from IS NULL THEN 'its history is not read yet'
       WHEN t.started_known = 0 THEN 'it was in progress before its history begins'
       WHEN t.started_at IS NULL THEN 'it was never in progress'
       WHEN t.done_at IS NULL THEN 'its move to done is not in its history'
       WHEN t.started_at > t.done_at THEN 'it moved to done before it was started' END AS note
FROM tickets t WHERE ` + finishedInRange,
	Reduce: ReduceMedian, Unit: UnitDays, Good: GoodDown, Bucket: BucketPeriod,
}

var stuckMetric = Metric{
	Name:  "stuck",
	Title: "Stuck right now",
	Words: []string{
		"The tickets in an in-progress status that have been in that status for more than 3 days.",
		"A ticket whose time in its status is not known is listed and not counted.",
	},
	SQL: `SELECT t.id AS ticket,
  iif(coalesce(t.status_since, 0) > 0, days(t.status_since, :now), NULL) AS value,
  iif(coalesce(t.status_since, 0) > 0,
      'in ' || coalesce(s.name, t.status) || ' for ' || CAST(days(t.status_since, :now) AS INTEGER) || ' days',
      'how long it has been in ' || coalesce(s.name, t.status) || ' is not known') AS note
FROM tickets t LEFT JOIN statuses s ON s.id = t.status
WHERE t.status_group = 'in_progress' AND (coalesce(t.status_since, 0) = 0 OR days(t.status_since, :now) > 3)`,
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
		{ID: "created_finished", Metric: createdFinishedMetric.Name, Form: FormBars, Width: 6},
		{ID: "start_to_done_line", Title: "Days from start to done, by " + by, Metric: cycleMetric.Name, Form: FormLine, Width: 6},
		{ID: "stuck_list", Title: "Stuck tickets", Metric: stuckMetric.Name, Form: FormList, Width: 12},
	}
	metrics := map[string]Metric{}
	for _, m := range []Metric{pace, cycleMetric, stuckMetric, carrying, createdFinishedMetric} {
		metrics[m.Name] = m
	}
	if k.Epics {
		d.Widgets[len(d.Widgets)-1].Width = 6
		d.Widgets = append(d.Widgets, Widget{ID: "open_by_epic", Metric: epics.Name, Form: FormBars, Width: 6})
		metrics[epics.Name] = epics
	}
	return d, metrics
}
