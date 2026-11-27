package trackerdash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// teamBoard is sprintBoard with its tickets:
//
//	WEB-1  done, 3 points, User 1, finished in s6; started 5 days ago, done 2 days ago
//	WEB-2  done, 5 points, User 2, finished in s5; started 12 days ago, done 9 days ago
//	WEB-3  in progress 5 days, 2 points, User 1, in s6 (not done) and s7; epic WEB-100
//	WEB-4  in progress since an unknown time, 1 point, User 2, in s7
//	WEB-5  in progress 1 day, User 1
//	WEB-6  done, 8 points, User 1, finished in s4; it was in progress when its history begins
//	WEB-7  done, 2 points, finished in s1
//	WEB-8  done, 4 points, finished in s2
//	WEB-9  to do, 3 points, User 1, in s7; epic WEB-100
//	WEB-10 done, 2 points, User 2, in s7, done now
func teamBoard() *fixture {
	f := sprintBoard()
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", points: "3", created: ago(20), historyFrom: ago(20), started: ago(5), done: ago(2)}).member("s6", "t1", true)
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", assignee: "u2", points: "5", created: ago(20), historyFrom: ago(20), started: ago(12), done: ago(9)}).member("s5", "t2", true)
	f.ticket(ticket{id: "t3", key: "WEB-3", status: "3", group: "in_progress", assignee: "u1", epic: "e1", points: "2", created: ago(20), historyFrom: ago(20), since: ago(5)}).
		member("s6", "t3", false).member("s7", "t3", false)
	f.ticket(ticket{id: "t4", key: "WEB-4", status: "3", group: "in_progress", assignee: "u2", points: "1", created: ago(20)}).member("s7", "t4", false)
	f.ticket(ticket{id: "t5", key: "WEB-5", status: "3", group: "in_progress", assignee: "u1", created: ago(20), since: ago(1)})
	f.ticket(ticket{id: "t6", key: "WEB-6", status: "5", group: "done", assignee: "u1", points: "8", created: ago(60), historyFrom: ago(30), started: ago(20), done: ago(16), startUnknown: true}).member("s4", "t6", true)
	f.ticket(ticket{id: "t7", key: "WEB-7", status: "5", group: "done", points: "2", created: ago(60), done: ago(37)}).member("s1", "t7", true)
	f.ticket(ticket{id: "t8", key: "WEB-8", status: "5", group: "done", points: "4", created: ago(60), done: ago(30)}).member("s2", "t8", true)
	f.ticket(ticket{id: "t9", key: "WEB-9", status: "1", group: "todo", assignee: "u1", epic: "e1", points: "3", created: ago(20)}).member("s7", "t9", false)
	f.ticket(ticket{id: "t10", key: "WEB-10", status: "5", group: "done", assignee: "u2", points: "2", created: ago(20)}).member("s7", "t10", true)
	return f
}

var utcSprints3 = Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC"}

// A sprint board's built-in dashboard counts its sprints: the pace is the
// average of what each of the range's sprints finished, with the sprints
// before it as the previous value and one trend point a sprint; the bars
// are the same sums; a "now" widget counts the tickets stuck today.
func TestEvaluateCountsTheBuiltInDashboard(t *testing.T) {
	t.Parallel()
	a := mustEvaluate(t, evaluator(), teamBoard().board(), utcSprints3)
	if a.Estimates != "points" || a.Range.ID != RangeSprints3 || len(a.Range.Periods) != 3 || a.Range.Periods[0].Label != "WEB Sprint 4" {
		t.Fatalf("range = %+v, estimates %s; want sprints 4 to 6 of a points board", a.Range, a.Estimates)
	}
	pace := widgetOf(t, a, "pace")
	if got := []string{num(pace.Value), num(pace.Previous), num(&pace.Trend[0]), num(&pace.Trend[1]), num(&pace.Trend[2])}; !reflect.DeepEqual(got, []string{"5.333333333333333", "2", "8", "5", "3"}) {
		t.Fatalf("pace value, previous and trend = %v; want 16/3 over sprints 4 to 6 (8, 5, 3) against 6/3 over 1 to 3", got)
	}
	bars := widgetOf(t, a, "finished")
	var totals []string
	for _, b := range bars.Buckets {
		totals = append(totals, b.Label+"="+num(&b.Total))
	}
	if !reflect.DeepEqual(totals, []string{"WEB Sprint 4=8", "WEB Sprint 5=5", "WEB Sprint 6=3"}) {
		t.Fatalf("finished bars = %v", totals)
	}
	stuck := widgetOf(t, a, "stuck")
	if num(stuck.Value) != "1, 1 not counted" {
		t.Fatalf("stuck = %s; want WEB-3 alone (WEB-4's time in its status is unknown, WEB-5 moved a day ago)", num(stuck.Value))
	}
	list := widgetOf(t, a, "stuck_list")
	if len(list.Tickets) != 1 || list.Tickets[0].Key != "WEB-3" || list.Tickets[0].Title != "Title of WEB-3" || list.Tickets[0].Assignee != "User 1" ||
		list.Tickets[0].Note != "in In Progress for 5 days" || list.NotCounted != 1 {
		t.Fatalf("stuck list = %+v, %d not counted; want WEB-3 in progress for 5 days and WEB-4 not counted", list.Tickets, list.NotCounted)
	}
	cycle := widgetOf(t, a, "start_to_done")
	if num(cycle.Value) == "none" || num(cycle.Value) == "absent" {
		t.Fatalf("start to done = %s", num(cycle.Value))
	}
	carrying := widgetOf(t, a, "carrying")
	if !reflect.DeepEqual(carrying.Series, []string{"To do", "Doing", "Done"}) {
		t.Fatalf("carrying series = %v", carrying.Series)
	}
	var rows []string
	for _, b := range carrying.Buckets {
		cells := []string{b.Label}
		for i := range b.Cells {
			cells = append(cells, num(&b.Cells[i]))
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	if !reflect.DeepEqual(rows, []string{"User 1 3 2 0", "User 2 0 1 2"}) {
		t.Fatalf("carrying = %v; want User 1 with 3 to do and 2 doing, User 2 with 1 doing and 2 done", rows)
	}
	epics := widgetOf(t, a, "open_by_epic")
	if len(epics.Buckets) != 2 || epics.Buckets[0].Label != "WEB-100 Checkout" || num(&epics.Buckets[0].Total) != "5" || epics.Buckets[1].Label != "No epic" {
		t.Fatalf("open by epic = %+v", epics.Buckets)
	}
	for _, w := range a.Widgets {
		if w.Error != "" {
			t.Errorf("widget %s failed: %s", w.ID, w.Error)
		}
	}
	if _, err := json.Marshal(a); err != nil {
		t.Fatal(err)
	}
}

// A number with nothing counted and a ticket not counted is not known,
// not zero, on the tile and in its drill: the board's only ticket in
// progress has no known time in its status.
func TestANumberOfNothingCountedIsNotKnown(t *testing.T) {
	t.Parallel()
	f := sprintBoard()
	f.ticket(ticket{id: "t4", key: "WEB-4", status: "3", group: "in_progress", assignee: "u2", points: "1", created: ago(20)})
	a := mustEvaluate(t, evaluator(), f.board(), utcSprints3)
	stuck := widgetOf(t, a, "stuck")
	if num(stuck.Value) != "not known, 1 not counted" {
		t.Fatalf("stuck = %s; want not known, WEB-4 not counted", num(stuck.Value))
	}
	if b, err := json.Marshal(stuck.Value); err != nil || string(b) != `{"nc":1}` {
		t.Fatalf("stuck on the wire = %s, %v", b, err)
	}
	d, err := evaluator().Drill(context.Background(), f.board(), DrillRequest{Request: utcSprints3, Widget: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	if num(&d.Value) != "not known, 1 not counted" {
		t.Fatalf("stuck's drill = %s; want the tile's number", num(&d.Value))
	}
}

// While a board's first read has not finished, the answer and a drill
// say so, and the range before is not compared: what the copy holds of
// either range is partial.
func TestAFirstReadNotFinishedIsSaid(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	f.reading = true
	a := mustEvaluate(t, evaluator(), f.board(), utcSprints3)
	if !a.Reading || a.Range.PreviousGap != firstReadGap || num(widgetOf(t, a, "pace").Previous) != "gap" {
		t.Fatalf("answer while reading: reading %v, previous gap %q, pace's previous %s", a.Reading, a.Range.PreviousGap, num(widgetOf(t, a, "pace").Previous))
	}
	d, err := evaluator().Drill(context.Background(), f.board(), DrillRequest{Request: utcSprints3, Widget: "pace"})
	if err != nil || !d.Reading {
		t.Fatalf("drill while reading = %v, %v; want reading", d.Reading, err)
	}
	if done := mustEvaluate(t, evaluator(), teamBoard().board(), utcSprints3); done.Reading || done.Range.PreviousGap != "" {
		t.Fatalf("answer after the first read: reading %v, previous gap %q", done.Reading, done.Range.PreviousGap)
	}
}

// A widget whose metric counts now has no previous value and no trend,
// so the screen can draw no change mark for it; its view says it counts
// now. A metric of the range has both, and a state's (Stuck) compares
// what holds now with what held where the range begins.
func TestNowMetricHasNoChangeMark(t *testing.T) {
	t.Parallel()
	e := evaluator()
	a := mustEvaluate(t, e, teamBoard().board(), utcSprints3)
	list, err := e.Dashboards(context.Background(), Kind{PlansSprints: true, Epics: true}, store.MirrorEstimatePoints, Own{})
	if err != nil {
		t.Fatal(err)
	}
	views := map[string]WidgetView{}
	for _, v := range list.Dashboards[0].Widgets {
		views[v.ID] = v
	}
	if !views["carrying"].Now || views["carrying"].State || !strings.Contains(views["carrying"].How.Range, "true now") {
		t.Fatalf("carrying: now %v, state %v, How %q; want a now widget", views["carrying"].Now, views["carrying"].State, views["carrying"].How.Range)
	}
	stuck := widgetOf(t, a, "stuck")
	if views["stuck"].Now || !views["stuck"].State || stuck.Previous == nil || len(stuck.Trend) != 3 || !strings.Contains(views["stuck"].How.Range, "where the range begins") {
		t.Fatalf("stuck: now %v, state %v, previous %s, trend %v, How %q; want a state with both", views["stuck"].Now, views["stuck"].State, num(stuck.Previous), stuck.Trend, views["stuck"].How.Range)
	}
	pace := widgetOf(t, a, "pace")
	if views["pace"].Now || views["pace"].State || pace.Previous == nil || len(pace.Trend) != 3 {
		t.Fatalf("pace: now %v, previous %s, trend %v; want both", views["pace"].Now, num(pace.Previous), pace.Trend)
	}
}

// The previous range is a gap when the mirror holds fewer sprints than
// the range has: five sprints back from the last five reach sprint 1
// alone.
func TestPreviousRangeIsAGapWhenTheMirrorHoldsTooFewSprints(t *testing.T) {
	t.Parallel()
	a := mustEvaluate(t, evaluator(), teamBoard().board(), Request{Dashboard: builtinSprintsID, Range: RangeSprints5, TimeZone: "UTC"})
	pace := widgetOf(t, a, "pace")
	if a.Range.PreviousGap != "This computer keeps fewer than 5 finished sprints before these to compare with." || !pace.Previous.Gap {
		t.Fatalf("previous = %s, range %+v; want a gap", num(pace.Previous), a.Range)
	}
	b, err := json.Marshal(pace.Previous)
	if err != nil || string(b) != `{"gap":true}` {
		t.Fatalf("a gap on the wire = %s, %v", b, err)
	}
}

// Narrowing keeps the rows of one person (the ticket's assignee) or one
// epic before the reduction; Mine is the viewer's account.
func TestNarrowingKeepsOnePersonsRows(t *testing.T) {
	t.Parallel()
	e := evaluator()
	mine := mustEvaluate(t, e, teamBoard().board(), Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC", Mine: true})
	if mine.Person == nil || mine.Person.Label != "User 1" {
		t.Fatalf("Mine narrowed to %+v; want User 1", mine.Person)
	}
	if got := num(widgetOf(t, mine, "pace").Value); got != "3.6666666666666665" {
		t.Fatalf("User 1's pace = %s; want (8 + 0 + 3) / 3", got)
	}
	carrying := widgetOf(t, mine, "carrying")
	if len(carrying.Buckets) != 1 || carrying.Buckets[0].Label != "User 1" {
		t.Fatalf("User 1's carrying = %+v", carrying.Buckets)
	}
	epic := mustEvaluate(t, e, teamBoard().board(), Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC", Epic: "e1"})
	if epic.Epic == nil || epic.Epic.Label != "WEB-100 Checkout" || num(widgetOf(t, epic, "stuck").Value) != "1" || num(widgetOf(t, epic, "pace").Value) != "0" {
		t.Fatalf("epic WEB-100: %+v, stuck %s, pace %s", epic.Epic, num(widgetOf(t, epic, "stuck").Value), num(widgetOf(t, epic, "pace").Value))
	}
	_, err := e.Evaluate(context.Background(), teamBoard().board(), Request{Dashboard: builtinSprintsID, TimeZone: "UTC", Mine: true, Person: "u2"})
	var r *Refusal
	if !errors.As(err, &r) || r.Code != "dashboard_request_invalid" {
		t.Fatalf("Mine and a person = %v; want a refusal", err)
	}
}

// A drill answers the number of its cell with the rows behind it, from
// the same evaluation: the widget's number, a bar's sum, or a bar's
// series. Rows the metric does not count are listed after those it does.
func TestDrillAnswersTheCellsNumberWithItsRows(t *testing.T) {
	t.Parallel()
	e := evaluator()
	b := teamBoard().board()
	a := mustEvaluate(t, e, b, utcSprints3)
	drill := func(widget string, cell *DrillCell) DrillAnswer {
		t.Helper()
		d, err := e.Drill(context.Background(), b, DrillRequest{Request: utcSprints3, Widget: widget, Cell: cell})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	pace := drill("pace", nil)
	if num(&pace.Value) != num(widgetOf(t, a, "pace").Value) || pace.Count != 3 {
		t.Fatalf("pace drill = %s with %d rows; the widget shows %s over 3 tickets", num(&pace.Value), pace.Count, num(widgetOf(t, a, "pace").Value))
	}
	bar := drill("finished", &DrillCell{Bucket: "sprint:s4"})
	if num(&bar.Value) != "8" || len(bar.Rows) != 1 || bar.Rows[0].Key != "WEB-6" || bar.Title != "Finished per sprint, WEB Sprint 4" {
		t.Fatalf("sprint 4's bar = %+v", bar)
	}
	cell := drill("carrying", &DrillCell{Bucket: "u1", Series: "To do"})
	if num(&cell.Value) != "3" || len(cell.Rows) != 1 || cell.Rows[0].Key != "WEB-9" {
		t.Fatalf("User 1's to do = %+v", cell)
	}
	stuck := drill("stuck_list", nil)
	if stuck.Count != 2 || stuck.Counted != 1 || stuck.Rows[0].Key != "WEB-3" || stuck.Rows[1].Counted || stuck.Rows[1].Note != "how long it has been in In Progress is not known" {
		t.Fatalf("stuck drill = %+v", stuck)
	}
}

// A drill's rows read as the metric counts them: a count's rows carry no
// value, since the value is not what it counts (the note says it), and a
// ticket finished in two sprints is two rows, each with its sprint and a
// key of its own, of one ticket.
func TestDrillRowsReadAsTheMetricCounts(t *testing.T) {
	t.Parallel()
	f := teamBoard().member("s6", "t2", true)
	e := evaluator()
	b := f.board()
	stuck, err := e.Drill(context.Background(), b, DrillRequest{Request: utcSprints3, Widget: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	if r := stuck.Rows[0]; r.Key != "WEB-3" || r.Value != nil || r.Note != "in In Progress for 5 days" || r.Title != "Title of WEB-3" || r.Status != "In Progress" || r.Points == nil || *r.Points != 2 {
		t.Fatalf("stuck's first row = %+v; want WEB-3, its title, status and points, with its days in the note and no value", r)
	}
	pace, err := e.Drill(context.Background(), b, DrillRequest{Request: utcSprints3, Widget: "pace"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range pace.Rows {
		rows = append(rows, fmt.Sprintf("%d %s %s %g", r.Row, r.Key, r.Period, *r.Value))
	}
	want := []string{"0 WEB-6 WEB Sprint 4 8", "1 WEB-2 WEB Sprint 5 5", "2 WEB-2 WEB Sprint 6 5", "3 WEB-1 WEB Sprint 6 3"}
	if !reflect.DeepEqual(rows, want) || pace.Count != 4 || pace.Tickets != 3 {
		t.Fatalf("pace drill = %v, %d rows of %d tickets; want %v, 4 rows of 3 tickets", rows, pace.Count, pace.Tickets, want)
	}
}

// A cell whose bucket key is empty, Unassigned or No epic, opens its own
// rows and its series alone, never the whole widget's; a drill's target
// is checked against its widget's form: a cell of a chart or table, the
// rest of one that ranks its buckets, or the whole widget.
func TestEmptyKeyCellsOpenTheirOwnRows(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	f.ticket(ticket{id: "t11", key: "WEB-11", status: "1", group: "todo", points: "3", created: ago(5)}).member("s7", "t11", false)
	f.ticket(ticket{id: "t12", key: "WEB-12", status: "3", group: "in_progress", points: "2", created: ago(5)}).member("s7", "t12", false)
	e := evaluator()
	b := f.board()
	drill := func(r DrillRequest) (DrillAnswer, error) {
		r.Request = utcSprints3
		return e.Drill(context.Background(), b, r)
	}
	d, err := drill(DrillRequest{Widget: "carrying", Cell: &DrillCell{Bucket: "", Series: "To do"}})
	if err != nil || num(&d.Value) != "3" || len(d.Rows) != 1 || d.Rows[0].Key != "WEB-11" || d.Title != "Who is carrying what, Unassigned, To do" {
		t.Fatalf("Unassigned / To do = %+v, %v; want WEB-11 alone, 3", d, err)
	}
	d, err = drill(DrillRequest{Widget: "carrying", Cell: &DrillCell{Bucket: ""}})
	if err != nil || num(&d.Value) != "5" || d.Count != 2 {
		t.Fatalf("Unassigned's total = %+v, %v; want WEB-11 and WEB-12, 5", d, err)
	}
	d, err = drill(DrillRequest{Widget: "open_by_epic", Cell: &DrillCell{Bucket: ""}})
	if err != nil || d.Title != "Open work by epic, No epic" {
		t.Fatalf("No epic = %+v, %v", d, err)
	}
	for _, k := range d.Rows {
		if k.Key == "WEB-3" || k.Key == "WEB-9" {
			t.Errorf("No epic lists %s, which is in epic WEB-100", k.Key)
		}
	}
	for _, c := range []struct {
		r    DrillRequest
		want string
	}{
		{DrillRequest{Widget: "pace", Cell: &DrillCell{Bucket: "sprint:s4"}}, `widget "pace" is one number`},
		{DrillRequest{Widget: "stuck_list", Cell: &DrillCell{Bucket: ""}}, `widget "stuck_list" is one number`},
		{DrillRequest{Widget: "carrying", Cell: &DrillCell{Bucket: "u1", Series: "Blocked"}}, `widget "carrying" has no series "Blocked"`},
		{DrillRequest{Widget: "carrying", Cell: &DrillCell{Bucket: "u1"}, Rest: true}, `names one cell or the rest, not both`},
		{DrillRequest{Widget: "pace", Rest: true}, `widget "pace" shows all it counts`},
		{DrillRequest{Widget: "stuck_list", Rest: true}, `widget "stuck_list" shows all it counts`},
		{DrillRequest{Widget: "finished", Rest: true}, `widget "finished" shows all it counts`},
	} {
		if _, err := drill(c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v; want %q", c.r, err, c.want)
		}
	}
}

// A page after the first that finds a different count than the page
// before it starts again at the first page with the new number, and so
// does a page past the last; a page whose count held stays where it was
// asked for.
func TestDrillStartsAgainWhenTheCountMoved(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	for i := range 150 {
		id := fmt.Sprintf("m%d", i)
		f.ticket(ticket{id: id, key: fmt.Sprintf("WEB-%d", 1000+i), status: "1", group: "todo", assignee: "u1", points: "1", created: ago(20)}).member("s7", id, false)
	}
	e := evaluator()
	b := f.board()
	ask := DrillRequest{Request: utcSprints3, Widget: "carrying", Cell: &DrillCell{Bucket: "u1"}}
	first, err := e.Drill(context.Background(), b, ask)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages != 2 {
		t.Fatalf("drill of u1's active tickets = %d pages of %d rows; want 2", first.Pages, first.Count)
	}
	for _, c := range []struct {
		name        string
		page, count int
		wantPage    int
		restarted   bool
	}{
		{"the count held", 1, first.Count, 1, false},
		{"the count moved", 1, first.Count + 1, 0, true},
		{"past the last page", 2, first.Count, 0, true},
	} {
		ask.Page, ask.Count = c.page, c.count
		d, err := e.Drill(context.Background(), b, ask)
		if err != nil {
			t.Fatal(err)
		}
		if d.Restarted != c.restarted || d.Page != c.wantPage || d.Count != first.Count {
			t.Errorf("%s: page %d restarted %v of %d rows; want page %d restarted %v of %d", c.name, d.Page, d.Restarted, d.Count, c.wantPage, c.restarted, first.Count)
		}
	}
}

// Every kind of board offers its built-in dashboard, every metric valid
// and prepared, every widget drawable: the epic widget only where the
// tracker has epics, the ranges of the board's kind, and its pace read
// from credit where its project counts credit.
func TestEveryKindOffersItsBuiltInDashboard(t *testing.T) {
	t.Parallel()
	var kinds []Kind
	for _, sprints := range []bool{true, false} {
		for _, epics := range []bool{true, false} {
			for _, credit := range []bool{true, false} {
				kinds = append(kinds, Kind{PlansSprints: sprints, Epics: epics, Credit: credit})
			}
		}
	}
	for _, k := range kinds {
		list, err := evaluator().Dashboards(context.Background(), k, store.MirrorEstimatePoints, Own{})
		if err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
		v := list.Dashboards[0]
		width, epics := 0, false
		for _, w := range v.Widgets {
			width += w.Width
			epics = epics || w.ID == "open_by_epic"
			if len(w.How.Reads) == 0 || w.How.SQL == "" || w.How.Counts == "" {
				t.Errorf("%+v %s: How = %+v", k, w.ID, w.How)
			}
		}
		if width%12 != 0 || epics != k.Epics || (v.DefaultRange == RangeSprints5) != k.PlansSprints {
			t.Errorf("%+v: widths add to %d, epics %v, default range %s", k, width, epics, v.DefaultRange)
		}
		if pace := v.Widgets[0]; slices.Contains(pace.How.Reads, "credit") != k.Credit || (len(pace.Series) == 3) != k.Credit {
			t.Errorf("%+v: the pace reads %v, series %v", k, pace.How.Reads, pace.Series)
		}
	}
}

// A board without sprints counts by week: what was finished each week by
// its last move into done (else its resolution), the current week to now,
// tickets counted where the board has no estimates.
func TestWeekBoardCountsByWeek(t *testing.T) {
	t.Parallel()
	f := newFixture(false, "none").person("u1", "User 1").
		status("1", "To Do", "todo").status("3", "In Progress", "in_progress").status("5", "Done", "done")
	// testNow is Wednesday 25 November: this week from Monday the 23rd.
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", created: ago(30), historyFrom: ago(30), started: ago(4), done: ago(1)})
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", created: ago(30), res: ago(8)})
	f.ticket(ticket{id: "t3", key: "WEB-3", status: "5", group: "done", created: ago(30), historyFrom: ago(30), started: ago(10), done: ago(9)})
	f.ticket(ticket{id: "t4", key: "WEB-4", status: "5", group: "done", created: ago(30), done: ago(20)})
	f.ticket(ticket{id: "t5", key: "WEB-5", status: "3", group: "in_progress", assignee: "u1", created: ago(3)})
	a := mustEvaluate(t, evaluator(), f.board(), Request{Dashboard: builtinWeeksID, Range: RangeWeeks2, TimeZone: "UTC"})
	if len(a.Range.Periods) != 2 || a.Range.Periods[0].ID != "week:2026-11-16" || !a.Range.Periods[1].Partial || a.Estimates != "none" {
		t.Fatalf("range = %+v, estimates %s", a.Range, a.Estimates)
	}
	pace := widgetOf(t, a, "pace")
	if num(pace.Value) != "1.5" || num(pace.Previous) != "0.5" {
		t.Fatalf("weekly pace = %s against %s; want (2 + 1) / 2 against (0 + 1) / 2", num(pace.Value), num(pace.Previous))
	}
	cf := widgetOf(t, a, "created_finished")
	var cells []string
	for _, b := range cf.Buckets {
		cells = append(cells, b.Label+" "+num(&b.Cells[0])+" "+num(&b.Cells[1]))
	}
	if !reflect.DeepEqual(cells, []string{"Nov 16 1 2", "Nov 23 0 1"}) {
		t.Fatalf("created and finished = %v; want WEB-5 created last week (Sunday), WEB-2 and WEB-3 finished then, WEB-1 this week", cells)
	}
	carrying := widgetOf(t, a, "carrying")
	if !reflect.DeepEqual(carrying.Series, []string{"To do", "Doing"}) || len(carrying.Buckets) != 1 || num(&carrying.Buckets[0].Cells[1]) != "1" {
		t.Fatalf("carrying = %v %+v", carrying.Series, carrying.Buckets)
	}
}

// A done ticket whose finish time is not known finished after its
// creation and at or before its last update. It belongs to each range
// that span meets, is counted in none, and is listed by the range on
// every widget: the numbers, their drills, and a chart's "not counted",
// which opens the whole widget. testNow is Wednesday 25 November; the
// last 2 weeks run from Monday the 16th, the 2 before from the 2nd.
//
//	WEB-1 finished yesterday: counted this range
//	WEB-6 made 26 October, updated 22 November: either range
//	WEB-7 updated two months ago: neither, and not read
//	WEB-8 made in September, updated 8 November: the range before only
//	WEB-9 made 20 November, updated 23 November: this range only
//	WEB-10 made in a done status 21 November, never updated: this range
func TestAnUnknownFinishBelongsToTheRangesItsTicketSpans(t *testing.T) {
	t.Parallel()
	f := newFixture(false, "none").status("5", "Done", "done")
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", created: ago(30), historyFrom: ago(30), started: ago(4), done: ago(1)})
	f.ticket(ticket{id: "t6", key: "WEB-6", status: "5", group: "done", created: ago(30), updated: ago(3)})
	f.ticket(ticket{id: "t7", key: "WEB-7", status: "5", group: "done", created: ago(90), updated: ago(60)})
	f.ticket(ticket{id: "t8", key: "WEB-8", status: "5", group: "done", created: ago(70), updated: ago(17)})
	f.ticket(ticket{id: "t9", key: "WEB-9", status: "5", group: "done", created: ago(5), updated: ago(2)})
	f.ticket(ticket{id: "t10", key: "WEB-10", status: "5", group: "done", created: ago(4), updated: ago(4)})
	e := evaluator()
	r := Request{Dashboard: builtinWeeksID, Range: RangeWeeks2, TimeZone: "UTC"}
	a := mustEvaluate(t, e, f.board(), r)
	pace := widgetOf(t, a, "pace")
	if got := num(pace.Value); got != "0.5, 3 not counted" {
		t.Errorf("weekly pace = %s; want WEB-1 over 2 weeks, WEB-6, WEB-9 and WEB-10 not counted (WEB-8 was last updated before the range)", got)
	}
	if got := num(pace.Previous); got != "not known, 2 not counted" {
		t.Errorf("the pace before = %s; want WEB-6 and WEB-8 not counted, which could have finished then", got)
	}
	cycle := widgetOf(t, a, "start_to_done")
	if got := num(cycle.Value) + " / " + num(cycle.Previous); got != "1.5, 3 not counted / not known, 2 not counted" {
		t.Errorf("start to done now / before = %s; want WEB-1's 1.5 working days with WEB-6, WEB-9 and WEB-10 not counted, before WEB-6 and WEB-8", got)
	}
	cf := widgetOf(t, a, "created_finished")
	var finished []string
	for _, b := range cf.Buckets {
		finished = append(finished, num(&b.Cells[1]))
	}
	if !reflect.DeepEqual(finished, []string{"0", "1"}) || cf.NotCounted != 3 {
		t.Errorf("finished by week = %v, %d not counted; want WEB-1 alone, this week, with WEB-6, WEB-9 and WEB-10 in no bar and not counted", finished, cf.NotCounted)
	}
	for _, id := range []string{"finished", "start_to_done_line"} {
		if w := widgetOf(t, a, id); w.NotCounted != 3 {
			t.Errorf("%s lists %d not counted; want WEB-6, WEB-9 and WEB-10", id, w.NotCounted)
		}
	}
	rows := func(widget string) []string {
		t.Helper()
		d, err := e.Drill(context.Background(), f.board(), DrillRequest{Request: r, Widget: widget})
		if err != nil {
			t.Fatalf("%s drill: %v", widget, err)
		}
		var out []string
		for _, row := range d.Rows {
			out = append(out, fmt.Sprintf("%s %v %s", row.Key, row.Counted, row.Note))
		}
		return append(out, num(&d.Value))
	}
	if got, want := rows("pace"), []string{"WEB-1 true ", "WEB-6 false when it finished is not known", "WEB-9 false when it finished is not known", "WEB-10 false when it finished is not known", "0.5, 3 not counted"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pace drill = %q; want %q", got, want)
	}
	// The chart's "3 not counted" opens the whole widget: WEB-9's and
	// WEB-10's creations are counted and their finishes are not.
	if got, want := rows("created_finished"), []string{"WEB-1 true ", "WEB-9 true ", "WEB-10 true ", "WEB-6 false when it finished is not known",
		"WEB-9 false when it finished is not known", "WEB-10 false when it finished is not known", "3, 3 not counted"}; !reflect.DeepEqual(got, want) {
		t.Errorf("created and finished drill = %q; want %q", got, want)
	}
}

// A metric of the range reads only the rows that can fall in it: a board
// whose 25,000 tickets were made and finished before its range counts
// its range, where reading every ticket would pass the row cap.
func TestRangeMetricsReadOnlyTheRange(t *testing.T) {
	t.Parallel()
	f := teamBoard().exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 25000)
		INSERT INTO tickets(id, key, title, type_level, status, status_group, created_at, updated_at, resolved_at, status_since, points, done_at, history_behind, started_known, open, counts)
		SELECT 'old' || i, 'OLD-' || i, 'Old', 0, '5', 'done', %d, 0, %d, 0, 1, %d, 0, 0, 0, 1 FROM n`, ago(400), ago(390), ago(390))
	a := mustEvaluate(t, evaluator(), f.board(), utcSprints3)
	for _, id := range []string{"created_finished", "start_to_done"} {
		if w := widgetOf(t, a, id); w.Error != "" {
			t.Fatalf("%s failed over a board of old tickets: %s", id, w.Error)
		}
	}
	w := mustEvaluate(t, evaluator(), newFixture(false, "none").status("5", "Done", "done").exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 25000)
		INSERT INTO tickets(id, key, title, type_level, status, status_group, created_at, updated_at, resolved_at, status_since, done_at, history_behind, started_known, open, counts)
		SELECT 'old' || i, 'OLD-' || i, 'Old', 0, '5', 'done', %d, 0, %d, 0, %d, 0, 0, 0, 1 FROM n`, ago(400), ago(390), ago(390)).board(),
		Request{Dashboard: builtinWeeksID, Range: RangeWeeks12, TimeZone: "UTC"})
	for _, id := range []string{"pace", "created_finished", "start_to_done"} {
		if v := widgetOf(t, w, id); v.Error != "" {
			t.Fatalf("week board %s failed over old tickets: %s", id, v.Error)
		}
	}
}

// A table of more people than it shows makes the cells of the people it
// shows only: 19,900 people in the active sprint give 50 rows of three
// cells, the rest counted in More, with no cell made for the others. The
// count of allocations is the process's, so the test runs alone: not in
// parallel with the package's others.
func TestATableMakesTheCellsItShows(t *testing.T) {
	f := teamBoard().exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 19900)
		INSERT INTO tickets(id, key, title, type_level, status, status_group, assignee, created_at, updated_at, status_since, points, history_behind, started_known, open, counts)
		SELECT 'p' || i, 'WEB-' || (5000 + i), 'T', 0, '1', 'todo', 'person' || i, %d, 0, 0, 1, 0, 0, 1, 1 FROM n`, ago(5)).
		exec(`INSERT INTO sprint_tickets SELECT 's7', id FROM tickets WHERE id LIKE 'p%%'`)
	e := evaluator()
	o, err := e.begin(context.Background(), f.board(), utcSprints3, func(Widget, bool) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := o.end(); err != nil {
			t.Error(err)
		}
	}()
	m := o.metrics["carrying"]
	c, err := o.count(m, reads{now: true})
	if err != nil || c.err != nil {
		t.Fatalf("carrying: %v %v", err, c.err)
	}
	w := Widget{ID: "carrying", Metric: m.Name, Form: FormTable, Width: 4}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	v := o.ev.widget(w, m, c, o.sp)
	runtime.ReadMemStats(&after)
	if len(v.Buckets) != maxTableRows || v.More != 19902-maxTableRows || len(v.Buckets[0].Cells) != 3 {
		t.Fatalf("table = %d rows of %d cells, %d more", len(v.Buckets), len(v.Buckets[0].Cells), v.More)
	}
	// Totalling 19,902 people costs a few allocations each; their 59,706
	// cells would cost more than twice that.
	if mallocs := after.Mallocs - before.Mallocs; mallocs > 120_000 {
		t.Errorf("the table allocated %d times; its shown cells are %d", mallocs, maxTableRows*3)
	}
}

// More opens the rows of the buckets a widget left out, ranked once for
// the widget and its drill: 50 people in the active sprint with one
// point each, beside User 1's 5 and User 2's 3, tie across the table's
// cut, their names in the reverse of their rows' order, and the drill
// lists exactly the two people the table does not show. The whole table,
// which its "not counted" opens, lists every row.
func TestMoreOpensTheBucketsTheWidgetLeftOut(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	for i := 1; i <= 50; i++ {
		f.person(fmt.Sprintf("p%02d", i), fmt.Sprintf("User %d", 200-i))
		f.ticket(ticket{id: fmt.Sprintf("tp%02d", i), key: fmt.Sprintf("WEB-%d", 500+i), status: "1", group: "todo", assignee: fmt.Sprintf("p%02d", i), points: "1", created: ago(5)}).
			member("s7", fmt.Sprintf("tp%02d", i), false)
	}
	e := evaluator()
	b := f.board()
	table := widgetOf(t, mustEvaluate(t, e, b, utcSprints3), "carrying")
	shown := map[string]bool{}
	for _, bk := range table.Buckets {
		shown[bk.Key] = true
	}
	if len(table.Buckets) != maxTableRows || table.More != 2 || num(&table.Buckets[maxTableRows-1].Total) != "1" {
		t.Fatalf("table = %d rows, %d more, the last %s; want 50 shown and 2 more, tied at 1 point across the cut", len(table.Buckets), table.More, num(&table.Buckets[maxTableRows-1].Total))
	}
	var left []string
	for i := 1; i <= 50; i++ {
		if !shown[fmt.Sprintf("p%02d", i)] {
			left = append(left, fmt.Sprintf("WEB-%d User %d", 500+i, 200-i))
		}
	}
	d, err := e.Drill(context.Background(), b, DrillRequest{Request: utcSprints3, Widget: "carrying", Rest: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range d.Rows {
		got = append(got, row.Key+" "+row.Assignee)
	}
	if !reflect.DeepEqual(got, left) || num(&d.Value) != "2" || d.Title != "Who is carrying what, 2 more people" {
		t.Fatalf("the rest = %q, %s, %q; want the people the table left out, %q, 2 points", got, num(&d.Value), d.Title, left)
	}
	whole, err := e.Drill(context.Background(), b, DrillRequest{Request: utcSprints3, Widget: "carrying"})
	if err != nil || whole.Count != 54 || num(&whole.Value) != "58" || whole.Title != "Who is carrying what" {
		t.Fatalf("the whole table = %+v, %v; want its 54 rows, 58 points", whole, err)
	}
}
