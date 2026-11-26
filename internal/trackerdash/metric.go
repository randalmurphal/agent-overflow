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
	// ColumnNote is a short text the ticket list and the drill show beside
	// the row.
	ColumnNote = "note"
)

var metricColumns = []string{ColumnTicket, ColumnValue, ColumnBucket, ColumnSeries, ColumnPerson, ColumnEpic, ColumnAt, ColumnNote}

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
)

// Good is the direction of a change that is good news.
type Good string

// The directions.
const (
	GoodUp   Good = "up"
	GoodDown Good = "down"
	GoodNone Good = ""
)

// Metric is one named query and how its rows are reduced. The same struct
// holds the app's metrics and, with authoring, an agent's; Validate is the
// one check both pass.
type Metric struct {
	// Name is the metric's immutable machine name.
	Name string
	// Title is what a widget of it is called.
	Title string
	// Words say in plain words what it counts, at most three sentences.
	Words []string
	// SQL is the one SELECT.
	SQL string
	// Reduce, Unit, Good and Bucket are as their types say.
	Reduce Reducer
	Unit   Unit
	Good   Good
	Bucket Bucket
	// Series is the order of the series it names, which a chart's legend
	// and a table's columns follow; a series it does not list follows in
	// text order.
	Series []string
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
	case UnitTickets, UnitEstimate, UnitDays:
	default:
		return fmt.Errorf("metric %s counts %q, which is none of tickets, estimate or days", m.Name, m.Unit)
	}
	switch m.Good {
	case GoodUp, GoodDown, GoodNone:
	default:
		return fmt.Errorf("metric %s calls %q good, which is neither up nor down", m.Name, m.Good)
	}
	switch m.Bucket {
	case BucketNone, BucketPeriod, BucketSprint, BucketPerson, BucketEpic, BucketText:
	default:
		return fmt.Errorf("metric %s groups by %q, which is none of period, sprint, person, epic or text", m.Name, m.Bucket)
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
	case m.Bucket == BucketPeriod && !c.has(ColumnAt):
		return columns{}, fmt.Errorf("metric %s groups by period, so its query returns an at column", m.Name)
	case m.Bucket != BucketNone && m.Bucket != BucketPeriod && !c.has(ColumnBucket):
		return columns{}, fmt.Errorf("metric %s groups by %s, so its query returns a bucket column", m.Name, m.Bucket)
	case m.Bucket == BucketNone && c.has(ColumnBucket):
		return columns{}, fmt.Errorf("metric %s groups by nothing, so its query returns no bucket column", m.Name)
	}
	return c, nil
}

func (c columns) has(name string) bool {
	_, ok := c.index[name]
	return ok
}

// periodBound is whether a metric's rows fall in the range's periods: it
// has an at, or groups by sprint. Any other metric counts now, and its
// number has no previous range and no trend.
func (c columns) periodBound(m Metric) bool {
	return c.has(ColumnAt) || m.Bucket == BucketSprint
}

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
		if w.Form == FormLine && m.Bucket != BucketPeriod && m.Bucket != BucketSprint {
			return fmt.Errorf("widget %s draws a line, so metric %s groups by period or sprint", w.ID, m.Name)
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
