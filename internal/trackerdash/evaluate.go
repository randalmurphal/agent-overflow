package trackerdash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
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
// after it. It retains nothing between calls.
type Evaluator struct {
	slots chan struct{}
	now   func() time.Time
}

// NewEvaluator is an evaluator reading the time from now.
func NewEvaluator(now func() time.Time) *Evaluator {
	return &Evaluator{slots: make(chan struct{}, maxEvaluations), now: now}
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
// Kind has it, and Viewer is the connection's verified account, which
// Mine narrows to ("" before one).
type Board struct {
	Fill   Fill
	Epics  bool
	Viewer string
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
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Form   Form     `json:"form"`
	Width  int      `json:"width"`
	Bucket Bucket   `json:"bucket,omitempty"`
	Unit   Unit     `json:"unit"`
	Good   Good     `json:"good,omitempty"`
	Now    bool     `json:"now,omitempty"`
	Series []string `json:"series,omitempty"`
	How    How      `json:"how"`
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
	Cells   []Num  `json:"cells"`
	Total   Num    `json:"total"`
}

// TicketRow is a ticket in a ticket list or a drill.
type TicketRow struct {
	ID       string   `json:"id"`
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Assignee string   `json:"assignee,omitempty"`
	Points   *float64 `json:"points,omitempty"`
	Value    *float64 `json:"value,omitempty"`
	Counted  bool     `json:"counted"`
	Note     string   `json:"note,omitempty"`
}

// WidgetValue is a widget's numbers, or the sentence of its failure.
type WidgetValue struct {
	ID         string        `json:"id"`
	Error      string        `json:"error,omitempty"`
	Value      *Num          `json:"value,omitempty"`
	Previous   *Num          `json:"previous,omitempty"`
	Trend      []Num         `json:"trend,omitempty"`
	Series     []string      `json:"series,omitempty"`
	Buckets    []BucketValue `json:"buckets,omitempty"`
	Tickets    []TicketRow   `json:"tickets,omitempty"`
	More       int           `json:"more,omitempty"`
	NotCounted int           `json:"not_counted,omitempty"`
}

// Answer is one evaluation of a dashboard.
type Answer struct {
	Dashboard DashboardView `json:"dashboard"`
	Range     RangeView     `json:"range"`
	Estimates string        `json:"estimates"`
	Person    *Name         `json:"person,omitempty"`
	Epic      *Name         `json:"epic,omitempty"`
	At        int64         `json:"at"`
	Widgets   []WidgetValue `json:"widgets"`
}

// Dashboards is the dashboards a board of kind k offers, each metric
// prepared against the export's empty relations for the relations it
// reads and whether it counts now.
func (e *Evaluator) Dashboards(ctx context.Context, k Kind) ([]DashboardView, error) {
	release, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	s, err := openSession(ctx, time.UTC)
	if err != nil {
		return nil, err
	}
	views, err := func() ([]DashboardView, error) {
		if _, err := s.conn.ExecContext(ctx, store.MirrorExportSchema); err != nil {
			return nil, fmt.Errorf("create the export's relations: %w", err)
		}
		if err := s.gate(ctx); err != nil {
			return nil, refuse("dashboard_unsealed", "the dashboard was not counted, because its copy of the board could not be sealed", err)
		}
		d, metrics := builtin(k)
		v, preps := s.view(d, metrics, "points")
		for name, pr := range preps {
			if pr.err != nil {
				return nil, fmt.Errorf("built-in metric %s: %w", name, pr.err)
			}
		}
		return []DashboardView{v}, nil
	}()
	if closeErr := s.close(); closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return views, err
}

// prepResult is a metric prepared, or the sentence of why it did not.
type prepResult struct {
	p   prepared
	err *MetricError
}

// view is d's view, its metrics validated and prepared on s: each
// widget's How and whether it counts now. A metric that fails either, or
// a widget that cannot draw its metric, is that metric's widgets'
// failure.
func (s *session) view(d Dashboard, metrics map[string]Metric, estimates string) (DashboardView, map[string]prepResult) {
	plansSprints := d.ID == builtinSprintsID
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
		v.Widgets = append(v.Widgets, WidgetView{ID: w.ID, Title: cmpOr(w.Title, m.Title), Form: w.Form, Width: w.Width, Bucket: m.Bucket, Unit: m.Unit, Good: m.Good,
			Now: pr.err == nil && !pr.p.cols.periodBound(m), Series: m.Series, How: how(m, pr.p, plansSprints, estimates)})
	}
	return v, preps
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	if p.cols.periodBound(m) {
		rng = "It counts the range's " + periods + " and compares them with the " + periods + " before."
	}
	return How{Words: m.Words, Counts: counts, Range: rng, Rules: "none", SQL: strings.TrimSpace(m.SQL), Reads: p.reads}
}

// evaluation is one opened, filled and sealed session with the board's
// rows the app reads beside the metrics.
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
	key, title, status, assignee, epic string
	points                             *float64
}

// open opens a session, fills it with b's export and seals it, then
// reads the rows every widget names.
func (e *Evaluator) open(ctx context.Context, b Board, loc *time.Location) (*evaluation, error) {
	s, err := openSession(ctx, loc)
	if err != nil {
		return nil, err
	}
	ev := &evaluation{s: s}
	if err := ev.fill(ctx, b); err != nil {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return ev, nil
}

func (ev *evaluation) fill(ctx context.Context, b Board) error {
	export, ok, err := b.Fill(ctx, ev.s.conn)
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
	ev.estimates = map[int]string{store.MirrorEstimatePoints: "points", store.MirrorEstimateTime: "time"}[export.EstimateUnit]
	if ev.estimates == "" {
		ev.estimates = "none"
	}
	return tooSlow(ctx, ev.readBoard(ctx))
}

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
	err := ev.query(ctx, `SELECT id, name, state, coalesce(start_at, 0), coalesce(completed_at, 0), position FROM sprints`, func(r *sql.Rows) error {
		var s sprintRow
		err := r.Scan(&s.id, &s.name, &s.state, &s.startAt, &s.completedAt, &s.position)
		ev.sprints = append(ev.sprints, s)
		return err
	})
	if err == nil {
		err = ev.query(ctx, `SELECT t.id, t.key, t.title, coalesce(s.name, ''), coalesce(t.assignee, ''), coalesce(t.epic, ''), t.points
			FROM tickets t LEFT JOIN statuses s ON s.id = t.status`, func(r *sql.Rows) error {
			var id string
			var t ticketRow
			err := r.Scan(&id, &t.key, &t.title, &t.status, &t.assignee, &t.epic, &t.points)
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

func (ev *evaluation) query(ctx context.Context, q string, scan func(*sql.Rows) error) error {
	rows, err := ev.s.conn.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (ev *evaluation) close() error { return ev.s.close() }

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

// run runs metric m's prepared statement p, narrowed by n. A failure of
// the metric is the widget's; one of the evaluation is returned.
func (ev *evaluation) run(ctx context.Context, m Metric, p prepared, n narrowing, now, since int64) (counted, error) {
	rows, err := ev.s.run(ctx, p, now, since)
	if ctx.Err() != nil {
		return counted{}, tooSlow(ctx, ctx.Err())
	}
	if me, ok := errors.AsType[*MetricError](err); ok {
		return counted{p: p, err: me}, nil
	}
	if err != nil {
		return counted{}, fmt.Errorf("count metric %s: %w", m.Name, err)
	}
	return counted{p: p, rows: ev.keep(n, p.cols, rows)}, nil
}

// opened is an evaluation begun: its session, the deadline's context, the
// dashboard of the board's kind with its metrics prepared, the range and
// the narrowing. end closes it.
type opened struct {
	ev      *evaluation
	ctx     context.Context
	end     func() error
	d       Dashboard
	metrics map[string]Metric
	view    DashboardView
	preps   map[string]prepResult
	sp      span
	n       narrowing
	now     time.Time
}

// begin is the common start of an evaluation and a drill: a slot, the
// deadline, the session, and the dashboard with its range and narrowing.
func (e *Evaluator) begin(ctx context.Context, b Board, r Request) (*opened, error) {
	loc, err := location(r.TimeZone)
	if err != nil {
		return nil, err
	}
	release, err := e.acquire(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, evaluationDeadline)
	ev, err := e.open(ctx, b, loc)
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	o := &opened{ev: ev, ctx: ctx, now: e.now(), end: func() error {
		err := ev.close()
		cancel()
		release()
		return err
	}}
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
	o.d, o.metrics = builtin(Kind{PlansSprints: plansSprints, Epics: b.Epics})
	if r.Dashboard != o.d.ID {
		return refuse("dashboard_unknown", fmt.Sprintf("this board has no dashboard %q; pick one from the list", truncate(r.Dashboard, 64)), nil)
	}
	rng := r.Range
	if rng == "" {
		_, rng = rangesOf(plansSprints)
	}
	var err error
	if o.sp, err = resolveSpan(rng, plansSprints, o.ev.sprints, o.now, loc); err != nil {
		return refuse("dashboard_request_invalid", err.Error(), nil)
	}
	if o.n, err = o.ev.narrowing(r, b.Viewer); err != nil {
		return err
	}
	o.view, o.preps = o.ev.s.view(o.d, o.metrics, o.ev.estimates)
	return nil
}

// count runs metric m once, its prepared statement from the view.
func (o *opened) count(m Metric) (counted, error) {
	pr := o.preps[m.Name]
	if pr.err != nil {
		return counted{err: pr.err}, nil
	}
	now := o.now.UnixMilli()
	return o.ev.run(o.ctx, m, pr.p, o.n, now, o.sp.since(now))
}

// Evaluate counts dashboard r.Dashboard of board b.
func (e *Evaluator) Evaluate(ctx context.Context, b Board, r Request) (_ Answer, err error) {
	o, err := e.begin(ctx, b, r)
	if err != nil {
		return Answer{}, err
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	a := Answer{Dashboard: o.view, Range: rangeView(o.sp), Estimates: o.ev.estimates, Person: o.n.person, Epic: o.n.epic, At: o.now.UnixMilli()}
	runs := map[string]counted{}
	for _, w := range o.d.Widgets {
		m := o.metrics[w.Metric]
		c, ok := runs[m.Name]
		if !ok {
			if c, err = o.count(m); err != nil {
				return Answer{}, err
			}
			runs[m.Name] = c
		}
		a.Widgets = append(a.Widgets, o.ev.widget(w, m, c, o.sp))
	}
	return a, nil
}

func rangeView(sp span) RangeView {
	v := RangeView{ID: sp.id, Label: sp.label, Previous: sp.previousLabel, PreviousGap: sp.previousGap, Note: sp.note, Periods: []PeriodView{}}
	for _, p := range sp.periods {
		v.Periods = append(v.Periods, PeriodView{ID: p.id, Label: p.label, Partial: p.partial})
	}
	return v
}

// inRange is c's rows placed in the range's periods, and the placement;
// all of them for a metric that counts now.
func inRange(m Metric, c counted, sp span) ([]contribution, map[string][]contribution) {
	if !c.p.cols.periodBound(m) {
		return c.rows, nil
	}
	placed := place(m, c.rows, sp.periods, sp.previous)
	var rows []contribution
	for _, p := range sp.periods {
		rows = append(rows, placed[p.id]...)
	}
	return rows, placed
}

// widget is w's numbers from its metric's rows.
func (ev *evaluation) widget(w Widget, m Metric, c counted, sp span) WidgetValue {
	out := WidgetValue{ID: w.ID}
	if c.err != nil {
		out.Error = c.err.Sentence
		return out
	}
	rows, placed := inRange(m, c, sp)
	switch w.Form {
	case FormNumber:
		if placed == nil {
			v := reduceRows(m.Reduce, rows)
			out.Value = &v
			return out
		}
		v := reduceRange(m.Reduce, sp.periods, placed)
		prev := gapNum
		if sp.previousGap == "" {
			prev = reduceRange(m.Reduce, sp.previous, placed)
		}
		out.Value, out.Previous = &v, &prev
		for _, p := range sp.periods {
			out.Trend = append(out.Trend, reduceRows(m.Reduce, placed[p.id]))
		}
	case FormList:
		rowsCopy := slices.Clone(rows)
		sortDrill(rowsCopy, ev.keyOf)
		for _, r := range rowsCopy {
			if !r.counted {
				out.NotCounted++
				continue
			}
			if len(out.Tickets) == maxListRows {
				out.More++
				continue
			}
			out.Tickets = append(out.Tickets, ev.ticket(r, c.p.cols))
		}
	default:
		out.Series = seriesOf(m, c.p.cols, c.rows)
		named := c.p.cols.has(ColumnSeries)
		cell := func(key, label string, partial bool, rows []contribution) BucketValue {
			b := BucketValue{Key: key, Label: label, Partial: partial, Total: reduceRows(m.Reduce, rows)}
			for _, s := range out.Series {
				b.Cells = append(b.Cells, reduceRows(m.Reduce, bySeries(rows, s, named)))
			}
			return b
		}
		if m.Bucket == BucketPeriod || m.Bucket == BucketSprint {
			for _, p := range sp.periods {
				out.Buckets = append(out.Buckets, cell(p.id, p.label, p.partial, placed[p.id]))
			}
			return out
		}
		keys, grouped := group(rows, func(c contribution) string { return c.bucket })
		for _, k := range keys {
			out.Buckets = append(out.Buckets, cell(k, ev.label(m.Bucket, k), false, grouped[k]))
		}
		slices.SortStableFunc(out.Buckets, func(a, b BucketValue) int {
			av, bv := -1.0, -1.0
			if a.Total.V != nil {
				av = *a.Total.V
			}
			if b.Total.V != nil {
				bv = *b.Total.V
			}
			if av != bv {
				if av > bv {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Label, b.Label)
		})
		limit := maxBars
		if w.Form == FormTable {
			limit = maxTableRows
		}
		if len(out.Buckets) > limit {
			out.More = len(out.Buckets) - limit
			out.Buckets = out.Buckets[:limit]
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
	default:
		return cmpOr(key, "(none)")
	}
}

func (ev *evaluation) keyOf(ticket string) string {
	if t, ok := ev.tickets[ticket]; ok {
		return t.key
	}
	return ticket
}

// ticket is row c's ticket as a list or a drill shows it.
func (ev *evaluation) ticket(c contribution, cols columns) TicketRow {
	t, ok := ev.tickets[c.ticket]
	r := TicketRow{ID: c.ticket, Key: c.ticket, Counted: c.counted, Note: c.note}
	if ok {
		r.Key, r.Title, r.Status, r.Points = t.key, t.title, t.status, t.points
		if t.assignee != "" {
			r.Assignee = cmpOr(ev.people[t.assignee], t.assignee)
		}
	}
	if c.counted && cols.has(ColumnValue) {
		v := c.value
		r.Value = &v
	}
	return r
}

// DrillRequest asks for the rows behind one number of a dashboard: the
// widget's number (Bucket empty), or one bar, point or table cell (Bucket
// its key, Series one of its series or empty for all of them). Page is
// the page of drillPage rows; Count is the row count page 0 answered, so
// a later page that finds a different count starts again at page 0.
type DrillRequest struct {
	Request
	Widget string `json:"widget"`
	Bucket string `json:"bucket,omitempty"`
	Series string `json:"series,omitempty"`
	Page   int    `json:"page"`
	Count  int    `json:"count,omitempty"`
}

// DrillAnswer is the number and one page of its rows, from one
// evaluation.
type DrillAnswer struct {
	Title     string      `json:"title"`
	Value     Num         `json:"value"`
	Unit      Unit        `json:"unit"`
	Estimates string      `json:"estimates"`
	Count     int         `json:"count"`
	Counted   int         `json:"counted"`
	Page      int         `json:"page"`
	Pages     int         `json:"pages"`
	Restarted bool        `json:"restarted,omitempty"`
	Rows      []TicketRow `json:"rows"`
	At        int64       `json:"at"`
}

// Drill evaluates the widget's metric again and answers the number r
// names with its rows.
func (e *Evaluator) Drill(ctx context.Context, b Board, r DrillRequest) (_ DrillAnswer, err error) {
	if r.Page < 0 {
		return DrillAnswer{}, refuse("dashboard_request_invalid", "a drill's page is 0 or more", nil)
	}
	o, err := e.begin(ctx, b, r.Request)
	if err != nil {
		return DrillAnswer{}, err
	}
	defer func() {
		if closeErr := o.end(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	i := slices.IndexFunc(o.d.Widgets, func(w Widget) bool { return w.ID == r.Widget })
	if i < 0 {
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("this dashboard has no widget %q", truncate(r.Widget, 64)), nil)
	}
	w := o.d.Widgets[i]
	m := o.metrics[w.Metric]
	c, err := o.count(m)
	if err != nil {
		return DrillAnswer{}, err
	}
	if c.err != nil {
		return DrillAnswer{}, refuse("dashboard_metric_failed", c.err.Sentence, nil)
	}
	ev, sp := o.ev, o.sp
	out := DrillAnswer{Title: cmpOr(w.Title, m.Title), Unit: m.Unit, Estimates: ev.estimates, Page: r.Page, At: o.now.UnixMilli(), Rows: []TicketRow{}}
	rows, placed := inRange(m, c, sp)
	switch {
	case r.Bucket == "":
		if placed == nil {
			out.Value = reduceRows(m.Reduce, rows)
		} else {
			out.Value = reduceRange(m.Reduce, sp.periods, placed)
			out.Title += ", " + strings.ToLower(sp.label[:1]) + sp.label[1:]
		}
	case m.Bucket == BucketPeriod || m.Bucket == BucketSprint:
		j := slices.IndexFunc(sp.periods, func(p period) bool { return p.id == r.Bucket })
		if j < 0 {
			return DrillAnswer{}, refuse("dashboard_request_invalid", "that bar is not in the range any more; read the dashboard again", nil)
		}
		p := sp.periods[j]
		out.Title += ", " + p.label
		rows = placed[p.id]
	default:
		var cell []contribution
		for _, row := range rows {
			if row.bucket == r.Bucket {
				cell = append(cell, row)
			}
		}
		rows = cell
		out.Title += ", " + ev.label(m.Bucket, r.Bucket)
	}
	if r.Bucket != "" {
		if r.Series != "" {
			rows = bySeries(rows, r.Series, c.p.cols.has(ColumnSeries))
			out.Title += ", " + r.Series
		}
		out.Value = reduceRows(m.Reduce, rows)
	}
	rows = slices.Clone(rows)
	sortDrill(rows, ev.keyOf)
	out.Count = len(rows)
	for _, row := range rows {
		if row.counted {
			out.Counted++
		}
	}
	out.Pages = max(1, (len(rows)+drillPage-1)/drillPage)
	if r.Page > 0 && (r.Count != out.Count || r.Page >= out.Pages) {
		out.Page, out.Restarted = 0, true
	}
	for _, row := range rows[min(len(rows), out.Page*drillPage):min(len(rows), (out.Page+1)*drillPage)] {
		out.Rows = append(out.Rows, ev.ticket(row, c.p.cols))
	}
	return out, nil
}
