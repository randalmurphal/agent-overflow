package trackerdash

import (
	"fmt"
	"slices"
	"strings"
)

// The columns a metric's SELECT may return, by name. ticket is required.
const (
	// ColumnTicket is the ticket a row counts (a tickets.id).
	ColumnTicket = "ticket"
	// ColumnValue is the row's number, 1 when absent; NULL marks a row the
	// metric does not count, which the drill lists as not counted with its
	// note.
	ColumnValue = "value"
	// ColumnBucket is the row's bar, table row or sprint (Metric.Bucket).
	ColumnBucket = "bucket"
	// ColumnSeries is the row's series: a bar's part, a line, a table
	// column.
	ColumnSeries = "series"
	// ColumnPerson is who the row counts for; absent, the ticket's
	// assignee.
	ColumnPerson = "person"
	// ColumnEpic is the epic the row counts for; absent, the ticket's.
	ColumnEpic = "epic"
	// ColumnAt is when the row happened (milliseconds), which places it in
	// the range and its periods.
	ColumnAt = "at"
	// ColumnUntil makes the row a state: it holds from at (NULL: before
	// anything) until until (NULL: still), and is counted at each instant
	// it holds (contribution.holdsAt).
	ColumnUntil = "until"
	// ColumnShare is the part of a ticket a counted row counts where the
	// board counts tickets (no estimates, or a project that counts
	// tickets), for a metric in the board's estimate: 1 when absent or
	// NULL (evaluation.run).
	ColumnShare = "share"
	// ColumnNote is a short text the ticket list and the drill show beside
	// the row. A row not counted is listed as "Not counted: " and its
	// note, so its note says only why.
	ColumnNote = "note"
)

var metricColumns = []string{ColumnTicket, ColumnValue, ColumnShare, ColumnBucket, ColumnSeries, ColumnPerson, ColumnEpic, ColumnAt, ColumnUntil, ColumnNote}

// Reducer is how a metric's rows become one number.
type Reducer string

// The reducers.
const (
	// ReduceCount counts the distinct tickets of the counted rows.
	ReduceCount Reducer = "count"
	// ReduceSum adds the rows' values.
	ReduceSum Reducer = "sum"
	// ReduceMedian is the middle value, the mean of the middle two of an
	// even count.
	ReduceMedian Reducer = "median"
	// ReduceAverage is the mean of the values.
	ReduceAverage Reducer = "average"
	// ReduceAverageOfPeriodSums adds each period's values and averages the
	// sums over the range's periods, a period with no rows as zero; a bar
	// or a cell of it is its sum.
	ReduceAverageOfPeriodSums Reducer = "average_of_period_sums"
)

// Bucket is what a metric's rows are grouped by.
type Bucket string

// The buckets.
const (
	// BucketNone groups nothing: a number or a list.
	BucketNone Bucket = ""
	// BucketPeriod groups rows by the range's period their at falls in.
	BucketPeriod Bucket = "period"
	// BucketSprint groups rows by the sprint their bucket names, the
	// range's sprints being its periods.
	BucketSprint Bucket = "sprint"
	// BucketPerson, BucketEpic and BucketText group rows by their bucket:
	// a person, an epic, or a text.
	BucketPerson Bucket = "person"
	BucketEpic   Bucket = "epic"
	BucketText   Bucket = "text"
	// BucketDay groups rows by the working days of the board's active
	// sprints, whatever the range: their start, then each working day's
	// end (sprintDays).
	BucketDay Bucket = "day"
)

// Unit is what a metric's number counts.
type Unit string

// The units.
const (
	UnitTickets Unit = "tickets"
	// UnitEstimate is the board's estimate: points, time, or tickets on a
	// board with no estimate.
	UnitEstimate Unit = "estimate"
	UnitDays     Unit = "days"
	// UnitTime is logged time, in seconds.
	UnitTime Unit = "time"
)

// numbersUnit is the unit m's numbers are in, the one every reader of
// them (the widget, its drill, the tool's answer) draws them in: a
// count's are tickets, whatever its unit.
func (m Metric) numbersUnit() Unit {
	if m.Reduce == ReduceCount {
		return UnitTickets
	}
	return m.Unit
}

// Good is the direction of a change that is good news.
type Good string

// The directions.
const (
	GoodUp   Good = "up"
	GoodDown Good = "down"
	GoodNone Good = ""
)

// Metric is one named query and how its rows are reduced. The same struct
// holds the app's metrics and an agent's; Validate is the
// one check both pass.
type Metric struct {
	// Name is the metric's immutable machine name; a project's metric is
	// named by its widget, so it is not part of the metric's JSON.
	Name string `json:"-"`
	// Title is what a widget of it is called.
	Title string `json:"title"`
	// Words say in plain words what it counts, at most three sentences.
	Words []string `json:"words"`
	// SQL is the one SELECT.
	SQL string `json:"sql"`
	// Reduce, Unit, Good and Bucket are as their types say.
	Reduce Reducer `json:"reduce"`
	Unit   Unit    `json:"unit"`
	Good   Good    `json:"good,omitempty"`
	Bucket Bucket  `json:"bucket,omitempty"`
	// Series is the series it names, in the order a chart's legend and a
	// table's columns follow; a row names one of them, and a metric that
	// names none returns no series column.
	Series []string `json:"series,omitempty"`
	// Pace is a built-in's remaining work by day (the burndown): its line
	// draws an even pace to zero and each point says what is left.
	// Overlap is a built-in's whose series overlap, a ticket counting in
	// several, so a table of it adds up no total. Neither is in a metric's
	// JSON: a definition that names either is refused as an unknown field.
	Pace    bool `json:"-"`
	Overlap bool `json:"-"`
}

// Bounds of a metric's definition.
const (
	maxMetricSQL   = 16 << 10
	maxWords       = 3
	maxWordsLength = 300
	maxTitle       = 120
	maxSeries      = 6
	maxChartSeries = 3
)

// Validate checks a metric's definition, before its SQL is prepared.
func (m Metric) Validate() error {
	switch {
	case !validName(m.Name):
		return fmt.Errorf("a metric's name is 1 to 64 lowercase letters, digits and underscores, not %q", truncate(m.Name, 64))
	case strings.TrimSpace(m.Title) == "" || len(m.Title) > maxTitle:
		return fmt.Errorf("metric %s's title is empty or over %d bytes", m.Name, maxTitle)
	case len(m.Words) == 0 || len(m.Words) > maxWords:
		return fmt.Errorf("metric %s says what it counts in 1 to %d sentences, not %d", m.Name, maxWords, len(m.Words))
	case len(m.SQL) > maxMetricSQL:
		return fmt.Errorf("metric %s's query is %d bytes, over %d", m.Name, len(m.SQL), maxMetricSQL)
	case len(m.Series) > maxSeries:
		return fmt.Errorf("metric %s names %d series, over %d", m.Name, len(m.Series), maxSeries)
	}
	for i, name := range m.Series {
		if strings.TrimSpace(name) == "" || len(name) > maxTitle || slices.Contains(m.Series[:i], name) {
			return fmt.Errorf("a series of metric %s is empty, over %d bytes or named twice", m.Name, maxTitle)
		}
	}
	for _, w := range m.Words {
		if strings.TrimSpace(w) == "" || len(w) > maxWordsLength {
			return fmt.Errorf("a sentence of metric %s is empty or over %d bytes", m.Name, maxWordsLength)
		}
	}
	switch m.Reduce {
	case ReduceCount, ReduceSum, ReduceMedian, ReduceAverage, ReduceAverageOfPeriodSums:
	default:
		return fmt.Errorf("metric %s reduces by %q, which is none of count, sum, median, average or average_of_period_sums", m.Name, m.Reduce)
	}
	switch m.Unit {
	case UnitTickets, UnitEstimate, UnitDays, UnitTime:
	default:
		return fmt.Errorf("metric %s counts %q, which is none of tickets, estimate, days or time", m.Name, m.Unit)
	}
	switch m.Good {
	case GoodUp, GoodDown, GoodNone:
	default:
		return fmt.Errorf("metric %s calls %q good, which is neither up nor down", m.Name, m.Good)
	}
	switch m.Bucket {
	case BucketNone, BucketPeriod, BucketSprint, BucketPerson, BucketEpic, BucketText, BucketDay:
	default:
		return fmt.Errorf("metric %s groups by %q, which is none of period, sprint, day, person, epic or text", m.Name, m.Bucket)
	}
	if m.Reduce == ReduceAverageOfPeriodSums && m.Bucket != BucketPeriod && m.Bucket != BucketSprint {
		return fmt.Errorf("metric %s averages its periods' sums, so it groups by period or sprint", m.Name)
	}
	return nil
}

// columns is which contract columns a metric's SELECT returns, by
// position.
type columns struct {
	index map[string]int
	n     int
}

// metricColumnsOf checks the result columns names of a metric's SELECT
// against the contract and the metric's grouping.
func metricColumnsOf(m Metric, names []string) (columns, error) {
	c := columns{index: map[string]int{}, n: len(names)}
	for i, name := range names {
		name = strings.ToLower(name)
		if !slices.Contains(metricColumns, name) {
			return columns{}, fmt.Errorf("the query returns a column named %q; a metric's columns are %s", truncate(name, 64), strings.Join(metricColumns, ", "))
		}
		if _, dup := c.index[name]; dup {
			return columns{}, fmt.Errorf("the query returns column %s twice", name)
		}
		c.index[name] = i
	}
	if !c.has(ColumnTicket) {
		return columns{}, fmt.Errorf("the query returns no ticket column; each row names the ticket it counts")
	}
	switch {
	case (m.Bucket == BucketPeriod || m.Bucket == BucketDay) && !c.has(ColumnAt) && !c.has(ColumnUntil):
		return columns{}, fmt.Errorf("metric %s groups by %s, so its query returns an at or an until column", m.Name, m.Bucket)
	case c.has(ColumnUntil) && m.Reduce == ReduceAverageOfPeriodSums:
		return columns{}, fmt.Errorf("metric %s returns until, so each row is a state counted where it holds; a state's periods are not added up, so reduce it by count, sum, median or average", m.Name)
	case m.Bucket != BucketNone && m.Bucket != BucketPeriod && m.Bucket != BucketDay && !c.has(ColumnBucket):
		return columns{}, fmt.Errorf("metric %s groups by %s, so its query returns a bucket column", m.Name, m.Bucket)
	case (m.Bucket == BucketNone || m.Bucket == BucketDay) && c.has(ColumnBucket):
		return columns{}, fmt.Errorf("metric %s groups by %s, so its query returns no bucket column", m.Name, cmpOr(string(m.Bucket), "nothing"))
	case len(m.Series) == 0 && c.has(ColumnSeries):
		return columns{}, fmt.Errorf("metric %s names no series, so its query returns no series column", m.Name)
	}
	return c, nil
}

func (c columns) has(name string) bool {
	_, ok := c.index[name]
	return ok
}

// periodBound is whether a metric's rows fall in the range's periods (or
// the sprint's days): it has an at or an until, or groups by sprint or
// day. Any other metric counts now, and its number has no previous range
// and no trend.
func (c columns) periodBound(m Metric) bool {
	return c.has(ColumnAt) || c.has(ColumnUntil) || m.Bucket == BucketSprint || m.Bucket == BucketDay
}

// state is whether a metric's rows are states (an until column), counted
// at the instants they hold instead of in the period they happen in.
func (c columns) state() bool { return c.has(ColumnUntil) }

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Form is how a widget draws its metric.
type Form string

// The five forms.
const (
	FormNumber Form = "number"
	FormBars   Form = "bars"
	FormLine   Form = "line"
	FormTable  Form = "table"
	FormList   Form = "list"
)

// Widget is one metric drawn in one form, Width columns of twelve wide,
// under Title, the metric's own when empty.
type Widget struct {
	ID     string
	Title  string
	Metric string
	Form   Form
	Width  int
}

// Dashboard is a named layout of widgets.
type Dashboard struct {
	ID      string
	Title   string
	Widgets []Widget
}

// checkWidget checks that a widget can draw its metric.
func checkWidget(w Widget, m Metric) error {
	switch w.Width {
	case 3, 4, 6, 8, 12:
	default:
		return fmt.Errorf("widget %s is %d columns wide; a widget is 3, 4, 6, 8 or 12", w.ID, w.Width)
	}
	switch w.Form {
	case FormNumber, FormList:
	case FormBars, FormLine:
		if m.Bucket == BucketNone {
			return fmt.Errorf("widget %s draws %s, but metric %s groups by nothing", w.ID, w.Form, m.Name)
		}
		if w.Form == FormLine && m.Bucket != BucketPeriod && m.Bucket != BucketSprint && m.Bucket != BucketDay {
			return fmt.Errorf("widget %s draws a line, so metric %s groups by period, sprint or day", w.ID, m.Name)
		}
		if len(m.Series) > maxChartSeries {
			return fmt.Errorf("widget %s draws %d series; a chart draws at most %d", w.ID, len(m.Series), maxChartSeries)
		}
	case FormTable:
		if m.Bucket == BucketNone {
			return fmt.Errorf("widget %s draws a table, but metric %s groups by nothing", w.ID, m.Name)
		}
	default:
		return fmt.Errorf("widget %s's form %q is none of number, bars, line, table or list", w.ID, w.Form)
	}
	return nil
}
