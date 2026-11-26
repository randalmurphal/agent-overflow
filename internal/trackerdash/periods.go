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
}

func (p period) holds(at int64) bool { return at > p.from && at <= p.to }

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
}

// sprintRow is a sprint of the export.
type sprintRow struct {
	id, name, state string
	startAt         int64
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
	sp := span{id: id, label: options[i].Label}
	if !plansSprints {
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
