package trackerdash

import (
	"context"
	"fmt"
	"reflect"
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

// days is the working days from first to last, as YYYY-MM-DD.
func workingDaysOf(first, last string) []string {
	var out []string
	for d := mustDay(first); !d.After(mustDay(last)); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != 0 && wd != 6 {
			out = append(out, d.Format("2006-01-02"))
		}
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

// A number, a period and a bar of the viewer's logged time count only
// where every working day of them was read: today unread, the range's
// number and its week are not known, one day not read, the week before
// counts, and the page is told which days to read; once today is read,
// they count. The range before, none of it read, is not known by its 10
// working days.
func TestLoggedTimeCountsOnlyWhereItsDaysWereRead(t *testing.T) {
	t.Parallel()
	read := append(workingDaysOf("2026-11-16", "2026-11-20"), "2026-11-23", "2026-11-24")
	r := Request{Dashboard: "mine", Range: RangeWeeks2, TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), loggedTime(worklogBoard(read...)), r)
	n := widgetOf(t, a, "n")
	if num(n.Value) != "not known, 1 days not read" || num(n.Previous) != "not known, 10 days not read" ||
		!reflect.DeepEqual(trendOf(n), []string{"3600", "not known, 1 days not read"}) || !reflect.DeepEqual(n.Unread, []string{"2026-11-25"}) {
		t.Fatalf("number = %s, before %s, trend %v, unread %v", num(n.Value), num(n.Previous), trendOf(n), n.Unread)
	}
	var bars []string
	for _, b := range widgetOf(t, a, "bars").Buckets {
		bars = append(bars, b.Key+" "+num(&b.Cells[0]))
	}
	if want := []string{"week:2026-11-16 3600", "week:2026-11-23 not known, 1 days not read"}; !reflect.DeepEqual(bars, want) {
		t.Fatalf("bars = %v; want %v", bars, want)
	}
	if p := widgetOf(t, a, "people").Buckets; len(p) != 2 || num(&p[0].Cells[0]) != "not known, 1 days not read" {
		t.Fatalf("people = %+v; want each not known", p)
	}
	if a.Viewer != "u1" {
		t.Fatalf("viewer = %q", a.Viewer)
	}
	d := mustDrill(t, loggedTime(worklogBoard(read...)), DrillRequest{Request: r, Widget: "bars", Cell: &DrillCell{Bucket: "week:2026-11-16"}})
	if num(&d.Value) != "3600" {
		t.Fatalf("week 1's drill = %s", num(&d.Value))
	}
	d = mustDrill(t, loggedTime(worklogBoard(read...)), DrillRequest{Request: r, Widget: "n"})
	if num(&d.Value) != "not known, 1 days not read" || len(d.Rows) != 2 {
		t.Fatalf("the number's drill = %s, %d rows", num(&d.Value), len(d.Rows))
	}
	a = mustEvaluate(t, evaluator(), loggedTime(worklogBoard(append(read, "2026-11-25")...)), r)
	if n := widgetOf(t, a, "n"); num(n.Value) != "5400" || n.Unread != nil {
		t.Fatalf("all read: %s, unread %v", num(n.Value), n.Unread)
	}
}

// A worklog's day is placed on its date in the request's zone: Monday 23
// November counts in the week that begins that Monday in Auckland as in
// Los Angeles, though the two Mondays begin 21 hours apart.
func TestALoggedDayCountsOnItsDateInTheRequestsZone(t *testing.T) {
	t.Parallel()
	read := workingDaysOf("2026-11-02", "2026-11-27")
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
	read := workingDaysOf("2026-11-02", "2026-11-27")
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
	unverified := loggedTime(worklogBoard(read...))
	unverified.Viewer = ""
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
// refuses a value that is no day.
func TestDayStartPlacesADayOnItsDate(t *testing.T) {
	t.Parallel()
	got := runOne(t, `SELECT day_start('2026-11-23'), day(day_start('2026-11-23')), day_start(NULL) IS NULL`, "Pacific/Auckland")
	if want := fmt.Sprintf("%d|2026-11-23|1", ms(mustDay("2026-11-22").Add(11*time.Hour))+1); got != want {
		t.Fatalf("day_start = %s; want %s", got, want)
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
