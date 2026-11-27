package trackerdash

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RangeOption is one range a dashboard can count over.
type RangeOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// The ranges: a board that plans sprints counts by sprint, any other by
// week, Monday to Sunday in the person's time zone.
const (
	RangeThisSprint = "this_sprint"
	RangeSprints3   = "sprints_3"
	RangeSprints5   = "sprints_5"
	RangeSprints10  = "sprints_10"
	RangeWeeks2     = "weeks_2"
	RangeWeeks4     = "weeks_4"
	RangeWeeks12    = "weeks_12"
)

var (
	sprintRanges = []RangeOption{{RangeThisSprint, "This sprint"}, {RangeSprints3, "Last 3 sprints"}, {RangeSprints5, "Last 5 sprints"}, {RangeSprints10, "Last 10 sprints"}}
	weekRanges   = []RangeOption{{RangeWeeks2, "Last 2 weeks"}, {RangeWeeks4, "Last 4 weeks"}, {RangeWeeks12, "Last 12 weeks"}}
)

// rangesOf is the ranges a board offers and the one it opens on.
func rangesOf(plansSprints bool) ([]RangeOption, string) {
	if plansSprints {
		return sprintRanges, RangeSprints5
	}
	return weekRanges, RangeWeeks4
}

// period is one bar of a range: a sprint (or the active sprints) or a
// week. Rows fall in it by their at, in (from, to], or by naming one of
// its sprints.
type period struct {
	id      string
	label   string
	from    int64
	to      int64
	sprints []string
	partial bool
	// future is a sprint day after today, which has no value yet; last is
	// the sprint's last working day, where its even pace reaches zero.
	future, last bool
}

func (p period) holds(at int64) bool { return at > p.from && at <= p.to }

// holdsAt is whether state row c holds at instant t: from its at (none:
// before anything) until its until (none: still), at <= t < until. A
// period's state is what holds at its end, a number's what holds now and
// at the range's start; the widget and the drill both place states here
// (placeStates).
func (c contribution) holdsAt(t int64) bool {
	return (!c.hasAt || c.at <= t) && (!c.hasUntil || t < c.until)
}

// since is where the span's periods, the previous range's included,
// begin: no row at or before it falls in one. It is now for a span with
// no periods.
func (sp span) since(now int64) int64 {
	from := now
	for _, p := range slices.Concat(sp.periods, sp.previous) {
		from = min(from, p.from)
	}
	return from
}

// span is a range resolved against a board: its periods oldest first and
// the previous range's of equal length. The previous range is a gap when
// the mirror holds fewer sprints before the range than the range has; a
// week range's never is, as the mirror keeps 180 days and the longest
// range with its previous one reaches 24 weeks.
type span struct {
	id       string
	label    string
	periods  []period
	previous []period
	// previousGap says why the previous range is a gap; empty when it is
	// not.
	previousGap string
	// previousLabel names the previous range in words.
	previousLabel string
	// note says why the range has no periods.
	note string
	// now is the instant the span is resolved at, where a state is read
	// for a number's value and a list.
	now int64
	// days is the active sprints' working days (sprintDays), which a
	// metric grouped by day counts whatever the range; daysLabel names
	// those sprints, and daysNote says why there are no days.
	days                []period
	daysLabel, daysNote string
}

// sprintRow is a sprint of the export.
type sprintRow struct {
	id, name, state string
	startAt, endAt  int64
	completedAt     int64
	position        int64
}

// resolveSpan resolves range id against the board's sprints at now in loc.
func resolveSpan(id string, plansSprints bool, sprints []sprintRow, now time.Time, loc *time.Location) (span, error) {
	options, _ := rangesOf(plansSprints)
	i := slices.IndexFunc(options, func(o RangeOption) bool { return o.ID == id })
	if i < 0 {
		ids := make([]string, len(options))
		for j, o := range options {
			ids[j] = o.ID
		}
		return span{}, fmt.Errorf("this board counts over %s, not %q", strings.Join(ids, ", "), id)
	}
	sp := span{id: id, label: options[i].Label, now: now.UnixMilli()}
	if !plansSprints {
		sp.daysNote = "This board plans no sprints, so it has no sprint days to count."
		n := map[string]int{RangeWeeks2: 2, RangeWeeks4: 4, RangeWeeks12: 12}[id]
		sp.periods, sp.previous = weeks(n, now, loc)
		sp.previousLabel = fmt.Sprintf("the %d weeks before", n)
		return sp, nil
	}
	// The closed sprints by completion, newest first, as the Backlog's
	// pace orders them; one with no completion time has no place.
	var closed []sprintRow
	for _, s := range sprints {
		if s.state == "closed" && s.completedAt > 0 {
			closed = append(closed, s)
		}
	}
	slices.SortFunc(closed, func(a, b sprintRow) int {
		return cmp.Or(cmp.Compare(b.completedAt, a.completedAt), cmp.Compare(b.position, a.position))
	})
	sp.days, sp.daysLabel, sp.daysNote = sprintDays(sprints, now, loc)
	window := func(i int) period {
		s := closed[i]
		from := s.startAt
		if i+1 < len(closed) {
			from = closed[i+1].completedAt
		}
		return period{id: "sprint:" + s.id, label: s.name, from: from, to: s.completedAt, sprints: []string{s.id}}
	}
	newestFirst := func(lo, hi int) []period {
		var out []period
		for i := lo; i < hi && i < len(closed); i++ {
			out = append(out, window(i))
		}
		slices.Reverse(out)
		return out
	}
	if id == RangeThisSprint {
		var active period
		for _, s := range sprints {
			if s.state == "active" {
				active.sprints = append(active.sprints, s.id)
				active.label = strings.TrimPrefix(active.label+", "+s.name, ", ")
				if active.from == 0 || (s.startAt > 0 && s.startAt < active.from) {
					active.from = s.startAt
				}
			}
		}
		if len(active.sprints) == 0 {
			sp.note = "No sprint is active on this board. Pick a range of finished sprints."
			return sp, nil
		}
		if len(closed) > 0 {
			active.from = closed[0].completedAt
		}
		active.id, active.to, active.partial = "active", now.UnixMilli(), true
		sp.periods = []period{active}
		sp.previous = newestFirst(0, 1)
		if len(sp.previous) < 1 {
			sp.previousGap = "This computer keeps no finished sprint to compare with."
		}
		sp.previousLabel = "the last sprint"
		return sp, nil
	}
	n := map[string]int{RangeSprints3: 3, RangeSprints5: 5, RangeSprints10: 10}[id]
	sp.periods = newestFirst(0, n)
	sp.previous = newestFirst(n, 2*n)
	if len(sp.previous) < n {
		sp.previousGap = fmt.Sprintf("This computer keeps fewer than %d finished sprints before these to compare with.", n)
	}
	sp.previousLabel = fmt.Sprintf("the %d sprints before", n)
	if len(sp.periods) == 0 {
		sp.note = "This board has no finished sprint in the last 180 days."
	}
	return sp, nil
}

// weeks is the last n weeks to now, Monday to Sunday in loc, this week
// partial, and the n before them.
func weeks(n int, now time.Time, loc *time.Location) (cur, prev []period) {
	local := now.In(loc)
	monday := midnight(local).AddDate(0, 0, -((int(local.Weekday()) + 6) % 7))
	week := func(k int) period {
		start := monday.AddDate(0, 0, -7*k)
		end := start.AddDate(0, 0, 7)
		p := period{id: "week:" + start.Format(time.DateOnly), label: start.Format("Jan 2"), from: start.UnixMilli() - 1, to: end.UnixMilli() - 1}
		if k == 0 {
			p.to, p.partial = now.UnixMilli(), true
		}
		return p
	}
	for k := 2*n - 1; k >= 0; k-- {
		if k >= n {
			prev = append(prev, week(k))
		} else {
			cur = append(cur, week(k))
		}
	}
	return cur, prev
}

// maxSprintDays is the most points a metric grouped by day draws: a
// sprint's start and its working days to the later of its end and today.
const maxSprintDays = 64

// sprintDays is the working days of the board's active sprints (their
// union when several are active): Start, at the earliest start, then the
// end of each working day in loc from the start's day to the later of the
// last end's day and today. Today's point is at now and partial, a later
// day's is future, and the last working day on or before the end is the
// sprint's last. Each day holds the rows of an at after the point before
// it, a weekend's in the next working day. label names the sprints; note
// says why there are no days: no active sprint, one with no dates, or
// more days than maxSprintDays.
func sprintDays(sprints []sprintRow, now time.Time, loc *time.Location) (days []period, label, note string) {
	var start, end int64
	var names []string
	dated := true
	for _, s := range sprints {
		if s.state != "active" {
			continue
		}
		names = append(names, s.name)
		if s.startAt <= 0 || s.endAt <= 0 {
			dated = false
			continue
		}
		if start == 0 || s.startAt < start {
			start = s.startAt
		}
		end = max(end, s.endAt)
	}
	label = strings.Join(names, ", ")
	switch {
	case len(names) == 0:
		return nil, "", "No sprint is active on this board."
	case !dated:
		return nil, label, label + " has no start or end date, so it has no days to count."
	}
	at := now.UnixMilli()
	days = []period{{id: "day:start", label: "Start", from: start - 1, to: start, future: start > at}}
	lastDay := midnight(time.UnixMilli(end).In(loc))
	today := midnight(now.In(loc))
	stop := lastDay
	if today.After(stop) {
		stop = today
	}
	last := 0
	for d := midnight(time.UnixMilli(start).In(loc)); !d.After(stop); d = d.AddDate(0, 0, 1) {
		if !working(d) {
			continue
		}
		if len(days) > maxSprintDays {
			return nil, label, fmt.Sprintf("%s runs past %d working days, more than a day chart draws.", label, maxSprintDays)
		}
		p := period{id: "day:" + d.Format(time.DateOnly), label: d.Format("Jan 2"), from: days[len(days)-1].to, to: d.AddDate(0, 0, 1).UnixMilli() - 1}
		switch {
		case d.Equal(today):
			p.to, p.partial = at, true
		case d.After(today):
			p.future = true
		}
		if !d.After(lastDay) {
			last = len(days)
		}
		days = append(days, p)
	}
	days[last].last = true
	return days, label, ""
}

// placeStates sorts a state metric's rows into periods by period ID: a
// period holds the rows that hold at its end (holdsAt), of its sprints
// for a metric grouped by sprint. A future day holds none.
func placeStates(m Metric, rows []contribution, periods []period) map[string][]contribution {
	out := map[string][]contribution{}
	for _, p := range periods {
		if p.future {
			continue
		}
		for _, c := range rows {
			if c.holdsAt(p.to) && (m.Bucket != BucketSprint || slices.Contains(p.sprints, c.bucket)) {
				c.period = p.label
				out[p.id] = append(out[p.id], c)
			}
		}
	}
	return out
}

// holding is the rows of rows that hold at t.
func holding(rows []contribution, t int64) []contribution {
	var out []contribution
	for _, c := range rows {
		if c.holdsAt(t) {
			out = append(out, c)
		}
	}
	return out
}
