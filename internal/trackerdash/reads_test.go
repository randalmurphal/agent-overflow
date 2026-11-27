package trackerdash

import (
	"fmt"
	"testing"
)

// manyReviews is 20,800 credit rows of sprint s, over the row limit.
func manyReviews(f *fixture, sprint string) *fixture {
	return f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20800)
		INSERT INTO credit(sprint, at, ticket, kind, person, value, share, points, review_share, review_need, did, qualified, hours, hours_per_point, note)
		SELECT '%s', %d, 't1', 'review', 'u2', 0.5, 0.1, 5, 10, 2, 'commented, logged time', 1, NULL, 4, NULL FROM n`, sprint, ago(40))
}

// A state number by sprint is what holds now, which a sprint of the range
// need not hold: a ticket in active WEB Sprint 7 since yesterday counts
// on Last 3 sprints, with no credit counted.
func TestAStateNumberBySprintCountsWhatHoldsNow(t *testing.T) {
	t.Parallel()
	m := Metric{Name: "w_1", Title: "In a sprint", Words: []string{"The tickets in a sprint."},
		SQL:    `SELECT ticket, sprint AS bucket, from_at AS at, to_at AS until FROM sprint_intervals`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketSprint}
	f := sprintBoard()
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "3", group: "in_progress", created: ago(20)}).stretch("s7", "t1", "", ago(1), 0)
	b := f.board()
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: FormNumber, Width: 3}}}},
		Metrics: map[string]Metric{"w_1": m}}
	w := widgetOf(t, mustEvaluate(t, evaluator(), b, Request{Dashboard: "mine", Range: RangeSprints3, TimeZone: "UTC"}), "w_1")
	if w.Error != "" || num(w.Value) != "1" {
		t.Fatalf("the number is %s (%q); want 1, WEB-1 in WEB Sprint 7 now", num(w.Value), w.Error)
	}
}

// A metric's rows are charged only when a widget of the evaluation takes
// them: the finished chart alone on This sprint reads none of the sprint
// before it, and the pace on Last 5 sprints with fewer than 5 sprints
// before them compares with nothing, so 20,800 rows of the sprint before
// either range leave it counted.
func TestARowNoWidgetTakesIsNotCharged(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		sprint string
		r      Request
		widget string
	}{
		{"the chart alone on This sprint", "s6", Request{Dashboard: builtinSprintsID, Range: RangeThisSprint, TimeZone: "UTC", Widgets: []string{"finished"}}, "finished"},
		{"the pace on Last 5 sprints with a gap before them", "s1", utcSprints5, "pace"},
	} {
		a := mustEvaluate(t, evaluator(), creditOn(manyReviews(creditBoard("points"), c.sprint)), c.r)
		if w := widgetOf(t, a, c.widget); w.Error != "" {
			t.Errorf("%s: %s", c.name, w.Error)
		}
	}
}

// A drill of one bar takes that bar's sprint alone: 20,800 rows of WEB
// Sprint 5 leave WEB Sprint 6's bar of Last 5 sprints drilled, though
// the chart, which takes them, is over the limit.
func TestADrillOfABarIsChargedOnlyItsSprint(t *testing.T) {
	t.Parallel()
	b := creditOn(manyReviews(creditBoard("points"), "s5"))
	if w := widgetOf(t, mustEvaluate(t, evaluator(), b, utcSprints5), "finished"); w.Error == "" {
		t.Fatal("the chart counted 20,800 rows of WEB Sprint 5; want it over the limit")
	}
	d := mustDrill(t, b, DrillRequest{Request: utcSprints5, Widget: "finished", Cell: &DrillCell{Bucket: "sprint:s6"}})
	if d.Count != 5 {
		t.Fatalf("WEB Sprint 6's bar holds %d rows; want its 5", d.Count)
	}
}

// A week's rows that no widget takes are not charged either: the rows of
// the four weeks before Last 4 weeks reach no chart of the range.
func TestAnEventByPeriodIsChargedOnlyThePeriodsItsWidgetsTake(t *testing.T) {
	t.Parallel()
	m := Metric{Name: "w_1", Title: "Changes", Words: []string{"Changes by week."}, SQL: `SELECT ticket, at FROM changes`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketPeriod}
	f := newFixture(false, "points").person("u1", "User 1").status("1", "To Do", "todo")
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "1", group: "todo", created: ago(40)})
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20800)
		INSERT INTO changes SELECT 't1', 'status', i, %d, NULL, '1', '1', NULL, NULL FROM n`, ago(40))
	f.exec(`INSERT INTO changes VALUES ('t1', 'status', 0, %d, NULL, '1', '1', NULL, NULL)`, ago(1))
	b := f.board()
	for _, form := range []Form{FormBars, FormNumber} {
		b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: form, Width: 6}}}},
			Metrics: map[string]Metric{"w_1": m}}
		w := widgetOf(t, mustEvaluate(t, evaluator(), b, Request{Dashboard: "mine", Range: RangeWeeks4, TimeZone: "UTC"}), "w_1")
		got := w.Error
		if got == "" {
			got = fmt.Sprintf("counted %s", num(w.Value))
		}
		want := map[Form]string{FormBars: "counted absent", FormNumber: fmt.Sprintf("This widget's query returned more than %d rows, its limit. Pick a shorter range.", maxRows)}[form]
		if got != want {
			t.Errorf("%s: %s; want %s", form, got, want)
		}
	}
}

// A state chart by sprint takes what holds at a sprint's end in that
// sprint alone: 20,800 memberships of WEB Sprint 1 still open leave This
// sprint's bar counted, its one ticket in WEB Sprint 7.
func TestAStateChartBySprintIsChargedOnlyItsSprints(t *testing.T) {
	t.Parallel()
	m := Metric{Name: "w_1", Title: "In a sprint", Words: []string{"The tickets in a sprint."},
		SQL:    `SELECT ticket, sprint AS bucket, from_at AS at, to_at AS until FROM sprint_intervals`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketSprint}
	f := sprintBoard()
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "3", group: "in_progress", created: ago(60)}).stretch("s7", "t1", "", ago(1), 0)
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20800)
		INSERT INTO sprint_intervals SELECT 's1', 't1', NULL, %d + i, NULL FROM n`, ago(50))
	b := f.board()
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: FormBars, Width: 6}}}},
		Metrics: map[string]Metric{"w_1": m}}
	w := widgetOf(t, mustEvaluate(t, evaluator(), b, Request{Dashboard: "mine", Range: RangeThisSprint, TimeZone: "UTC"}), "w_1")
	if w.Error != "" || len(w.Buckets) != 1 || num(&w.Buckets[0].Total) != "1" {
		t.Fatalf("This sprint's bar: %+v (%q); want WEB-1 in WEB Sprint 7", w.Buckets, w.Error)
	}
}

// A row with no instant is charged only when its ticket's life meets a
// period its widgets take, as it is placed: 20,800 of a ticket last
// changed 300 days ago leave Last 4 weeks counted.
func TestAnUndatedRowIsChargedOnlyWhereItsTicketLived(t *testing.T) {
	t.Parallel()
	m := Metric{Name: "w_1", Title: "Changes", Words: []string{"Changes, WEB-1's of no known time."}, SQL: `SELECT ticket, iif(ticket = 't1', NULL, at) AS at FROM changes`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketPeriod}
	f := newFixture(false, "points").person("u1", "User 1").status("5", "Done", "done")
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", created: ago(400), updated: ago(300)})
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", created: ago(10), updated: ago(2)})
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20800)
		INSERT INTO changes SELECT 't1', 'status', i, %d, NULL, '1', '5', NULL, NULL FROM n`, ago(300))
	f.exec(`INSERT INTO changes VALUES ('t2', 'status', 0, %d, NULL, '1', '5', NULL, NULL)`, ago(2))
	b := f.board()
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: FormBars, Width: 6}}}},
		Metrics: map[string]Metric{"w_1": m}}
	w := widgetOf(t, mustEvaluate(t, evaluator(), b, Request{Dashboard: "mine", Range: RangeWeeks4, TimeZone: "UTC"}), "w_1")
	if w.Error != "" || w.NotCounted != 0 {
		t.Fatalf("Last 4 weeks: %q, %d not counted; want counted, none of WEB-1's", w.Error, w.NotCounted)
	}
}
