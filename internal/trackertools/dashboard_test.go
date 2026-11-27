package trackertools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
)

// widgetCall runs dashboard_widget as thread-1 with h's boards, answering
// its JSON or its error.
func widgetCall(t *testing.T, f *fakeTracker, h *fakeHost, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := New(f, h).Call(t.Context(), testCaller, "dashboard_widget", raw)
	if err != nil {
		return "", err
	}
	answer, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer), nil
}

func num(v float64) trackerdash.Num { return trackerdash.Num{V: &v} }

// webBoards is a project listing two boards, and a board another project
// lists, which no widget of this thread reaches.
func webBoards() *fakeHost {
	return &fakeHost{project: "p1", boards: []ListedBoard{
		{Connection: "conn-jira", Board: "1", Name: "Web Team", Thread: true},
		{Connection: "conn-jira", Board: "2", Name: "Mobile", Thread: true},
		{Connection: "conn-jira", Board: "3", Name: "Ops"},
	}}
}

var openWidget = map[string]any{"title": "Open bugs", "words": []string{"Tickets open now."}, "sql": "SELECT id AS ticket FROM tickets", "reduce": "count", "unit": "tickets", "form": "number"}

// Without a widget the tool answers the rules, the relations a query
// reads among them, and counts nothing.
func TestDashboardWidgetWithoutAWidgetAnswersTheRules(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{}
	answer, err := widgetCall(t, f, webBoards(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var rules string
	if err := json.Unmarshal([]byte(answer), &rules); err != nil {
		t.Fatalf("answer %s is not the rules text: %v", answer, err)
	}
	if rules != trackerdash.Rules() || !strings.Contains(rules, "CREATE TABLE main.tickets(") {
		t.Fatalf("rules = %q", rules)
	}
	if len(f.tries) != 0 {
		t.Fatalf("tries = %v", f.tries)
	}
}

// A widget field the rules do not name is refused before anything is
// counted, saying where the fields are.
func TestDashboardWidgetRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{}
	_, err := widgetCall(t, f, webBoards(), map[string]any{"board": "Web Team", "widget": map[string]any{"title": "x", "query": "SELECT 1"}})
	refused(t, err, CodeInvalidRequest, `widget: json: unknown field "query". Call dashboard_widget with no widget for its fields.`)
	if len(f.tries) != 0 {
		t.Fatalf("tries = %v", f.tries)
	}
}

// The board is one the thread's project lists, by name in any case or by
// ID; the project's one board when none is named. A board another
// project lists is not among them.
func TestDashboardWidgetResolvesTheThreadProjectsBoard(t *testing.T) {
	t.Parallel()
	tried := func(trackerapp.KeptWidget) (trackerdash.Tried, error) { return trackerdash.Tried{}, nil }
	for _, c := range []struct {
		name, board string
		host        *fakeHost
		want        string
		refusal     string
	}{
		{name: "by name", board: "web team", host: webBoards(), want: "p1 conn-jira/1 3 false thread-1"},
		{name: "by ID", board: "2", host: webBoards(), want: "p1 conn-jira/2 3 false thread-1"},
		{name: "the one board", host: &fakeHost{project: "p1", boards: webBoards().boards[:1]}, want: "p1 conn-jira/1 3 false thread-1"},
		{name: "unnamed of two", host: webBoards(), refusal: `This thread's project lists "Web Team" and "Mobile"; give board.`},
		{name: "another project's", board: "Ops", host: webBoards(), refusal: `This thread's project lists no board "Ops"; it lists "Web Team" and "Mobile".`},
		{name: "none", host: &fakeHost{boards: webBoards().boards[2:]}, refusal: "This thread's project lists no board on this computer; the user adds one in the project's Issues settings."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTracker{try: tried}
			_, err := widgetCall(t, f, c.host, map[string]any{"board": c.board, "widget": openWidget})
			if c.refusal != "" {
				refused(t, err, CodeNoBoard, c.refusal)
				if len(f.tries) != 0 {
					t.Fatalf("tries = %v", f.tries)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.tries) != 1 || f.tries[0] != c.want {
				t.Fatalf("tries = %v, want %q", f.tries, c.want)
			}
		})
	}
}

// A widget without a width is a quarter row for a number and half a row
// for anything else; a width given is kept, to be checked as a kept
// widget's is.
func TestDashboardWidgetDefaultsItsWidthByForm(t *testing.T) {
	t.Parallel()
	for form, want := range map[string]int{"number": 3, "bars": 6, "list": 6} {
		var got trackerapp.KeptWidget
		f := &fakeTracker{try: func(w trackerapp.KeptWidget) (trackerdash.Tried, error) { got = w; return trackerdash.Tried{}, nil }}
		w := map[string]any{"title": "x", "form": form}
		if _, err := widgetCall(t, f, webBoards(), map[string]any{"board": "1", "widget": w}); err != nil {
			t.Fatal(err)
		}
		if got.Width != want || string(got.Form) != form {
			t.Fatalf("%s: widget = %+v, want width %d", form, got, want)
		}
	}
	var got trackerapp.KeptWidget
	f := &fakeTracker{try: func(w trackerapp.KeptWidget) (trackerdash.Tried, error) { got = w; return trackerdash.Tried{}, nil }}
	if _, err := widgetCall(t, f, webBoards(), map[string]any{"board": "1", "widget": map[string]any{"form": "number", "width": 5}}); err != nil {
		t.Fatal(err)
	}
	if got.Width != 5 {
		t.Fatalf("width = %d, want the 5 given", got.Width)
	}
}

// The answer carries what How this is counted says, the numbers over the
// range, how many rows the query returned and the first of them; shown,
// where the person finds it.
func TestDashboardWidgetAnswersTheCount(t *testing.T) {
	t.Parallel()
	one := 1.0
	f := &fakeTracker{try: func(w trackerapp.KeptWidget) (trackerdash.Tried, error) {
		if w.Metric.SQL != "SELECT id AS ticket FROM tickets" || w.Metric.Reduce != trackerdash.ReduceCount || w.Metric.Title != "Open bugs" {
			t.Errorf("metric = %+v", w.Metric)
		}
		return trackerdash.Tried{
			View:  trackerdash.WidgetView{How: trackerdash.How{Counts: "The number of tickets."}},
			Unit:  "tickets",
			Value: trackerdash.WidgetValue{Buckets: []trackerdash.BucketValue{{Label: "Sprint 14", Cells: []trackerdash.Num{num(2)}, Total: num(2)}, {Label: "Sprint 15", Total: trackerdash.Num{Gap: true}}}, NotCounted: 1},
			Range: trackerdash.RangeView{Label: "Last 3 sprints"},
			Rows:  []trackerdash.TicketRow{{Key: "WEB-1", Title: "Login fails", Value: &one, Counted: true, Period: "Sprint 14"}, {Key: "WEB-2", Note: "no estimate"}},
			Count: 7, Reading: true,
		}, nil
	}}
	answer, err := widgetCall(t, f, webBoards(), map[string]any{"board": "Web Team", "widget": openWidget, "show": true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"board":"Web Team","range":"Last 3 sprints","counts":"The number of tickets.","unit":"tickets","buckets":[{"label":"Sprint 14","total":2},{"label":"Sprint 15","total":null}],` +
		`"not_counted":1,"rows":7,"first_rows":[{"key":"WEB-1","title":"Login fails","value":1,"period":"Sprint 14","counted":true},{"key":"WEB-2","counted":false,"note":"no estimate"}],` +
		`"partial":"The board's first read has not finished: these count the tickets read so far.","shown":"It is on the board's dashboards for the user to keep or dismiss."}`
	if answer != want {
		t.Fatalf("answer =\n%s\nwant\n%s", answer, want)
	}
	if f.tries[0] != "p1 conn-jira/1 3 true thread-1" {
		t.Fatalf("tries = %v", f.tries)
	}
}

// What the agent asks for the widget's tile reaches the service as it
// asked: a pin alone, or a limit with it.
func TestDashboardWidgetPassesTheTileMarks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		marks map[string]any
		want  []string
	}{
		{"a pin alone", map[string]any{"pin": true}, []string{"pin"}},
		{"a limit and a pin", map[string]any{"pin": true, "limit": 3}, []string{"limit 3", "pin"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeTracker{try: func(trackerapp.KeptWidget) (trackerdash.Tried, error) {
				return trackerdash.Tried{View: trackerdash.WidgetView{How: trackerdash.How{Counts: "The number of tickets."}}, Unit: "tickets"}, nil
			}}
			widget := map[string]any{}
			for k, v := range openWidget {
				widget[k] = v
			}
			for k, v := range c.marks {
				widget[k] = v
			}
			if _, err := widgetCall(t, f, webBoards(), map[string]any{"board": "1", "widget": widget, "show": true}); err != nil {
				t.Fatal(err)
			}
			if got := f.tries[1:]; !slices.Equal(got, c.want) {
				t.Fatalf("marks = %v, want %v", got, c.want)
			}
		})
	}
}

// The count's refusal reaches the agent with its own code and sentence,
// and a read-only thread may count and propose: nothing it shows changes
// until the person keeps it.
func TestDashboardWidgetPassesTheRefusalAndNeedsNoWrite(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{try: func(trackerapp.KeptWidget) (trackerdash.Tried, error) {
		return trackerdash.Tried{}, errorsx.Public("dashboard_widget_refused", "This widget's query could not be read: no such column: points.", nil)
	}}
	h := webBoards()
	h.readOnly = true
	_, err := widgetCall(t, f, h, map[string]any{"board": "1", "widget": openWidget, "show": true})
	refused(t, err, "dashboard_widget_refused", "This widget's query could not be read: no such column: points.")
	if len(h.approvals) != 0 {
		t.Fatalf("approvals = %v", h.approvals)
	}
	f.try = func(trackerapp.KeptWidget) (trackerdash.Tried, error) { return trackerdash.Tried{}, nil }
	if _, err := widgetCall(t, f, h, map[string]any{"board": "1", "widget": openWidget, "show": true}); err != nil {
		t.Fatalf("a read-only thread's proposal: %v", err)
	}
}

// Two boards of the thread's project with one name, or one ID on two
// sites, are not told apart by first match: the call is refused naming
// each by its site and ID, with no connection's ID, and counts nothing;
// the site and ID it names picks one. The same board listed through two
// connections of its site is refused naming the connections by the
// labels the user gave them: each is one account's copy of the board.
func TestDashboardWidgetRefusesABoardNamedTwice(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{
		try:     func(trackerapp.KeptWidget) (trackerdash.Tried, error) { return trackerdash.Tried{}, nil },
		sources: []trackerapp.TicketSource{{ID: "conn-a", Site: "a.example.net"}, {ID: "conn-a2", Site: "a.example.net"}, {ID: "conn-b", Site: "b.example.net"}},
	}
	h := &fakeHost{project: "p1", boards: []ListedBoard{
		{Connection: "conn-a", ConnectionLabel: "Work", Board: "1", Name: "Engineering", Thread: true},
		{Connection: "conn-a2", ConnectionLabel: "Personal", Board: "1", Name: "Engineering", Thread: true},
		{Connection: "conn-b", Board: "1", Name: "Engineering", Thread: true},
		{Connection: "conn-b", Board: "2", Name: "Mobile", Thread: true},
	}}
	for _, board := range []string{"Engineering", "1"} {
		_, err := widgetCall(t, f, h, map[string]any{"board": board, "widget": openWidget})
		refused(t, err, CodeNoBoard, fmt.Sprintf(`Board %q names more than one board of this thread's project: "Engineering" at a.example.net/1 and "Engineering" at b.example.net/1; give board as the site/ID of one.`, board))
	}
	if len(f.tries) != 0 {
		t.Fatalf("an ambiguous board was counted: %v", f.tries)
	}
	if _, err := widgetCall(t, f, h, map[string]any{"board": "B.example.net/1", "widget": openWidget}); err != nil {
		t.Fatal(err)
	}
	if len(f.tries) != 1 || !strings.Contains(f.tries[0], " conn-b/1 ") {
		t.Fatalf("site/ID counted %+v; want the board on b.example.net", f.tries)
	}
	_, err := widgetCall(t, f, h, map[string]any{"board": "a.example.net/1", "widget": openWidget})
	refused(t, err, CodeNoBoard, `Board "a.example.net/1" is listed through 2 connections of a.example.net ("Work" and "Personal"), each one account's copy; the user removes one of them from the project's boards in its Issues settings.`)
	if len(f.tries) != 1 {
		t.Fatalf("a board listed through two connections was counted: %v", f.tries)
	}
}

// The unit of a widget's numbers and the tickets each could not count
// reach the agent: a time board's estimates in seconds, a number's
// tickets not counted and its previous one's, a bucket's and its cells'.
func TestDashboardWidgetSaysItsUnitAndTicketsNotCounted(t *testing.T) {
	t.Parallel()
	five, three := 7200.0, 3600.0
	f := &fakeTracker{try: func(trackerapp.KeptWidget) (trackerdash.Tried, error) {
		return trackerdash.Tried{Unit: "seconds", Value: trackerdash.WidgetValue{
			Value:    &trackerdash.Num{V: &five, NC: 2},
			Previous: &trackerdash.Num{V: &three, NC: 4},
			Buckets: []trackerdash.BucketValue{
				{Label: "User 1", Total: trackerdash.Num{V: &three, NC: 1}},
				{Label: "User 2", Cells: []trackerdash.Num{{V: &three}, {V: &three, NC: 3}}, Total: trackerdash.Num{V: &five, NC: 3}},
			},
		}}, nil
	}}
	answer, err := widgetCall(t, f, webBoards(), map[string]any{"board": "1", "widget": openWidget})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"unit":"seconds"`, `"value":7200`, `"not_counted":2`, `"previous_not_counted":4`, `{"label":"User 1","total":3600,"not_counted":1}`,
		`{"label":"User 2","cells":[3600,3600],"cells_not_counted":[0,3],"total":7200,"not_counted":3}`} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer %s lacks %s", answer, want)
		}
	}
}
