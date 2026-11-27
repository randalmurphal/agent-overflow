package trackerdash

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sqlite "modernc.org/sqlite"
)

// The app's functions, registered once per time zone on a driver of that
// zone. The driver keeps a function it registers for the life of the
// process (modernc's registrations are never freed), and a call cannot
// tell which connection made it, so the registration is the process's
// and what is a session's is found through the sessions registry:
//
//   - bounded_rows(key, ...): the wrapper's aggregate into the sink of
//     the session the key names. The key is a random literal the wrapper
//     writes into its own text; a metric cannot name it, and a key that
//     is not an open session's fails the statement.
//   - days(a, b): the days from instant a to instant b (milliseconds), with
//     a fraction; NULL when either is NULL.
//   - working_days(a, b): the same, Saturdays and Sundays in the zone left
//     out.
//   - day(a): instant a's date in the zone, as YYYY-MM-DD.
//
// A process holds one driver for each zone name a count names that
// time.LoadLocation loads (location refuses any other before a driver is
// made), as the count spells it: Europe/Paris and Europe/./Paris are two.
// It holds at most maxZoneDrivers of them, which a driver cannot be given
// back to bound: a count that names another is refused until the app
// restarts.

// maxZoneDrivers is how many zones' drivers a process holds.
const maxZoneDrivers = 32

// zoneDrivers is the driver of each zone a session opened in, by name,
// and how many functions they registered, which a test reads.
var zoneDrivers = struct {
	mu            sync.Mutex
	m             map[string]*sqlite.Driver
	registrations int
}{m: map[string]*sqlite.Driver{}}

// driverFor is loc's driver, made with its functions the first time.
func driverFor(loc *time.Location) (*sqlite.Driver, error) {
	zoneDrivers.mu.Lock()
	defer zoneDrivers.mu.Unlock()
	if d, ok := zoneDrivers.m[loc.String()]; ok {
		return d, nil
	}
	if len(zoneDrivers.m) >= maxZoneDrivers {
		return nil, refuse("dashboard_too_many_zones", fmt.Sprintf("dashboards have been counted in %d time zones since the app started, as many as it keeps; restart the app to count in %s", maxZoneDrivers, truncate(loc.String(), 64)), nil)
	}
	d := &sqlite.Driver{}
	register := func(name string, impl *sqlite.FunctionImpl) error {
		if err := d.RegisterFunction(name, impl); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
		zoneDrivers.registrations++
		return nil
	}
	scalar := func(name string, n int32, fn func(args []driver.Value) (driver.Value, error)) error {
		return register(name, &sqlite.FunctionImpl{NArgs: n, Deterministic: true, Scalar: func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			return fn(args)
		}})
	}
	err := errors.Join(
		register("bounded_rows", &sqlite.FunctionImpl{NArgs: -1, MakeAggregate: func(sqlite.FunctionContext) (sqlite.AggregateFunction, error) {
			return &sinkStep{}, nil
		}}),
		scalar("days", 2, func(args []driver.Value) (driver.Value, error) {
			a, b, ok, err := instants("days", args)
			if !ok || err != nil {
				return nil, err
			}
			return float64(b-a) / float64(24*time.Hour/time.Millisecond), nil
		}),
		scalar("working_days", 2, func(args []driver.Value) (driver.Value, error) {
			a, b, ok, err := instants("working_days", args)
			if !ok || err != nil {
				return nil, err
			}
			return workingDays(a, b, loc), nil
		}),
		scalar("day", 1, func(args []driver.Value) (driver.Value, error) {
			if args[0] == nil {
				return nil, nil
			}
			a, err := instant("day", args[0])
			if err != nil {
				return nil, err
			}
			return time.UnixMilli(a).In(loc).Format(time.DateOnly), nil
		}),
	)
	if err != nil {
		// The driver is not kept: a later session makes it again.
		return nil, fmt.Errorf("the dashboard's functions in %s: %w", loc, err)
	}
	zoneDrivers.m[loc.String()] = d
	return d, nil
}

// sessionSinks is the sink of each open session by its key: added when
// the session opens, removed when it closes or fails to open.
var sessionSinks = struct {
	mu sync.Mutex
	m  map[string]*rowSink
}{m: map[string]*rowSink{}}

// openSink registers sink under a new random key.
func openSink(sink *rowSink) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("make the dashboard session's key: %w", err)
	}
	key := hex.EncodeToString(b[:])
	sessionSinks.mu.Lock()
	sessionSinks.m[key] = sink
	sessionSinks.mu.Unlock()
	return key, nil
}

func closeSink(key string) {
	sessionSinks.mu.Lock()
	delete(sessionSinks.m, key)
	sessionSinks.mu.Unlock()
}

func sinkOf(key string) (*rowSink, bool) {
	sessionSinks.mu.Lock()
	defer sessionSinks.mu.Unlock()
	s, ok := sessionSinks.m[key]
	return s, ok
}

// maxInstant bounds an instant a function takes or a row's at holds:
// milliseconds before the year 2286, past any date a tracker records.
const maxInstant = 10_000_000_000_000

// maxValue bounds a row's value either way: no estimate, day count or
// ticket count comes near a trillion, and 20,000 rows of at most this
// add to a number every reduction and the answer's JSON hold exactly.
const maxValue = 1e12

func instants(name string, args []driver.Value) (a, b int64, ok bool, err error) {
	if args[0] == nil || args[1] == nil {
		return 0, 0, false, nil
	}
	if a, err = instant(name, args[0]); err != nil {
		return 0, 0, false, err
	}
	if b, err = instant(name, args[1]); err != nil {
		return 0, 0, false, err
	}
	return a, b, true, nil
}

func instant(name string, v driver.Value) (int64, error) {
	var ms int64
	switch x := v.(type) {
	case int64:
		ms = x
	case float64:
		ms = int64(x)
	default:
		return 0, fmt.Errorf("%s takes instants in milliseconds, not %T", name, v)
	}
	if ms < 0 || ms > maxInstant {
		return 0, fmt.Errorf("%s takes instants in milliseconds since 1970, not %d", name, ms)
	}
	return ms, nil
}

// workingDays is the days from a to b with Saturdays and Sundays in loc
// left out, a part of a day counted as its share of that day's length
// (which a change of clocks makes 23 or 25 hours); negative when b is
// before a.
func workingDays(a, b int64, loc *time.Location) float64 {
	if b < a {
		return -workingDays(b, a, loc)
	}
	ta, tb := time.UnixMilli(a).In(loc), time.UnixMilli(b).In(loc)
	da, db := midnight(ta), midnight(tb)
	if da.Equal(db) {
		if !working(da) {
			return 0
		}
		return float64(b-a) / float64(dayLength(da))
	}
	total := 0.0
	if working(da) {
		next := da.AddDate(0, 0, 1)
		total += float64(next.UnixMilli()-a) / float64(dayLength(da))
	}
	if working(db) {
		total += float64(b-db.UnixMilli()) / float64(dayLength(db))
	}
	// The whole days between: civil dates, counted in weeks of five and
	// the rest one by one.
	first := da.AddDate(0, 0, 1)
	whole := civilDays(first, db)
	total += float64(whole / 7 * 5)
	for d, i := first.AddDate(0, 0, whole/7*7), 0; i < whole%7; d, i = d.AddDate(0, 0, 1), i+1 {
		if working(d) {
			total++
		}
	}
	return total
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func working(day time.Time) bool {
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
}

// dayLength is the milliseconds of the day starting at midnight m.
func dayLength(m time.Time) int64 { return m.AddDate(0, 0, 1).UnixMilli() - m.UnixMilli() }

// civilDays is the calendar days from midnight a to midnight b.
func civilDays(a, b time.Time) int {
	ua := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	ub := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(ub.Sub(ua) / (24 * time.Hour))
}

// contribution is one row of a metric, read by its columns.
type contribution struct {
	ticket  string
	value   float64
	counted bool
	// share is the part of a ticket the row counts where the board counts
	// tickets (ColumnShare).
	share  float64
	bucket string
	series string
	person string
	epic   string
	at     int64
	hasAt  bool
	// until is when a state row stops holding (ColumnUntil); hasUntil is
	// false for one that still holds.
	until    int64
	hasUntil bool
	note     string
	// period is the label of the period place put the row in.
	period string
}

// rowSink gathers the rows of the metric that runs, charged against the
// row limit and the byte budget. One metric runs at a time on a session.
// A state metric's row is kept only when it holds at one of the instants
// its widgets read (at); the limit and the budget count the rows kept.
type rowSink struct {
	cols columns
	// series is the metric's declared series, which a row's series is one of.
	series []string
	at     heldAt
	rows   []contribution
	bytes  int
	err    error
}

func (s *rowSink) reset(cols columns, series []string, at heldAt) {
	s.cols, s.series, s.at, s.rows, s.bytes, s.err = cols, series, at, nil, 0, nil
}

// heldAt is the instants a state metric's widgets read, sorted, with
// where they are in words for the row limit's sentence (reads.instants).
type heldAt struct {
	at    []int64
	where string
}

// holdAny is whether state row c holds at one of the instants (holdsAt).
func (in heldAt) holdAny(c contribution) bool {
	i := 0
	if c.hasAt {
		i, _ = slices.BinarySearch(in.at, c.at)
	}
	return i < len(in.at) && c.holdsAt(in.at[i])
}

// errRows is the row limit's failure: of a state metric, the rows that
// hold where its widgets read it, which the sentence says.
func (in heldAt) errRows(state bool) *MetricError {
	if !state {
		return &MetricError{Sentence: fmt.Sprintf("This widget's query returned more than %d rows, its limit. Pick a shorter range.", maxRows), Limit: true}
	}
	advice := " Pick a shorter range."
	if len(in.at) == 1 {
		advice = ""
	}
	return &MetricError{Sentence: fmt.Sprintf("This widget's query returned more than %d rows that hold %s, its limit.%s", maxRows, in.where, advice), Limit: true}
}

func (s *rowSink) take() []contribution {
	rows := s.rows
	s.rows = nil
	return rows
}

// errBytes is the budget's failure.
var errBytes = &MetricError{Sentence: fmt.Sprintf("This widget's rows passed %d MiB, their limit. Pick a shorter range.", maxRowBytes>>20), Limit: true}

const maxNote = 80

// add reads one row into a contribution: onBoard is whether its ticket
// is one of the copy's tickets, which the wrapper asks of the copy.
func (s *rowSink) add(onBoard driver.Value, args []driver.Value) error {
	if s.err != nil {
		return s.err
	}
	if len(args) != s.cols.n {
		s.err = fmt.Errorf("bounded_rows takes the metric's %d columns, not %d", s.cols.n, len(args))
		return s.err
	}
	c, err := s.read(args)
	if err != nil {
		s.err = &MetricError{Sentence: "This widget's query returned a row it cannot count: " + err.Error() + ". Change its query."}
		return s.err
	}
	if known, _ := onBoard.(int64); known != 1 {
		s.err = &MetricError{Sentence: fmt.Sprintf("This widget's query returned ticket %q, which is not one of this board's tickets. A row names a ticket from the tickets relation; change its query.", truncate(c.ticket, 64))}
		return s.err
	}
	if s.cols.has(ColumnSeries) && !slices.Contains(s.series, c.series) {
		s.err = &MetricError{Sentence: fmt.Sprintf("This widget's query returned series %q, which its metric does not name (it names %s). Add the series to the metric, or change its query.", truncate(c.series, 64), strings.Join(s.series, ", "))}
		return s.err
	}

	if s.cols.state() {
		if !s.at.holdAny(c) {
			return nil
		}
	}
	if len(s.rows) == maxRows {
		s.err = s.at.errRows(s.cols.state())
		return s.err
	}
	if len(c.note) > maxNote {
		cut := maxNote
		for cut > 0 && !utf8.RuneStart(c.note[cut]) {
			cut--
		}
		c.note = c.note[:cut] + "…"
	}
	s.bytes += 96 + len(c.ticket) + len(c.bucket) + len(c.series) + len(c.person) + len(c.epic) + len(c.note)
	if s.bytes > maxRowBytes {
		s.err = errBytes
		return s.err
	}
	s.rows = append(s.rows, c)
	return nil
}

// read reads a row's columns, each value and instant within its bound.
func (s *rowSink) read(args []driver.Value) (contribution, error) {
	var c contribution
	var err error
	at := func(name string) driver.Value { return args[s.cols.index[name]] }
	if c.ticket, err = text(ColumnTicket, at(ColumnTicket)); err == nil && c.ticket == "" {
		err = errors.New("a row's ticket is empty or NULL; each row names the ticket it counts")
	}
	c.value, c.counted, c.share = 1, true, 1
	if err == nil && s.cols.has(ColumnValue) {
		c.value, c.counted, err = number(ColumnValue, at(ColumnValue))
		if err == nil && c.counted && (math.IsNaN(c.value) || math.Abs(c.value) > maxValue) {
			err = fmt.Errorf("ticket %q's value is %g, past %g either way, the most a metric counts", truncate(c.ticket, 64), c.value, maxValue)
		}
	}
	if err == nil && s.cols.has(ColumnShare) {
		var share float64
		var ok bool
		share, ok, err = number(ColumnShare, at(ColumnShare))
		switch {
		case err != nil || !ok:
		case math.IsNaN(share) || math.Abs(share) > maxValue:
			err = fmt.Errorf("ticket %q's share is %g, past %g either way, the most a metric counts", truncate(c.ticket, 64), share, maxValue)
		default:
			c.share = share
		}
	}
	for _, f := range []struct {
		name string
		to   *string
	}{{ColumnBucket, &c.bucket}, {ColumnSeries, &c.series}, {ColumnPerson, &c.person}, {ColumnEpic, &c.epic}, {ColumnNote, &c.note}} {
		if err == nil && s.cols.has(f.name) {
			*f.to, err = text(f.name, at(f.name))
		}
	}
	if err == nil && s.cols.has(ColumnAt) {
		var v float64
		v, c.hasAt, err = number(ColumnAt, at(ColumnAt))
		if err == nil && c.hasAt && !(v >= 0 && v <= maxInstant) {
			err = fmt.Errorf("ticket %q's at is %g, not an instant in milliseconds from 1970 to 2286", truncate(c.ticket, 64), v)
		}
		c.at = int64(v)
	}
	if err == nil && s.cols.has(ColumnUntil) {
		var v float64
		v, c.hasUntil, err = number(ColumnUntil, at(ColumnUntil))
		switch {
		case err != nil || !c.hasUntil:
		case !(v >= 0 && v <= maxInstant):
			err = fmt.Errorf("ticket %q's until is %g, not an instant in milliseconds from 1970 to 2286", truncate(c.ticket, 64), v)
		case c.hasAt && int64(v) < c.at:
			err = fmt.Errorf("ticket %q's until is before its at; a state holds from at until until", truncate(c.ticket, 64))
		}
		c.until = int64(v)
	}
	return c, err
}

func text(name string, v driver.Value) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case []byte:
		return "", fmt.Errorf("column %s holds a blob", name)
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	default:
		return fmt.Sprint(x), nil
	}
}

// number is a numeric column's value; ok is false for NULL.
func number(name string, v driver.Value) (float64, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case int64:
		return float64(x), true, nil
	case float64:
		return x, true, nil
	default:
		return 0, false, fmt.Errorf("column %s holds %s, not a number", name, typeName(v))
	}
}

func typeName(v driver.Value) string {
	switch v.(type) {
	case string:
		return "text"
	case []byte:
		return "a blob"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// sinkStep is bounded_rows: its first argument names the session, its
// second whether the row's ticket is on the board, and each step is a
// row into that session's sink, the result the number of rows.
type sinkStep struct{ sink *rowSink }

// errSessionClosed is a bounded_rows step whose key names no open session.
var errSessionClosed = errors.New("bounded_rows names no open dashboard session")

func (a *sinkStep) Step(_ *sqlite.FunctionContext, args []driver.Value) error {
	if a.sink == nil {
		key, _ := args[0].(string)
		sink, ok := sinkOf(key)
		if !ok {
			return errSessionClosed
		}
		a.sink = sink
	}
	return a.sink.add(args[1], args[2:])
}

func (a *sinkStep) WindowInverse(*sqlite.FunctionContext, []driver.Value) error {
	return errors.New("bounded_rows is not a window function")
}

func (a *sinkStep) WindowValue(*sqlite.FunctionContext) (driver.Value, error) {
	if a.sink == nil {
		return int64(0), nil
	}
	if a.sink.err != nil {
		return nil, a.sink.err
	}
	return int64(len(a.sink.rows)), nil
}

func (a *sinkStep) Final(*sqlite.FunctionContext) {}
