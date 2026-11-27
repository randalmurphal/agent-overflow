package trackerdash

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// worklogBoard is a weeks board of tickets T1 (u1's) and T2 (u2's) on
// which the viewer u1 logged an hour on Tuesday 17 November and half an
// hour on Monday 23, with days read: read (YYYY-MM-DD).
func worklogBoard(read ...string) *fixture {
	f := newFixture(false, "points").person("u1", "User 1").person("u2", "User 2").status("1", "To Do", "todo")
	for _, t := range []struct{ id, assignee string }{{"T1", "u1"}, {"T2", "u2"}} {
		f.exec(`INSERT INTO tickets(id, key, title, type_level, status, status_group, assignee, created_at, updated_at, history_behind, started_known, open, counts)
			VALUES ('%s', 'WEB-%s', 'T', 0, '1', 'todo', '%s', %d, %d, 0, 1, 1, 1)`, t.id, t.id, t.assignee, ago(60), ago(1))
	}
	f.exec(`INSERT INTO worklogs VALUES ('w1', 'T1', '2026-11-17', 3600), ('w2', 'T2', '2026-11-23', 1800)`)
	for _, d := range read {
		f.exec(`INSERT INTO worklog_days VALUES ('%s')`, d)
	}
	return f
}

// daysOf is the days from first to last, as YYYY-MM-DD.
func daysOf(first, last string) []string {
	var out []string
	for d := mustDay(first); !d.After(mustDay(last)); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

// loggedTime is a board of the own widgets of a metric of the viewer's
// logged time by week: a number and bars of the weeks, and bars of the
// people.
func loggedTime(f *fixture) Board {
	week := Metric{Name: "w_1", Title: "Your hours", Words: []string{"Your logged time by week."},
		SQL: `SELECT ticket, seconds AS value, day_start(day) AS at FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime, Bucket: BucketPeriod}
	people := Metric{Name: "w_2", Title: "Your hours by assignee", Words: []string{"Your logged time by the ticket's assignee."},
		SQL:    `SELECT w.ticket, w.seconds AS value, day_start(w.day) AS at, t.assignee AS bucket FROM worklogs w JOIN tickets t ON t.id = w.ticket`,
		Reduce: ReduceSum, Unit: UnitTime, Bucket: BucketText}
	b := f.board()
	b.TimeTracking = true
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{
		{ID: "n", Metric: "w_1", Form: FormNumber, Width: 3}, {ID: "bars", Metric: "w_1", Form: FormBars, Width: 6},
		{ID: "people", Metric: "w_2", Form: FormBars, Width: 6}}}},
		Metrics: map[string]Metric{"w_1": week, "w_2": people}}
	return b
}

func trendOf(w WidgetValue) []string {
	var out []string
	for _, n := range w.Trend {
		out = append(out, num(&n))
	}
	return out
}

// window is the days of the copy's history window up to testNow
// (2026-05-30 to 2026-11-25), but those of except.
func window(except ...string) []string {
	return slices.DeleteFunc(daysOf("2026-05-30", "2026-11-25"), func(d string) bool { return slices.Contains(except, d) })
}

// A number, a period and a bar of the viewer's logged time count only
// once every day of the window is read: with today unread, the range's
// number, the range before, each week and each bar are not known by that
// day, and the page is told to read it; once it is read, they count.
func TestLoggedTimeCountsOnlyOnceTheWindowIsRead(t *testing.T) {
	t.Parallel()
	read := window("2026-11-25")
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), loggedTime(worklogBoard(read...)), r)
	n := widgetOf(t, a, "n")
	unknown := "not known, 1 days not read"
	if num(n.Value) != unknown || num(n.Previous) != unknown ||
		!reflect.DeepEqual(trendOf(n), []string{unknown, unknown}) || !reflect.DeepEqual(n.Unread, []string{"2026-11-25"}) {
		t.Fatalf("number = %s, before %s, trend %v, unread %v", num(n.Value), num(n.Previous), trendOf(n), n.Unread)
	}
	var bars []string
	for _, b := range widgetOf(t, a, "bars").Buckets {
		bars = append(bars, b.Key+" "+num(&b.Cells[0]))
	}
	if want := []string{"week:2026-11-16 " + unknown, "week:2026-11-23 " + unknown}; !reflect.DeepEqual(bars, want) || !reflect.DeepEqual(widgetOf(t, a, "bars").Unread, []string{"2026-11-25"}) {
		t.Fatalf("bars = %v, unread %v; want %v", bars, widgetOf(t, a, "bars").Unread, want)
	}
	if p := widgetOf(t, a, "people").Buckets; len(p) != 2 || num(&p[0].Cells[0]) != unknown || num(&p[1].Total) != unknown {
		t.Fatalf("people = %+v; want each not known", p)
	}
	d := mustDrill(t, loggedTime(worklogBoard(read...)), DrillRequest{Request: r, Widget: "bars", Cell: &DrillCell{Bucket: "week:2026-11-16"}})
	if num(&d.Value) != unknown {
		t.Fatalf("week 1's drill = %s", num(&d.Value))
	}
	d = mustDrill(t, loggedTime(worklogBoard(read...)), DrillRequest{Request: r, Widget: "n"})
	if num(&d.Value) != unknown || len(d.Rows) != 2 {
		t.Fatalf("the number's drill = %s, %d rows", num(&d.Value), len(d.Rows))
	}
	a = mustEvaluate(t, evaluator(), loggedTime(worklogBoard(window()...)), r)
	if n := widgetOf(t, a, "n"); num(n.Value) != "5400" || num(n.Previous) != "0" || n.Unread != nil {
		t.Fatalf("the window read: %s, before %s, unread %v", num(n.Value), num(n.Previous), n.Unread)
	}
	if d := mustDrill(t, loggedTime(worklogBoard(window()...)), DrillRequest{Request: r, Widget: "bars", Cell: &DrillCell{Bucket: "week:2026-11-16"}}); num(&d.Value) != "3600" {
		t.Fatalf("week 1's drill, the window read = %s", num(&d.Value))
	}
}

// A number of logged time compares with the range before once the days
// of the window are read, the range before's among them: one of them not
// read leaves the number and the range before not known; once read, the
// change has both numbers.
func TestLoggedTimeComparesOnceTheRangeBeforeIsRead(t *testing.T) {
	t.Parallel()
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	f := worklogBoard(window("2026-11-10")...)
	f.exec(`INSERT INTO worklogs VALUES ('w0', 'T1', '2026-11-10', 600)`)
	n := widgetOf(t, mustEvaluate(t, evaluator(), loggedTime(f), r), "n")
	if num(n.Value) != "not known, 1 days not read" || num(n.Previous) != "not known, 1 days not read" || !reflect.DeepEqual(n.Unread, []string{"2026-11-10"}) {
		t.Fatalf("before the read: %s, before %s, unread %v", num(n.Value), num(n.Previous), n.Unread)
	}
	f = worklogBoard(window()...)
	f.exec(`INSERT INTO worklogs VALUES ('w0', 'T1', '2026-11-10', 600)`)
	n = widgetOf(t, mustEvaluate(t, evaluator(), loggedTime(f), r), "n")
	if num(n.Value) != "5400" || num(n.Previous) != "600" || n.Unread != nil {
		t.Fatalf("after the read: %s, before %s, unread %v", num(n.Value), num(n.Previous), n.Unread)
	}
}

// A row of logged time placed by something other than its own day counts
// only once the window is read, so a day outside the range that is not
// read leaves the number not known rather than a part of it: hours on a
// sprint's tickets placed in the sprint (bucket by sprint, logged the day
// before it began), and a state that holds from its day on, counted now
// and where the range begins. Both ranges read, an older day not read
// holds two hours each counts.
func TestLoggedTimePlacedOffItsDayNeedsTheWholeWindow(t *testing.T) {
	t.Parallel()
	sprintHours := func(read ...string) Board {
		f := worklogBoard(read...)
		f.plansSprints = true
		f.sprint("s1", "Sprint 1", "closed", ago(8), ago(1), 1)
		f.member("s1", "T1", false)
		f.member("s1", "T2", false)
		f.exec(`INSERT INTO worklogs VALUES ('w9', 'T1', '2026-11-16', 7200)`)
		b := loggedTime(f)
		b.Own.Metrics["w_4"] = Metric{Name: "w_4", Title: "Hours on sprint tickets", Words: []string{"Your logged time on each sprint's tickets."},
			SQL: `SELECT w.ticket, w.seconds AS value, s.sprint AS bucket FROM worklogs w JOIN sprint_tickets s ON s.ticket = w.ticket`, Reduce: ReduceSum, Unit: UnitTime, Bucket: BucketSprint}
		b.Own.Dashboards[0].Widgets = append(b.Own.Dashboards[0].Widgets, Widget{ID: "sprints", Metric: "w_4", Form: FormNumber, Width: 3})
		return b
	}
	r := Request{Dashboard: "mine", Range: RangeSprints3, TimeZone: "UTC"}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), sprintHours(window("2026-11-16")...), r), "sprints"); num(n.Value) != "not known, 1 days not read" || !reflect.DeepEqual(n.Unread, []string{"2026-11-16"}) {
		t.Fatalf("the sprint's hours, the day before it not read: %s, unread %v", num(n.Value), n.Unread)
	}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), sprintHours(window()...), r), "sprints"); num(n.Value) != "12600" {
		t.Fatalf("the sprint's hours, the window read: %s", num(n.Value))
	}
	held := func(read ...string) Board {
		f := worklogBoard(read...)
		f.exec(`INSERT INTO worklogs VALUES ('w9', 'T1', '2026-10-01', 7200)`)
		b := loggedTime(f)
		b.Own.Metrics["w_5"] = Metric{Name: "w_5", Title: "Hours so far", Words: []string{"Your logged time so far."},
			SQL: `SELECT ticket, seconds AS value, day_start(day) AS at, NULL AS until FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime}
		b.Own.Dashboards[0].Widgets = append(b.Own.Dashboards[0].Widgets, Widget{ID: "held", Metric: "w_5", Form: FormNumber, Width: 3})
		return b
	}
	r.Range = RangeWeeks2
	if n := widgetOf(t, mustEvaluate(t, evaluator(), held(window("2026-10-01")...), r), "held"); num(n.Value) != "not known, 1 days not read" || num(n.Previous) != "not known, 1 days not read" {
		t.Fatalf("the hours so far, an older day not read: %s, before %s", num(n.Value), num(n.Previous))
	}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), held(window()...), r), "held"); num(n.Value) != "12600" || num(n.Previous) != "7200" {
		t.Fatalf("the hours so far, the window read: %s, before %s", num(n.Value), num(n.Previous))
	}
}

// A widget of logged time counted by sprint over a range with no sprint
// (This sprint, none active) counts nothing and lists no day to read; one
// that counts now on the same page lists the window's.
func TestLoggedTimeOverARangeWithNoPeriodReadsNothing(t *testing.T) {
	t.Parallel()
	f := worklogBoard()
	f.plansSprints = true
	f.sprint("s1", "Sprint 1", "closed", ago(20), ago(6), 1)
	b := loggedTime(f)
	b.Own.Metrics["w_3"] = Metric{Name: "w_3", Title: "All your hours", Words: []string{"All your logged time."},
		SQL: `SELECT ticket, seconds AS value FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime}
	b.Own.Dashboards[0].Widgets = append(b.Own.Dashboards[0].Widgets, Widget{ID: "all", Metric: "w_3", Form: FormNumber, Width: 3})
	a := mustEvaluate(t, evaluator(), b, Request{Dashboard: "mine", Range: RangeThisSprint, TimeZone: "UTC"})
	for _, id := range []string{"n", "bars", "people"} {
		if w := widgetOf(t, a, id); w.Unread != nil || (w.Value != nil && w.Value.UR > 0) {
			t.Errorf("%s over no sprint: unread %d days, value %s", id, len(w.Unread), num(w.Value))
		}
	}
	if w := widgetOf(t, a, "all"); len(w.Unread) != 180 {
		t.Errorf("the hours that count now list %d days; want the window's 180", len(w.Unread))
	}
}

// A number of logged time that counts now, its rows placed at no instant,
// counts every day the copy can hold: with none read, the 180 days from
// the copy's earliest to today are not read; with one not read, that one.
// A kept sprint that began before them changes nothing: the copy holds
// the 180 days.
func TestLoggedTimeCountingNowNeedsEveryDayTheCopyHolds(t *testing.T) {
	t.Parallel()
	all := func(f *fixture) Board {
		b := loggedTime(f)
		b.Own.Metrics["w_3"] = Metric{Name: "w_3", Title: "All your hours", Words: []string{"All your logged time."},
			SQL: `SELECT ticket, seconds AS value FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime}
		b.Own.Dashboards[0].Widgets = append(b.Own.Dashboards[0].Widgets, Widget{ID: "all", Metric: "w_3", Form: FormNumber, Width: 3})
		return b
	}
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	n := widgetOf(t, mustEvaluate(t, evaluator(), all(worklogBoard()), r), "all")
	if len(n.Unread) != 180 || n.Unread[0] != "2026-05-30" || n.Unread[179] != "2026-11-25" || num(n.Value) != "not known, 180 days not read" {
		t.Fatalf("none read: %s, %d unread from %v", num(n.Value), len(n.Unread), n.Unread[:min(1, len(n.Unread))])
	}
	n = widgetOf(t, mustEvaluate(t, evaluator(), all(worklogBoard(window("2026-11-23")...)), r), "all")
	if !reflect.DeepEqual(n.Unread, []string{"2026-11-23"}) || num(n.Value) != "not known, 1 days not read" {
		t.Fatalf("one not read: %s, unread %v", num(n.Value), n.Unread)
	}
	n = widgetOf(t, mustEvaluate(t, evaluator(), all(worklogBoard(window()...)), r), "all")
	if n.Unread != nil || num(n.Value) != "5400" {
		t.Fatalf("all read: %s, unread %v", num(n.Value), n.Unread)
	}
	// A kept sprint that began 10 days before the copy's 180 days: the
	// number counts the 180 the copy holds, and is known once they are read.
	old := worklogBoard()
	old.earliest = ms(testNow) - 190*day
	n = widgetOf(t, mustEvaluate(t, evaluator(), all(old), r), "all")
	if len(n.Unread) != 180 || n.Unread[0] != "2026-05-30" || num(n.Value) != "not known, 180 days not read" {
		t.Fatalf("an older sprint kept: %s, %d unread from %v", num(n.Value), len(n.Unread), n.Unread[:min(1, len(n.Unread))])
	}
	old = worklogBoard(window()...)
	old.earliest = ms(testNow) - 190*day
	if n := widgetOf(t, mustEvaluate(t, evaluator(), all(old), r), "all"); n.Unread != nil || num(n.Value) != "5400" {
		t.Fatalf("an older sprint kept, all read: %s, unread %v", num(n.Value), n.Unread)
	}
}

// A sprint that began before the copy's 180 days holds days the page
// never reads: its period stays not known by them, never a part of its
// time, and the page is told to read only the window's. The sprint ran
// from 190 days ago to 176 days ago: 14 days, the first 10 before the
// window.
func TestLoggedTimeOfASprintBeforeTheWindowIsNotKnown(t *testing.T) {
	t.Parallel()
	board := func(read ...string) Board {
		f := worklogBoard(read...)
		f.plansSprints = true
		f.earliest = ms(testNow) - 190*day
		f.sprint("s1", "Sprint 1", "closed", ms(testNow)-190*day, ms(testNow)-176*day, 1)
		f.exec(`INSERT INTO worklogs VALUES ('w8', 'T1', '2026-06-01', 600)`)
		return loggedTime(f)
	}
	r := Request{Dashboard: "mine", Range: RangeSprints3, TimeZone: "UTC"}
	n := widgetOf(t, mustEvaluate(t, evaluator(), board(), r), "n")
	if num(n.Value) != "not known, 190 days not read" || !reflect.DeepEqual(n.Unread, window()) {
		t.Fatalf("none read: %s, unread %d days", num(n.Value), len(n.Unread))
	}
	n = widgetOf(t, mustEvaluate(t, evaluator(), board(window()...), r), "n")
	if num(n.Value) != "not known, 10 days not read" || n.Unread != nil {
		t.Fatalf("the window's days read: %s, unread %v", num(n.Value), n.Unread)
	}
}

// A day of a weekend is read like any other: time logged on Saturday 21
// November, its weekend not read, leaves the weeks not known by two days,
// and the page is told to read them.
func TestALoggedWeekendDayCountsOnlyOnceRead(t *testing.T) {
	t.Parallel()
	f := worklogBoard(window("2026-11-21", "2026-11-22")...)
	f.exec(`INSERT INTO worklogs VALUES ('w3', 'T1', '2026-11-21', 600)`)
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	n := widgetOf(t, mustEvaluate(t, evaluator(), loggedTime(f), r), "n")
	if !reflect.DeepEqual(trendOf(n), []string{"not known, 2 days not read", "not known, 2 days not read"}) || !reflect.DeepEqual(n.Unread, []string{"2026-11-21", "2026-11-22"}) {
		t.Fatalf("trend %v, unread %v", trendOf(n), n.Unread)
	}
	f = worklogBoard(window()...)
	f.exec(`INSERT INTO worklogs VALUES ('w3', 'T1', '2026-11-21', 600)`)
	if n := widgetOf(t, mustEvaluate(t, evaluator(), loggedTime(f), r), "n"); !reflect.DeepEqual(trendOf(n), []string{"4200", "1800"}) {
		t.Fatalf("all read: trend %v", trendOf(n))
	}
}

// A worklog's day is placed on its date in the request's zone: Monday 23
// November counts in the week that begins that Monday in Auckland as in
// Los Angeles, though the two Mondays begin 21 hours apart.
func TestALoggedDayCountsOnItsDateInTheRequestsZone(t *testing.T) {
	t.Parallel()
	read := daysOf("2026-05-28", "2026-11-27")
	for _, zone := range []string{"Pacific/Auckland", "America/Los_Angeles"} {
		a := mustEvaluate(t, evaluator(), loggedTime(worklogBoard(read...)), Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: zone})
		var bars []string
		for _, b := range widgetOf(t, a, "bars").Buckets {
			bars = append(bars, b.Key+" "+num(&b.Cells[0]))
		}
		if want := []string{"week:2026-11-16 3600", "week:2026-11-23 1800"}; !reflect.DeepEqual(bars, want) {
			t.Errorf("in %s: bars = %v; want %v", zone, bars, want)
		}
	}
}

// The viewer's logged time is the viewer's whatever ticket it is on: Mine
// keeps it all; a page narrowed to another person, a board without time
// tracking and a connection whose account is not verified say so in the
// widget's place and refuse its drill.
func TestLoggedTimeIsTheViewersOwn(t *testing.T) {
	t.Parallel()
	read := window()
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	mine := r
	mine.Mine = true
	if n := widgetOf(t, mustEvaluate(t, evaluator(), loggedTime(worklogBoard(read...)), mine), "n"); num(n.Value) != "5400" {
		t.Fatalf("Mine = %s; want 5400, T2's half hour among it", num(n.Value))
	}
	other := r
	other.Person = "u2"
	linear := loggedTime(worklogBoard(read...))
	linear.TimeTracking = false
	f := worklogBoard(read...)
	f.unverified = true
	unverified := loggedTime(f)
	for _, c := range []struct {
		name string
		b    Board
		r    Request
		want string
	}{{"narrowed to User 2", loggedTime(worklogBoard(read...)), other, onlyYourTime}, {"Linear", linear, r, noTimeTracking}, {"not verified", unverified, r, viewerUnverified}} {
		if w := widgetOf(t, mustEvaluate(t, evaluator(), c.b, c.r), "n"); w.Note != c.want || w.Value != nil {
			t.Errorf("%s: note %q, value %s", c.name, w.Note, num(w.Value))
		}
		if _, err := evaluator().Drill(context.Background(), c.b, DrillRequest{Request: c.r, Widget: "n"}); err == nil || err.Error() != c.want {
			t.Errorf("%s: drill %v", c.name, err)
		}
	}
}

// day_start places a day at its date's first millisecond in the zone, and
// refuses a value that is no day. A date whose midnight a zone change
// skips (America/Santiago's 6 September 2026 begins at 01:00) is placed
// where it begins, on its own date.
func TestDayStartPlacesADayOnItsDate(t *testing.T) {
	t.Parallel()
	got := runOne(t, `SELECT day_start('2026-11-23'), day(day_start('2026-11-23')), day_start(NULL) IS NULL`, "Pacific/Auckland")
	if want := fmt.Sprintf("%d|2026-11-23|1", ms(mustDay("2026-11-22").Add(11*time.Hour))+1); got != want {
		t.Fatalf("day_start = %s; want %s", got, want)
	}
	got = runOne(t, `SELECT day_start('2026-09-06'), day(day_start('2026-09-06')), day(day_start('2026-09-05'))`, "America/Santiago")
	if want := fmt.Sprintf("%d|2026-09-06|2026-09-05", ms(mustDay("2026-09-06").Add(4*time.Hour))+1); got != want {
		t.Fatalf("Santiago's day_start = %s; want %s", got, want)
	}
}

// The days not read are civil dates: in America/Santiago, whose 6
// September 2026 begins at 01:00, a Sunday not read is listed at noon
// that day, and at noon on Monday with Sunday read, Monday is.
func TestUnreadCountsEachDateOfAZoneChange(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	ev := &evaluation{loc: loc, readDays: map[string]bool{}}
	for _, d := range daysOf("2026-09-01", "2026-09-05") {
		ev.readDays[d] = true
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).UnixMilli() - 1
	if got := ev.unread(from, from+10*day, time.Date(2026, 9, 6, 12, 0, 0, 0, loc).UnixMilli()); !reflect.DeepEqual(got, []string{"2026-09-06"}) {
		t.Fatalf("at noon on Sunday: %v; want the Sunday", got)
	}
	ev.readDays["2026-09-06"] = true
	if got := ev.unread(from, from+10*day, time.Date(2026, 9, 7, 12, 0, 0, 0, loc).UnixMilli()); !reflect.DeepEqual(got, []string{"2026-09-07"}) {
		t.Fatalf("at noon on Monday: %v; want the Monday", got)
	}
}

func mustDay(d string) time.Time {
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		panic(err)
	}
	return t
}

// runOne is the one row q answers on a session of zone, its columns
// joined by |.
func runOne(t *testing.T, q, zone string) string {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openSession(context.Background(), loc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	}()
	var a, b, c string
	if err := s.conn.QueryRowContext(context.Background(), q).Scan(&a, &b, &c); err != nil {
		t.Fatal(err)
	}
	return a + "|" + b + "|" + c
}

// A period that ends after now, a sprint's day still to come among them,
// has no day not read past today.
func TestUnreadStopsAtNow(t *testing.T) {
	t.Parallel()
	ev := &evaluation{loc: time.UTC, readDays: map[string]bool{"2026-11-24": true}}
	got := ev.unread(ms(mustDay("2026-11-23"))-1, ms(mustDay("2026-11-30"))-1, ms(testNow))
	if want := []string{"2026-11-23", "2026-11-25"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unread = %v; want %v", got, want)
	}
	if got := ev.unread(ms(mustDay("2026-11-26"))-1, ms(mustDay("2026-11-27"))-1, ms(testNow)); got != nil {
		t.Fatalf("a day to come: %v", got)
	}
}

// How this is counted says a widget of logged time counts each row for
// you, whose time it is, not for the ticket's assignee.
func TestHowSaysLoggedTimeCountsForYou(t *testing.T) {
	t.Parallel()
	b := loggedTime(worklogBoard())
	list, err := evaluator().Dashboards(context.Background(), Kind{Epics: true}, 0, b.Own)
	if err != nil {
		t.Fatal(err)
	}
	said := 0
	for _, d := range list.Dashboards {
		for _, w := range d.Widgets {
			if w.ID != "n" && w.ID != "bars" {
				continue
			}
			if said++; !strings.HasSuffix(w.How.Counts, "; each row of logged time counts for you, whose time it is.") {
				t.Errorf("%s says %q", w.ID, w.How.Counts)
			}
		}
	}
	if said != 2 {
		t.Fatalf("the list has %d of the two widgets of logged time", said)
	}
}
