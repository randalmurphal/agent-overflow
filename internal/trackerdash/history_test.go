package trackerdash

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// utc is an instant of testNow's November and December in UTC.
func utc(month time.Month, d, hour int) int64 {
	return ms(time.Date(2026, month, d, hour, 0, 0, 0, time.UTC))
}

// burnBoard is a board (estimates: points, or none) whose active sprint
// b1 runs from Monday 23 November at 09:00 to Friday 4 December at 17:00,
// testNow being Wednesday 25 November at noon. Its tickets:
//
//	A  3 points, to do, in b1 from before its start
//	B  5 points, done Tuesday at 15:00
//	C  2 points, added Tuesday at 10:00
//	D  2 points, re-estimated to 4 Monday at 15:00
//	E  in b1, its history not read
//	F  1 point, its status not known from Monday at noon
//	G  1 point, removed from b1 Monday at 14:00
//	H  1 point, canceled Tuesday at 11:00
//	I  1 point, made in b1 Tuesday at 11:00, its history from then
func burnBoard(estimates string) *fixture {
	f := newFixture(true, estimates).
		status("1", "To Do", "todo").status("3", "In Progress", "in_progress").status("5", "Done", "done").status("6", "Canceled", "canceled").
		dated("b1", "WEB Sprint 9", "active", utc(11, 23, 9), utc(12, 4, 17), 0, 1)
	long := ago(30)
	for _, t := range []struct {
		id, status, group, points string
		created, since            int64
		history                   bool
	}{
		{"A", "1", "todo", "3", long, long, true}, {"B", "5", "done", "5", long, utc(11, 24, 15), true}, {"C", "1", "todo", "2", long, long, true},
		{"D", "1", "todo", "4", long, long, true}, {"E", "1", "todo", "2", long, 0, false}, {"F", "3", "in_progress", "1", long, 0, true},
		{"G", "1", "todo", "1", long, long, true}, {"H", "6", "canceled", "1", long, utc(11, 24, 11), true},
		{"I", "1", "todo", "1", utc(11, 24, 11), utc(11, 24, 11), true},
	} {
		tk := ticket{id: t.id, key: "WEB-" + t.id, status: t.status, group: t.group, points: t.points, created: t.created, updated: ago(0.5), since: t.since}
		if t.history {
			tk.historyFrom = t.created
		}
		f.ticket(tk)
		if t.id != "G" {
			f.member("b1", t.id, t.group == "done")
		}
	}
	f.stay("B", "1", long, utc(11, 24, 15)).stay("F", "1", long, utc(11, 23, 12)).stay("H", "1", long, utc(11, 24, 11))
	before := ago(20)
	f.stretch("b1", "A", "3", before, 0).stretch("b1", "B", "5", before, 0).stretch("b1", "C", "2", utc(11, 24, 10), 0).
		stretch("b1", "D", "2", before, utc(11, 23, 15)).stretch("b1", "D", "4", utc(11, 23, 15), 0).
		stretch("b1", "F", "1", before, 0).stretch("b1", "G", "1", before, utc(11, 23, 14)).stretch("b1", "H", "1", before, 0).
		stretch("b1", "I", "1", utc(11, 24, 11), 0)
	return f
}

// days is a day chart's points as "label value" (a not counted count
// beside it), "label -" for none, with "!" on a partial day and "|" on the
// last.
func days(w WidgetValue) []string {
	var out []string
	for _, b := range w.Buckets {
		s := b.Label + " " + strings.ReplaceAll(num(&b.Cells[0]), "none", "-")
		if b.Partial {
			s += " !"
		}
		if b.Last {
			s += " |"
		}
		out = append(out, s)
	}
	return out
}

// The burndown is the active sprint's open work at its start and at the
// end of each working day, each ticket by its estimate then: done and
// canceled work leaves it, added work joins it from its day, a removed
// ticket leaves it, a ticket whose history is not read or whose status is
// not known is listed and not counted, today is so far, and the days to
// come have no value, the last one marked. On a board without estimates
// it counts tickets. A day's point opens its tickets.
func TestTheBurndownCountsTheOpenWorkOfEachDay(t *testing.T) {
	t.Parallel()
	a := mustEvaluate(t, evaluator(), burnBoard("points").board(), Request{Dashboard: builtinSprintsID, TimeZone: "UTC"})
	w := widgetOf(t, a, "burndown")
	want := []string{
		"Start 13, 1 not counted",    // A, B, D at 2, F, G and H; E's history is not read
		"Nov 23 13, 2 not counted",   // G removed, D at 4, F's status not known
		"Nov 24 10, 2 not counted",   // B done, H canceled, C and I added
		"Nov 25 10, 2 not counted !", // so far
		"Nov 26 -", "Nov 27 -", "Nov 30 -", "Dec 1 -", "Dec 2 -", "Dec 3 -", "Dec 4 - |",
	}
	if got := days(w); !reflect.DeepEqual(got, want) {
		t.Fatalf("burndown =\n%v\nwant\n%v", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(w.RangeWords, "WEB Sprint 9") {
		t.Fatalf("How this is counted = %q; want the sprint named", w.RangeWords)
	}
	tickets := mustEvaluate(t, evaluator(), burnBoard("none").board(), Request{Dashboard: builtinSprintsID, TimeZone: "UTC"})
	if got := days(widgetOf(t, tickets, "burndown"))[:3]; !reflect.DeepEqual(got, []string{"Start 6, 1 not counted", "Nov 23 4, 2 not counted", "Nov 24 4, 2 not counted"}) {
		t.Fatalf("burndown in tickets = %v", got)
	}
	d, err := evaluator().Drill(context.Background(), burnBoard("points").board(), DrillRequest{Request: Request{Dashboard: builtinSprintsID, TimeZone: "UTC"},
		Widget: "burndown", Cell: &DrillCell{Bucket: "day:2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range d.Rows {
		rows = append(rows, r.Key+" "+map[bool]string{true: "counted", false: "not counted: " + r.Note}[r.Counted])
	}
	if want := []string{"WEB-D counted", "WEB-A counted", "WEB-C counted", "WEB-I counted", "WEB-E not counted: its history is not read yet", "WEB-F not counted: its status then is not known"}; !reflect.DeepEqual(rows, want) || num(&d.Value) != "10, 2 not counted" {
		t.Fatalf("Nov 24's drill = %s %v; want %v", num(&d.Value), rows, want)
	}
	// A later day has not happened: its point opens no tickets.
	d, err = evaluator().Drill(context.Background(), burnBoard("points").board(), DrillRequest{Request: Request{Dashboard: builtinSprintsID, TimeZone: "UTC"},
		Widget: "burndown", Cell: &DrillCell{Bucket: "day:2026-12-04"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Rows) != 0 || d.Value.V != nil {
		t.Fatalf("Dec 4's drill = %s with %d rows; want none", num(&d.Value), len(d.Rows))
	}
}

// A board with no active sprint, one whose active sprint has no dates,
// and a board that plans no sprints say so in place of a day chart, and
// a drill of it is refused with the same sentence.
func TestADayChartSaysWhyItHasNoDays(t *testing.T) {
	t.Parallel()
	closed := burnBoard("points").exec(`UPDATE sprints SET state = 'closed', completed_at = %d`, ago(1))
	a := mustEvaluate(t, evaluator(), closed.board(), Request{Dashboard: builtinSprintsID, TimeZone: "UTC"})
	if w := widgetOf(t, a, "burndown"); w.Note != "No sprint is active on this board." || w.Buckets != nil {
		t.Fatalf("no active sprint: %+v", w)
	}
	undated := burnBoard("points").exec(`UPDATE sprints SET end_at = NULL`)
	a = mustEvaluate(t, evaluator(), undated.board(), Request{Dashboard: builtinSprintsID, TimeZone: "UTC"})
	if w := widgetOf(t, a, "burndown"); w.Note != "WEB Sprint 9 has no start or end date, so it has no days to count." {
		t.Fatalf("undated sprint: %+v", w)
	}
	_, err := evaluator().Drill(context.Background(), undated.board(), DrillRequest{Request: Request{Dashboard: builtinSprintsID, TimeZone: "UTC"}, Widget: "burndown"})
	if r, ok := err.(*Refusal); !ok || !strings.Contains(r.Sentence, "has no start or end date") {
		t.Fatalf("drill of an undated sprint's burndown: %v", err)
	}
	weeks := newFixture(false, "points").status("1", "To Do", "todo").ticket(ticket{id: "t1", key: "WEB-1", status: "1", group: "todo", created: ago(3), since: ago(3)})
	m := Metric{Name: "w_1", Title: "Open", Words: []string{"Open tickets by day."}, SQL: `SELECT ticket, from_at AS at, to_at AS until FROM status_intervals`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketDay}
	tried, err := evaluator().Try(context.Background(), weeks.board(), Widget{Form: FormLine, Width: 6}, m, "UTC")
	if err != nil || tried.Value.Note != "This board plans no sprints, so it has no sprint days to count." {
		t.Fatalf("a day chart on a week board: %+v, %v", tried.Value, err)
	}
}

// The working days run from the start's day to the later of the end's day
// and today, Saturdays and Sundays left out, in the request's zone; the
// last working day on or before the end is the sprint's last, and a
// sprint long past its end draws its days to today until it passes
// maxSprintDays.
func TestSprintDaysFollowTheZoneAndTheEnd(t *testing.T) {
	t.Parallel()
	sprint := func(start, end int64) []sprintRow {
		return []sprintRow{{id: "b1", name: "WEB Sprint 9", state: "active", startAt: start, endAt: end}}
	}
	labels := func(ps []period) string {
		var out []string
		for _, p := range ps {
			l := p.label
			if p.last {
				l += "|"
			}
			out = append(out, l)
		}
		return strings.Join(out, " ")
	}
	// Ending on a Sunday: Friday is the last working day.
	ps, _, note := sprintDays(sprint(utc(11, 23, 9), utc(11, 29, 12)), testNow, time.UTC)
	if got := labels(ps); note != "" || got != "Start Nov 23 Nov 24 Nov 25 Nov 26 Nov 27|" {
		t.Fatalf("days = %q, %q", got, note)
	}
	// Past its end: the days run to today, the end's day still the last.
	ps, _, _ = sprintDays(sprint(utc(11, 16, 9), utc(11, 20, 17)), testNow, time.UTC)
	if got := labels(ps); got != "Start Nov 16 Nov 17 Nov 18 Nov 19 Nov 20| Nov 23 Nov 24 Nov 25" || !ps[len(ps)-1].partial {
		t.Fatalf("days past the end = %q", got)
	}
	// In Auckland, 09:00 UTC Monday is Monday night, and now is Thursday.
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatal(err)
	}
	ps, _, _ = sprintDays(sprint(utc(11, 23, 9), utc(11, 27, 2)), testNow, auckland)
	if got := labels(ps); got != "Start Nov 23 Nov 24 Nov 25 Nov 26 Nov 27|" || !ps[4].partial || !ps[5].future {
		t.Fatalf("days in Auckland = %q", got)
	}
	if _, _, note := sprintDays(sprint(utc(1, 5, 9), utc(1, 16, 17)), testNow, time.UTC); !strings.Contains(note, "runs past 64 working days") {
		t.Fatalf("a sprint far past its end: %q", note)
	}
}

// A state row holds from its at until its until: at <= t < until, a
// missing at from before anything and a missing until still.
func TestAStateHoldsFromAtUntilUntil(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		row  contribution
		t    int64
		want bool
	}{
		{contribution{at: 10, hasAt: true, until: 20, hasUntil: true}, 10, true},
		{contribution{at: 10, hasAt: true, until: 20, hasUntil: true}, 20, false},
		{contribution{at: 10, hasAt: true, until: 20, hasUntil: true}, 9, false},
		{contribution{until: 20, hasUntil: true}, 0, true},
		{contribution{at: 10, hasAt: true}, 1 << 40, true},
		{contribution{at: 10, hasAt: true, until: 10, hasUntil: true}, 10, false},
	} {
		if got := c.row.holdsAt(c.t); got != c.want {
			t.Errorf("%+v holds at %d = %v", c.row, c.t, got)
		}
	}
}

// A state's number is what holds now, its previous value what held where
// the range begins, and its trend what held at each period's end; a
// metric that returns until is refused when it adds periods' sums, a day
// chart needs an at or an until and no bucket, and a row whose until is
// before its at is refused.
func TestAStateCountsWhereItHolds(t *testing.T) {
	t.Parallel()
	b := teamBoard().stay("t9", "3", ago(40), ago(20)).board()
	a := mustEvaluate(t, evaluator(), b, utcSprints3)
	stuck := widgetOf(t, a, "stuck")
	// Range: sprints 4 to 6, from s3's completion 22 days ago. Now WEB-3 is
	// stuck and WEB-4's time in progress is not known. 22 days ago WEB-9
	// was stuck (in progress from 40 days ago to 20), and the status of
	// WEB-6, 7 and 8, made 60 days ago with no interval, is not known; the
	// others were not made yet. At the end of s4, 15 days ago, and of s6,
	// a day ago, every ticket with no interval then is not counted.
	if got := []string{num(stuck.Value), num(stuck.Previous), num(&stuck.Trend[0]), num(&stuck.Trend[2])}; !reflect.DeepEqual(got, []string{
		"1, 1 not counted", "1, 3 not counted", "not known, 10 not counted", "1, 8 not counted"}) {
		t.Fatalf("stuck value, previous and trend = %v", got)
	}
	try := func(sql string, r Reducer, bucket Bucket) error {
		m := Metric{Name: "w_1", Title: "States", Words: []string{"States."}, SQL: sql, Reduce: r, Unit: UnitTickets, Bucket: bucket}
		_, err := evaluator().Try(context.Background(), teamBoard().board(), Widget{Form: FormNumber, Width: 3}, m, "UTC")
		return err
	}
	for _, c := range []struct {
		sql    string
		r      Reducer
		bucket Bucket
		want   string
	}{
		{`SELECT ticket, from_at AS at, to_at AS until, 'x' AS bucket FROM status_intervals`, ReduceAverageOfPeriodSums, BucketSprint, "periods are not added up"},
		{`SELECT id AS ticket FROM tickets`, ReduceCount, BucketDay, "an at or an until column"},
		{`SELECT ticket, from_at AS at, 'x' AS bucket FROM status_intervals`, ReduceCount, BucketDay, "returns no bucket column"},
		{`SELECT ticket, from_at AS at, from_at - 1 AS until FROM status_intervals`, ReduceCount, BucketNone, "until is before its at"},
	} {
		if err := try(c.sql, c.r, c.bucket); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.sql, err, c.want)
		}
	}
}

// scopeBoard is a points board with two closed sprints: c1, from 20 days
// ago to 6 days ago, and c2, completed 3 days ago, with no start date.
//
//	P  3 points, in c1 from before its start, done at its completion
//	Q  2 points, added to c1 15 days ago and removed 10 days ago
//	R  1 point, in c1 from before its start, removed 12 days ago
//	S  in c1 at its completion, its history from 18 days ago
//	T  added to c1 14 days ago at 4 points, re-estimated to 5 a day later
//	U  in c2 from before
//	V  1 point, made 15 days ago in c1, its history from then
//	W  1 point, in c1 from 30 days ago until 22 days ago, before its start
//	X  in c1 from before its start at 2 points, re-estimated to 3 10 days ago
func scopeBoard() *fixture {
	f := newFixture(true, "points").status("1", "To Do", "todo").status("5", "Done", "done").
		dated("c1", "WEB Sprint 1", "closed", ago(20), ago(6), ago(6), 1).
		dated("c2", "WEB Sprint 2", "closed", 0, ago(3), ago(3), 2)
	for _, t := range []struct {
		id, status, group, points string
		created, history          int64
	}{
		{"P", "5", "done", "3", ago(40), ago(40)}, {"Q", "1", "todo", "2", ago(40), ago(40)}, {"R", "1", "todo", "1", ago(40), ago(40)},
		{"S", "1", "todo", "2", ago(40), ago(18)}, {"T", "1", "todo", "5", ago(40), ago(40)}, {"U", "1", "todo", "1", ago(40), ago(40)},
		{"V", "1", "todo", "1", ago(15), ago(15)}, {"W", "1", "todo", "1", ago(40), ago(40)}, {"X", "1", "todo", "3", ago(40), ago(40)},
	} {
		f.ticket(ticket{id: t.id, key: "WEB-" + t.id, status: t.status, group: t.group, points: t.points, created: t.created, historyFrom: t.history})
	}
	f.member("c1", "P", true).member("c1", "S", false).member("c1", "T", false).member("c1", "X", false).member("c2", "U", false)
	return f.stretch("c1", "P", "3", ago(25), 0).stretch("c1", "Q", "2", ago(15), ago(10)).stretch("c1", "R", "1", ago(25), ago(12)).
		stretch("c1", "S", "2", ago(18), 0).stretch("c1", "T", "4", ago(14), ago(13)).stretch("c1", "T", "5", ago(13), 0).
		stretch("c2", "U", "1", ago(25), 0).stretch("c1", "V", "1", ago(15), 0).stretch("c1", "W", "1", ago(30), ago(22)).
		stretch("c1", "X", "2", ago(25), ago(10)).stretch("c1", "X", "3", ago(10), 0)
}

// A finished sprint's table says what was committed at its start, added
// after it and removed before its end, each at its estimate then, and
// what was completed by the pace's rule; a ticket added and then removed
// counts in both, each ticket once in each. A ticket whose history does
// not reach the start, and every member of a sprint with no start date,
// is listed and not counted in committed and added. Each cell opens its
// tickets.
func TestTheSprintScopeTableCountsEachSprintsChanges(t *testing.T) {
	t.Parallel()
	r := Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), scopeBoard().board(), r)
	w := widgetOf(t, a, "sprint_scope")
	var got []string
	for _, b := range w.Buckets {
		row := b.Label
		for _, c := range b.Cells {
			row += " | " + num(&c)
		}
		got = append(got, row)
	}
	want := []string{
		"WEB Sprint 1 | 6, 1 not counted | 7, 1 not counted | 3 | 3", // P, R and X at 2; Q, T at 4 and V; Q and R; P
		"WEB Sprint 2 | not known, 1 not counted | not known, 1 not counted | 0 | 0",
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(w.Series, []string{"Committed", "Added", "Removed", "Completed"}) {
		t.Fatalf("scope = %v %v\nwant %v", w.Series, got, want)
	}
	d, err := evaluator().Drill(context.Background(), scopeBoard().board(), DrillRequest{Request: r, Widget: "sprint_scope", Cell: &DrillCell{Bucket: "sprint:c1", Series: "Added"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, row := range d.Rows {
		rows = append(rows, row.Key+" "+map[bool]string{true: "counted", false: "not counted: " + row.Note}[row.Counted])
	}
	if want := []string{"WEB-T counted", "WEB-Q counted", "WEB-V counted", "WEB-S not counted: its history begins after the sprint started"}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("added in c1 = %v; want %v", rows, want)
	}
}
