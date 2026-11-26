package trackerdash

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// inProgress is a metric a project keeps: the tickets in progress now, by
// assignee.
var inProgress = Metric{Name: "w_1", Title: "In progress", Words: []string{"Tickets in progress now."},
	SQL:    "SELECT id AS ticket, assignee AS bucket FROM tickets WHERE status_group = 'in_progress'",
	Reduce: ReduceCount, Unit: UnitTickets, Bucket: BucketPerson}

// keeping is a board whose project keeps one dashboard of one widget and
// whose agents proposed another.
func keeping(b Board) Board {
	proposed := inProgress
	proposed.Name, proposed.Title = "w_2", "Proposed"
	b.Own = Own{
		Dashboards: []Dashboard{{ID: "d_1", Title: "Mine", Widgets: []Widget{{ID: "w_1", Metric: "w_1", Form: FormBars, Width: 6}}}},
		Proposals:  []Proposal{{Widget: Widget{ID: "w_2", Metric: "w_2", Form: FormNumber, Width: 3}, Thread: "thread-1"}},
		Metrics:    map[string]Metric{"w_1": inProgress, "w_2": proposed},
	}
	return b
}

// A kept dashboard is listed after the built-in and counted as it is; the
// proposals count beside whichever dashboard is shown and drill as its
// widgets do. The built-in of the other kind of board is refused.
func TestAKeptDashboardAndItsProposalsAreCounted(t *testing.T) {
	t.Parallel()
	e := evaluator()
	b := keeping(teamBoard().board())
	list, err := e.Dashboards(context.Background(), Kind{PlansSprints: true, Epics: true}, b.Own)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != builtinSprintsID || list[1].ID != "d_1" || list[1].Widgets[0].How.SQL != inProgress.SQL {
		t.Fatalf("dashboards = %+v", list)
	}
	a := mustEvaluate(t, e, b, Request{Dashboard: "d_1", TimeZone: "UTC"})
	if len(a.Widgets) != 2 || a.Widgets[0].ID != "w_1" || a.Widgets[1].ID != "w_2" || *a.Widgets[1].Value.V != 3 {
		t.Fatalf("widgets = %+v", a.Widgets)
	}
	if len(a.Proposals) != 1 || a.Proposals[0].Thread != "thread-1" || a.Proposals[0].View.Title != "Proposed" {
		t.Fatalf("proposals = %+v", a.Proposals)
	}
	if bars := a.Widgets[0].Buckets; len(bars) != 2 {
		t.Fatalf("in progress by person = %+v", bars)
	}
	builtin := mustEvaluate(t, e, b, Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC"})
	if len(builtin.Proposals) != 1 || builtin.Widgets[len(builtin.Widgets)-1].ID != "w_2" {
		t.Fatalf("the built-in with the proposal: %+v", builtin.Proposals)
	}
	drill, err := e.Drill(context.Background(), b, DrillRequest{Request: Request{Dashboard: builtinSprintsID, TimeZone: "UTC"}, Widget: "w_2"})
	if err != nil || drill.Count != 3 {
		t.Fatalf("a proposal's drill: %+v, %v", drill, err)
	}
	if _, err := e.Evaluate(context.Background(), b, Request{Dashboard: builtinWeeksID, TimeZone: "UTC"}); err == nil {
		t.Fatal("a board that plans sprints counted the weeks built-in")
	}
}

// Try counts one widget over the board's default range with its first
// rows and how many there are; a widget that cannot draw its metric, or
// a query that does not run, is refused with the sentence its widget
// would show.
func TestTryCountsOneWidgetOrRefusesIt(t *testing.T) {
	t.Parallel()
	e := evaluator()
	b := teamBoard().board()
	got, err := e.Try(context.Background(), b, Widget{Form: FormTable, Width: 6}, inProgress, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 3 || len(got.Rows) != 3 || got.Rows[0].Title == "" || got.Range.ID != RangeSprints5 || got.View.How.Counts == "" || len(got.Value.Buckets) != 2 {
		t.Fatalf("tried = %+v", got)
	}

	_, err = e.Try(context.Background(), b, Widget{Form: FormLine, Width: 6}, inProgress, "UTC")
	if r, ok := err.(*Refusal); !ok || r.Code != "dashboard_widget_refused" || !strings.Contains(r.Sentence, "draws a line") {
		t.Fatalf("a line by person: %v", err)
	}
	bad := inProgress
	bad.SQL = "SELECT id AS ticket FROM tickets WHERE priority = 'High'"
	_, err = e.Try(context.Background(), b, Widget{Form: FormNumber, Width: 3}, bad, "UTC")
	if r, ok := err.(*Refusal); !ok || r.Code != "dashboard_widget_refused" || !strings.Contains(r.Sentence, "priority") {
		t.Fatalf("a query on a column the copy lacks: %v", err)
	}
}

// Proposals count after the dashboard's own widgets, in the time those
// leave: a proposal the evaluation's deadline stops, and each after it,
// says so in its own place, and the dashboard's widgets keep their
// numbers.
func TestProposalsPastTheDeadlineFailInTheirOwnPlace(t *testing.T) {
	t.Parallel()
	e := evaluator()
	e.deadline = 300 * time.Millisecond
	b := keeping(teamBoard().board())
	slow := b.Own.Metrics["w_2"]
	slow.SQL = "SELECT a.id AS ticket, a.assignee AS bucket FROM tickets a, tickets b, tickets c, tickets d, tickets e, tickets f, tickets g, tickets h ORDER BY a.created_at * b.created_at * c.created_at * d.created_at * e.created_at * f.created_at * g.created_at * h.created_at LIMIT 1"
	after := b.Own.Metrics["w_2"]
	after.Name = "w_3"
	b.Own.Metrics = maps.Clone(b.Own.Metrics)
	b.Own.Metrics["w_2"], b.Own.Metrics["w_3"] = slow, after
	b.Own.Proposals = append(slices.Clone(b.Own.Proposals), Proposal{Widget: Widget{ID: "w_3", Metric: "w_3", Form: FormNumber, Width: 3}, Thread: "thread-1"})
	a := mustEvaluate(t, e, b, Request{Dashboard: "d_1", TimeZone: "UTC"})
	if len(a.Widgets) != 3 || a.Widgets[0].Error != "" || len(a.Widgets[0].Buckets) != 2 {
		t.Fatalf("the dashboard's widget = %+v", a.Widgets)
	}
	for _, w := range a.Widgets[1:] {
		if !strings.Contains(w.Error, "ran out before this widget was counted") {
			t.Errorf("proposal %s: %+v", w.ID, w)
		}
	}
}

// A tried widget says the unit its numbers are in: a count's tickets, a
// time board's estimates in seconds, as the copy holds them.
func TestTrySaysTheUnitOfItsNumbers(t *testing.T) {
	t.Parallel()
	board := newFixture(false, "time").status("3", "In progress", "in_progress").
		ticket(ticket{id: "t1", key: "WEB-1", status: "3", group: "in_progress", points: "7200", created: ago(3)}).board()
	sum := Metric{Name: "w_1", Title: "Estimated", Words: []string{"Estimates of tickets open now."},
		SQL: "SELECT id AS ticket, points AS value FROM tickets WHERE open = 1", Reduce: ReduceSum, Unit: UnitEstimate}
	count := sum
	count.Title, count.Reduce = "Open", ReduceCount
	for _, c := range []struct {
		m    Metric
		unit string
	}{{sum, "seconds"}, {count, "tickets"}} {
		got, err := evaluator().Try(context.Background(), board, Widget{Form: FormNumber, Width: 3}, c.m, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if got.Unit != c.unit {
			t.Errorf("%s: unit %q; want %q", c.m.Title, got.Unit, c.unit)
		}
	}
}

// The rules name every widget field, the contract's columns, and every
// relation a query reads with what its columns hold.
func TestRulesNameTheFieldsAndTheRelations(t *testing.T) {
	t.Parallel()
	rules := Rules()
	for _, want := range []string{"title;", "words", "sql;", "reduce:", "unit:", "good:", "bucket:", "series:", "form:", "width:",
		"ticket (required", "CREATE TABLE main.tickets(", "-- status_group:", "Ticket types, priorities and labels are not in the copy.",
		"A count stops at 1s", "time estimates in seconds"} {
		if !strings.Contains(rules, want) {
			t.Errorf("the rules lack %q", want)
		}
	}
	t.Logf("rules: %d bytes", len(rules))
}
