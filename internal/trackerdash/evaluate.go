package trackerdash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The bounds of an evaluation.
const (
	evaluationDeadline = 4 * time.Second
	maxEvaluations     = 2
	busyWait           = 2 * time.Second
	drillPage          = 100
	maxBars            = 12
	maxTableRows       = 50
	maxListRows        = 50
)

// Refusal is an evaluation the evaluator did not answer, with a code the
// client tells it by and the sentence the person reads.
type Refusal struct {
	Code     string
	Sentence string
	Err      error
}

func (r *Refusal) Error() string {
	if r.Err != nil {
		return r.Sentence + ": " + r.Err.Error()
	}
	return r.Sentence
}

func (r *Refusal) Unwrap() error { return r.Err }

func refuse(code, sentence string, err error) *Refusal {
	return &Refusal{Code: code, Sentence: sentence, Err: err}
}

// Evaluator evaluates dashboards, at most maxEvaluations at once; a
// further one waits up to busyWait for a turn and is refused as busy
// after it. Listing them counts nothing and takes no turn. It retains nothing of a board between calls: only the
// relations each kind's built-in metrics read, which are fixed.
type Evaluator struct {
	slots chan struct{}
	now   func() time.Time
	// deadline is how long one evaluation may take, evaluationDeadline.
	deadline time.Duration

	readsMu sync.Mutex
	// reads is, by kind, what each built-in metric reads of the copy, from
	// its plan on an empty session (metricReads).
	reads map[Kind]map[string]asks
}

// NewEvaluator is an evaluator reading the time from now.
func NewEvaluator(now func() time.Time) *Evaluator {
	return &Evaluator{slots: make(chan struct{}, maxEvaluations), now: now, deadline: evaluationDeadline, reads: map[Kind]map[string]asks{}}
}

func (e *Evaluator) acquire(ctx context.Context) (func(), error) {
	t := time.NewTimer(busyWait)
	defer t.Stop()
	select {
	case e.slots <- struct{}{}:
		return func() { <-e.slots }, nil
	case <-t.C:
		return nil, refuse("dashboard_busy", fmt.Sprintf("this computer is already counting %d dashboards; try again in a moment", maxEvaluations), nil)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Kind is what decides a board's built-in dashboard.
type Kind struct {
	PlansSprints bool
	// Epics is whether the board's tracker has epics (Jira).
	Epics bool
}

// Board is the board an evaluation reads: Fill exports it, Epics is as
// Kind has it, Viewer is the connection's verified account, which Mine
// narrows to ("" before one), and Own is what its project keeps and its
// agents proposed beside the built-in dashboard.
type Board struct {
	Fill   Fill
	Epics  bool
	Viewer string
	Own    Own
}

// Request is what one evaluation counts: a dashboard, over a range,
// narrowed to Mine (the viewer), a person or an epic, with weeks and days
// in TimeZone (IANA).
type Request struct {
	Dashboard string `json:"dashboard"`
	Range     string `json:"range"`
	Mine      bool   `json:"mine,omitempty"`
	Person    string `json:"person,omitempty"`
	Epic      string `json:"epic,omitempty"`
	TimeZone  string `json:"time_zone"`
}

// How is how a widget's number is counted, from its metric's definition.
type How struct {
	Words  []string `json:"words"`
	Counts string   `json:"counts"`
	Range  string   `json:"range"`
	Rules  string   `json:"rules"`
	SQL    string   `json:"sql"`
	Reads  []string `json:"reads"`
}

// WidgetView is a widget as the screen lays it out.
type WidgetView struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Form   Form   `json:"form"`
	Width  int    `json:"width"`
	Bucket Bucket `json:"bucket,omitempty"`
	Unit   Unit   `json:"unit"`
	Good   Good   `json:"good,omitempty"`
	Now    bool   `json:"now,omitempty"`
	// State is a number of a state (Metric's until): its value is what
	// holds now and its previous value what held where the range begins.
	State  bool     `json:"state,omitempty"`
	Series []string `json:"series,omitempty"`
	// Pace and Overlap are the metric's (Metric.Pace, Metric.Overlap).
	Pace    bool `json:"pace,omitempty"`
	Overlap bool `json:"overlap,omitempty"`
	How     How  `json:"how"`
}

// DashboardView is a dashboard with its ranges and widgets.
type DashboardView struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Ranges       []RangeOption `json:"ranges"`
	DefaultRange string        `json:"default_range"`
	Widgets      []WidgetView  `json:"widgets"`
}

// PeriodView is one period of the range.
type PeriodView struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Partial bool   `json:"partial,omitempty"`
}

// RangeView is the range an answer counted over.
type RangeView struct {
	ID          string       `json:"id"`
	Label       string       `json:"label"`
	Periods     []PeriodView `json:"periods"`
	Previous    string       `json:"previous"`
	PreviousGap string       `json:"previous_gap,omitempty"`
	Note        string       `json:"note,omitempty"`
}

// Name is a person or an epic the page is narrowed to.
type Name struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// BucketValue is one bar, point or table row.
type BucketValue struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Partial bool   `json:"partial,omitempty"`
	// Last is the sprint's last working day of a day chart, where its even
	// pace reaches zero.
	Last  bool  `json:"last,omitempty"`
	Cells []Num `json:"cells"`
	Total Num   `json:"total"`
}

// TicketRow is one row of a metric in a ticket list or a drill: a ticket
// as one row counts it. Row is the row's place among the answer's rows,
// unique within it, since a ticket can count more than once (done in two
// sprints). Period is the sprint or week a row of the range falls in.
// Value is the row's number in the metric's unit, absent for a metric
// that counts tickets, whose rows' values are not what it counts.
type TicketRow struct {
	Row      int      `json:"row"`
	ID       string   `json:"id"`
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Assignee string   `json:"assignee,omitempty"`
	Points   *float64 `json:"points,omitempty"`
	Value    *float64 `json:"value,omitempty"`
	Counted  bool     `json:"counted"`
	Period   string   `json:"period,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// WidgetValue is a widget's numbers, or the sentence of its failure.
type WidgetValue struct {
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
	// Note says why a widget of the sprint's days has nothing to draw: no
	// sprint is active, or its dates are not known.
	Note string `json:"note,omitempty"`
	// RangeWords is How this is counted's range sentence as this answer
	// counted it, when it names what the listing could not: the active
	// sprints a widget of the sprint's days counts.
	RangeWords string        `json:"range_words,omitempty"`
	Value      *Num          `json:"value,omitempty"`
	Previous   *Num          `json:"previous,omitempty"`
	Trend      []Num         `json:"trend,omitempty"`
	Series     []string      `json:"series,omitempty"`
	Buckets    []BucketValue `json:"buckets,omitempty"`
	Tickets    []TicketRow   `json:"tickets,omitempty"`
	// More is a list's rows past those it shows, or the buckets a widget
	// that ranks its buckets left out (DrillRequest.Rest opens them).
	More int `json:"more,omitempty"`
	// NotCounted is a list's tickets that its metric could not count, by
	// the rule every number's NC follows, or the tickets of a chart's or
	// table's range that it places in no bucket (a row with no instant).
	NotCounted int `json:"not_counted,omitempty"`
}

// Answer is one evaluation of a dashboard and of the widgets agents
// proposed on the board: the values of the dashboard's widgets and then
// of the proposals, by widget ID, which the screen draws in the layout
// Listing gives. Reading is whether the board's first read has not
// finished, so its numbers count the tickets read so far.
type Answer struct {
	Range     RangeView     `json:"range"`
	Reading   bool          `json:"reading,omitempty"`
	Estimates string        `json:"estimates"`
	Person    *Name         `json:"person,omitempty"`
	Epic      *Name         `json:"epic,omitempty"`
	At        int64         `json:"at"`
	Widgets   []WidgetValue `json:"widgets"`
}

// Listing is what a board offers as the screen lays it out: its
// dashboards, the built-in first and then the project's own, and the
// widgets agents proposed on it with the thread that proposed each.
type Listing struct {
	Dashboards []DashboardView `json:"dashboards"`
	Proposals  []ProposalView  `json:"proposals"`
}

// Dashboards is what a board of kind k, whose estimates are estimateUnit
// (store.MirrorEstimate*), offers: each metric prepared against the
// export's empty relations for How this is counted and whether it counts
// now. An own or proposed widget whose metric does not prepare is listed,
// and its count shows the failure. It counts nothing, so it takes no
// counting slot and answers while every count is out.
func (e *Evaluator) Dashboards(ctx context.Context, k Kind, estimateUnit int, own Own) (Listing, error) {
	estimates := estimatesOf(estimateUnit)
	l := Listing{Proposals: []ProposalView{}}
	err := onEmpty(ctx, func(s *session) error {
		v, err := builtinView(s, k, estimates)
		if err != nil {
			return err
		}
		l.Dashboards = append(l.Dashboards, v)
		for _, d := range own.Dashboards {
			v, _ := s.view(d, own.Metrics, k.PlansSprints, estimates)
			l.Dashboards = append(l.Dashboards, v)
		}
		proposed := make([]Widget, 0, len(own.Proposals))
		for _, p := range own.Proposals {
			proposed = append(proposed, p.Widget)
		}
		pv, _ := s.view(Dashboard{Widgets: proposed}, own.Metrics, k.PlansSprints, estimates)
		for i, v := range pv.Widgets {
			l.Proposals = append(l.Proposals, ProposalView{View: v, Thread: own.Proposals[i].Thread})
		}
		return nil
	})
	return l, err
}

// estimatesOf is how the copy holds a board's estimates, as How this is
// counted and the screen read it: points, time (seconds) or none.
func estimatesOf(unit int) string {
	switch unit {
	case store.MirrorEstimatePoints:
		return "points"
	case store.MirrorEstimateTime:
		return "time"
	}
	return "none"
}

// onEmpty runs f on a session of the export's empty relations, sealed as
// a count's is, whose prepares read what each statement reads (plan.go),
// and closes it.
func onEmpty(ctx context.Context, f func(*session) error) error {
	s, err := openSession(ctx, time.UTC)
	if err != nil {
		return err
	}
	err = func() error {
		if _, err := s.conn.ExecContext(ctx, store.MirrorExportSchema); err != nil {
			return fmt.Errorf("create the export's relations: %w", err)
		}
		if err := s.readLayout(ctx); err != nil {
			return err
		}
		if err := s.gate(ctx); err != nil {
			return refuse("dashboard_unsealed", "the dashboard was not counted, because its copy of the board could not be sealed", err)
		}
		return f(s)
	}()
	if closeErr := s.close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

// builtinView is the built-in dashboard of kind k with its metrics
// prepared on the empty session s. A built-in metric that does not
// prepare is this package's defect.
func builtinView(s *session, k Kind, estimates string) (DashboardView, error) {
	d, metrics := builtin(k)
	v, preps := s.view(d, metrics, k.PlansSprints, estimates)
	for name, pr := range preps {
		if pr.err != nil {
			return DashboardView{}, fmt.Errorf("built-in metric %s: %w", name, pr.err)
		}
	}
	return v, nil
}

// metricReads is what each built-in metric of kind k reads of the copy,
// by metric name, from its plan on the kind's first count, and kept: the
// built-ins are fixed, so their plans read the same relations every
// time. A count fills only these and what the board's labels need
// (boardRelations).
func (e *Evaluator) metricReads(ctx context.Context, k Kind) (map[string]asks, error) {
	e.readsMu.Lock()
	defer e.readsMu.Unlock()
	if r, ok := e.reads[k]; ok {
		return r, nil
	}
	r := map[string]asks{}
	err := onEmpty(ctx, func(s *session) error {
		d, metrics := builtin(k)
		_, preps := s.view(d, metrics, k.PlansSprints, "points")
		for name, pr := range preps {
			if pr.err != nil {
				return fmt.Errorf("built-in metric %s: %w", name, pr.err)
			}
			r[name] = pr.p.asks
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.reads[k] = r
	return r, nil
}

// ownReads is what each of metrics reads of the copy, by name, from their
// plans on an empty session. A metric whose prepare fails reads none and
// keeps the failure, which its widget shows once the count prepares it
// again: the copy's schema is the empty session's, so a statement fails
// there as it failed here, and one whose plan the reader could not read
// fails with that.
func ownReads(ctx context.Context, metrics []Metric) (map[string]asks, error) {
	r := map[string]asks{}
	if len(metrics) == 0 {
		return r, nil
	}
	err := onEmpty(ctx, func(s *session) error {
		for _, m := range metrics {
			if m.Validate() != nil {
				continue
			}
			p, err := s.prepare(m)
			if err != nil {
				p.asks = asks{failed: err}
			}
			r[m.Name] = p.asks
		}
		return nil
	})
	return r, err
}

// boardRelations are the relations every count fills beside its metrics':
// what readBoard and the rows' labels read.
var boardRelations = []string{"tickets", "statuses", "people", "epics"}

// prepResult is a metric prepared, or the sentence of why it did not.
type prepResult struct {
	p   prepared
	err *MetricError
}

// view is d's view, its metrics validated and prepared on s: each
// widget's How and whether it counts now. A metric that fails either, or
// a widget that cannot draw its metric, is that metric's widgets'
// failure.
func (s *session) view(d Dashboard, metrics map[string]Metric, plansSprints bool, estimates string) (DashboardView, map[string]prepResult) {
	ranges, def := rangesOf(plansSprints)
	v := DashboardView{ID: d.ID, Title: d.Title, Ranges: ranges, DefaultRange: def}
	preps := map[string]prepResult{}
	for _, w := range d.Widgets {
		m := metrics[w.Metric]
		pr, ok := preps[m.Name]
		if !ok {
			err := m.Validate()
			var p prepared
			if err == nil {
				p, err = s.prepare(m)
			}
			pr = prepResult{p: p}
			if err != nil {
				me, isMetric := errors.AsType[*MetricError](err)
				if !isMetric {
					me = &MetricError{Sentence: "This widget's query could not be read: " + err.Error() + "."}
				}
				pr.err = me
			}
			preps[m.Name] = pr
		}
		if pr.err == nil {
			if err := checkWidget(w, m); err != nil {
				pr = prepResult{err: &MetricError{Sentence: "This widget cannot draw its metric: " + err.Error() + "."}}
				preps[m.Name] = pr
			}
		}
		v.Widgets = append(v.Widgets, WidgetView{ID: w.ID, Title: cmpOr(w.Title, m.Title), Form: w.Form, Width: w.Width, Bucket: m.Bucket, Unit: m.numbersUnit(), Good: m.Good,
			Now: pr.err == nil && !pr.p.cols.periodBound(m), State: pr.err == nil && pr.p.cols.state(), Series: m.Series, Pace: m.Pace, Overlap: m.Overlap,
			How: how(m, pr.p, plansSprints, estimates)})
	}
	return v, preps
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// valueUnit is the unit m's numbers are in as the copy holds them, for a
// reader without the screen's formatting: tickets, points, seconds (a
// time board's estimates) or days.
func valueUnit(m Metric, estimates string) string {
	switch m.numbersUnit() {
	case UnitTickets:
		return "tickets"
	case UnitDays:
		return "days"
	}
	return map[string]string{"points": "points", "time": "seconds", "none": "tickets"}[estimates]
}

// how renders How this is counted from m's definition.
func how(m Metric, p prepared, plansSprints bool, estimates string) How {
	unit := map[Unit]string{UnitTickets: "tickets", UnitDays: "days"}[m.Unit]
	if m.Unit == UnitEstimate {
		unit = map[string]string{"points": "points", "time": "estimated hours", "none": "tickets"}[estimates]
	}
	period, periods := "week", "weeks"
	if plansSprints {
		period, periods = "sprint", "sprints"
	}
	var counts string
	switch m.Reduce {
	case ReduceCount:
		counts = "The number of tickets"
	case ReduceSum:
		counts = "The " + unit + " added up"
	case ReduceMedian:
		counts = "The middle value of the " + unit
	case ReduceAverage:
		counts = "The average of the " + unit
	case ReduceAverageOfPeriodSums:
		counts = "The " + unit + " of each " + period + " added up, then averaged over the range's " + periods + "; a " + period + " with none counts as zero"
	}
	if p.cols.has(ColumnPerson) {
		counts += "; each row counts for the person the query names."
	} else {
		counts += "; each ticket counts for its assignee now."
	}
	rng := "It counts what is true now; the range does not change it, and it has no earlier value to compare with."
	switch {
	case m.Bucket == BucketDay:
		rng = dayRangeWords("the active sprint")
	case p.cols.state():
		rng = "It counts what holds now, compared with what held where the range begins, and what held at the end of each of the range's " + periods + "."
	case p.cols.periodBound(m):
		rng = "It counts the range's " + periods + " and compares them with the " + periods + " before."
	}
	return How{Words: m.Words, Counts: counts, Range: rng, Rules: "none", SQL: strings.TrimSpace(m.SQL), Reads: p.asks.tables}
}

// dayRangeWords is How this is counted's range sentence of a metric
// grouped by day, which counts sprint: the listing names no sprint, and a
// count's answer names the active ones (WidgetValue.RangeWords).
func dayRangeWords(sprint string) string {
	return "It counts " + sprint + " at its start and at the end of each working day; the range does not change it."
}

// evaluation is one opened, filled and sealed session with the board's
// rows the app reads beside the metrics: each ticket's key, assignee,
// epic, creation and last update, which the narrowing, the order of rows
// and the placement of a row with no instant read for every row; a shown
// row's title, status and estimate are read with its page (titles).
type evaluation struct {
	s         *session
	export    store.MirrorExport
	estimates string
	sprints   []sprintRow
	tickets   map[string]ticketRow
	people    map[string]string
	epics     map[string]Name
}

type ticketRow struct {
	key, assignee, epic string
	created, updated    int64
}

// open opens a session, fills it with b's export of relations and seals
// it, then reads the rows every widget names. asked is what each metric
// reads of the copy, from its plan on an empty session.
func (e *Evaluator) open(ctx context.Context, b Board, loc *time.Location, relations []string, asked map[string]asks) (*evaluation, error) {
	s, err := openSession(ctx, loc)
	if err != nil {
		return nil, err
	}
	s.asked = asked
	ev := &evaluation{s: s}
	if err := ev.fill(ctx, b, relations); err != nil {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return ev, nil
}

func (ev *evaluation) fill(ctx context.Context, b Board, relations []string) error {
	export, ok, err := b.Fill(ctx, ev.s.conn, relations)
	switch {
	case err != nil:
		return tooSlow(ctx, fmt.Errorf("copy the board for its dashboard: %w", err))
	case !ok:
		return refuse("dashboard_board_gone", "this board is no longer on this computer's list of boards; pick another board", nil)
	}
	if err := ev.s.gate(ctx); err != nil {
		return refuse("dashboard_unsealed", "the dashboard was not counted, because its copy of the board could not be sealed", err)
	}
	ev.export = export
	ev.estimates = estimatesOf(export.EstimateUnit)
	return tooSlow(ctx, ev.readBoard(ctx))
}

// firstReadGap is the range before's gap while the board's first read has
// not finished: what the copy holds of either range is partial.
const firstReadGap = "This board's first read is not finished, so its numbers are not compared."

// tooSlow is err, told as the evaluation's deadline when that ended it.
func tooSlow(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return refuse("dashboard_too_slow", fmt.Sprintf("counting this dashboard took longer than %v and stopped; pick a shorter range, or try again when this computer is less busy", evaluationDeadline), err)
	}
	return err
}

// readBoard reads the sprints, tickets, people and epics of the export,
// which the ranges, the narrowing and the labels need.
func (ev *evaluation) readBoard(ctx context.Context) error {
	ev.tickets, ev.people, ev.epics = map[string]ticketRow{}, map[string]string{}, map[string]Name{}
	err := ev.query(ctx, `SELECT id, name, state, coalesce(start_at, 0), coalesce(end_at, 0), coalesce(completed_at, 0), position FROM sprints`, func(r *sql.Rows) error {
		var s sprintRow
		err := r.Scan(&s.id, &s.name, &s.state, &s.startAt, &s.endAt, &s.completedAt, &s.position)
		ev.sprints = append(ev.sprints, s)
		return err
	})
	if err == nil {
		// The creation and last update are held for the evaluation's life
		// alone, as the rest of the evaluation is: the span a row with no
		// instant could have happened in (place). A creation not known is
		// 0, any time before the update.
		err = ev.query(ctx, `SELECT id, key, coalesce(assignee, ''), coalesce(epic, ''), coalesce(created_at, 0), coalesce(updated_at, 0) FROM tickets`, func(r *sql.Rows) error {
			var id string
			var t ticketRow
			err := r.Scan(&id, &t.key, &t.assignee, &t.epic, &t.created, &t.updated)
			ev.tickets[id] = t
			return err
		})
	}
	if err == nil {
		err = ev.query(ctx, `SELECT id, name FROM people`, func(r *sql.Rows) error {
			var id, name string
			err := r.Scan(&id, &name)
			ev.people[id] = name
			return err
		})
	}
	if err == nil {
		err = ev.query(ctx, `SELECT id, key, title FROM epics`, func(r *sql.Rows) error {
			var id, key, title string
			err := r.Scan(&id, &key, &title)
			ev.epics[id] = Name{ID: id, Label: key + " " + title}
			return err
		})
	}
	if err != nil {
		return fmt.Errorf("read the board's copy: %w", err)
	}
	return nil
}

func (ev *evaluation) query(ctx context.Context, q string, scan func(*sql.Rows) error, args ...any) (err error) {
	rows, err := ev.s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (ev *evaluation) close() error { return ev.s.close() }

// titles fills each of rows (a list's or a drill page's, at most
// drillPage) with its ticket's title, status and estimate, read by ID,
// maxParams IDs a statement.
func (ev *evaluation) titles(ctx context.Context, rows []*TicketRow) error {
	byID := map[string][]*TicketRow{}
	var ids []any
	for _, r := range rows {
		if _, ok := byID[r.ID]; !ok {
			ids = append(ids, r.ID)
		}
		byID[r.ID] = append(byID[r.ID], r)
	}
	for chunk := range slices.Chunk(ids, maxParams) {
		q := `SELECT t.id, t.title, coalesce(s.name, ''), t.points FROM tickets t LEFT JOIN statuses s ON s.id = t.status
			WHERE t.id IN (?` + strings.Repeat(", ?", len(chunk)-1) + `)`
		err := ev.query(ctx, q, func(r *sql.Rows) error {
			var id, title, status string
			var points *float64
			if err := r.Scan(&id, &title, &status, &points); err != nil {
				return err
			}
			for _, row := range byID[id] {
				row.Title, row.Status, row.Points = title, status, points
			}
			return nil
		}, chunk...)
		if err != nil {
			return tooSlow(ctx, fmt.Errorf("read the shown tickets' titles: %w", err))
		}
	}
	return nil
}

// narrowing is the person and epic r narrows the page to.
type narrowing struct {
	person, epic *Name
}

func (ev *evaluation) narrowing(r Request, viewer string) (narrowing, error) {
	var n narrowing
	person := r.Person
	if r.Mine {
		if r.Person != "" {
			return n, refuse("dashboard_request_invalid", "Mine and a person narrow the page to two people; clear one", nil)
		}
		if viewer == "" {
			return n, refuse("dashboard_request_invalid", "Mine needs this connection's account, which is not verified yet; check the connection in the Issues settings", nil)
		}
		person = viewer
	}
	if person != "" {
		n.person = &Name{ID: person, Label: cmpOr(ev.people[person], person)}
	}
	if r.Epic != "" {
		e, ok := ev.epics[r.Epic]
		if !ok {
			e = Name{ID: r.Epic, Label: r.Epic}
		}
		n.epic = &e
	}
	return n, nil
}

// keep is the rows of the narrowed page: a row's person is the metric's
// person column, else its ticket's assignee; its epic likewise.
func (ev *evaluation) keep(n narrowing, cols columns, rows []contribution) []contribution {
	if n.person == nil && n.epic == nil {
		return rows
	}
	var out []contribution
	for _, c := range rows {
		t := ev.tickets[c.ticket]
		person, epic := t.assignee, t.epic
		if cols.has(ColumnPerson) {
			person = c.person
		}
		if cols.has(ColumnEpic) {
			epic = c.epic
		}
		if (n.person == nil || person == n.person.ID) && (n.epic == nil || epic == n.epic.ID) {
			out = append(out, c)
		}
	}
	return out
}

// location is the IANA zone name names.
func location(name string) (*time.Location, error) {
	if name == "" {
		return nil, refuse("dashboard_request_invalid", "the request names no time zone to count weeks and days in", nil)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, refuse("dashboard_request_invalid", fmt.Sprintf("this computer does not know the time zone %q", truncate(name, 64)), err)
	}
	return loc, nil
}

// counted is one metric's rows, narrowed, or the sentence of its failure.
type counted struct {
	p    prepared
	rows []contribution
	err  *MetricError
}

// run runs metric m's prepared statement p, narrowed by n, a state
// metric's rows those that hold at one of at. A failure of the metric is
// the widget's; one of the evaluation is returned.
func (ev *evaluation) run(ctx context.Context, m Metric, p prepared, n narrowing, now, since int64, at heldAt) (counted, error) {
	rows, err := ev.s.run(ctx, p, now, since, at)
	if ctx.Err() != nil {
		return counted{}, tooSlow(ctx, ctx.Err())
	}
	if me, ok := errors.AsType[*MetricError](err); ok {
		return counted{p: p, err: me}, nil
	}
	if err != nil {
		return counted{}, fmt.Errorf("count metric %s: %w", m.Name, err)
	}
	rows = ev.keep(n, p.cols, rows)
	if m.numbersUnit() == UnitEstimate && ev.estimates == "none" {
		// The board counts tickets (it has no estimates, or its project
		// counts tickets): each counted row is one ticket, whatever value
		// the query gave it, and a row it did not count stays uncounted.
		for i := range rows {
			if rows[i].counted {
				rows[i].value = 1
			}
		}
	}
	return counted{p: p, rows: rows}, nil
}

// opened is an evaluation begun: its session, the deadline's context, the
// dashboard of the board's kind with its metrics prepared, the range and
// the narrowing. end closes it.
type opened struct {
	ev  *evaluation
	ctx context.Context
	end func() error
	// d is the dashboard counted, and kind the board's kind its ID names
	// when it is the built-in, nil for one the project keeps.
	d    Dashboard
	kind *Kind
	// proposals is the widgets agents proposed on the board. Of both,
	// only the widgets the evaluation counts are here.
	proposals []Widget
	metrics   map[string]Metric
	view      DashboardView
	preps     map[string]prepResult
	sp        span
	n         narrowing
	now       time.Time
}

// widgets is the dashboard's widgets and then the proposals.
func (o *opened) widgets() []Widget {
	return append(slices.Clone(o.d.Widgets), o.proposals...)
}

// begin is the common start of an evaluation and a drill: a slot, the
// deadline, the session filled with what the metrics of the widgets it
// counts read, and those of the dashboard's widgets and the board's
// proposals (proposed) that counts keeps, prepared, with their range and
// narrowing. The request's dashboard is a built-in, whose ID names the
// board's kind, or one of b.Own's: any other is refused before anything
// is copied.
func (e *Evaluator) begin(ctx context.Context, b Board, r Request, counts func(w Widget, proposed bool) bool) (*opened, error) {
	loc, err := location(r.TimeZone)
	if err != nil {
		return nil, err
	}
	o := &opened{now: e.now()}
	if o.d, o.metrics, o.kind, err = dashboardOf(r, b); err != nil {
		return nil, err
	}
	o.d.Widgets = slices.DeleteFunc(slices.Clone(o.d.Widgets), func(w Widget) bool { return !counts(w, false) })
	for _, p := range b.Own.Proposals {
		if counts(p.Widget, true) {
			o.proposals = append(o.proposals, p.Widget)
		}
	}
	var builtins []string
	var own []Metric
	for _, w := range o.widgets() {
		if m, ok := b.Own.Metrics[w.Metric]; ok {
			own = append(own, m)
		} else {
			builtins = append(builtins, w.Metric)
		}
	}
	release, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.deadline)
	relations, asked, err := e.relationsOf(ctx, o.kind, o.metrics, builtins, own)
	var ev *evaluation
	if err == nil {
		ev, err = e.open(ctx, b, loc, relations, asked)
	}
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	o.ev, o.ctx, o.end = ev, ctx, func() error {
		err := ev.close()
		cancel()
		release()
		return err
	}
	if err := o.resolve(b, r, loc); err != nil {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return o, nil
}

func (o *opened) resolve(b Board, r Request, loc *time.Location) error {
	plansSprints := o.ev.export.PlansSprints
	if o.kind != nil && o.kind.PlansSprints != plansSprints {
		return unknownDashboard(r.Dashboard)
	}
	rng := r.Range
	if rng == "" {
		_, rng = rangesOf(plansSprints)
	}
	var err error
	if o.sp, err = resolveSpan(rng, plansSprints, o.ev.sprints, o.now, loc); err != nil {
		return refuse("dashboard_request_invalid", err.Error(), nil)
	}
	if !o.ev.export.FirstReadDone {
		o.sp.previousGap = firstReadGap
	}
	if o.n, err = o.ev.narrowing(r, b.Viewer); err != nil {
		return err
	}
	o.view, o.preps = o.ev.s.view(o.d, o.metrics, plansSprints, o.ev.estimates)
	_, preps := o.ev.s.view(Dashboard{Widgets: o.proposals}, o.metrics, plansSprints, o.ev.estimates)
	maps.Copy(o.preps, preps)
	return nil
}

// dashboardOf is the dashboard r names on b with every metric its
// widgets and b's proposals draw, by name: a built-in, with the kind of
// board its ID names, which the board's own kind is checked against once
// its copy is read (resolve), or one of b.Own's, with no kind.
func dashboardOf(r Request, b Board) (Dashboard, map[string]Metric, *Kind, error) {
	metrics := maps.Clone(b.Own.Metrics)
	if metrics == nil {
		metrics = map[string]Metric{}
	}
	if r.Dashboard == builtinSprintsID || r.Dashboard == builtinWeeksID {
		k := Kind{PlansSprints: r.Dashboard == builtinSprintsID, Epics: b.Epics}
		d, builtins := builtin(k)
		maps.Copy(metrics, builtins)
		return d, metrics, &k, nil
	}
	for _, d := range b.Own.Dashboards {
		if d.ID == r.Dashboard {
			return d, metrics, nil, nil
		}
	}
	return Dashboard{}, nil, nil, unknownDashboard(r.Dashboard)
}

func unknownDashboard(id string) *Refusal {
	return refuse("dashboard_unknown", fmt.Sprintf("this board has no dashboard %q; pick one from the list", truncate(id, 64)), nil)
}

// relationsOf is what a count fills, in order: boardRelations and what
// the named built-in metrics of kind k and the own metrics read, and what
// each of those metrics reads, by name, which its prepare on the filled
// copy says (session.asked). k is nil when no built-in metric counts.
func (e *Evaluator) relationsOf(ctx context.Context, k *Kind, metrics map[string]Metric, builtins []string, own []Metric) ([]string, map[string]asks, error) {
	asked := map[string]asks{}
	if k != nil && len(builtins) > 0 {
		reads, err := e.metricReads(ctx, *k)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range builtins {
			name := metrics[n].Name
			asked[name] = reads[name]
		}
	}
	reads, err := ownReads(ctx, own)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range own {
		if a, ok := reads[m.Name]; ok {
			asked[m.Name] = a
		}
	}
	out := slices.Clone(boardRelations)
	for _, a := range asked {
		out = append(out, a.fills()...)
	}
	slices.Sort(out)
	return slices.Compact(out), asked, nil
}

// count runs metric m once, its prepared statement from the view, for
// what its widgets read (r): a state metric keeps only the rows that hold
// where they read it.
func (o *opened) count(m Metric, r reads) (counted, error) {
	pr := o.preps[m.Name]
	if pr.err != nil {
		return counted{err: pr.err}, nil
	}
	now := o.now.UnixMilli()
	return o.ev.run(o.ctx, m, pr.p, o.n, now, o.sp.since(now), r.instants(now))
}

// Evaluate counts dashboard r.Dashboard of board b and then the board's
// proposals, each in an evaluation of its own: its own copy of the board,
// of what its widgets read, closed before the next opens, and its own
// deadline. What the proposals read or take never reaches the
// dashboard's widgets, which count first with their tickets' titles; a
// proposal its evaluation does not count says why in its own place
// (proposals) instead of failing the answer.
func (e *Evaluator) Evaluate(ctx context.Context, b Board, r Request) (Answer, error) {
	a, err := e.dashboard(ctx, b, r)
	if err != nil || len(b.Own.Proposals) == 0 {
		return a, err
	}
	proposed, err := e.proposals(ctx, b, r)
	if err != nil {
		return Answer{}, err
	}
	a.Widgets = append(a.Widgets, proposed...)
	return a, nil
}

// dashboard is the evaluation of dashboard r.Dashboard's own widgets.
func (e *Evaluator) dashboard(ctx context.Context, b Board, r Request) (_ Answer, err error) {
	o, err := e.begin(ctx, b, r, func(_ Widget, proposed bool) bool { return !proposed })
	if err != nil {
		return Answer{}, err
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	a := Answer{Range: rangeView(o.sp), Reading: !o.ev.export.FirstReadDone, Estimates: o.ev.estimates, Person: o.n.person, Epic: o.n.epic, At: o.now.UnixMilli(), Widgets: []WidgetValue{}}
	// A metric runs once for all of its widgets, for what they read.
	read := map[string]reads{}
	for _, w := range o.d.Widgets {
		m := o.metrics[w.Metric]
		read[m.Name] = read[m.Name].and(widgetReads(w, m, o.sp))
	}
	runs := map[string]counted{}
	for _, w := range o.d.Widgets {
		m := o.metrics[w.Metric]
		c, ok := runs[m.Name]
		if !ok {
			if c, err = o.count(m, read[m.Name]); err != nil {
				return Answer{}, err
			}
			runs[m.Name] = c
		}
		a.Widgets = append(a.Widgets, o.ev.widget(w, m, c, o.sp))
	}
	if err := o.ev.titles(o.ctx, listed(a.Widgets)); err != nil {
		return Answer{}, err
	}
	return a, nil
}

// proposals is the values of b's proposals over r's range and narrowing,
// from an evaluation of their own, each proposal's rows let go once its
// value is drawn. A proposal the deadline stops, or leaves no time for,
// says so (outOfTime), and a failure of the evaluation itself is each
// proposal's sentence; only the caller's leaving fails the call.
func (e *Evaluator) proposals(ctx context.Context, b Board, r Request) (_ []WidgetValue, err error) {
	failed := func(err error) ([]WidgetValue, error) {
		if ctx.Err() != nil {
			return nil, err
		}
		sentence := proposalsFailed(err)
		out := make([]WidgetValue, 0, len(b.Own.Proposals))
		for _, p := range b.Own.Proposals {
			out = append(out, WidgetValue{ID: p.Widget.ID, Error: sentence})
		}
		return out, nil
	}
	o, err := e.begin(ctx, b, r, func(_ Widget, proposed bool) bool { return proposed })
	if err != nil {
		return failed(err)
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	out := make([]WidgetValue, 0, len(o.proposals))
	for _, w := range o.proposals {
		m := o.metrics[w.Metric]
		c := counted{err: outOfTime}
		if o.ctx.Err() == nil {
			var runErr error
			if c, runErr = o.count(m, widgetReads(w, m, o.sp)); runErr != nil {
				if o.ctx.Err() == nil {
					return failed(runErr)
				}
				c = counted{err: outOfTime}
			}
		}
		out = append(out, o.ev.widget(w, m, c, o.sp))
	}
	if err := o.ev.titles(o.ctx, listed(out)); err != nil {
		if o.ctx.Err() == nil {
			return failed(err)
		}
		for i := range out {
			if len(out[i].Tickets) > 0 {
				out[i] = WidgetValue{ID: out[i].ID, Error: outOfTime.Sentence}
			}
		}
	}
	return out, nil
}

// proposalsFailed is the sentence each proposal shows when their
// evaluation failed with err.
func proposalsFailed(err error) string {
	r, ok := errors.AsType[*Refusal](err)
	switch {
	case ok && r.Code == "dashboard_too_slow":
		return outOfTime.Sentence
	case ok:
		return "Not counted: " + r.Sentence + "."
	}
	return "Not counted: " + err.Error() + "."
}

// outOfTime is a proposal's widget the proposals' deadline stopped or
// left no time for, whichever proposals took the time.
var outOfTime = &MetricError{Sentence: fmt.Sprintf("Not counted: the proposed widgets' %v to count ran out before this widget was counted. Dismiss the proposals you do not need, or refresh.", evaluationDeadline), Limit: true}

// listed is the rows the lists of ws show, whose titles are read.
func listed(ws []WidgetValue) []*TicketRow {
	var out []*TicketRow
	for i := range ws {
		for j := range ws[i].Tickets {
			out = append(out, &ws[i].Tickets[j])
		}
	}
	return out
}

func rangeView(sp span) RangeView {
	v := RangeView{ID: sp.id, Label: sp.label, Previous: sp.previousLabel, PreviousGap: sp.previousGap, Note: sp.note, Periods: []PeriodView{}}
	for _, p := range sp.periods {
		v.Periods = append(v.Periods, PeriodView{ID: p.id, Label: p.label, Partial: p.partial})
	}
	return v
}

// ranged is a metric's rows sorted into a range and the one before it
// (inRange). A period's rows and number are read through it alone (of,
// value), so every widget form and drill reads a period the same way.
type ranged struct {
	// periods is what the metric counts by: the range's periods, or the
	// sprint's days for a metric grouped by day.
	periods []period
	// rows is the rows in the periods; all of the metric's rows for one
	// that counts now; those that hold now for a state.
	rows []contribution
	// placed is an event metric's rows of the periods and the previous
	// range's by period ID; nil for a metric that counts now or a state.
	placed map[string][]contribution
	// unplaced and unplacedBefore are the rows with no instant of the
	// range and of the one before it: not counted, in no period (place).
	unplaced, unplacedBefore []contribution
	// state is a state metric's: all is its rows, which a period selects
	// where they hold at its end; before is the rows that held where the
	// range begins, which its number compares with; compared is false when
	// there is nothing to compare (the sprint's days).
	// bySprint is a state's grouped by sprint: a period holds only the rows
	// of its sprints.
	state, bySprint bool
	all             []contribution
	before          []contribution
	compared        bool
	// buf holds the rows of the state period of last asked for.
	buf []contribution
}

// of is period p's rows: none for a period still to come, which has not
// happened; a state's rows that hold at its end (holdsAt), of its sprints
// for one grouped by sprint; an event's that fall in it. A state's are
// valid until the next call.
func (g *ranged) of(p period) []contribution {
	switch {
	case p.future:
		return nil
	case !g.state:
		return g.placed[p.id]
	}
	g.buf = g.buf[:0]
	for _, c := range g.all {
		if c.holdsAt(p.to) && (!g.bySprint || slices.Contains(p.sprints, c.bucket)) {
			c.period = p.label
			g.buf = append(g.buf, c)
		}
	}
	return g.buf
}

// inRange is c's rows sorted into the range sp and the one before it, or
// into the sprint's days for a metric grouped by day; a state's rows are
// read where they hold.
func (ev *evaluation) inRange(m Metric, c counted, sp span) *ranged {
	if !c.p.cols.periodBound(m) {
		return &ranged{rows: c.rows}
	}
	periods, previous := periodsOf(m, sp), sp.previous
	if m.Bucket == BucketDay {
		previous = nil
	}
	if c.p.cols.state() {
		g := &ranged{periods: periods, rows: holding(c.rows, sp.now), all: c.rows, state: true, bySprint: m.Bucket == BucketSprint}
		if m.Bucket != BucketDay && len(periods) > 0 {
			g.before, g.compared = holding(c.rows, periods[0].from), true
		}
		return g
	}
	spanOf := func(ticket string) (int64, int64) {
		t := ev.tickets[ticket]
		return t.created, t.updated
	}
	placed, unplaced := place(m, c.rows, spanOf, periods, previous)
	g := &ranged{periods: periods, placed: placed, unplaced: unplaced[0], unplacedBefore: unplaced[1], compared: m.Bucket != BucketDay}
	for _, p := range periods {
		g.rows = append(g.rows, placed[p.id]...)
	}
	return g
}

// periodsOf is what metric m counts by in span sp: the range's periods,
// or the sprint's days for a metric grouped by day.
func periodsOf(m Metric, sp span) []period {
	if m.Bucket == BucketDay {
		return sp.days
	}
	return sp.periods
}

// reads is where widgets read a state metric's rows: now, where the range
// begins, and at the end of periods. A metric that runs for several
// widgets runs for all of what they read (and).
type reads struct {
	now, start bool
	periods    []period
	day        bool
}

func (r reads) and(o reads) reads {
	r.now, r.start, r.day = r.now || o.now, r.start || o.start, r.day || o.day
	for _, p := range o.periods {
		if !slices.ContainsFunc(r.periods, func(q period) bool { return q.id == p.id }) {
			r.periods = append(r.periods, p)
		}
	}
	return r
}

// widgetReads is where widget w reads metric m's states: a number now,
// where the range begins and at its periods' ends (its trend); a list,
// and a chart whose buckets are not periods, now; a chart of periods at
// their ends.
func widgetReads(w Widget, m Metric, sp span) reads {
	switch {
	case w.Form == FormNumber:
		ps := periodsOf(m, sp)
		return reads{now: true, start: m.Bucket != BucketDay && len(ps) > 0, periods: ps, day: m.Bucket == BucketDay}
	case w.Form == FormList || ranks(w, m):
		return reads{now: true}
	}
	return reads{periods: periodsOf(m, sp), day: m.Bucket == BucketDay}
}

// instants is r's instants at now, a period to come left out, and where
// they are in words.
func (r reads) instants(now int64) heldAt {
	var in heldAt
	var where []string
	if r.now {
		in.at = append(in.at, now)
		where = append(where, "now")
	}
	if r.start {
		in.at = append(in.at, r.periods[0].from)
		where = append(where, "where the range begins")
	}
	ends := 0
	for _, p := range r.periods {
		if !p.future {
			in.at = append(in.at, p.to)
			ends++
		}
	}
	switch {
	case ends == 1 && len(r.periods) == 1:
		where = append(where, "at the end of "+r.periods[0].label)
	case ends > 0 && r.day:
		where = append(where, "at the end of a sprint day")
	case ends > 0:
		where = append(where, "at the end of a period")
	}
	slices.Sort(in.at)
	in.at = slices.Compact(in.at)
	switch len(where) {
	case 0:
	case 1:
		in.where = where[0]
	case 2:
		in.where = where[0] + " or " + where[1]
	default:
		in.where = strings.Join(where[:len(where)-1], ", ") + ", or " + where[len(where)-1]
	}
	return in
}

// ranks is whether a widget ranks its buckets and shows the first of
// them: a bars or table widget whose buckets are not periods. Its drill
// can name the rest (DrillRequest.Rest).
func ranks(w Widget, m Metric) bool {
	return (w.Form == FormBars || w.Form == FormTable) && m.Bucket != BucketPeriod && m.Bucket != BucketSprint && m.Bucket != BucketDay
}

// ranked is a bucket of a widget that ranks its buckets, with its total.
type ranked struct {
	key, label string
	total      Num
}

// rank is the buckets of rows as widget w shows them: by total, largest
// first, then by label; the first limit shown, the rest left out. The
// widget and its drill of the rest take them from here.
func (ev *evaluation) rank(w Widget, m Metric, rows []contribution) (shown, rest []ranked, grouped map[string][]contribution) {
	keys, grouped := group(rows, func(c contribution) string { return c.bucket })
	all := make([]ranked, 0, len(keys))
	for _, k := range keys {
		all = append(all, ranked{key: k, label: ev.label(m.Bucket, k), total: reduceRows(m.Reduce, grouped[k])})
	}
	slices.SortStableFunc(all, func(a, b ranked) int {
		av, bv := -1.0, -1.0
		if a.total.V != nil {
			av = *a.total.V
		}
		if b.total.V != nil {
			bv = *b.total.V
		}
		if av != bv {
			if av > bv {
				return -1
			}
			return 1
		}
		return strings.Compare(a.label, b.label)
	})
	limit := maxBars
	if w.Form == FormTable {
		limit = maxTableRows
	}
	if len(all) <= limit {
		return all, nil, grouped
	}
	return all[:limit], all[limit:], grouped
}

// widget is w's numbers from its metric's rows.
func (ev *evaluation) widget(w Widget, m Metric, c counted, sp span) WidgetValue {
	out := WidgetValue{ID: w.ID}
	if c.err != nil {
		out.Error = c.err.Sentence
		return out
	}
	if m.Bucket == BucketDay && sp.daysNote != "" {
		out.Note = sp.daysNote
		return out
	}
	if m.Bucket == BucketDay {
		out.RangeWords = dayRangeWords(sp.daysLabel)
	}
	g := ev.inRange(m, c, sp)
	rows := g.rows
	switch w.Form {
	case FormNumber:
		if g.periods == nil && !g.state && g.placed == nil {
			v := reduceRows(m.Reduce, rows)
			out.Value = &v
			return out
		}
		var v, prev Num
		switch {
		case g.state:
			v = reduceRows(m.Reduce, rows)
			// What held where the range begins needs no earlier sprints: only
			// a copy still in its first read has nothing to compare with.
			prev = reduceRows(m.Reduce, g.before)
			if sp.previousGap == firstReadGap {
				prev = gapNum
			}
		default:
			v = reduceRange(m.Reduce, g.periods, g.placed, g.unplaced)
			prev = gapNum
			if sp.previousGap == "" {
				prev = reduceRange(m.Reduce, sp.previous, g.placed, g.unplacedBefore)
			}
		}
		out.Value = &v
		if g.compared {
			out.Previous = &prev
		}
		for _, p := range g.periods {
			out.Trend = append(out.Trend, p.value(m.Reduce, g.of(p)))
		}
	case FormList:
		rows = append(slices.Clone(rows), g.unplaced...)
		rowsCopy := slices.Clone(rows)
		sortDrill(rowsCopy, ev.keyOf)
		out.NotCounted = notCounted(rows)
		for _, r := range rowsCopy {
			if !r.counted {
				continue
			}
			if len(out.Tickets) == maxListRows {
				out.More++
				continue
			}
			out.Tickets = append(out.Tickets, ev.ticket(r, m, c.p.cols, len(out.Tickets)))
		}
	case FormBars, FormLine, FormTable:
		out.Series = seriesOf(m, c.p.cols)
		named := c.p.cols.has(ColumnSeries)
		cell := func(key, label string, partial bool, total Num, rows []contribution, reduce func([]contribution) Num) BucketValue {
			b := BucketValue{Key: key, Label: label, Partial: partial, Total: total}
			for _, s := range out.Series {
				b.Cells = append(b.Cells, reduce(bySeries(rows, s, named)))
			}
			return b
		}
		// The range's rows with no instant are in no bucket; its "not
		// counted" lists them and opens the whole widget.
		out.NotCounted = notCounted(g.unplaced)
		if !ranks(w, m) {
			for _, p := range g.periods {
				reduce := func(rows []contribution) Num { return p.value(m.Reduce, rows) }
				rows := g.of(p)
				b := cell(p.id, p.label, p.partial, reduce(rows), rows, reduce)
				b.Last = p.last
				out.Buckets = append(out.Buckets, b)
			}
			return out
		}
		// The shown buckets are chosen by their totals before any of their
		// cells is made, so an answer holds at most limit buckets of the
		// declared series' cells however many buckets the rows name.
		shown, rest, grouped := ev.rank(w, m, rows)
		out.More = len(rest)
		for _, b := range shown {
			out.Buckets = append(out.Buckets, cell(b.key, b.label, false, b.total, grouped[b.key], func(rows []contribution) Num { return reduceRows(m.Reduce, rows) }))
		}
	}
	return out
}

// label is a bucket key's words.
func (ev *evaluation) label(b Bucket, key string) string {
	switch b {
	case BucketPerson:
		if key == "" {
			return "Unassigned"
		}
		return cmpOr(ev.people[key], key)
	case BucketEpic:
		if key == "" {
			return "No epic"
		}
		if e, ok := ev.epics[key]; ok {
			return e.Label
		}
		return key
	case BucketNone, BucketPeriod, BucketSprint, BucketText:
		return cmpOr(key, "(none)")
	}
	panic(fmt.Sprintf("trackerdash: bucket %q reached a label without validation", b))
}

func (ev *evaluation) keyOf(ticket string) string {
	if t, ok := ev.tickets[ticket]; ok {
		return t.key
	}
	return ticket
}

// ticket is row c, the answer's row i, as a list or a drill shows it.
func (ev *evaluation) ticket(c contribution, m Metric, cols columns, i int) TicketRow {
	t, ok := ev.tickets[c.ticket]
	r := TicketRow{Row: i, ID: c.ticket, Key: c.ticket, Counted: c.counted, Period: c.period, Note: c.note}
	if ok {
		r.Key = t.key
		if t.assignee != "" {
			r.Assignee = cmpOr(ev.people[t.assignee], t.assignee)
		}
	}
	if c.counted && cols.has(ColumnValue) && m.Reduce != ReduceCount {
		v := c.value
		r.Value = &v
	}
	return r
}

// DrillRequest asks for the rows behind one number of a dashboard, one
// of three targets: the whole widget (no Cell, no Rest), one bar, point or
// table cell of a bars, line or table widget (Cell), or the buckets a
// widget that ranks its buckets left out (Rest, what its More opens).
// Page is the page of drillPage rows; Count is the row count page 0
// answered, so a later page that finds a different count starts again at
// page 0.
type DrillRequest struct {
	Request
	Widget string     `json:"widget"`
	Cell   *DrillCell `json:"cell,omitempty"`
	Rest   bool       `json:"rest,omitempty"`
	Page   int        `json:"page"`
	Count  int        `json:"count,omitempty"`
}

// DrillCell is one bar, point or table cell: its bucket's key, "" the
// bucket of rows that name none (Unassigned, No epic), and one of the
// metric's series, "" for all of the bucket's (a table's total).
type DrillCell struct {
	Bucket string `json:"bucket"`
	Series string `json:"series"`
}

// DrillAnswer is the number and one page of its rows, from one
// evaluation: Count rows of Tickets tickets, Counted of the rows counted.
// Reading is as an Answer's.
type DrillAnswer struct {
	Title     string      `json:"title"`
	Reading   bool        `json:"reading,omitempty"`
	Value     Num         `json:"value"`
	Unit      Unit        `json:"unit"`
	Estimates string      `json:"estimates"`
	Count     int         `json:"count"`
	Tickets   int         `json:"tickets"`
	Counted   int         `json:"counted"`
	Page      int         `json:"page"`
	Pages     int         `json:"pages"`
	Restarted bool        `json:"restarted,omitempty"`
	Rows      []TicketRow `json:"rows"`
	At        int64       `json:"at"`
}

// drillReads is where drill r of widget w reads metric m's states: at
// the end of the period its cell names, else now.
func drillReads(w Widget, m Metric, sp span, r DrillRequest) reads {
	if r.Cell == nil || r.Rest || ranks(w, m) {
		return reads{now: true}
	}
	for _, p := range periodsOf(m, sp) {
		if p.id == r.Cell.Bucket {
			return reads{periods: []period{p}, day: m.Bucket == BucketDay}
		}
	}
	return reads{}
}

// Drill evaluates the widget's metric again and answers the number r
// names with its rows.
func (e *Evaluator) Drill(ctx context.Context, b Board, r DrillRequest) (_ DrillAnswer, err error) {
	if r.Page < 0 {
		return DrillAnswer{}, refuse("dashboard_request_invalid", "a drill's page is 0 or more", nil)
	}
	d, _, _, err := dashboardOf(r.Request, b)
	if err != nil {
		return DrillAnswer{}, err
	}
	named := func(w Widget) bool { return w.ID == r.Widget }
	if !slices.ContainsFunc(d.Widgets, named) && !slices.ContainsFunc(b.Own.Proposals, func(p Proposal) bool { return named(p.Widget) }) {
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("this dashboard has no widget %q", truncate(r.Widget, 64)), nil)
	}
	o, err := e.begin(ctx, b, r.Request, func(w Widget, _ bool) bool { return named(w) })
	if err != nil {
		return DrillAnswer{}, err
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	// begin's dashboard and proposals are the ones checked above.
	ws := o.widgets()
	w := ws[slices.IndexFunc(ws, named)]
	m := o.metrics[w.Metric]
	c, err := o.count(m, drillReads(w, m, o.sp, r))
	if err != nil {
		return DrillAnswer{}, err
	}
	if c.err != nil {
		return DrillAnswer{}, refuse("dashboard_metric_failed", c.err.Sentence, nil)
	}
	ev, sp := o.ev, o.sp
	out := DrillAnswer{Title: cmpOr(w.Title, m.Title), Reading: !ev.export.FirstReadDone, Unit: m.numbersUnit(), Estimates: ev.estimates, Page: r.Page, At: o.now.UnixMilli(), Rows: []TicketRow{}}
	if m.Bucket == BucketDay && sp.daysNote != "" {
		return DrillAnswer{}, refuse("dashboard_request_invalid", sp.daysNote, nil)
	}
	g := ev.inRange(m, c, sp)
	rows := g.rows
	reduce := func(rows []contribution) Num { return reduceRows(m.Reduce, rows) }
	switch {
	case r.Cell != nil && r.Rest:
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("a drill of widget %q names one cell or the rest, not both", truncate(w.ID, 64)), nil)
	case r.Cell != nil && (w.Form == FormNumber || w.Form == FormList):
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q is one number; its drill names no bar or cell", truncate(w.ID, 64)), nil)
	case r.Rest && !ranks(w, m):
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q shows all it counts; its drill names no rest", truncate(w.ID, 64)), nil)
	case r.Rest:
		_, rest, grouped := ev.rank(w, m, rows)
		rows = nil
		for _, b := range rest {
			rows = append(rows, grouped[b.key]...)
		}
		out.Title += fmt.Sprintf(", %d more%s", len(rest), map[Bucket]string{BucketPerson: " people", BucketEpic: " epics"}[m.Bucket])
		out.Value = reduceRows(m.Reduce, rows)
	case r.Cell == nil && (g.placed == nil || g.state):
		out.Value = reduceRows(m.Reduce, rows)
	case r.Cell == nil:
		out.Value = reduceRange(m.Reduce, g.periods, g.placed, g.unplaced)
		if m.Bucket == BucketDay {
			out.Title += ", " + sp.daysLabel
		} else {
			out.Title += ", " + strings.ToLower(sp.label[:1]) + sp.label[1:]
		}
		rows = append(rows, g.unplaced...)
	case !ranks(w, m):
		j := slices.IndexFunc(g.periods, func(p period) bool { return p.id == r.Cell.Bucket })
		if j < 0 {
			return DrillAnswer{}, refuse("dashboard_request_invalid", "that bar is not in the range any more; read the dashboard again", nil)
		}
		p := g.periods[j]
		out.Title += ", " + p.label
		rows = g.of(p)
		reduce = func(rows []contribution) Num { return p.value(m.Reduce, rows) }
	default:
		var cell []contribution
		for _, row := range rows {
			if row.bucket == r.Cell.Bucket {
				cell = append(cell, row)
			}
		}
		rows = cell
		out.Title += ", " + ev.label(m.Bucket, r.Cell.Bucket)
	}
	if r.Cell != nil {
		if r.Cell.Series != "" {
			if !slices.Contains(m.Series, r.Cell.Series) || !c.p.cols.has(ColumnSeries) {
				return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q has no series %q", truncate(w.ID, 64), truncate(r.Cell.Series, 64)), nil)
			}
			rows = bySeries(rows, r.Cell.Series, true)
			out.Title += ", " + r.Cell.Series
		}
		out.Value = reduce(rows)
	}
	rows = slices.Clone(rows)
	sortDrill(rows, ev.keyOf)
	out.Count = len(rows)
	tickets := map[string]bool{}
	for _, row := range rows {
		tickets[row.ticket] = true
		if row.counted {
			out.Counted++
		}
	}
	out.Tickets = len(tickets)
	out.Pages = max(1, (len(rows)+drillPage-1)/drillPage)
	if r.Page > 0 && (r.Count != out.Count || r.Page >= out.Pages) {
		out.Page, out.Restarted = 0, true
	}
	from := min(len(rows), out.Page*drillPage)
	for i, row := range rows[from:min(len(rows), (out.Page+1)*drillPage)] {
		out.Rows = append(out.Rows, ev.ticket(row, m, c.p.cols, from+i))
	}
	shown := make([]*TicketRow, len(out.Rows))
	for i := range out.Rows {
		shown[i] = &out.Rows[i]
	}
	if err := ev.titles(o.ctx, shown); err != nil {
		return DrillAnswer{}, err
	}
	return out, nil
}
