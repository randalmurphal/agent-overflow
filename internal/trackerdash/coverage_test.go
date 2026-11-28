package trackerdash

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// drillRows is a drill's rows as "key counted" or "key not counted: note".
func drillRows(d DrillAnswer) []string {
	var rows []string
	for _, r := range d.Rows {
		rows = append(rows, r.Key+" "+map[bool]string{true: "counted", false: "not counted: " + r.Note}[r.Counted])
	}
	return rows
}

func mustDrill(t *testing.T, b Board, r DrillRequest) DrillAnswer {
	t.Helper()
	d, err := evaluator().Drill(context.Background(), b, r)
	if err != nil {
		t.Fatalf("drill %+v: %v", r, err)
	}
	return d
}

// A ticket of the active sprint is not counted on the burndown where its
// sprint history is not known, and only there: one made after the start
// whose history is not read from its creation, not at the start; one
// changed since its history was read, over the whole sprint, saying its
// latest change is not read yet.
func TestTheBurndownListsATicketWhereItsSprintHistoryIsNotKnown(t *testing.T) {
	t.Parallel()
	f := burnBoard("points")
	f.ticket(ticket{id: "J", key: "WEB-J", status: "1", group: "todo", points: "1", created: utc(11, 24, 11), updated: ago(0.5)}).member("b1", "J", false)
	f.ticket(ticket{id: "K", key: "WEB-K", status: "1", group: "todo", points: "2", created: ago(30), updated: ago(0.5), since: ago(30), historyFrom: ago(30), behind: true}).
		member("b1", "K", false)
	b := f.board()
	r := Request{Dashboard: builtinSprintsID, TimeZone: "UTC"}
	start := mustDrill(t, b, DrillRequest{Request: r, Widget: "burndown", Cell: &DrillCell{Bucket: "day:start"}})
	if want := []string{"WEB-B counted", "WEB-A counted", "WEB-D counted", "WEB-F counted", "WEB-G counted", "WEB-H counted",
		"WEB-E not counted: its history is not read yet", "WEB-K not counted: its latest change is not read yet"}; !reflect.DeepEqual(drillRows(start), want) {
		t.Fatalf("Start's drill = %v; want %v", drillRows(start), want)
	}
	nov24 := mustDrill(t, b, DrillRequest{Request: r, Widget: "burndown", Cell: &DrillCell{Bucket: "day:2026-11-24"}})
	if want := []string{"WEB-D counted", "WEB-A counted", "WEB-C counted", "WEB-I counted", "WEB-E not counted: its history is not read yet",
		"WEB-F not counted: its status then is not known", "WEB-J not counted: its history is not read yet",
		"WEB-K not counted: its latest change is not read yet"}; !reflect.DeepEqual(drillRows(nov24), want) {
		t.Fatalf("Nov 24's drill = %v; want %v", drillRows(nov24), want)
	}
}

// Each cell of the sprint table counts a ticket where its evidence for
// that cell is: a ticket made after the start whose history begins
// minutes after its creation (Linear's fold) and that entered later is
// added; a removed ticket whose history begins after the start is removed
// and not counted in committed and added; a ticket changed since its
// history was read is not counted in committed and added; a member whose
// status at the completion is not known is not counted in completed.
func TestTheSprintScopeCountsEachCellWhereItsEvidenceIs(t *testing.T) {
	t.Parallel()
	f := scopeBoard()
	fold := int64(3 * 60 * 1000)
	f.ticket(ticket{id: "Y", key: "WEB-Y", status: "1", group: "todo", points: "2", created: ago(12), historyFrom: ago(12) + fold}).
		stretch("c1", "Y", "2", ago(10), 0).member("c1", "Y", false)
	f.ticket(ticket{id: "Z", key: "WEB-Z", status: "1", group: "todo", points: "4", created: ago(40), historyFrom: ago(15)}).
		stretch("c1", "Z", "4", ago(15), ago(8))
	f.ticket(ticket{id: "B2", key: "WEB-B2", status: "1", group: "todo", points: "1", created: ago(40), historyFrom: ago(40), behind: true}).member("c1", "B2", false)
	f.ticket(ticket{id: "U2", key: "WEB-U2", status: "1", group: "todo", points: "1", created: ago(40), historyFrom: ago(40)}).unsettled("c1", "U2")
	r := Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC"}
	b := f.board()
	w := widgetOf(t, mustEvaluate(t, evaluator(), b, r), "sprint_scope")
	var got []string
	for _, c := range w.Buckets[0].Cells {
		got = append(got, num(&c))
	}
	// Committed P, R and X at 2; added Q, T at 4, V and Y; removed Q, R
	// and Z; completed P.
	if want := []string{"6, 3 not counted", "9, 3 not counted", "7", "3, 1 not counted"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WEB Sprint 1 = %v; want %v", got, want)
	}
	for series, want := range map[string][]string{
		"Added": {"WEB-T counted", "WEB-Q counted", "WEB-Y counted", "WEB-V counted", "WEB-B2 not counted: its latest change is not read yet",
			"WEB-S not counted: its history begins after the sprint started", "WEB-Z not counted: its history begins after the sprint started"},
		"Removed":   {"WEB-Z counted", "WEB-Q counted", "WEB-R counted"},
		"Completed": {"WEB-P counted", "WEB-U2 not counted: its status when the sprint ended is not known"},
	} {
		d := mustDrill(t, b, DrillRequest{Request: r, Widget: "sprint_scope", Cell: &DrillCell{Bucket: "sprint:c1", Series: series}})
		if !reflect.DeepEqual(drillRows(d), want) {
			t.Errorf("%s in c1 = %v; want %v", series, drillRows(d), want)
		}
	}
}

// A stay whose start is not known (its history begins in it) is listed
// and not counted on Stuck, saying how long it has been in its status is
// not known, as a ticket whose status time is not known always was.
func TestStuckDoesNotCountAStayWhoseStartIsNotKnown(t *testing.T) {
	t.Parallel()
	f := newFixture(false, "points").status("3", "In Progress", "in_progress").
		ticket(ticket{id: "L", key: "WEB-L", status: "3", group: "in_progress", created: ago(30), updated: ago(1), historyFrom: ago(30) + 180_000}).
		covered("L", "3", ago(30)+180_000, 0, 0)
	a := mustEvaluate(t, evaluator(), f.board(), Request{Dashboard: builtinWeeksID, TimeZone: "UTC"})
	if got := num(widgetOf(t, a, "stuck").Value); got != "not known, 1 not counted" {
		t.Fatalf("stuck = %s", got)
	}
	d := mustDrill(t, f.board(), DrillRequest{Request: Request{Dashboard: builtinWeeksID, TimeZone: "UTC"}, Widget: "stuck"})
	if want := []string{"WEB-L not counted: how long it has been in In Progress is not known"}; !reflect.DeepEqual(drillRows(d), want) {
		t.Fatalf("stuck's drill = %v", drillRows(d))
	}
}

// stuckBoard is a board without sprints of n tickets, each in To Do from
// 60 days ago but for its stays in progress, from each entry's from until
// its to, the last one open when its to is 0.
func stuckBoard(n int, stays ...[2]int64) *fixture {
	f := newFixture(false, "points").status("1", "To Do", "todo").status("3", "In Progress", "in_progress")
	var rows []string
	stay := func(status string, from, to int64) {
		rows = append(rows, fmt.Sprintf("SELECT 'k' || i, '%s', %d, %s, %d, 1 FROM n", status, from, nullable(to), from))
	}
	prev := ago(60)
	for _, s := range stays {
		stay("1", prev, s[0])
		stay("3", s[0], s[1])
		prev = s[1]
	}
	status, group, since := "3", "in_progress", stays[len(stays)-1][0]
	if prev != 0 {
		stay("1", prev, 0)
		status, group, since = "1", "todo", prev
	}
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		INSERT INTO tickets(id, key, title, type_level, status, status_group, created_at, updated_at, status_since, history_from, history_behind,
		  started_known, open, counts)
		SELECT 'k' || i, 'WEB-' || i, 'T', 0, '%s', '%s', %d, %d, %d, %d, 0, 1, 1, 1 FROM n`, n, status, group, ago(60), ago(1), since, ago(60))
	for _, r := range rows {
		f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
			INSERT INTO status_intervals(ticket, status, from_at, to_at, began, open) %s`, n, r)
	}
	return f
}

// Stuck's row limit counts the stays that hold where its widgets read
// them: 10,001 tickets stuck now, each with two stuck stays in the range
// before that hold at none of the number's points, count; 20,001 stuck at
// the ends of past weeks and none now fail the number and the list that
// share its run, with a sentence that says where the rows hold, and a
// drill of the list, which reads now alone, counts.
func TestStucksRowLimitCountsTheStaysItsWidgetsRead(t *testing.T) {
	t.Parallel()
	r := Request{Dashboard: builtinWeeksID, TimeZone: "UTC"}
	// Monday 12 and 19 October to the Friday after: stuck from Thursday to
	// Friday, between the previous range's week ends.
	day := func(d int) int64 { return utc(10, d, 0) }
	b := stuckBoard(10_001, [2]int64{day(12), day(16)}, [2]int64{day(19), day(23)}, [2]int64{ago(10), 0}).board()
	a := mustEvaluate(t, evaluator(), b, r)
	if w := widgetOf(t, a, "stuck"); w.Error != "" || num(w.Value) != "10001" {
		t.Fatalf("stuck = %s, %q", num(w.Value), w.Error)
	}
	if w := widgetOf(t, a, "stuck_list"); w.Error != "" || w.More+len(w.Tickets) != 10_001 {
		t.Fatalf("stuck list = %d tickets and %d more, %q", len(w.Tickets), w.More, w.Error)
	}
	// In progress from 15 days ago to 3 days ago: stuck at the end of the
	// weeks of 9 and 16 November, to do now.
	past := stuckBoard(20_001, [2]int64{ago(15), ago(3)}).board()
	a = mustEvaluate(t, evaluator(), past, r)
	want := "This widget's query returned more than 20000 rows that hold now, where the range begins, or at the end of a period, its limit. Pick a shorter range."
	for _, id := range []string{"stuck", "stuck_list"} {
		if w := widgetOf(t, a, id); w.Error != want {
			t.Fatalf("%s's error = %q; want %q", id, w.Error, want)
		}
	}
	d := mustDrill(t, past, DrillRequest{Request: r, Widget: "stuck_list"})
	if d.Count != 0 {
		t.Fatalf("the list's drill = %d rows; want none stuck now", d.Count)
	}
}

// A day still to come has no value and opens no tickets, whatever reads
// it: the trend of a number of states by day, and a line of events dated
// on the sprint's last day, its cell and its drill.
func TestADayStillToComeHasNoValueOrRows(t *testing.T) {
	t.Parallel()
	states := Metric{Name: "w_1", Title: "Statuses", Words: []string{"Statuses by day."}, SQL: `SELECT ticket, from_at AS at, to_at AS until FROM status_intervals`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketDay}
	events := Metric{Name: "w_2", Title: "Due", Words: []string{"Due by day."},
		SQL:    `SELECT m.ticket, s.end_at AS at FROM sprint_tickets m JOIN sprints s ON s.id = m.sprint WHERE s.state = 'active'`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketDay}
	b := burnBoard("points").board()
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{
		{ID: "w_1", Metric: "w_1", Form: FormNumber, Width: 3}, {ID: "w_2", Metric: "w_2", Form: FormLine, Width: 6}}}},
		Metrics: map[string]Metric{"w_1": states, "w_2": events}}
	r := Request{Dashboard: "mine", TimeZone: "UTC"}
	a := mustEvaluate(t, evaluator(), b, r)
	number := widgetOf(t, a, "w_1")
	var trend []string
	for _, n := range number.Trend {
		trend = append(trend, num(&n))
	}
	if strings.Join(trend[4:], " ") != "none none none none none none none" {
		t.Fatalf("the number's trend = %v; want none from Nov 26", trend)
	}
	line := widgetOf(t, a, "w_2")
	if got := days(line)[len(line.Buckets)-1]; got != "Dec 4 - |" {
		t.Fatalf("the line's last day = %q", got)
	}
	d := mustDrill(t, b, DrillRequest{Request: r, Widget: "w_2", Cell: &DrillCell{Bucket: "day:2026-12-04"}})
	if d.Count != 0 || d.Value.V != nil {
		t.Fatalf("Dec 4's drill = %s with %d rows; want none", num(&d.Value), d.Count)
	}
}

// The burndown's line draws an even pace and the sprint table adds no
// total, as their metrics say; a day line an agent writes draws none.
func TestOnlyTheBurndownDrawsAPaceAndOnlyTheSprintTableHasNoTotal(t *testing.T) {
	t.Parallel()
	list, err := evaluator().Dashboards(context.Background(), Kind{PlansSprints: true}, store.MirrorEstimatePoints, Own{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list.Dashboards[0].Widgets {
		if w.Pace != (w.ID == "burndown") || w.Overlap != (w.ID == "sprint_scope") {
			t.Errorf("widget %s: pace %v, overlap %v", w.ID, w.Pace, w.Overlap)
		}
	}
	m := Metric{Name: "w_1", Title: "Made", Words: []string{"Made by day."}, SQL: `SELECT id AS ticket, created_at AS at FROM tickets`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketDay}
	tried, err := evaluator().Try(context.Background(), burnBoard("points").board(), Widget{Form: FormLine, Width: 6}, m, "UTC")
	if err != nil || tried.View.Pace || tried.View.Overlap {
		t.Fatalf("an agent's day line: %+v, %v", tried.View, err)
	}
}

// On a Saturday the sprint has no day so far: Friday is the last day
// counted, and Monday the next one to come.
func TestASaturdayIsNoSprintDay(t *testing.T) {
	t.Parallel()
	saturday := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	ps, _, note := sprintDays([]sprintRow{{id: "b1", name: "WEB Sprint 9", state: "active", startAt: utc(11, 23, 9), endAt: utc(12, 4, 17)}}, saturday, time.UTC)
	var got []string
	for _, p := range ps {
		l := p.label
		switch {
		case p.partial:
			l += " so far"
		case p.future:
			l += " to come"
		}
		got = append(got, l)
	}
	if want := "Start Nov 23 Nov 24 Nov 25 Nov 26 Nov 27 Nov 30 to come Dec 1 to come Dec 2 to come Dec 3 to come Dec 4 to come"; note != "" || strings.Join(got, " ") != want {
		t.Fatalf("days on a Saturday = %q, %q; want %q", strings.Join(got, " "), note, want)
	}
}

// A state grouped by sprint counts in a sprint's column only the rows of
// that sprint that hold at its end: U, in WEB Sprint 2 from before WEB
// Sprint 1 ended, counts in Sprint 2 alone, and Sprint 1's tickets still
// in it at Sprint 2's end count in Sprint 1 alone, in the widget and in
// its drill.
func TestAStateBySprintCountsInItsOwnSprintsOnly(t *testing.T) {
	t.Parallel()
	m := Metric{Name: "w_1", Title: "In each sprint at its end", Words: []string{"The tickets in each sprint at its end."},
		SQL:    `SELECT ticket, sprint AS bucket, from_at AS at, to_at AS until FROM sprint_intervals`,
		Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketSprint}
	b := scopeBoard().board()
	b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: FormTable, Width: 6}}}},
		Metrics: map[string]Metric{"w_1": m}}
	r := Request{Dashboard: "mine", Range: RangeSprints3, TimeZone: "UTC"}
	var got []string
	for _, c := range widgetOf(t, mustEvaluate(t, evaluator(), b, r), "w_1").Buckets {
		got = append(got, c.Label+" "+num(&c.Cells[0]))
	}
	if want := []string{"WEB Sprint 1 5", "WEB Sprint 2 1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the sprints = %v; want %v", got, want)
	}
	d := mustDrill(t, b, DrillRequest{Request: r, Widget: "w_1", Cell: &DrillCell{Bucket: "sprint:c2"}})
	if want := []string{"WEB-U counted"}; !reflect.DeepEqual(drillRows(d), want) {
		t.Fatalf("WEB Sprint 2's drill = %v; want %v", drillRows(d), want)
	}
}
