package trackerdash

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// creditBoard is the sprint board of a project that counts credit, with
// its credit in sprint 6:
//
//	WEB-1  finished, 5 points, for User 1; User 2 reviewed it (10%: 0.5)
//	WEB-2  finished, 3 points, for User 2; who reviewed it is not read
//	WEB-3  support, User 2 logged 2 hours (a point a 4 hours: 0.5)
//
// and, with estimates none, a board that counts tickets.
func creditBoard(estimates string) *fixture {
	f := sprintBoard()
	f.estimates = estimates
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", points: "5", created: ago(20)}).member("s6", "t1", true)
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", assignee: "u2", points: "3", created: ago(20)}).member("s6", "t2", true)
	f.ticket(ticket{id: "t3", key: "WEB-3", status: "1", group: "todo", created: ago(20)})
	row := func(ticket, kind, person, points, value, share, did string, qualified, hours, note string) {
		f.exec(`INSERT INTO credit(sprint, at, ticket, kind, person, value, share, points, review_share, review_need, review_kinds, did, qualified, hours,
			hours_per_point, note) VALUES ('s6', %d, '%s', '%s', %s, %s, %s, %s, 10, 2, 'commented, logged time', %s, %s, %s, 4, %s)`,
			ago(2), ticket, kind, quote(person), value, share, points, quote(did), qualified, hours, quote(note))
	}
	row("t1", "finished", "u1", "5", "5", "1", "", "NULL", "NULL", "")
	row("t1", "review", "u2", "5", "0.5", "0.1", "commented, logged time", "1", "NULL", "")
	row("t2", "finished", "u2", "3", "3", "1", "", "NULL", "NULL", "")
	row("t2", "review", "", "3", "NULL", "NULL", "", "NULL", "NULL", "who worked on it is not read yet")
	row("t3", "support", "u2", "NULL", "0.5", "0.5", "", "NULL", "2", "")
	return f
}

// creditOn is f's board of a project that counts credit.
func creditOn(f *fixture) Board {
	b := f.board()
	b.Credit = true
	return b
}

var utcSprints5 = Request{Dashboard: builtinSprintsID, Range: RangeSprints5, TimeZone: "UTC"}

// The pace of a project that counts credit is each sprint's finished
// tickets, review credit and support credit, each row for its person: the
// team's, Mine's and a person's, a ticket whose review is not read not
// counted in each of them, and on a board that counts tickets each row's
// share of a ticket.
func TestThePaceCountsCreditInThreeSeries(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		estimates string
		r         Request
		pace, bar string
	}{
		{"team", "points", utcSprints5, "1.8, 1 not counted", "8 0.5, 1 not counted 0.5"},
		{"mine", "points", Request{Dashboard: builtinSprintsID, Range: RangeSprints5, TimeZone: "UTC", Mine: true}, "1, 1 not counted", "5 not known, 1 not counted 0"},
		{"User 2", "points", Request{Dashboard: builtinSprintsID, Range: RangeSprints5, TimeZone: "UTC", Person: "u2"}, "0.8, 1 not counted", "3 0.5, 1 not counted 0.5"},
		{"tickets", "none", utcSprints5, "0.52, 1 not counted", "2 0.1, 1 not counted 0.5"},
	} {
		a := mustEvaluate(t, evaluator(), creditOn(creditBoard(c.estimates)), c.r)
		pace := widgetOf(t, a, "pace")
		bars := widgetOf(t, a, "finished")
		last := bars.Buckets[len(bars.Buckets)-1]
		var cells []string
		for i := range last.Cells {
			cells = append(cells, num(&last.Cells[i]))
		}
		if num(pace.Value) != c.pace || strings.Join(cells, " ") != c.bar || !reflect.DeepEqual(bars.Series, []string{seriesFinished, seriesReview, seriesSupport}) {
			t.Errorf("%s: pace %s, sprint 6's cells %v of %v; want %s and %s", c.name, num(pace.Value), cells, bars.Series, c.pace, c.bar)
		}
	}
}

// The pace's drill lists each credit row with the person it counts for,
// its series and how it was counted; the row not read names no person.
func TestThePacesDrillSaysHowCreditWasCounted(t *testing.T) {
	t.Parallel()
	d, err := evaluator().Drill(context.Background(), creditOn(creditBoard("points")), DrillRequest{Request: utcSprints5, Widget: "pace"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range d.Rows {
		value := "-"
		if r.Value != nil {
			value = fmt.Sprint(*r.Value)
		}
		rows = append(rows, strings.Join([]string{r.Key, r.Series, r.Assignee, value, fmt.Sprint(r.Counted), r.Note}, " | "))
	}
	want := []string{
		"WEB-1 | Finished tickets | User 1 | 5 | true | ",
		"WEB-2 | Finished tickets | User 2 | 3 | true | ",
		"WEB-1 | Review credit | User 2 | 0.5 | true | 10% of 5 points for commented, logged time",
		"WEB-3 | Support credit | User 2 | 0.5 | true | 2h ÷ 4 = 0.5",
		"WEB-2 | Review credit |  | - | false | who worked on it is not read yet",
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("drill\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
}

// While the last read of who worked on the tickets failed as a whole,
// every value of a widget that reads the credit relation says so and why,
// whatever its rows hold: the built-in pace, a kept number of finished
// tickets alone, a proposal, and a tried widget. A widget beside them that
// does not read it, and any widget with no failure, says nothing.
func TestTheCreditSentenceGoesWithEachWidgetOfCredit(t *testing.T) {
	t.Parallel()
	const failure = "Jira refused the request: 401 Unauthorized."
	sentence := "Credit may not count everything: the last read of who worked on the tickets failed. " + failure
	failing := func(b Board) Board {
		b.EvidenceFailure = failure
		return b
	}
	finished := Metric{Name: "w_1", Title: "Finished", Words: []string{"Finished tickets."}, SQL: `SELECT ticket, value FROM credit WHERE kind = 'finished'`,
		Reduce: ReduceSum, Unit: UnitEstimate}
	open := Metric{Name: "w_2", Title: "Open", Words: []string{"Open tickets."}, SQL: `SELECT id AS ticket FROM tickets WHERE open = 1`,
		Reduce: ReduceCount, Unit: UnitTickets}
	own := func(kept, proposed Metric) Board {
		b := creditOn(creditBoard("points"))
		b.Own = Own{Dashboards: []Dashboard{{ID: "mine", Title: "Mine", Widgets: []Widget{{ID: "kept", Metric: kept.Name, Form: FormNumber, Width: 3}}}},
			Proposals: []Proposal{{Widget: Widget{ID: "proposed", Metric: proposed.Name, Form: FormNumber, Width: 3}}},
			Metrics:   map[string]Metric{kept.Name: kept, proposed.Name: proposed}}
		return b
	}
	mine := Request{Dashboard: "mine", Range: RangeSprints5, TimeZone: "UTC"}
	for _, c := range []struct {
		name string
		b    Board
		r    Request
		want map[string]string
	}{
		{"the built-in pace", failing(creditOn(creditBoard("points"))), utcSprints5, map[string]string{"pace": sentence}},
		{"a kept number of finished tickets beside a proposal of tickets", failing(own(finished, open)), mine, map[string]string{"kept": sentence, "proposed": ""}},
		{"a proposal of finished tickets beside a kept number of tickets", failing(own(open, finished)), mine, map[string]string{"kept": "", "proposed": sentence}},
		{"credit not counted", failing(creditBoard("points").board()), utcSprints5, map[string]string{"pace": ""}},
		{"no failure", creditOn(creditBoard("points")), utcSprints5, map[string]string{"pace": ""}},
	} {
		a := mustEvaluate(t, evaluator(), c.b, c.r)
		for id, want := range c.want {
			if got := widgetOf(t, a, id).Evidence; got != want {
				t.Errorf("%s: %s says %q; want %q", c.name, id, got, want)
			}
		}
	}
	for _, c := range []struct {
		name string
		m    Metric
		want string
	}{{"a tried widget of credit", finished, sentence}, {"a tried widget of tickets", open, ""}} {
		tried, err := evaluator().Try(context.Background(), failing(creditOn(creditBoard("points"))), Widget{Form: FormNumber, Width: 3}, c.m, "UTC")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if tried.Value.Evidence != c.want {
			t.Errorf("%s: evidence %q; want %q", c.name, tried.Value.Evidence, c.want)
		}
	}
}

// A row of a metric with a person column that is not counted and names
// no person could be anyone's, so a person's page keeps it, not counted;
// a counted row that names no person, and one that names another, are
// not kept.
func TestAPersonsPageKeepsARowThatCouldBeAnyones(t *testing.T) {
	t.Parallel()
	f := sprintBoard()
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", created: ago(20)})
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", assignee: "u1", created: ago(20)})
	f.ticket(ticket{id: "t3", key: "WEB-3", status: "5", group: "done", assignee: "u1", created: ago(20)})
	b := f.board()
	b.Own = Own{Metrics: map[string]Metric{"m": {Name: "m", Title: "By person", Words: []string{"Rows by person."}, Reduce: ReduceSum, Unit: UnitTickets,
		SQL: `SELECT 't1' AS ticket, NULL AS value, NULL AS person UNION ALL SELECT 't2', 1, NULL UNION ALL SELECT 't3', 1, 'u2'`}},
		Dashboards: []Dashboard{{ID: "own", Title: "Own", Widgets: []Widget{{ID: "w", Metric: "m", Form: FormNumber, Width: 3}}}}}
	a := mustEvaluate(t, evaluator(), b, Request{Dashboard: "own", TimeZone: "UTC", Person: "u1"})
	if got := num(widgetOf(t, a, "w").Value); got != "not known, 1 not counted" {
		t.Fatalf("User 1's number = %s; want WEB-1 alone, not counted", got)
	}
}

// On a board without sprints the pace with credit counts each row in the
// week of its at: a review in the week its ticket finished, support in
// the week worked, and a ticket whose finish is not known in none, not
// counted; How this is counted says a review counts whenever it was left.
func TestTheWeeksPaceCountsCreditByWeek(t *testing.T) {
	t.Parallel()
	f := newFixture(false, "points").person("u1", "User 1").person("u2", "User 2").status("5", "Done", "done").status("1", "To Do", "todo")
	f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", points: "5", created: ago(20), updated: ago(1)})
	f.ticket(ticket{id: "t2", key: "WEB-2", status: "5", group: "done", assignee: "u1", points: "3", created: ago(20), updated: ago(1)})
	f.ticket(ticket{id: "t3", key: "WEB-3", status: "1", group: "todo", created: ago(20), updated: ago(1)})
	row := func(at, ticket, kind, person, value, share, note string) {
		f.exec(`INSERT INTO credit(sprint, at, ticket, kind, person, value, share, review_share, did, qualified, hours, hours_per_point, note)
			VALUES (NULL, %s, '%s', '%s', %s, %s, %s, 10, 'commented, logged time', 1, 2, 4, %s)`, at, ticket, kind, quote(person), value, share, quote(note))
	}
	row(fmt.Sprint(ago(1)), "t1", "finished", "u1", "5", "1", "")
	row(fmt.Sprint(ago(1)), "t1", "review", "u2", "0.5", "0.1", "")
	row(fmt.Sprint(ago(8)), "t3", "support", "u2", "0.5", "0.5", "")
	row("NULL", "t2", "finished", "u1", "NULL", "NULL", "when it finished is not known")
	a := mustEvaluate(t, evaluator(), creditOn(f), Request{Dashboard: builtinWeeksID, Range: RangeWeeks2, TimeZone: "UTC"})
	if got := num(widgetOf(t, a, "pace").Value); got != "3, 1 not counted" {
		t.Fatalf("pace = %s; want (5.5 this week + 0.5 last week) / 2, WEB-2 not counted", got)
	}
	list, err := evaluator().Dashboards(context.Background(), Kind{Credit: true}, 1, Own{})
	if err != nil {
		t.Fatal(err)
	}
	if words := strings.Join(list.Dashboards[0].Widgets[0].How.Words, " "); !strings.Contains(words, "A review counts in the week its ticket finished, whenever it was left") {
		t.Fatalf("How this is counted: %s", words)
	}
}

// A review row's note gives its arithmetic in the base its number was
// counted from: the share of the ticket's estimate where the board counts
// its estimates, of a ticket where it counts tickets (no estimates, or a
// project that counts tickets), and what the reviewer did; one that did
// not qualify, the kinds the rule counts and how many it needs. A support
// row says its hours, and on a board counting time no more. On a board
// without sprints a review whose ticket's finish is not known keeps why
// it is not read.
func TestACreditRowsNoteGivesItsArithmetic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, estimates string
		tickets         bool
		sprints         bool
		at, points      string
		value, did      string
		qualified, note string
		want            string
	}{
		{"points", "points", false, true, "%d", "5", "0.5", "commented, logged time", "1", "NULL", "10% of 5 points for commented, logged time"},
		{"time", "time", false, true, "%d", "7200", "720", "commented", "1", "NULL", "10% of 2h for commented"},
		{"none", "none", false, true, "%d", "NULL", "0.1", "logged time", "1", "'approvals are not read yet'", "10% of a ticket for logged time · approvals are not read yet"},
		{"a project counting tickets", "points", true, true, "%d", "5", "0.5", "commented, logged time", "1", "NULL", "10% of a ticket for commented, logged time"},
		{"not qualified", "points", false, true, "%d", "5", "0", "commented", "0", "NULL", "no credit: commented; needs 2 of commented, logged time"},
		{"weeks, finish and evidence unknown", "points", false, false, "NULL", "5", "NULL", "", "NULL", "'who worked on it is not read yet'",
			"when it finished is not known · who worked on it is not read yet"},
	} {
		f := newFixture(c.sprints, c.estimates).person("u1", "User 1").person("u2", "User 2").status("5", "Done", "done")
		f.countsTickets = c.tickets
		f.ticket(ticket{id: "t1", key: "WEB-1", status: "5", group: "done", assignee: "u1", created: ago(20), updated: ago(1)})
		sprint, r := "NULL", Request{Dashboard: builtinWeeksID, Range: RangeWeeks2, TimeZone: "UTC"}
		if c.sprints {
			f.sprint("s1", "WEB Sprint 1", "closed", ago(10), ago(2), 1).member("s1", "t1", true)
			sprint, r = "'s1'", utcSprints5
		}
		at := c.at
		if at == "%d" {
			at = fmt.Sprint(ago(3))
		}
		person := "'u2'"
		if c.value == "NULL" {
			person = "NULL"
		}
		f.exec(`INSERT INTO credit(sprint, at, ticket, kind, person, value, share, points, review_share, review_need, review_kinds, did, qualified, hours,
			hours_per_point, note) VALUES (%s, %s, 't1', 'review', %s, %s, %s, %s, 10, 2, 'commented, logged time', %s, %s, NULL, NULL, %s)`,
			sprint, at, person, c.value, c.value, c.points, quote(c.did), c.qualified, c.note)
		d, err := evaluator().Drill(context.Background(), creditOn(f), DrillRequest{Request: r, Widget: "pace"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var notes []string
		for _, row := range d.Rows {
			notes = append(notes, row.Note)
		}
		if len(notes) != 1 || notes[0] != c.want {
			t.Errorf("%s: notes %q; want %q", c.name, notes, c.want)
		}
	}
	const partial = "time logged before 2026-06-01 is not read"
	for _, c := range []struct {
		name, estimates string
		tickets         bool
		note, want      string
	}{
		{"support, points", "points", false, "", "2h ÷ 4 = 0.5"},
		{"support, time", "time", false, "", "2h logged"},
		{"support, a project counting tickets on a time board", "time", true, "", "2h ÷ 4 = 0.5"},
		{"support read in part, points", "points", false, partial, "2h ÷ 4 = 0.5 · " + partial},
		{"support read in part, time", "time", false, partial, "2h logged · " + partial},
	} {
		f := newFixture(true, c.estimates).person("u1", "User 1").person("u2", "User 2").status("1", "To Do", "todo")
		f.countsTickets = c.tickets
		f.ticket(ticket{id: "t1", key: "WEB-1", status: "1", group: "todo", created: ago(20), updated: ago(1)})
		f.sprint("s1", "WEB Sprint 1", "closed", ago(10), ago(2), 1)
		f.exec(`INSERT INTO credit(sprint, at, ticket, kind, person, value, share, hours, hours_per_point, note)
			VALUES ('s1', NULL, 't1', 'support', 'u2', 0.5, 0.5, 2, 4, %s)`, quote(c.note))
		d, err := evaluator().Drill(context.Background(), creditOn(f), DrillRequest{Request: utcSprints5, Widget: "pace"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(d.Rows) != 1 || d.Rows[0].Note != c.want {
			t.Errorf("%s: rows %+v; want one noted %q", c.name, d.Rows, c.want)
		}
	}
}

// A metric by sprint is charged its rows of the sprints its widgets read
// alone, the range's and the one before it: 20,800 review rows of an
// earlier sprint, over the row limit, leave This sprint counted, and its
// rows are the only ones held.
func TestASprintMetricIsChargedOnlyTheSprintsItsWidgetsRead(t *testing.T) {
	t.Parallel()
	f := creditBoard("points")
	f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20800)
		INSERT INTO credit(sprint, at, ticket, kind, person, value, share, points, review_share, review_need, did, qualified, hours, hours_per_point, note)
		SELECT 's1', %d, 't1', 'review', 'u2', 0.5, 0.1, 5, 10, 2, 'commented, logged time', 1, NULL, 4, NULL FROM n`, ago(40))
	start := time.Now()
	a := mustEvaluate(t, evaluator(), creditOn(f), Request{Dashboard: builtinSprintsID, Range: RangeThisSprint, TimeZone: "UTC"})
	t.Logf("evaluated This sprint over 20,800 rows of another sprint in %v", time.Since(start))
	for _, w := range a.Widgets {
		if w.Error != "" {
			t.Fatalf("widget %s failed: %s", w.ID, w.Error)
		}
	}
	if pace := widgetOf(t, a, "pace"); num(pace.Previous) != "9, 1 not counted" {
		t.Fatalf("the pace before This sprint (sprint 6) is %s; want sprint 6's credit alone", num(pace.Previous))
	}
}
