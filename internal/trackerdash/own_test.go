package trackerdash

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
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
// proposals are listed with their threads, count beside whichever
// dashboard is shown and drill as its widgets do. The built-in of the
// other kind of board is refused.
func TestAKeptDashboardAndItsProposalsAreCounted(t *testing.T) {
	t.Parallel()
	e := evaluator()
	b := keeping(teamBoard().board())
	list, err := e.Dashboards(context.Background(), Kind{PlansSprints: true, Epics: true}, store.MirrorEstimatePoints, b.Own)
	if err != nil {
		t.Fatal(err)
	}
	ds := list.Dashboards
	if len(ds) != 2 || ds[0].ID != builtinSprintsID || ds[1].ID != "d_1" || ds[1].Widgets[0].How.SQL != inProgress.SQL {
		t.Fatalf("dashboards = %+v", ds)
	}
	if ps := list.Proposals; len(ps) != 1 || ps[0].Thread != "thread-1" || ps[0].View.Title != "Proposed" || ps[0].View.How.SQL != inProgress.SQL {
		t.Fatalf("proposals = %+v", ps)
	}
	a := mustEvaluate(t, e, b, Request{Dashboard: "d_1", TimeZone: "UTC"})
	if len(a.Widgets) != 2 || a.Widgets[0].ID != "w_1" || a.Widgets[1].ID != "w_2" || *a.Widgets[1].Value.V != 3 {
		t.Fatalf("widgets = %+v", a.Widgets)
	}
	if bars := a.Widgets[0].Buckets; len(bars) != 2 {
		t.Fatalf("in progress by person = %+v", bars)
	}
	builtin := mustEvaluate(t, e, b, Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: "UTC"})
	if last := builtin.Widgets[len(builtin.Widgets)-1]; last.ID != "w_2" || last.Value == nil {
		t.Fatalf("the built-in with the proposal: %+v", last)
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
	bad.SQL = "SELECT id AS ticket FROM tickets WHERE severity = 'High'"
	_, err = e.Try(context.Background(), b, Widget{Form: FormNumber, Width: 3}, bad, "UTC")
	if r, ok := err.(*Refusal); !ok || r.Code != "dashboard_widget_refused" || !strings.Contains(r.Sentence, "severity") {
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

// A count asks the copy for what its query reads as SQLite resolves it
// (probe.go): a relation by any form of its name, a ticket's type or
// priority read by name, a USING or NATURAL join's condition or a *
// SQLite reads, and nothing a comment or a * it never reads names; How
// this is counted names relations alone. The store's
// TestADashboardCountCopiesWhatItsQueryReads runs each read on a real
// copy against a copy of everything.
func TestACountAsksForWhatItsQueryReads(t *testing.T) {
	t.Parallel()
	board := newFixture(false, "none").status("3", "In progress", "in_progress").
		ticket(ticket{id: "t1", key: "WEB-1", status: "3", group: "in_progress", created: ago(3)}).board()
	var asked [][]string
	fill := board.Fill
	board.Fill = func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		asked = append(asked, relations)
		return fill(ctx, conn, relations)
	}
	m := Metric{Name: "w_1", Title: "Bugs", Words: []string{"Bugs open now."}, Reduce: ReduceCount, Unit: UnitTickets}
	both := []string{"tickets.type", "tickets.priority"}
	for _, c := range []struct {
		sql       string
		asks, not []string
	}{
		{"SELECT id AS ticket FROM tickets WHERE type = 'Bug'", []string{"tickets.type"}, []string{"tickets.priority", "labels"}},
		{"SELECT id AS ticket FROM tickets WHERE TYPE = 'Bug'", []string{"tickets.type"}, nil},
		{`SELECT id AS ticket FROM tickets WHERE "type" = 'Bug'`, []string{"tickets.type"}, nil},
		{"SELECT id AS ticket FROM tickets WHERE [priority] = 'High'", []string{"tickets.priority"}, []string{"tickets.type"}},
		{"SELECT id AS ticket FROM tickets WHERE `priority` = 'High'", []string{"tickets.priority"}, nil},
		{"SELECT id AS ticket FROM main.tickets WHERE main.tickets.type = 'Bug'", []string{"tickets.type"}, nil},
		{"WITH w(type) AS (VALUES('Bug')) SELECT t.id AS ticket FROM tickets t JOIN w USING(type)", []string{"tickets.type"}, []string{"tickets.priority"}},
		{"WITH w(priority) AS (VALUES('High')) SELECT t.id AS ticket FROM tickets t JOIN w USING (priority)", []string{"tickets.priority"}, nil},
		{"WITH w(type) AS (VALUES('Bug')) SELECT t.id AS ticket FROM tickets t NATURAL JOIN w", []string{"tickets.type"}, []string{"tickets.priority"}},
		{"SELECT a.id AS ticket FROM tickets a NATURAL JOIN tickets b", both, nil},
		{"SELECT id AS ticket FROM (SELECT * FROM tickets)", nil, both},
		{"WITH c AS MATERIALIZED (SELECT * FROM tickets) SELECT id AS ticket FROM c", both, nil},
		{"SELECT id AS ticket FROM tickets WHERE (type, priority) = ('Bug', 'High')", both, nil},
		{"SELECT t.id AS ticket FROM tickets t JOIN labels l ON l.ticket = t.id WHERE l.name = 'bug'", []string{"labels"}, both},
		{"WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c JOIN labels USING(ticket)", []string{"labels"}, nil},
		{`SELECT t.id AS ticket FROM tickets t JOIN "labels" l ON l.ticket = t.id`, []string{"labels"}, nil},
		{"SELECT t.id AS ticket FROM tickets t JOIN [LABELS] l ON l.ticket = t.id", []string{"labels"}, nil},
		{"SELECT t.id AS ticket FROM tickets t JOIN main.labels l ON l.ticket = t.id", []string{"labels"}, nil},
		{"WITH a AS (WITH b AS (SELECT ticket FROM labels) SELECT ticket FROM b) SELECT ticket AS ticket FROM a", []string{"labels"}, nil},
		{"WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c JOIN sprint_tickets USING(ticket)", []string{"sprint_tickets"}, nil},
		{"WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c JOIN changes USING(ticket)", []string{"changes"}, nil},
		{"WITH c AS (SELECT id AS ticket FROM tickets) SELECT c.ticket FROM c JOIN sprint_finished USING(ticket)", []string{"sprint_finished"}, nil},
		{"SELECT id AS ticket FROM tickets WHERE open = 1 -- type, labels and * in a comment\n", nil, []string{"tickets.type", "tickets.priority", "labels"}},
		{"SELECT id AS ticket FROM tickets WHERE status_group = 'in_progress' /* NATURAL */", nil, []string{"tickets.type", "tickets.priority", "labels"}},
	} {
		asked = nil
		m.SQL = c.sql
		got, err := evaluator().Try(context.Background(), board, Widget{Form: FormNumber, Width: 3}, m, "UTC")
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(asked) != 1 {
			t.Fatalf("%s: %d fills", c.sql, len(asked))
		}
		for _, want := range c.asks {
			if !slices.Contains(asked[0], want) {
				t.Errorf("%s: asked %v; want %s", c.sql, asked[0], want)
			}
		}
		for _, not := range c.not {
			if slices.Contains(asked[0], not) {
				t.Errorf("%s: asked %v; want no %s", c.sql, asked[0], not)
			}
		}
		if slices.ContainsFunc(got.View.How.Reads, func(r string) bool { return strings.Contains(r, ".") }) {
			t.Errorf("%s: How this is counted reads %v; want relations alone", c.sql, got.View.How.Reads)
		}
	}
	asked = nil
	mustEvaluate(t, evaluator(), board, Request{Dashboard: builtinWeeksID, TimeZone: "UTC"})
	if slices.ContainsFunc(asked[0], func(r string) bool { return slices.Contains(store.MirrorExportColumns, r) || r == "labels" }) {
		t.Errorf("the built-in dashboard asked %v; want no type, priority or labels", asked[0])
	}
}

// The proposals count on a copy of the board of their own, after the
// dashboard's is closed: a proposal whose copy reads what the dashboard
// does not, and reaches the deadline copying it, says so in its own
// place, and the dashboard's widgets keep their numbers.
func TestAProposalsCopyPastTheDeadlineLeavesTheDashboardWhole(t *testing.T) {
	t.Parallel()
	e := evaluator()
	e.deadline = 300 * time.Millisecond
	b := keeping(teamBoard().board())
	history := b.Own.Metrics["w_2"]
	history.SQL, history.Bucket = "SELECT ticket AS ticket FROM changes", ""
	b.Own.Metrics = maps.Clone(b.Own.Metrics)
	b.Own.Metrics["w_2"] = history
	fill := b.Fill
	held := -1
	b.Fill = func(ctx context.Context, conn *sql.Conn, relations []string) (store.MirrorExport, bool, error) {
		if slices.Contains(relations, "changes") {
			held = len(e.slots)
			<-ctx.Done()
			return store.MirrorExport{}, false, ctx.Err()
		}
		return fill(ctx, conn, relations)
	}
	a := mustEvaluate(t, e, b, Request{Dashboard: "d_1", TimeZone: "UTC"})
	if len(a.Widgets) != 2 || a.Widgets[0].Error != "" || len(a.Widgets[0].Buckets) != 2 {
		t.Fatalf("the dashboard's widget = %+v", a.Widgets)
	}
	if !strings.Contains(a.Widgets[1].Error, "ran out before this widget was counted") {
		t.Errorf("the proposal: %+v", a.Widgets[1])
	}
	if held != 1 {
		t.Errorf("the proposals' copy was made holding %d counting slots, not its own one", held)
	}
}

// The list of dashboards and proposals counts nothing, so it answers
// while every count this computer allows is out; How this is counted
// names the board's estimates as a count does.
func TestTheListAnswersWhileEveryCountIsOut(t *testing.T) {
	t.Parallel()
	e := evaluator()
	for range maxEvaluations {
		e.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), busyWait/4)
	defer cancel()
	b := keeping(teamBoard().board())
	list, err := e.Dashboards(ctx, Kind{PlansSprints: true, Epics: true}, store.MirrorEstimateTime, b.Own)
	if err != nil || len(list.Dashboards) != 2 || len(list.Proposals) != 1 {
		t.Fatalf("the list with every count out: %+v, %v", list, err)
	}
	if !slices.ContainsFunc(list.Dashboards[0].Widgets, func(w WidgetView) bool { return strings.Contains(w.How.Counts, "estimated hours") }) {
		t.Errorf("a time board's built-in does not count estimated hours: %+v", list.Dashboards[0].Widgets)
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
		view Unit
	}{{sum, "seconds", UnitEstimate}, {count, "tickets", UnitTickets}} {
		got, err := evaluator().Try(context.Background(), board, Widget{Form: FormNumber, Width: 3}, c.m, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if got.Unit != c.unit || got.View.Unit != c.view {
			t.Errorf("%s: unit %q, the view's %q; want %q and %q", c.m.Title, got.Unit, got.View.Unit, c.unit, c.view)
		}
		// The screen draws a widget and its drill in the unit they publish.
		b := board
		b.Own = Own{Proposals: []Proposal{{Widget: Widget{ID: "w_1", Metric: "w_1", Form: FormNumber, Width: 3}}}, Metrics: map[string]Metric{"w_1": c.m}}
		drill, err := evaluator().Drill(context.Background(), b, DrillRequest{Request: Request{Dashboard: builtinWeeksID, TimeZone: "UTC"}, Widget: "w_1"})
		if err != nil {
			t.Fatal(err)
		}
		if drill.Unit != c.view {
			t.Errorf("%s: the drill's unit %q; want %q", c.m.Title, drill.Unit, c.view)
		}
	}
}

// The rules name every widget field, the contract's columns, and every
// relation a query reads with what its columns hold.
func TestRulesNameTheFieldsAndTheRelations(t *testing.T) {
	t.Parallel()
	rules := Rules()
	for _, want := range []string{"title;", "words", "sql;", "reduce:", "unit:", "good:", "bucket:", "series:", "form:", "width:",
		"ticket (required", "CREATE TABLE main.tickets(", "-- status_group:", "-- type:", "use labels", "no order",
		"CREATE TABLE main.labels(", "an unnamed one by its ID", "one per bucket for a ticket in several", "A count stops at 1s", "time estimates in seconds"} {
		if !strings.Contains(rules, want) {
			t.Errorf("the rules lack %q", want)
		}
	}
	t.Logf("rules: %d bytes", len(rules))
}
