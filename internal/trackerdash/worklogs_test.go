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

// window is the days of the copy's logged time up to testNow, from its
// first day (MirrorExport.LoggedFrom: 2026-05-28 to 2026-11-25), but those
// of except.
func window(except ...string) []string {
	return slices.DeleteFunc(daysOf("2026-05-28", "2026-11-25"), func(d string) bool { return slices.Contains(except, d) })
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
// (This sprint, none active) counts nothing and lists no day to read, nor
// does its drill; one that counts now on the same page lists the window's.
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
	if w := widgetOf(t, a, "all"); len(w.Unread) != 182 {
		t.Errorf("the hours that count now list %d days; want the window's 182", len(w.Unread))
	}
	if d := mustDrill(t, b, DrillRequest{Request: Request{Dashboard: "mine", Range: RangeThisSprint, TimeZone: "UTC"}, Widget: "n"}); d.Value.UR > 0 {
		t.Errorf("the number's drill over no sprint: %s", num(&d.Value))
	}
}

// A number of logged time that counts now, its rows placed at no instant,
// counts every day the copy can hold: with none read, the 182 days from
// the copy's first to today are not read; with one not read, that one.
// A kept sprint that began before them changes nothing: the copy holds
// the 182 days.
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
	if len(n.Unread) != 182 || n.Unread[0] != "2026-05-28" || n.Unread[181] != "2026-11-25" || num(n.Value) != "not known, 182 days not read" {
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
	// A kept sprint that began 8 days before the copy's 182 days: the
	// number counts the 182 the copy holds, and is known once they are read.
	old := worklogBoard()
	old.earliest = ms(testNow) - 190*day
	n = widgetOf(t, mustEvaluate(t, evaluator(), all(old), r), "all")
	if len(n.Unread) != 182 || n.Unread[0] != "2026-05-28" || num(n.Value) != "not known, 182 days not read" {
		t.Fatalf("an older sprint kept: %s, %d unread from %v", num(n.Value), len(n.Unread), n.Unread[:min(1, len(n.Unread))])
	}
	old = worklogBoard(window()...)
	old.earliest = ms(testNow) - 190*day
	if n := widgetOf(t, mustEvaluate(t, evaluator(), all(old), r), "all"); n.Unread != nil || num(n.Value) != "5400" {
		t.Fatalf("an older sprint kept, all read: %s, unread %v", num(n.Value), n.Unread)
	}
}

// A sprint that began before the copy's first day holds days the copy
// never holds: its number and its period are not known for good, with no
// day to read, whatever the copy has read, and ask the reads for nothing.
// The sprint ran from 190 days ago to 176 days ago: 14 days, the first 8
// before 28 May.
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
	for _, read := range [][]string{nil, window()} {
		a := mustEvaluate(t, evaluator(), board(read...), r)
		n := widgetOf(t, a, "n")
		if num(n.Value) != "not known, older than kept" || !reflect.DeepEqual(trendOf(n), []string{"not known, older than kept"}) || n.Unread != nil {
			t.Fatalf("%d days read: %s, trend %v, unread %d days", len(read), num(n.Value), trendOf(n), len(n.Unread))
		}
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

// The copy's logged time and the days it checks begin at one day,
// MirrorExport.LoggedFrom: a number counted from every row the copy holds
// is not known while that first day is not read, and counts its hours
// once it is.
func TestLoggedTimeChecksEveryDayTheCopyHolds(t *testing.T) {
	t.Parallel()
	all := func(read ...string) Board {
		f := worklogBoard(read...)
		f.exec(`INSERT INTO worklogs VALUES ('w9', 'T1', '2026-05-29', 7200)`)
		b := loggedTime(f)
		b.Own.Metrics["w_3"] = Metric{Name: "w_3", Title: "All your hours", Words: []string{"All your logged time."},
			SQL: `SELECT ticket, seconds AS value FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime}
		b.Own.Dashboards[0].Widgets = append(b.Own.Dashboards[0].Widgets, Widget{ID: "all", Metric: "w_3", Form: FormNumber, Width: 3})
		return b
	}
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), all(window("2026-05-28", "2026-05-29")...), r), "all"); num(n.Value) != "not known, 2 days not read" {
		t.Fatalf("its first two days not read: %s, unread %v", num(n.Value), n.Unread)
	}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), all(window()...), r), "all"); num(n.Value) != "12600" {
		t.Fatalf("all read: %s", num(n.Value))
	}
}

// A state of logged time over a range with no period still counts the
// rows that hold now, so it is not known while a day is not read, and its
// drill too; a chart of its periods counts nothing and lists no day.
func TestLoggedTimeHeldNowOverARangeWithNoPeriodNeedsTheWindow(t *testing.T) {
	t.Parallel()
	board := func(read ...string) Board {
		f := worklogBoard(read...)
		f.plansSprints = true
		f.sprint("s1", "Sprint 1", "closed", ago(20), ago(6), 1)
		b := loggedTime(f)
		b.Own.Metrics["w_5"] = Metric{Name: "w_5", Title: "Hours so far", Words: []string{"Your logged time so far."},
			SQL: `SELECT ticket, seconds AS value, day_start(day) AS at, NULL AS until FROM worklogs`, Reduce: ReduceSum, Unit: UnitTime, Bucket: BucketPeriod}
		b.Own.Dashboards[0].Widgets = []Widget{{ID: "held", Metric: "w_5", Form: FormNumber, Width: 3}, {ID: "line", Metric: "w_5", Form: FormLine, Width: 6}}
		return b
	}
	r := Request{Dashboard: "mine", Range: RangeThisSprint, TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), board("2026-11-17", "2026-11-23"), r)
	if n := widgetOf(t, a, "held"); num(n.Value) != "not known, 180 days not read" || len(n.Unread) != 180 {
		t.Fatalf("held now, two days read: %s, %d unread", num(n.Value), len(n.Unread))
	}
	if l := widgetOf(t, a, "line"); l.Unread != nil || len(l.Buckets) != 0 {
		t.Fatalf("a line of no period: %d unread, %d buckets", len(l.Unread), len(l.Buckets))
	}
	if d := mustDrill(t, board("2026-11-17", "2026-11-23"), DrillRequest{Request: r, Widget: "held"}); num(&d.Value) != "not known, 180 days not read" {
		t.Fatalf("its drill: %s", num(&d.Value))
	}
	if n := widgetOf(t, mustEvaluate(t, evaluator(), board(window()...), r), "held"); num(n.Value) != "5400" {
		t.Fatalf("held now, all read: %s", num(n.Value))
	}
}

// An evaluation in which a widget counted the viewer's logged time tells
// the board's reads once what the window needs and answers their state;
// one in which none could (narrowed to another person, a range with no
// period for a metric counted by period) tells them nothing. A proposal
// that counts it tells them as a widget does, and so does a Try.
func TestAnEvaluationTellsTheReadsWhatItNeeds(t *testing.T) {
	t.Parallel()
	var told []WorklogNeed
	tell := func(b Board) Board {
		b.Worklogs = func(n WorklogNeed) WorklogState {
			told = append(told, n)
			return WorklogState{Reading: true, Failure: "refused"}
		}
		return b
	}
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "Pacific/Auckland"}
	a := mustEvaluate(t, evaluator(), tell(loggedTime(worklogBoard(window("2026-11-20")...))), r)
	if len(told) != 1 || !reflect.DeepEqual(told[0].Unread, []string{"2026-11-20", "2026-11-26"}) || told[0].Today != "2026-11-26" ||
		a.Logged == nil || *a.Logged != (WorklogState{Reading: true, Failure: "refused"}) {
		t.Fatalf("told %+v, answered %+v", told, a.Logged)
	}
	told = nil
	other := r
	other.Person = "u2"
	sprints := worklogBoard()
	sprints.plansSprints = true
	sprints.sprint("s1", "Sprint 1", "closed", ago(20), ago(6), 1)
	for _, c := range []struct {
		name string
		b    Board
		r    Request
	}{{"narrowed", loggedTime(worklogBoard()), other}, {"no sprint", loggedTime(sprints), Request{Dashboard: "mine", Range: RangeThisSprint, TimeZone: "UTC"}}} {
		if a := mustEvaluate(t, evaluator(), tell(c.b), c.r); len(told) != 0 || a.Logged != nil {
			t.Errorf("%s: told %+v, answered %+v", c.name, told, a.Logged)
		}
	}
	proposed := loggedTime(worklogBoard())
	week := proposed.Own.Metrics["w_1"]
	proposed.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine"}}, Proposals: []Proposal{{Widget: Widget{ID: "p", Metric: "w_1", Form: FormNumber, Width: 3}}}, Metrics: map[string]Metric{"w_1": week}}
	if a := mustEvaluate(t, evaluator(), tell(proposed), Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}); len(told) != 1 || a.Logged == nil {
		t.Fatalf("a proposal: told %d, answered %+v", len(told), a.Logged)
	}
	told = nil
	if _, err := evaluator().Try(context.Background(), tell(loggedTime(worklogBoard())), Widget{Form: FormNumber, Width: 3}, week, "UTC"); err != nil || len(told) != 1 {
		t.Fatalf("a try: told %d, %v", len(told), err)
	}
}

// coverageBoard is a sprints board of the viewer's logged time, today
// (25 November) not read, with six closed sprints: p1, p2 and p3 from 200
// days ago to 160 days ago, around the copy's first day (28 May, 181 days
// ago; p3 begins as it does, at its midnight), and r1, r2 and r3 from 60
// days ago to 18 days ago; and active,
// from 5 days ago to 9 days on. Its metrics count the logged time now
// (now), by the period of each row's day (event), and as a state that
// holds from its day on (state), and by the active sprint's days (days).
func coverageBoard() Board {
	f := worklogBoard(window("2026-11-25")...)
	f.plansSprints = true
	f.earliest = ago(200)
	for i, s := range []struct {
		id         string
		start, end float64
	}{{"p1", 200, 190}, {"p2", 190, 176}, {"p3", 181.5, 160}, {"r1", 60, 46}, {"r2", 46, 32}, {"r3", 32, 18}} {
		f.sprint(s.id, s.id, "closed", ago(s.start), ago(s.end), i+1)
	}
	f.dated("active", "active", "active", ago(5), ago(-9), 0, 7)
	metric := func(name, sql string, bucket Bucket) Metric {
		return Metric{Name: name, Title: name, Words: []string{"Your logged time."}, SQL: sql, Reduce: ReduceSum, Unit: UnitTime, Bucket: bucket}
	}
	b := f.board()
	b.TimeTracking = true
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{
		{ID: "now", Metric: "now", Form: FormNumber, Width: 3}, {ID: "now_bars", Metric: "now_by", Form: FormBars, Width: 6},
		{ID: "event", Metric: "event", Form: FormNumber, Width: 3}, {ID: "event_bars", Metric: "event", Form: FormBars, Width: 6},
		{ID: "event_list", Metric: "event", Form: FormList, Width: 6},
		{ID: "state", Metric: "state", Form: FormNumber, Width: 3}, {ID: "state_bars", Metric: "state", Form: FormBars, Width: 6},
		{ID: "days", Metric: "days", Form: FormNumber, Width: 3}, {ID: "days_bars", Metric: "days", Form: FormBars, Width: 6},
	}}}, Metrics: map[string]Metric{
		"now":    metric("now", `SELECT ticket, seconds AS value FROM worklogs`, ""),
		"now_by": metric("now_by", `SELECT ticket, seconds AS value, ticket AS bucket FROM worklogs`, BucketText),
		"event":  metric("event", `SELECT ticket, seconds AS value, day_start(day) AS at FROM worklogs`, BucketPeriod),
		"state":  metric("state", `SELECT ticket, seconds AS value, day_start(day) AS at, NULL AS until FROM worklogs`, BucketPeriod),
		"days":   metric("days", `SELECT ticket, seconds AS value, day_start(day) AS at FROM worklogs`, BucketDay),
	}}
	return b
}

// Each number of logged time is not known by what its own reduction
// took: a value of now, of rows placed in periods, and of a state at an
// instant on or after the copy's first day by the window's day not read;
// a period, several periods or a state at an instant that reach before
// that day for good (old); a period still to come, and a range with no
// period, not at all. A period that begins on that day's midnight needs
// the window alone. Each kind is checked where it can occur: the widget's
// number, a bucket, a trend point, the comparison, a list, the drill and
// Try.
func TestEachLoggedNumberIsKnownByWhatItsReductionTook(t *testing.T) {
	t.Parallel()
	const unread, old, none = "not known, 1 days not read", "not known, older than kept", "none"
	b := coverageBoard()
	drill := func(r Request, widget string, cell string) string {
		dr := DrillRequest{Request: r, Widget: widget}
		if cell != "" {
			dr.Cell = &DrillCell{Bucket: cell}
		}
		d := mustDrill(t, b, dr)
		return num(&d.Value)
	}
	buckets := func(w WidgetValue) []string {
		var out []string
		for _, bk := range w.Buckets {
			out = append(out, num(&bk.Total))
		}
		return out
	}
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %s; want %s", what, got, want)
		}
	}
	checkAll := func(what string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v; want %v", what, got, want)
		}
	}

	// The last 3 sprints, r1 to r3; the range before is p1 to p3, which
	// begins before the copy's first day.
	r := Request{Dashboard: "mine", Range: RangeSprints3, TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), b, r)
	now := widgetOf(t, a, "now")
	check("now: the number", num(now.Value), unread)
	checkAll("now: its buckets", buckets(widgetOf(t, a, "now_bars")), []string{unread, unread})
	check("now: the drill", drill(r, "now", ""), unread)
	event := widgetOf(t, a, "event")
	check("several periods: the number", num(event.Value), unread)
	check("several periods before the window: the comparison", num(event.Previous), old)
	checkAll("an event period: the trend", trendOf(event), []string{unread, unread, unread})
	checkAll("an event period: the buckets", buckets(widgetOf(t, a, "event_bars")), []string{unread, unread, unread})
	check("an event period: a bar's drill", drill(r, "event_bars", "sprint:r1"), unread)
	check("several periods: the drill", drill(r, "event", ""), unread)
	state := widgetOf(t, a, "state")
	check("a state now: the number", num(state.Value), unread)
	check("a state at the range's start, 60 days ago: the comparison", num(state.Previous), unread)
	checkAll("a state at a period's end: the trend", trendOf(state), []string{unread, unread, unread})
	check("a state now: the drill", drill(r, "state", ""), unread)
	if len(now.Unread) != 1 || len(event.Unread) != 1 {
		t.Errorf("the widgets list %v and %v; want today", now.Unread, event.Unread)
	}
	if list := widgetOf(t, a, "event_list"); len(list.Unread) != 1 || list.Old {
		t.Errorf("an event period's list lists %v, old %v; want today, not old", list.Unread, list.Old)
	}

	// The last 10 sprints, p1 to r3: p1 and p2 begin before the copy's
	// first day; p1 ends before it, p2 after.
	r.Range = RangeSprints10
	a = mustEvaluate(t, evaluator(), b, r)
	event = widgetOf(t, a, "event")
	check("several periods from before the window: the number", num(event.Value), old)
	// p3 begins at the first day's midnight: none of its rows is of a day
	// before it.
	checkAll("an event period from before the window: the trend", trendOf(event), []string{old, old, unread, unread, unread, unread})
	if list := widgetOf(t, a, "event_list"); list.Unread != nil || !list.Old {
		t.Errorf("a list of event periods from before the window lists %v, old %v; want old and no day", list.Unread, list.Old)
	}
	checkAll("an event period from before the window: the buckets", buckets(widgetOf(t, a, "event_bars")), []string{old, old, unread, unread, unread, unread})
	check("an event period from before the window: a bar's drill", drill(r, "event_bars", "sprint:p2"), old)
	check("several periods from before the window: the drill", drill(r, "event", ""), old)
	state = widgetOf(t, a, "state")
	check("a state now, the range from before the window: the number", num(state.Value), unread)
	check("a state at a start before the window: the comparison", num(state.Previous), old)
	checkAll("a state at a period's end, before the window and after: the trend", trendOf(state), []string{old, unread, unread, unread, unread, unread})
	checkAll("a state at a period's end: the buckets", buckets(widgetOf(t, a, "state_bars")), []string{old, unread, unread, unread, unread, unread})
	check("a state at the end of p1, before the window: a bar's drill", drill(r, "state_bars", "sprint:p1"), old)
	check("a state at the end of p2, in the window: a bar's drill", drill(r, "state_bars", "sprint:p2"), unread)
	check("a state now, the range from before the window: the drill", drill(r, "state", ""), unread)

	// This sprint by its days: the days still to come take no row.
	r.Range = RangeThisSprint
	a = mustEvaluate(t, evaluator(), b, r)
	// The days so far are not known by the day not read, the days still
	// to come none: the first of them is where they change.
	split := func(what string, got []string) int {
		t.Helper()
		k := slices.Index(got, none)
		if k < 1 || slices.ContainsFunc(got[:k], func(v string) bool { return v != unread }) || slices.ContainsFunc(got[k:], func(v string) bool { return v != none }) {
			t.Errorf("%s = %v; want the days so far %q, then the days to come %q", what, got, unread, none)
		}
		return k
	}
	split("the active sprint's days: the trend", trendOf(widgetOf(t, a, "days")))
	daysBars := widgetOf(t, a, "days_bars")
	if k := split("the active sprint's days: the buckets", buckets(daysBars)); k > 0 {
		check("a day still to come: its drill", drill(r, "days_bars", daysBars.Buckets[k].Key), none)
		check("a day so far: its drill", drill(r, "days_bars", daysBars.Buckets[k-1].Key), unread)
	}

	// Try counts over the board's default range, the last 5 sprints, p2
	// to r3, which begins before the copy's first day.
	for _, c := range []struct{ metric, value, previous string }{{"now", unread, "absent"}, {"event", old, "gap"}, {"state", unread, old}} {
		tried, err := evaluator().Try(context.Background(), b, Widget{Form: FormNumber, Width: 3}, b.Own.Metrics[c.metric], "UTC")
		if err != nil {
			t.Fatal(err)
		}
		check("Try of "+c.metric+": the number", num(tried.Value.Value), c.value)
		check("Try of "+c.metric+": the comparison", num(tried.Value.Previous), c.previous)
	}
}
