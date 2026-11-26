package trackerdash

import (
	"reflect"
	"testing"
	"time"
)

// The sprint ranges are the last closed sprints by completion, newest
// last, each from the completion before it; the previous range is the
// sprints before, a gap when there are fewer. This sprint is the active
// sprints from the last completion to now, against the last sprint.
func TestSprintRangesFollowCompletion(t *testing.T) {
	t.Parallel()
	sprints := []sprintRow{
		{id: "a", name: "A", state: "closed", startAt: 10, completedAt: 100, position: 1},
		{id: "b", name: "B", state: "closed", startAt: 90, completedAt: 200, position: 2},
		{id: "c", name: "C", state: "closed", startAt: 190, completedAt: 300, position: 3},
		{id: "x", name: "X", state: "closed", position: 4}, // never completed: no place
		{id: "d", name: "D", state: "active", startAt: 290, position: 5},
		{id: "e", name: "E", state: "future", position: 6},
	}
	sp, err := resolveSpan(RangeSprints3, true, sprints, time.UnixMilli(400), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range sp.periods {
		got = append(got, p.label)
	}
	if !reflect.DeepEqual(got, []string{"A", "B", "C"}) || sp.periods[0].from != 10 || sp.periods[1].from != 100 || sp.periods[2].to != 300 || sp.previousGap == "" {
		t.Fatalf("last 3 = %+v, previous gap %v", sp.periods, sp.previousGap)
	}
	sp, err = resolveSpan(RangeThisSprint, true, sprints, time.UnixMilli(400), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.periods) != 1 || sp.periods[0].label != "D" || sp.periods[0].from != 300 || sp.periods[0].to != 400 || !sp.periods[0].partial ||
		len(sp.previous) != 1 || sp.previous[0].label != "C" || sp.previousGap != "" {
		t.Fatalf("this sprint = %+v against %+v", sp.periods, sp.previous)
	}
	if _, err := resolveSpan(RangeWeeks4, true, sprints, time.UnixMilli(400), time.UTC); err == nil {
		t.Fatal("a sprint board counted by week")
	}
	sp, err = resolveSpan(RangeThisSprint, true, sprints[:3], time.UnixMilli(400), time.UTC)
	if err != nil || len(sp.periods) != 0 || sp.note == "" {
		t.Fatalf("this sprint with none active = %+v, %v; want a note", sp, err)
	}
}

// Weeks run Monday to Sunday in the person's zone, this week to now.
func TestWeeksAreMondayToSundayInTheZone(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// Sunday 23:00 UTC is Monday 08:00 in Tokyo: this week starts that
	// Monday there, and a week before in UTC.
	now := time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC)
	cur, prev := weeks(2, now, tokyo)
	if cur[1].id != "week:2026-11-23" || cur[0].id != "week:2026-11-16" || !cur[1].partial || cur[1].to != now.UnixMilli() || prev[1].id != "week:2026-11-09" {
		t.Fatalf("Tokyo weeks = %+v before %+v", cur, prev)
	}
	monday := time.Date(2026, 11, 23, 0, 0, 0, 0, tokyo).UnixMilli()
	if !cur[1].holds(monday) || cur[0].holds(monday) || !cur[0].holds(monday-1) {
		t.Fatal("Monday midnight belongs to the week it starts")
	}
	cur, _ = weeks(2, now, time.UTC)
	if cur[1].id != "week:2026-11-16" {
		t.Fatalf("UTC's week = %s", cur[1].id)
	}
}

// working_days leaves out Saturdays and Sundays in the zone, a part of a
// day counted as its share.
func TestWorkingDaysLeaveOutTheWeekend(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, berlin).UnixMilli() }
	for _, c := range []struct {
		name string
		a, b int64
		want float64
	}{
		{"Friday noon to Monday noon", at(2026, 11, 20, 12), at(2026, 11, 23, 12), 1},
		{"Monday to Monday", at(2026, 11, 16, 9), at(2026, 11, 23, 9), 5},
		{"Saturday to Sunday", at(2026, 11, 21, 9), at(2026, 11, 22, 18), 0},
		{"three weeks and a day", at(2026, 11, 2, 0), at(2026, 11, 24, 0), 16},
		{"over the clock change, a 25-hour Sunday", at(2026, 10, 23, 12), at(2026, 10, 26, 12), 1},
		{"backwards", at(2026, 11, 23, 12), at(2026, 11, 20, 12), -1},
	} {
		if got := workingDays(c.a, c.b, berlin); got != c.want {
			t.Errorf("%s: %v; want %v", c.name, got, c.want)
		}
	}
}

// The reducers: count is distinct counted tickets, sum is zero for no
// rows, median and average are none, and an average of period sums is
// the mean over the range's periods, an empty one as zero. Each number
// carries the tickets it could not count, a ticket another of its rows
// counts not among them; with none counted and some not, the number is
// not known rather than zero.
func TestReducers(t *testing.T) {
	t.Parallel()
	rows := []contribution{
		{ticket: "a", value: 1, counted: true}, {ticket: "a", value: 3, counted: true},
		{ticket: "b", value: 10, counted: true}, {ticket: "c", counted: false},
		{ticket: "c", counted: false}, {ticket: "b", counted: false},
	}
	for r, want := range map[Reducer]string{ReduceCount: "2, 1 not counted", ReduceSum: "14, 1 not counted", ReduceMedian: "3, 1 not counted", ReduceAverage: "4.666666666666667, 1 not counted", ReduceAverageOfPeriodSums: "14, 1 not counted"} {
		got := reduceRows(r, rows)
		if num(&got) != want {
			t.Errorf("%s = %s; want %s", r, num(&got), want)
		}
	}
	even := reduceRows(ReduceMedian, rows[:2])
	if num(&even) != "2" {
		t.Errorf("median of 1 and 3 = %s", num(&even))
	}
	for r, want := range map[Reducer]string{ReduceCount: "0", ReduceSum: "0", ReduceMedian: "none", ReduceAverage: "none"} {
		got := reduceRows(r, nil)
		if num(&got) != want {
			t.Errorf("%s of nothing = %s; want %s", r, num(&got), want)
		}
	}
	for _, r := range []Reducer{ReduceCount, ReduceSum, ReduceMedian, ReduceAverage, ReduceAverageOfPeriodSums} {
		if got := reduceRows(r, rows[3:5]); num(&got) != "not known, 1 not counted" {
			t.Errorf("%s of one ticket not counted = %s; want not known", r, num(&got))
		}
	}
	periods := []period{{id: "1"}, {id: "2"}, {id: "4"}}
	placed := map[string][]contribution{"1": rows[:2], "4": rows[2:3], "3": {{ticket: "z", value: 100, counted: true}}}
	if got := reduceRange(ReduceAverageOfPeriodSums, periods, map[string][]contribution{"2": rows[3:5]}); num(&got) != "not known, 1 not counted" {
		t.Errorf("average of period sums with nothing counted = %s; want not known", num(&got))
	}
	if got := reduceRange(ReduceAverageOfPeriodSums, periods, placed); num(&got) != "4.666666666666667" {
		t.Errorf("average of period sums = %s; want (4 + 0 + 10) / 3, period 3 outside the range", num(&got))
	}
	if got := reduceRange(ReduceCount, periods, placed); num(&got) != "2" {
		t.Errorf("count over the range = %s; want a and b", num(&got))
	}
	if got := reduceRange(ReduceMedian, nil, placed); num(&got) != "none" {
		t.Errorf("a range of no periods = %s; want none", num(&got))
	}
}
