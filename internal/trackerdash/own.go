package trackerdash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/store"
)

// What a project adds to a board beside its built-in dashboard: the
// dashboards it keeps and the widgets its agents proposed, each widget
// drawing a metric of its own. They are counted as the built-in is, by
// the same sandbox and bounds; this package holds none of them.

// Own is what a project keeps and its agents proposed for one board:
// its dashboards, the proposed widgets, and the metric of each of their
// widgets by name.
type Own struct {
	Dashboards []Dashboard
	Proposals  []Proposal
	Metrics    map[string]Metric
}

// Proposal is a widget an agent proposed on the board, which shows on
// each of its dashboards until the person keeps or dismisses it, and the
// thread that proposed it.
type Proposal struct {
	Widget Widget
	Thread string
}

// ProposalView is a proposed widget as the screen lays it out, and the
// thread that proposed it.
type ProposalView struct {
	View   WidgetView `json:"view"`
	Thread string     `json:"thread"`
}

// tryID names the one-widget dashboard Try counts.
const tryID = "try"

// triedRows is how many of a tried widget's rows its answer shows.
const triedRows = 5

// Tried is one count of a widget on its own: its view and numbers over
// the board's default range, the first of its rows as a drill orders
// them, how many rows its metric returned in the range, and the unit its
// numbers are in (valueUnit).
type Tried struct {
	View    WidgetView  `json:"view"`
	Value   WidgetValue `json:"value"`
	Unit    string      `json:"unit"`
	Range   RangeView   `json:"range"`
	Reading bool        `json:"reading,omitempty"`
	Rows    []TicketRow `json:"rows"`
	Count   int         `json:"count"`
}

// Try counts widget w, drawing metric m, on board b over the board's
// default range, with weeks and days in timeZone. A metric or widget that
// does not validate, prepare or run is refused with its widget's
// sentence (dashboard_widget_refused); a refusal of the evaluation itself
// (busy, too slow, the board gone) is as Evaluate's.
func (e *Evaluator) Try(ctx context.Context, b Board, w Widget, m Metric, timeZone string) (_ Tried, err error) {
	w.ID, w.Metric = tryID, m.Name
	b.Own = Own{Dashboards: []Dashboard{{ID: tryID, Title: m.Title, Widgets: []Widget{w}}}, Metrics: map[string]Metric{m.Name: m}}
	o, err := e.begin(ctx, b, Request{Dashboard: tryID, TimeZone: timeZone}, func(Widget, bool) bool { return true })
	if err != nil {
		return Tried{}, err
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	c, err := o.count(m, widgetReads(w, m, o.sp).and(reads{now: true}))
	if err != nil {
		return Tried{}, err
	}
	if c.err != nil {
		return Tried{}, refuse("dashboard_widget_refused", c.err.Sentence, nil)
	}
	ev := o.ev
	out := Tried{View: o.view.Widgets[0], Value: ev.widget(w, m, c, o.sp), Unit: valueUnit(m, ev.estimates), Range: rangeView(o.sp), Reading: !ev.export.FirstReadDone, Rows: []TicketRow{}}
	g := ev.inRange(m, c, o.sp)
	rows := append(slices.Clone(g.held()), g.unplaced...)
	sortDrill(rows, ev.keyOf)
	out.Count = len(rows)
	for i, r := range rows[:min(len(rows), triedRows)] {
		out.Rows = append(out.Rows, ev.ticket(r, m, c.p.cols, i))
	}
	shown := make([]*TicketRow, 0, len(out.Rows)+len(out.Value.Tickets))
	for i := range out.Rows {
		shown = append(shown, &out.Rows[i])
	}
	for i := range out.Value.Tickets {
		shown = append(shown, &out.Value.Tickets[i])
	}
	if err := ev.titles(o.ctx, shown); err != nil {
		return Tried{}, err
	}
	tellWorklogs(b, o.worklogNeed())
	return out, nil
}

// Rules is what an agent reads before it writes a widget: the widget's
// fields, the query's contract and the relations it reads
// (store.MirrorExportSchema, whose comments say what each column holds).
func Rules() string {
	var b strings.Builder
	fmt.Fprintf(&b, `A widget is one SQLite SELECT over a copy of the board, whose rows the app reduces and draws.
widget fields: title; words (1 to %d sentences saying what it counts, shown to the user); sql; reduce; unit; good; bucket; series; form; width; limit; pin.
The query returns one row per ticket counted, one per bucket for a ticket in several, with columns named from: ticket (required, a tickets.id); value (the row's number, 1 when absent; NULL lists the row as not counted, with its note); bucket (a bar, table row or sprint); series (one of the widget's series); person (whom the row counts for, else you for a query of worklogs, else the assignee); epic; at (milliseconds: places the row in the range's periods); until (makes the row a state from at until until, NULL: still, counted where it holds); note.
A query with no at or until, and no sprint or day bucket, counts what is true now.
:now is now and :since where the range's periods begin, in milliseconds. days(a, b) and working_days(a, b) are the days between two instants; day(a) is its date, YYYY-MM-DD; day_start(d) is where a row of day d is placed (worklogs.day). Date functions, random and JSON functions are not available, and only SELECT reads these tables. A count stops at %v or past %d rows; a state's row counts toward them only if it holds now, where the range begins, or at the end of a period its widgets show.
reduce: count (distinct tickets), sum, median, average, average_of_period_sums (bucket period or sprint).
unit: tickets, estimate (the board's points, or its time estimates in seconds; each counted row is 1 ticket on a board without estimates or of a project that counts tickets), days, time (logged seconds). good: up or down, the direction that is good news; omit for neither.
bucket: omit for none; period (needs at or until), sprint (a sprints.id), day (the active sprint's working days, needs at or until, no bucket column), person (a people.id), epic (an epics.id), text.
series: up to %d names, the series column's values; a chart draws up to %d.
form: number, bars, line (bucket period, sprint or day), table (needs a bucket), list. width: 3, 4, 6, 8 or 12 of 12 columns.
limit (a number's): the tile turns amber below it when good is up, else above it. pin: true asks that Keep also shows it on the board's summary row.
`, maxWords, metricDeadline, maxRows, maxSeries, maxChartSeries)
	b.WriteString(store.MirrorExportSchema)
	return b.String()
}
