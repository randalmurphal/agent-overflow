package trackerdash

import (
	"cmp"
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
	// Credit is whether the board's project counts credit, which its pace
	// counts beside what each period finished (creditMetric).
	Credit bool
}

// Board is the board an evaluation reads: Fill exports it, Epics is as
// Kind has it, Credit too, TimeTracking is whether its tracker logs time (Jira), and
// Own is what its project keeps and its agents proposed beside the
// built-in dashboard. The viewer, whom Mine narrows to and whose logged
// time the copy holds, is the export's (store.MirrorExport.Viewer), read
// in the snapshot its rows are. Worklogs is the reads of the viewer's
// logged time (worklogs.go): an evaluation or a Try in which a widget
// counted it tells them what it needs, once, and answers the state they
// return; nil, there are none. It must not wait on I/O.
// EvidenceFailure is why the last read of who worked on the connection's
// tickets failed, "" while none did, which each value of a widget of
// credit says (WidgetValue.Evidence).
type Board struct {
	Fill            Fill
	Epics           bool
	Credit          bool
	TimeTracking    bool
	Own             Own
	Worklogs        func(WorklogNeed) WorklogState
	EvidenceFailure string
}

// Request is what one evaluation counts: a dashboard, over a range,
// narrowed to Mine (the viewer), a person or an epic, with weeks and days
// in TimeZone (IANA). With Widgets, the dashboard is those widgets of the
// board's dashboards in that order (a board's summary row): the built-in
// one's, which Dashboard names, and the project's own; a widget the board
// no longer has is left out, and the proposals are not counted.
type Request struct {
	Dashboard string   `json:"dashboard"`
	Range     string   `json:"range"`
	Mine      bool     `json:"mine,omitempty"`
	Person    string   `json:"person,omitempty"`
	Epic      string   `json:"epic,omitempty"`
	TimeZone  string   `json:"time_zone"`
	Widgets   []string `json:"widgets,omitempty"`
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
// sprints). Assignee is the person the row counts for: the metric's
// person column, else the ticket's assignee. Period is the sprint or week
// a row of the range falls in, Series the metric's series it is in, and
// Heading the drill's heading of it within the series.
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
	Series   string   `json:"series,omitempty"`
	Heading  string   `json:"heading,omitempty"`
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
	// Unread is the range's working days of the viewer's logged time the
	// copy holds no read of, for a metric that reads it (worklogs.go).
	Unread []string `json:"unread,omitempty"`
	// Old is a list's or a ranked chart's whose rows reach a day of the
	// viewer's logged time older than the copy holds, so it is never
	// whole, even with no rows (worklogs.go); a number says so in its Num.
	Old bool `json:"old,omitempty"`
	// Evidence says, beside a widget whose metric reads the credit
	// relation while the last read of who worked on the tickets failed as
	// a whole, that it failed and why (EvidenceSentence); "" otherwise.
	Evidence string `json:"evidence,omitempty"`
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
	// Logged is the state of the reads of the viewer's logged time
	// (Board.Worklogs) when a widget counted it.
	Logged *WorklogState `json:"logged,omitempty"`
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
	case UnitTime:
		return "seconds"
	case UnitEstimate:
	}
	return map[string]string{"points": "points", "time": "seconds", "none": "tickets"}[estimates]
}

// how renders How this is counted from m's definition.
func how(m Metric, p prepared, plansSprints bool, estimates string) How {
	unit := map[Unit]string{UnitTickets: "tickets", UnitDays: "days", UnitTime: "hours logged"}[m.Unit]
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
	switch {
	case p.cols.has(ColumnPerson):
		counts += "; each row counts for the person the query names."
	case p.readsWorklogs():
		counts += "; each row of logged time counts for you, whose time it is."
	default:
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
	rules := "none"
	if slices.Contains(p.asks.tables, "credit") {
		rules = "the project's credit rules, in its Issues settings"
	}
	return How{Words: m.Words, Counts: counts, Range: rng, Rules: rules, SQL: strings.TrimSpace(m.SQL), Reads: p.asks.tables}
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
	s      *session
	loc    *time.Location
	export store.MirrorExport
	// readDays is the days of the viewer's (export.Viewer) logged time the
	// copy holds as read, filled when a metric reads worklogs.
	readDays map[string]bool
	// now is the evaluation's instant. keptFrom is the first instant of
	// the copy's first day of logged time (export.LoggedFrom) in the
	// request's zone, before which it holds none (reach). windowDays is
	// the days from it up to now not read, once windowRead
	// (windowUnread). logged counts the widgets that needed the window
	// (wantWindow), so the reads hear of them (Board.Worklogs).
	now        int64
	keptFrom   int64
	windowDays []string
	windowRead bool
	logged     int
	estimates  string
	sprints    []sprintRow
	tickets    map[string]ticketRow
	people     map[string]string
	epics      map[string]Name
}

type ticketRow struct {
	key, assignee, epic string
	created, updated    int64
}

// open opens a session, fills it with b's export of relations, with the
// viewer's logged time up to today in loc at now, and seals it, then
// reads the rows every widget names. asked is what each metric
// reads of the copy, from its plan on an empty session.
func (e *Evaluator) open(ctx context.Context, b Board, loc *time.Location, now time.Time, relations []string, asked map[string]asks) (*evaluation, error) {
	s, err := openSession(ctx, loc)
	if err != nil {
		return nil, err
	}
	s.asked = asked
	ev := &evaluation{s: s, loc: loc, now: now.UnixMilli()}
	if err := ev.fill(ctx, b, relations, now.In(loc).Format(time.DateOnly)); err != nil {
		if closeErr := s.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return ev, nil
}

func (ev *evaluation) fill(ctx context.Context, b Board, relations []string, loggedThrough string) error {
	export, ok, err := b.Fill(ctx, ev.s.conn, relations, loggedThrough)
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
	if err := ev.readBoard(ctx); err != nil || !slices.Contains(relations, "worklog_days") {
		return tooSlow(ctx, err)
	}
	first, err := time.Parse(time.DateOnly, export.LoggedFrom)
	if err != nil {
		return fmt.Errorf("the copy's first day of logged time %q is not a date: %w", export.LoggedFrom, err)
	}
	ev.keptFrom = placed(first, ev.loc) - 1
	ev.readDays = map[string]bool{}
	return tooSlow(ctx, ev.query(ctx, `SELECT day FROM worklog_days`, func(r *sql.Rows) error {
		var day string
		err := r.Scan(&day)
		ev.readDays[day] = true
		return err
	}))
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

func (ev *evaluation) narrowing(r Request) (narrowing, error) {
	var n narrowing
	viewer := ev.export.Viewer
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

// keeps is whether the narrowed page keeps row c, nil for a page not
// narrowed: a row's person is the metric's person column, else the
// viewer for a metric of the viewer's logged time (p reads worklogs),
// else its ticket's assignee; its epic likewise. A row of a metric with a
// person column that is not counted and names no person could be
// anyone's (credit whose evidence is not read), so every person narrowing
// keeps it, as not counted. The row sink keeps no other row (count).
func (ev *evaluation) keeps(n narrowing, p prepared) func(c contribution) bool {
	if n.person == nil && n.epic == nil {
		return nil
	}
	cols := p.cols
	return func(c contribution) bool {
		t := ev.tickets[c.ticket]
		person, epic := t.assignee, t.epic
		anyone := false
		if p.readsWorklogs() {
			person = ev.export.Viewer
		}
		if cols.has(ColumnPerson) {
			person, anyone = c.person, !c.counted && c.person == ""
		}
		if cols.has(ColumnEpic) {
			epic = c.epic
		}
		return (n.person == nil || person == n.person.ID || anyone) && (n.epic == nil || epic == n.epic.ID)
	}
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

// counted is one metric's rows, narrowed, or the sentence of its failure,
// or why it counts nothing on this page (note, worklogNote).
type counted struct {
	p    prepared
	rows []contribution
	err  *MetricError
	note string
}

// run runs metric m's prepared statement p for the rows k keeps. A
// failure of the metric is the widget's; one of the evaluation is
// returned.
func (ev *evaluation) run(ctx context.Context, m Metric, p prepared, now, since int64, k kept) (counted, error) {
	rows, err := ev.s.run(ctx, p, now, since, ev.estimates, k)
	if ctx.Err() != nil {
		return counted{}, tooSlow(ctx, ctx.Err())
	}
	if me, ok := errors.AsType[*MetricError](err); ok {
		return counted{p: p, err: me}, nil
	}
	if err != nil {
		return counted{}, fmt.Errorf("count metric %s: %w", m.Name, err)
	}
	if m.numbersUnit() == UnitEstimate && ev.estimates == "none" {
		// The board counts tickets (it has no estimates, or its project
		// counts tickets): each counted row is its share of a ticket, one
		// unless the query says (ColumnShare), whatever value it gave it,
		// and a row it did not count stays uncounted.
		for i := range rows {
			if rows[i].counted {
				rows[i].value = rows[i].share
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
	// timeTracking is Board.TimeTracking.
	timeTracking bool
	// evidenceFailure is Board.EvidenceFailure.
	evidenceFailure string
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
	o := &opened{now: e.now(), evidenceFailure: b.EvidenceFailure}
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
		ev, err = e.open(ctx, b, loc, o.now, relations, asked)
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
	o.timeTracking = b.TimeTracking
	if o.n, err = o.ev.narrowing(r); err != nil {
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
// its copy is read (resolve), or one of b.Own's, with no kind. With
// r.Widgets it is those widgets of the built-in and b.Own's dashboards,
// with the built-in's kind.
func dashboardOf(r Request, b Board) (Dashboard, map[string]Metric, *Kind, error) {
	metrics := maps.Clone(b.Own.Metrics)
	if metrics == nil {
		metrics = map[string]Metric{}
	}
	if r.Dashboard == builtinSprintsID || r.Dashboard == builtinWeeksID {
		k := Kind{PlansSprints: r.Dashboard == builtinSprintsID, Epics: b.Epics, Credit: b.Credit}
		d, builtins := builtin(k)
		maps.Copy(metrics, builtins)
		if r.Widgets != nil {
			d = Dashboard{ID: d.ID, Title: d.Title, Widgets: picked(r.Widgets, append([]Dashboard{d}, b.Own.Dashboards...))}
		}
		return d, metrics, &k, nil
	}
	if r.Widgets != nil {
		return Dashboard{}, nil, nil, refuse("dashboard_request_invalid", "a request that names widgets names the built-in dashboard", nil)
	}
	for _, d := range b.Own.Dashboards {
		if d.ID == r.Dashboard {
			return d, metrics, nil, nil
		}
	}
	return Dashboard{}, nil, nil, unknownDashboard(r.Dashboard)
}

// picked is the widgets of ds that ids name, in ids' order, each once;
// an ID no widget has is left out.
func picked(ids []string, ds []Dashboard) []Widget {
	var out []Widget
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			continue
		}
		for _, d := range ds {
			if j := slices.IndexFunc(d.Widgets, func(w Widget) bool { return w.ID == id }); j >= 0 {
				out = append(out, d.Widgets[j])
				break
			}
		}
	}
	return out
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
		if slices.Contains(a.tables, "worklogs") {
			// Which days of it were read (worklogs.go).
			out = append(out, "worklog_days")
		}
	}
	slices.Sort(out)
	return slices.Compact(out), asked, nil
}

// count runs metric m once, its prepared statement from the view, for
// the rows its widgets take (r, reads.kept) of the narrowed page
// (evaluation.keeps): the sink holds and charges no other row.
func (o *opened) count(m Metric, r reads) (counted, error) {
	pr := o.preps[m.Name]
	if pr.err != nil {
		return counted{err: pr.err}, nil
	}
	if note := o.worklogNote(pr.p); note != "" {
		return counted{p: pr.p, note: note}, nil
	}
	now := o.now.UnixMilli()
	k := r.kept(m, pr.p.cols, now, o.ev.spanOf)
	if narrowed := o.ev.keeps(o.n, pr.p); narrowed != nil {
		taken := k.keep
		k.keep = func(c contribution) bool { return narrowed(c) && (taken == nil || taken(c)) }
	}
	return o.ev.run(o.ctx, m, pr.p, now, o.sp.since(now), k)
}

// Evaluate counts dashboard r.Dashboard of board b and then the board's
// proposals, each in an evaluation of its own: its own copy of the board,
// of what its widgets read, closed before the next opens, and its own
// deadline. What the proposals read or take never reaches the
// dashboard's widgets, which count first with their tickets' titles; a
// proposal its evaluation does not count says why in its own place
// (proposals) instead of failing the answer. A request that names its
// widgets counts only those.
func (e *Evaluator) Evaluate(ctx context.Context, b Board, r Request) (Answer, error) {
	a, need, err := e.dashboard(ctx, b, r)
	if err == nil && len(b.Own.Proposals) > 0 && r.Widgets == nil {
		var proposed []WidgetValue
		var also *WorklogNeed
		if proposed, also, err = e.proposals(ctx, b, r); err != nil {
			return Answer{}, err
		}
		a.Widgets = append(a.Widgets, proposed...)
		need = cmp.Or(need, also)
	}
	if err != nil {
		return a, err
	}
	a.Logged = tellWorklogs(b, need)
	return a, nil
}

// dashboard is the evaluation of dashboard r.Dashboard's own widgets, and
// what it needs of the viewer's logged time.
func (e *Evaluator) dashboard(ctx context.Context, b Board, r Request) (_ Answer, _ *WorklogNeed, err error) {
	o, err := e.begin(ctx, b, r, func(_ Widget, proposed bool) bool { return !proposed })
	if err != nil {
		return Answer{}, nil, err
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
				return Answer{}, nil, err
			}
			runs[m.Name] = c
		}
		a.Widgets = append(a.Widgets, o.widget(w, m, c))
	}
	if err := o.ev.titles(o.ctx, listed(a.Widgets)); err != nil {
		return Answer{}, nil, err
	}
	return a, o.worklogNeed(), nil
}

// widget is the value of widget w drawing metric m from c, as every
// answer gives it (ev.widget), with the sentence beside it
// (EvidenceSentence): while the evidence unit's last read of the board's
// connection failed as a whole, for a metric that reads the credit
// relation, whatever its rows hold.
func (o *opened) widget(w Widget, m Metric, c counted) WidgetValue {
	v := o.ev.widget(w, m, c, o.sp)
	if slices.Contains(o.preps[m.Name].p.asks.tables, "credit") {
		v.Evidence = EvidenceSentence(o.evidenceFailure)
	}
	return v
}

// EvidenceSentence is what a screen says beside credit it counts while
// the evidence unit's last read of who worked on the tickets failed as a
// whole with failure: what it counts may lack what that read was to
// read. "" for none (Board.EvidenceFailure, WidgetValue.Evidence).
func EvidenceSentence(failure string) string {
	if failure == "" {
		return ""
	}
	return "Credit may not count everything: the last read of who worked on the tickets failed. " + failure
}

// proposals is the values of b's proposals over r's range and narrowing,
// from an evaluation of their own, each proposal's rows let go once its
// value is drawn. A proposal the deadline stops, or leaves no time for,
// says so (outOfTime), and a failure of the evaluation itself is each
// proposal's sentence; only the caller's leaving fails the call. It
// returns what they need of the viewer's logged time.
func (e *Evaluator) proposals(ctx context.Context, b Board, r Request) (_ []WidgetValue, _ *WorklogNeed, err error) {
	failed := func(err error) ([]WidgetValue, *WorklogNeed, error) {
		if ctx.Err() != nil {
			return nil, nil, err
		}
		sentence := proposalsFailed(err)
		out := make([]WidgetValue, 0, len(b.Own.Proposals))
		for _, p := range b.Own.Proposals {
			out = append(out, WidgetValue{ID: p.Widget.ID, Error: sentence})
		}
		return out, nil, nil
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
		out = append(out, o.widget(w, m, c))
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
	return out, o.worklogNeed(), nil
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
// (inRange). Its rows are handed out only by its takes, each with what
// they need of the viewer's logged time (taken), so every widget form,
// drill and Try reads a period the same way and no number is made from
// rows of logged time without it (num). The takes are closures over the
// rows: nothing else holds them.
type ranged struct {
	// periods is what the metric counts by: the range's periods, or the
	// sprint's days for a metric grouped by day; nil for one that counts
	// now.
	periods []period
	// events is an event metric's counted by period, whose number is its
	// periods' (whole); state is a state metric's, whose number is what
	// holds now (held). compared is false when there is nothing to
	// compare (the sprint's days).
	events, state, compared bool
	// logged is whether the metric reads the viewer's logged time
	// (needsWindow).
	logged bool
	// notCounted is the range's tickets that its rows with no instant
	// leave not counted (notCounted).
	notCounted int
	// held is the range's rows: all of the metric's for one that counts
	// now, those that hold now for a state, those in the periods for an
	// event. whole is the rows the range's number counts: an event's in
	// the periods with its rows of no instant; held for the others.
	// earlier is what that number compares with: an event's previous
	// range, a state's rows that held where the range begins. listed is
	// held with the rows of no instant. of is period p's rows: none for a
	// period still to come, which has not happened; a state's rows that
	// hold at its end (holdsAt), of its sprints for one grouped by sprint,
	// valid until the next call; an event's that fall in it.
	held, whole, earlier, listed func() taken
	of                           func(p period) taken
}

// taken is rows a reduction takes from a ranged (worklogs.go).
type taken struct {
	rows []contribution
	// periods is how many periods the rows are of, which an average of
	// period sums divides by; 0 for rows of no range.
	periods int
	// none is nothing to reduce: a period still to come, a range with no
	// period.
	none bool
	// cover is what the rows need of the viewer's logged time.
	cover cover
}

// sub is rows, a part of t's rows, as t takes them.
func (t taken) sub(rows []contribution) taken {
	t.rows = rows
	return t
}

// spanOf is ticket's creation and last update: where a row of it with
// no instant could have happened (place).
func (ev *evaluation) spanOf(ticket string) (from, to int64) {
	t := ev.tickets[ticket]
	return t.created, t.updated
}

// inRange is c's rows sorted into the range sp and the one before it, or
// into the sprint's days for a metric grouped by day; a state's rows are
// read where they hold.
func (ev *evaluation) inRange(m Metric, c counted, sp span) *ranged {
	logged := c.p.readsWorklogs()
	// A state at t holds the instant t alone; a period holds the instants
	// after its start, so rows placed in periods can hold the instant
	// after the earliest start.
	at := func(t int64) cover { return ev.reach(logged, t) }
	from := func(ps []period) cover {
		first := ps[0].from
		for _, p := range ps {
			first = min(first, p.from)
		}
		return ev.reach(logged, first+1)
	}
	nothing := func() taken { return taken{none: true} }
	if !c.p.cols.periodBound(m) {
		all := func() taken { return taken{rows: c.rows, cover: at(sp.now)} }
		return &ranged{logged: logged, held: all, whole: all, earlier: nothing, listed: all, of: func(period) taken { return taken{none: true} }}
	}
	periods, previous := periodsOf(m, sp), sp.previous
	if m.Bucket == BucketDay {
		previous = nil
	}
	if c.p.cols.state() {
		g := &ranged{periods: periods, state: true, logged: logged, earlier: nothing}
		now := func() taken { return taken{rows: holding(c.rows, sp.now), cover: at(sp.now)} }
		g.held, g.whole, g.listed = now, now, now
		if m.Bucket != BucketDay && len(periods) > 0 {
			start := periods[0].from
			g.earlier = func() taken { return taken{rows: holding(c.rows, start), cover: at(start)} }
			g.compared = true
		}
		bySprint := m.Bucket == BucketSprint
		var buf []contribution
		g.of = func(p period) taken {
			if p.future {
				return taken{none: true}
			}
			buf = buf[:0]
			for _, r := range c.rows {
				if p.holdsState(r, bySprint) {
					r.period = p.label
					buf = append(buf, r)
				}
			}
			return taken{rows: buf, cover: at(p.to)}
		}
		return g
	}
	placed, unplaced := place(m, c.rows, ev.spanOf, periods, previous)
	over := func(ps []period, loose []contribution) taken {
		if len(ps) == 0 {
			return taken{none: true}
		}
		rows := slices.Clone(loose)
		for _, p := range ps {
			rows = append(rows, placed[p.id]...)
		}
		return taken{rows: rows, periods: len(ps), cover: from(ps)}
	}
	var inPeriods []contribution
	for _, p := range periods {
		inPeriods = append(inPeriods, placed[p.id]...)
	}
	held := taken{rows: inPeriods, none: len(periods) == 0}
	if len(periods) > 0 {
		held.cover = from(periods)
	}
	return &ranged{
		periods: periods, events: true, compared: m.Bucket != BucketDay, logged: logged, notCounted: notCounted(unplaced[0]),
		held:    func() taken { return held },
		whole:   func() taken { return over(periods, unplaced[0]) },
		earlier: func() taken { return over(previous, unplaced[1]) },
		listed:  func() taken { return held.sub(append(slices.Clone(inPeriods), unplaced[0]...)) },
		of: func(p period) taken {
			if p.future {
				return taken{none: true}
			}
			return taken{rows: placed[p.id], cover: from([]period{p})}
		},
	}
}

// periodsOf is what metric m counts by in span sp: the range's periods,
// or the sprint's days for a metric grouped by day.
func periodsOf(m Metric, sp span) []period {
	if m.Bucket == BucketDay {
		return sp.days
	}
	return sp.periods
}

// reads is what widgets take of a metric's rows (inRange): of a state
// metric, the rows that hold now, where the range begins, or at the end
// of periods; of an event metric placed in periods, the rows of the
// periods it takes (takes). A metric that runs for several widgets runs
// for all of what they read (and). kept is the rows they take, which is
// all the sink keeps (count).
type reads struct {
	now, start bool
	periods    []period
	day        bool
	takes      []period
	// unplaced is whether they take an event row of no instant, which
	// place puts in no period: a widget counts it as not counted, a cell
	// does not hold it.
	unplaced bool
}

func (r reads) and(o reads) reads {
	r.now, r.start, r.day, r.unplaced = r.now || o.now, r.start || o.start, r.day || o.day, r.unplaced || o.unplaced
	union := func(to, from []period) []period {
		for _, p := range from {
			if !slices.ContainsFunc(to, func(q period) bool { return q.id == p.id }) {
				to = append(to, p)
			}
		}
		return to
	}
	r.periods, r.takes = union(r.periods, o.periods), union(r.takes, o.takes)
	return r
}

// widgetReads is what widget w reads of metric m. Its states: a number
// now, where the range begins and at its periods' ends (its trend); a
// list, and a chart whose buckets are not periods, now; a chart of
// periods at their ends. Its events: every form the range's periods, and
// a number the previous range's too when it compares with it, which it
// does not across a gap or by the sprint's days (widget).
func widgetReads(w Widget, m Metric, sp span) reads {
	ps := periodsOf(m, sp)
	takes := ps
	if w.Form == FormNumber && m.Bucket != BucketDay && sp.previousGap == "" {
		takes = slices.Concat(ps, sp.previous)
	}
	switch {
	case w.Form == FormNumber:
		return reads{now: true, start: m.Bucket != BucketDay && len(ps) > 0, periods: ps, day: m.Bucket == BucketDay, takes: takes, unplaced: true}
	case w.Form == FormList || ranks(w, m):
		return reads{now: true, takes: takes, unplaced: true}
	}
	return reads{periods: ps, day: m.Bucket == BucketDay, takes: takes, unplaced: true}
}

// kept is the rows of metric m, of columns cols, that r takes, by the
// rules the reduction selects them by (inRange): every row of a metric
// that counts now; a state row that holds at an instant r reads, now or
// where the range begins in any sprint (holdsAt), or at the end of a
// period r reads, in the period's own sprints for a metric by sprint
// (period.holdsState); an event row in a period r takes (period.places),
// one with no instant, where r takes those, when its ticket's life
// (spanOf) meets such a period (period.meets), as place puts it.
func (r reads) kept(m Metric, cols columns, now int64, spanOf func(ticket string) (from, to int64)) kept {
	bySprint := m.Bucket == BucketSprint
	switch {
	case cols.state():
		at, ends, where := r.instants(now, bySprint)
		return kept{where: where, shorter: len(at)+len(ends) != 1, keep: func(c contribution) bool {
			i := 0
			if c.hasAt {
				i, _ = slices.BinarySearch(at, c.at)
			}
			if i < len(at) && c.holdsAt(at[i]) {
				return true
			}
			return slices.ContainsFunc(ends, func(p period) bool { return p.holdsState(c, true) })
		}}
	case cols.periodBound(m):
		return kept{keep: func(c contribution) bool {
			if bySprint || c.hasAt {
				return slices.ContainsFunc(r.takes, func(p period) bool { return p.places(c, bySprint) })
			}
			if !r.unplaced {
				return false
			}
			from, to := spanOf(c.ticket)
			return slices.ContainsFunc(r.takes, func(p period) bool { return p.meets(from, to) })
		}}
	}
	return kept{}
}

// instants is where r reads a state metric at now, a period to come left
// out: the instants read in any sprint, sorted (at: now, where the range
// begins, and a period's end for a metric not by sprint), the periods
// whose end is read in the period's own sprints for a metric by sprint
// (ends), and where they are in words.
func (r reads) instants(now int64, bySprint bool) (at []int64, ends []period, where string) {
	var words []string
	if r.now {
		at = append(at, now)
		words = append(words, "now")
	}
	if r.start {
		at = append(at, r.periods[0].from)
		words = append(words, "where the range begins")
	}
	n := 0
	for _, p := range r.periods {
		switch {
		case p.future:
			continue
		case bySprint:
			ends = append(ends, p)
		default:
			at = append(at, p.to)
		}
		n++
	}
	switch {
	case n == 1 && len(r.periods) == 1:
		words = append(words, "at the end of "+r.periods[0].label)
	case n > 0 && r.day:
		words = append(words, "at the end of a sprint day")
	case n > 0:
		words = append(words, "at the end of a period")
	}
	slices.Sort(at)
	at = slices.Compact(at)
	switch len(words) {
	case 0:
	case 1:
		where = words[0]
	case 2:
		where = words[0] + " or " + words[1]
	default:
		where = strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1]
	}
	return at, ends, where
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

// rank is the buckets of t's rows as widget w shows them: by total,
// largest first, then by label; the first limit shown, the rest left out.
// The widget and its drill of the rest take them from here.
func (ev *evaluation) rank(w Widget, m Metric, t taken) (shown, rest []ranked, grouped map[string][]contribution) {
	keys, grouped := group(t.rows, func(c contribution) string { return c.bucket })
	all := make([]ranked, 0, len(keys))
	for _, k := range keys {
		all = append(all, ranked{key: k, label: ev.label(m.Bucket, k), total: ev.num(m.Reduce, t.sub(grouped[k]))})
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

// widget is w's numbers from its metric's rows. It lists the window's
// days of logged time not read when one of its numbers needed them.
func (ev *evaluation) widget(w Widget, m Metric, c counted, sp span) (out WidgetValue) {
	out = WidgetValue{ID: w.ID}
	if c.err != nil {
		out.Error = c.err.Sentence
		return out
	}
	if c.note != "" {
		out.Note = c.note
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
	if g.needsWindow(w, m) {
		out.Unread = ev.wantWindow()
	}
	switch w.Form {
	case FormNumber:
		v := ev.num(m.Reduce, g.whole())
		out.Value = &v
		if g.periods == nil && !g.state && !g.events {
			return out
		}
		prev := gapNum
		switch {
		case g.state && sp.previousGap == firstReadGap:
			// What held where the range begins needs no earlier sprints:
			// only a copy still in its first read has nothing to compare
			// with.
		case g.state || sp.previousGap == "":
			prev = ev.num(m.Reduce, g.earlier())
		}
		if g.compared {
			out.Previous = &prev
		}
		for _, p := range g.periods {
			out.Trend = append(out.Trend, ev.num(m.Reduce, g.of(p)))
		}
	case FormList:
		t := g.listed()
		_, out.Old = ev.lacks(t.cover)
		rows := slices.Clone(t.rows)
		sortDrill(rows, ev.keyOf)
		out.NotCounted = notCounted(t.rows)
		for _, r := range rows {
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
		cell := func(key, label string, partial bool, total Num, t taken) BucketValue {
			b := BucketValue{Key: key, Label: label, Partial: partial, Total: total}
			for _, s := range out.Series {
				b.Cells = append(b.Cells, ev.num(m.Reduce, t.sub(bySeries(t.rows, s, named))))
			}
			return b
		}
		// The range's rows with no instant are in no bucket; its "not
		// counted" lists them and opens the whole widget.
		out.NotCounted = g.notCounted
		if !ranks(w, m) {
			for _, p := range g.periods {
				t := g.of(p)
				b := cell(p.id, p.label, p.partial, ev.num(m.Reduce, t), t)
				b.Last = p.last
				out.Buckets = append(out.Buckets, b)
			}
			return out
		}
		// The shown buckets are chosen by their totals before any of their
		// cells is made, so an answer holds at most limit buckets of the
		// declared series' cells however many buckets the rows name.
		t := g.held()
		_, out.Old = ev.lacks(t.cover)
		shown, rest, grouped := ev.rank(w, m, t)
		out.More = len(rest)
		for _, b := range shown {
			out.Buckets = append(out.Buckets, cell(b.key, b.label, false, b.total, t.sub(grouped[b.key])))
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
	case BucketNone, BucketPeriod, BucketSprint, BucketDay, BucketText:
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

// ticket is row c, the answer's row i, as a list or a drill shows it:
// with the person the row counts for (its person column, else its
// ticket's assignee) and its series.
func (ev *evaluation) ticket(c contribution, m Metric, cols columns, i int) TicketRow {
	t, ok := ev.tickets[c.ticket]
	r := TicketRow{Row: i, ID: c.ticket, Key: c.ticket, Counted: c.counted, Period: c.period, Series: c.series, Heading: c.heading, Note: c.note}
	person := t.assignee
	if cols.has(ColumnPerson) {
		person = c.person
	}
	if ok {
		r.Key = t.key
	}
	if person != "" {
		r.Assignee = cmpOr(ev.people[person], person)
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
// page 0. With IDs the answer names every ticket of the rows (the set a
// tile filters the board to) and lists no page.
type DrillRequest struct {
	Request
	Widget string     `json:"widget"`
	Cell   *DrillCell `json:"cell,omitempty"`
	Rest   bool       `json:"rest,omitempty"`
	IDs    bool       `json:"ids,omitempty"`
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
// Reading is as an Answer's. Asked for IDs, it holds those tickets' IDs
// in the drill's order, and no page.
type DrillAnswer struct {
	Title     string       `json:"title"`
	Reading   bool         `json:"reading,omitempty"`
	Value     Num          `json:"value"`
	Unit      Unit         `json:"unit"`
	Estimates string       `json:"estimates"`
	Count     int          `json:"count"`
	Tickets   int          `json:"tickets"`
	Counted   int          `json:"counted"`
	Page      int          `json:"page"`
	Pages     int          `json:"pages"`
	Restarted bool         `json:"restarted,omitempty"`
	Rows      []TicketRow  `json:"rows"`
	Groups    []DrillGroup `json:"groups,omitempty"`
	IDs       []string     `json:"ids,omitempty"`
	At        int64        `json:"at"`
}

// DrillGroup is one group of a drill's rows, of a metric that returns a
// series or a heading column: its series (in the metric's order) and its
// heading within the series (none first, then by name), the number of its
// rows alone as the drill counts its own (Value), and how many rows it
// lists over every page (Rows). The rows of every page are in the
// groups' order.
type DrillGroup struct {
	Series  string `json:"series,omitempty"`
	Heading string `json:"heading,omitempty"`
	Value   Num    `json:"value"`
	Rows    int    `json:"rows"`
}

// drillGroups orders rows, sorted as sortDrill sorts them, into the
// groups of a metric that returns a series or a heading column, keeping
// each group's order, and returns the groups, each with the number of
// its part of counts, the rows the drill's own number reduces; nil for a
// metric that returns neither.
func (ev *evaluation) drillGroups(rows []contribution, m Metric, cols columns, counts taken) []DrillGroup {
	if !cols.has(ColumnSeries) && !cols.has(ColumnHeading) {
		return nil
	}
	type group struct{ series, heading string }
	parts := map[group][]contribution{}
	for _, row := range counts.rows {
		k := group{row.series, row.heading}
		parts[k] = append(parts[k], row)
	}
	slices.SortStableFunc(rows, func(a, b contribution) int {
		if a.series != b.series {
			return cmp.Compare(slices.Index(m.Series, a.series), slices.Index(m.Series, b.series))
		}
		if (a.heading == "") != (b.heading == "") {
			if a.heading == "" {
				return -1
			}
			return 1
		}
		return strings.Compare(a.heading, b.heading)
	})
	var out []DrillGroup
	for from := 0; from < len(rows); {
		to := from + 1
		for to < len(rows) && rows[to].series == rows[from].series && rows[to].heading == rows[from].heading {
			to++
		}
		k := group{rows[from].series, rows[from].heading}
		out = append(out, DrillGroup{Series: k.series, Heading: k.heading, Value: ev.num(m.Reduce, counts.sub(parts[k])), Rows: to - from})
		from = to
	}
	return out
}

// drillReads is what drill r of widget w reads of metric m (reads): the
// period its cell names, at its end for a state; else, as the range's
// number does (Drill), its states now and its events of the range's
// periods.
func drillReads(w Widget, m Metric, sp span, r DrillRequest) reads {
	ps := periodsOf(m, sp)
	if r.Cell == nil || r.Rest || ranks(w, m) {
		return reads{now: true, takes: ps, unplaced: true}
	}
	for _, p := range ps {
		if p.id == r.Cell.Bucket {
			return reads{periods: []period{p}, day: m.Bucket == BucketDay, takes: []period{p}}
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
	return o.drill(w, m, c, r)
}

// drill answers the number r names of widget w from c, its metric m's
// rows.
func (o *opened) drill(w Widget, m Metric, c counted, r DrillRequest) (DrillAnswer, error) {
	if c.err != nil {
		return DrillAnswer{}, refuse("dashboard_metric_failed", c.err.Sentence, nil)
	}
	if c.note != "" {
		return DrillAnswer{}, refuse("dashboard_request_invalid", c.note, nil)
	}
	ev, sp := o.ev, o.sp
	out := DrillAnswer{Title: cmpOr(w.Title, m.Title), Reading: !ev.export.FirstReadDone, Unit: m.numbersUnit(), Estimates: ev.estimates, Page: r.Page, At: o.now.UnixMilli(), Rows: []TicketRow{}}
	if m.Bucket == BucketDay && sp.daysNote != "" {
		return DrillAnswer{}, refuse("dashboard_request_invalid", sp.daysNote, nil)
	}
	g := ev.inRange(m, c, sp)
	// t is the rows the drill lists, and ranged the range's rows its
	// number reduces when they are not the rows listed.
	t := g.held()
	var ranged *taken
	switch {
	case r.Cell != nil && r.Rest:
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("a drill of widget %q names one cell or the rest, not both", truncate(w.ID, 64)), nil)
	case r.Cell != nil && (w.Form == FormNumber || w.Form == FormList):
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q is one number; its drill names no bar or cell", truncate(w.ID, 64)), nil)
	case r.Rest && !ranks(w, m):
		return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q shows all it counts; its drill names no rest", truncate(w.ID, 64)), nil)
	case r.Rest:
		_, rest, grouped := ev.rank(w, m, t)
		var rows []contribution
		for _, b := range rest {
			rows = append(rows, grouped[b.key]...)
		}
		t = t.sub(rows)
		out.Title += fmt.Sprintf(", %d more%s", len(rest), map[Bucket]string{BucketPerson: " people", BucketEpic: " epics"}[m.Bucket])
	case r.Cell == nil && !g.events:
	case r.Cell == nil:
		if m.Bucket == BucketDay {
			out.Title += ", " + sp.daysLabel
		} else {
			out.Title += ", " + strings.ToLower(sp.label[:1]) + sp.label[1:]
		}
		// The number is the range's; the rows are listed with those of no
		// instant.
		whole := g.whole()
		ranged, t = &whole, g.listed()
	case !ranks(w, m):
		j := slices.IndexFunc(g.periods, func(p period) bool { return p.id == r.Cell.Bucket })
		if j < 0 {
			return DrillAnswer{}, refuse("dashboard_request_invalid", "that bar is not in the range any more; read the dashboard again", nil)
		}
		p := g.periods[j]
		out.Title += ", " + p.label
		t = g.of(p)
	default:
		var cell []contribution
		for _, row := range t.rows {
			if row.bucket == r.Cell.Bucket {
				cell = append(cell, row)
			}
		}
		t = t.sub(cell)
		out.Title += ", " + ev.label(m.Bucket, r.Cell.Bucket)
	}
	if r.Cell != nil && r.Cell.Series != "" {
		if !slices.Contains(m.Series, r.Cell.Series) || !c.p.cols.has(ColumnSeries) {
			return DrillAnswer{}, refuse("dashboard_request_invalid", fmt.Sprintf("widget %q has no series %q", truncate(w.ID, 64), truncate(r.Cell.Series, 64)), nil)
		}
		t = t.sub(bySeries(t.rows, r.Cell.Series, true))
		out.Title += ", " + r.Cell.Series
	}
	counts := t
	if ranged != nil {
		counts = *ranged
	}
	out.Value = ev.num(m.Reduce, counts)
	rows := slices.Clone(t.rows)
	sortDrill(rows, ev.keyOf)
	out.Groups = ev.drillGroups(rows, m, c.p.cols, counts)
	out.Count = len(rows)
	tickets := map[string]bool{}
	for _, row := range rows {
		tickets[row.ticket] = true
		if row.counted {
			out.Counted++
		}
	}
	out.Tickets = len(tickets)
	if r.IDs {
		out.IDs = make([]string, 0, len(tickets))
		for _, row := range rows {
			if tickets[row.ticket] {
				out.IDs = append(out.IDs, row.ticket)
				delete(tickets, row.ticket)
			}
		}
		return out, nil
	}
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
