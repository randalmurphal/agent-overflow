package trackerdash

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/store"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Fill fills the empty main database of conn with a board's export
// (store.ExportMirrorBoard bound to the board) of relations, the ones the
// count reads: ok is false for a board the mirror does not show. The
// export's EstimateUnit is the unit the board is counted in, none where
// its project counts tickets. The fill must leave conn holding its main
// database alone; the gate checks it.
type Fill func(ctx context.Context, conn *sql.Conn, relations []string) (export store.MirrorExport, ok bool, err error)

// snapshotDSN is the export's database: private and in memory, a sort or
// a materialized subquery that outgrows the 4 MiB page cache spilling to
// a temporary file.
const snapshotDSN = "file::memory:?_pragma=temp_store(1)&_pragma=cache_size(-4096)"

// The bounds of one metric's run.
const (
	maxRows        = 20_000
	maxRowBytes    = 4 << 20
	metricDeadline = time.Second
)

// maxParams is the parameters one statement on a sealed session binds: a
// metric binds :now and :since, and the app's reads of a page's titles
// bind as many IDs a statement.
const maxParams = 8

// limits are the connection's SQLite limits once the gate closes, by
// SQLITE_LIMIT_*. The attached-database limit is the gate's own.
var limits = []struct {
	id    int
	name  string
	value int
}{
	{sqlite3.SQLITE_LIMIT_SQL_LENGTH, "query length", maxMetricSQL + 1024},
	{sqlite3.SQLITE_LIMIT_LENGTH, "value length", 64 << 10},
	{sqlite3.SQLITE_LIMIT_EXPR_DEPTH, "expression depth", 100},
	{sqlite3.SQLITE_LIMIT_COMPOUND_SELECT, "compound terms", 8},
	{sqlite3.SQLITE_LIMIT_COLUMN, "columns", 32},
	{sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, "parameters", maxParams},
	{sqlite3.SQLITE_LIMIT_VDBE_OP, "statement size", 25_000},
	{sqlite3.SQLITE_LIMIT_FUNCTION_ARG, "function arguments", 16},
	{sqlite3.SQLITE_LIMIT_WORKER_THREADS, "worker threads", 0},
	// One LIKE or GLOB call cannot be interrupted, and its cost grows with
	// its pattern times its subject: 30,000 bytes against 60,000 took
	// 1.1 s. 256 bytes bounds a call at about 10 ms.
	{sqlite3.SQLITE_LIMIT_LIKE_PATTERN_LENGTH, "LIKE and GLOB pattern length", 256},
}

// allowedFunctions are the functions a metric may call: SQLite's
// deterministic core, aggregate and window functions, and the app's
// (funcs.go) but bounded_rows, which only the wrapper calls. SQLite's
// date and time functions, random, randomblob, zeroblob, load_extension,
// the JSON functions and every function not listed are denied, trim,
// ltrim and rtrim among them: one call with a list of characters to cut
// is one step the deadline cannot interrupt, its cost the subject's
// length times the list's.
var allowedFunctions = map[string]bool{}

func init() {
	for _, name := range strings.Fields(`abs coalesce ifnull iif if nullif min max round length lower upper
		substr substring replace instr printf format typeof like glob concat concat_ws
		octet_length likely unlikely likelihood hex unicode char quote sign
		count sum total avg group_concat string_agg
		row_number rank dense_rank percent_rank cume_dist ntile lag lead first_value last_value nth_value
		ceil ceiling floor trunc ln log log10 log2 exp pow power sqrt mod pi
		days working_days day day_start`) {
		allowedFunctions[name] = true
	}
}

// exportTables is which tables of main a metric may read.
var exportTables = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range store.MirrorExportTables {
		m[t] = true
	}
	return m
}()

// session is one evaluation's connection: opened, filled, gated, run and
// closed by one goroutine.
type session struct {
	db   *sql.DB
	conn *sql.Conn
	sink *rowSink
	// key names the sink in the sessions registry (sessionSinks).
	key string
	// denied is what the authorizer refused last, for the sentence of the
	// error SQLite reports as "not authorized". The authorizer runs while
	// the statement prepares, on the goroutine that prepares it; mu is for
	// the race detector's sake and for a reprepare SQLite may run on a
	// step.
	mu     sync.Mutex
	denied string
	// wrapping is whether the wrapper prepares, the one statement that may
	// call bounded_rows. A metric's own text is prepared first without it,
	// so a metric that calls bounded_rows never runs.
	wrapping bool
	// layout is how an empty session stores the export, from which its
	// prepares read what each statement reads (plan.go); nil on a session
	// filled with a copy, whose prepares read asked instead.
	layout *layout
	// asked is what the copy was filled with for each metric, by name,
	// from its plan on an empty session: what a filled session's prepare
	// says it reads.
	asked map[string]asks
}

// connector opens the snapshot on its zone's driver (driverFor).
type connector struct{ d *sqlite.Driver }

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.d.Open(snapshotDSN) }
func (c connector) Driver() driver.Driver                        { return c.d }

// openSession opens a private in-memory database on loc's driver, its
// sink registered under the session's key until it closes.
func openSession(ctx context.Context, loc *time.Location) (*session, error) {
	d, err := driverFor(loc)
	if err != nil {
		return nil, err
	}
	s := &session{sink: &rowSink{}}
	if s.key, err = openSink(s.sink); err != nil {
		return nil, err
	}
	s.db = sql.OpenDB(connector{d})
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		closeSink(s.key)
		closeErr := s.db.Close()
		return nil, errors.Join(fmt.Errorf("open the dashboard's database: %w", err), closeErr)
	}
	s.conn = conn
	return s, nil
}

// close closes the connection and its database: nothing of the
// evaluation outlives it.
func (s *session) close() error {
	closeSink(s.key)
	err := s.conn.Close()
	if closeErr := s.db.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("close the dashboard's database: %w", err)
	}
	return nil
}

// errGate is a connection the gate refused to run a metric on.
var errGate = errors.New("the board's copy for the dashboard was not sealed")

// gate seals the filled connection before any metric runs: no database
// can be attached any more, the connection holds no database but main and
// temp, it writes nothing, its limits are lowered, and the authorizer
// judges every statement prepared on it from here on. The order matters:
// the authorizer refuses PRAGMA, so query_only is set before it.
func (s *session) gate(ctx context.Context) error {
	if _, err := sqlite.Limit(s.conn, sqlite3.SQLITE_LIMIT_ATTACHED, 0); err != nil {
		return fmt.Errorf("%w: limit attached databases: %w", errGate, err)
	}
	if n, err := sqlite.Limit(s.conn, sqlite3.SQLITE_LIMIT_ATTACHED, -1); err != nil || n != 0 {
		return fmt.Errorf("%w: the attached-database limit is %d, not 0 (%v)", errGate, n, err)
	}
	names, err := s.databases(ctx)
	if err != nil {
		return fmt.Errorf("%w: list its databases: %w", errGate, err)
	}
	for _, name := range names {
		if name != "main" && name != "temp" {
			return fmt.Errorf("%w: it still holds database %q after the fill", errGate, name)
		}
	}
	if _, err := s.conn.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		return fmt.Errorf("%w: make it read-only: %w", errGate, err)
	}
	for _, l := range limits {
		if _, err := sqlite.Limit(s.conn, l.id, l.value); err != nil {
			return fmt.Errorf("%w: limit %s: %w", errGate, l.name, err)
		}
	}
	err = s.conn.Raw(func(dc any) error {
		r, ok := dc.(sqlite.AuthorizerRegisterer)
		if !ok {
			return fmt.Errorf("the driver connection %T has no authorizer", dc)
		}
		return r.RegisterAuthorizer(s.authorize)
	})
	if err != nil {
		return fmt.Errorf("%w: install the authorizer: %w", errGate, err)
	}
	return nil
}

// databases is the names the connection lists, in order.
func (s *session) databases(ctx context.Context) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, `SELECT name FROM pragma_database_list ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		names = append(names, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return names, nil
}

// authorize allows a SELECT, reads of the export's tables in main and the
// allowed functions, and denies every other action.
func (s *session) authorize(action sqlite.AuthorizerActionCode, arg1, arg2, database, _ string) sqlite.AuthorizerReturnCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if action == sqlite.AuthSelect {
		return sqlite.AuthorizerOK
	}
	// A table a statement names but reads no column of comes with no
	// column and no database. The gate leaves main the only database that
	// can hold an export table's name.
	if action == sqlite.AuthRead && exportTables[arg1] && (database == "main" || (database == "" && arg2 == "")) {
		return sqlite.AuthorizerOK
	}
	if action == sqlite.AuthFunction {
		name := strings.ToLower(arg2)
		if allowedFunctions[name] || (s.wrapping && name == "bounded_rows") {
			return sqlite.AuthorizerOK
		}
	}
	s.denied = denial(action, arg1, arg2, database)
	return sqlite.AuthorizerDeny
}

// denial says what a refused action would have done.
func denial(action sqlite.AuthorizerActionCode, arg1, arg2, database string) string {
	switch {
	case action == sqlite.AuthRead && database != "main":
		return fmt.Sprintf("reads %s.%s, outside the board's relations", truncate(database, 32), truncate(arg1, 64))
	case action == sqlite.AuthRead:
		return fmt.Sprintf("reads %s, which is not one of the board's relations (%s)", truncate(arg1, 64), strings.Join(store.MirrorExportTables, ", "))
	case action == sqlite.AuthFunction:
		return fmt.Sprintf("calls %s(), which a metric may not call", truncate(arg2, 64))
	case action == sqlite.AuthPragma:
		return fmt.Sprintf("runs PRAGMA %s", truncate(arg1, 64))
	case action == sqlite.AuthAttach || action == sqlite.AuthDetach:
		return "attaches or detaches a database"
	case action == sqlite.AuthRecursive:
		return "is recursive (WITH RECURSIVE)"
	case action == sqlite.AuthTransaction || action == sqlite.AuthSavepoint:
		return "opens or ends a transaction"
	case action == sqlite.AuthInsert || action == sqlite.AuthUpdate || action == sqlite.AuthDelete:
		return fmt.Sprintf("writes %s", truncate(arg1, 64))
	default:
		return "creates, drops or changes a database object"
	}
}

func (s *session) setWrapping(on bool) {
	s.mu.Lock()
	s.wrapping = on
	s.mu.Unlock()
}

// take clears and returns what the authorizer denied last.
func (s *session) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	denied := s.denied
	s.denied = ""
	return denied
}

// prepared is a metric's statement checked against the contract.
type prepared struct {
	sql   string
	names []string
	cols  columns
	// asks is what it reads of the copy, from its plan (plan.go).
	asks asks
	// series is the metric's declared series (Metric.Series).
	series []string
	// took is how long the prepare took, which the metric's deadline
	// counts: the driver's prepare watches no context, so it is timed,
	// not interrupted. The limits bound it (0.65 ms for the slowest
	// statement measured under them).
	took time.Duration
}

// prepare checks a metric's SQL is one statement the authorizer allows,
// and its columns against the contract.
func (s *session) prepare(m Metric) (prepared, error) {
	start := time.Now()
	text, err := oneStatement(m.SQL)
	if err != nil {
		return prepared{}, &MetricError{Sentence: "This widget's query " + err.Error() + ". A metric is one SELECT; change its query."}
	}
	s.take()
	var info []sqlite.ColumnInfo
	err = s.conn.Raw(func(dc any) error {
		ci, ok := dc.(interface {
			ColumnInfo(query string) ([]sqlite.ColumnInfo, error)
		})
		if !ok {
			return fmt.Errorf("the driver connection %T cannot describe a statement", dc)
		}
		var err error
		info, err = ci.ColumnInfo(text)
		return err
	})
	if denied := s.take(); err != nil {
		return prepared{}, sqlError(err, denied)
	}
	if len(info) == 0 {
		return prepared{}, &MetricError{Sentence: "This widget's query is empty. A metric is one SELECT; change its query."}
	}
	names := make([]string, len(info))
	for i, c := range info {
		names[i] = strings.ToLower(c.Name)
	}
	cols, err := metricColumnsOf(m, names)
	if err != nil {
		return prepared{}, &MetricError{Sentence: "This widget's query " + strings.TrimPrefix(err.Error(), "the query ") + "; change its query."}
	}
	p := prepared{sql: text, names: names, cols: cols, series: m.Series}
	if p.asks, err = s.asksOf(m, p); err != nil {
		return prepared{}, err
	}
	p.took = time.Since(start)
	return p, nil
}

// asksOf is what metric m, prepared as p, reads of the copy: on an empty
// session what the plan of the statement a count runs reads, on a filled
// session what the copy was filled with for it, or why its plan could not
// be read there.
func (s *session) asksOf(m Metric, p prepared) (asks, error) {
	if s.layout != nil {
		return s.planAsks(s.wrapped(p))
	}
	a, ok := s.asked[m.Name]
	if !ok {
		return asks{}, fmt.Errorf("metric %s was not planned before its count", m.Name)
	}
	return a, a.failed
}

// hostParams is what the host binds to the wrapped statement: :now, the
// evaluation's instant, and :since, where the range's periods begin.
func hostParams(now, since int64) []any {
	return []any{sql.Named("now", now), sql.Named("since", since)}
}

// wrapped is the statement a count of p runs: p's own between
// parentheses, each row with whether its ticket is on the board, into
// the session's sink through bounded_rows, which keeps and caps them
// (rowSink.add).
func (s *session) wrapped(p prepared) string {
	var b strings.Builder
	b.WriteString("WITH r AS (\n")
	b.WriteString(p.sql)
	b.WriteString("\n) SELECT bounded_rows('" + s.key + "', EXISTS (SELECT 1 FROM main.tickets WHERE id = r.\"ticket\")")
	for _, name := range p.names {
		b.WriteString(`, r."` + name + `"`)
	}
	b.WriteString(") FROM r")
	return b.String()
}

// run runs a prepared metric under its deadline, which began when its
// prepare did, wrapped so SQLite
// produces the whole result in its first step and the rows go straight
// to the sink, and answers its rows. The host binds :now, the
// evaluation's instant, and :since, where the range's periods begin
// (span.since), so a metric of the range reads only the rows it can
// place. A state metric's rows are those that hold at one of at.
func (s *session) run(ctx context.Context, p prepared, now, since int64, at heldAt) ([]contribution, error) {
	ctx, cancel := context.WithTimeout(ctx, metricDeadline-p.took)
	defer cancel()
	s.sink.reset(p.cols, p.series, at)
	defer s.sink.reset(columns{}, nil, heldAt{})
	s.take()
	s.setWrapping(true)
	var n int64
	err := s.conn.QueryRowContext(ctx, s.wrapped(p), hostParams(now, since)...).Scan(&n)
	s.setWrapping(false)
	denied := s.take()
	switch {
	case s.sink.err != nil:
		return nil, s.sink.err
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return nil, &MetricError{Sentence: fmt.Sprintf("This widget's query ran past its limit of %v and was stopped. Pick a shorter range, or try again when this computer is less busy.", metricDeadline), Limit: true}
	case err != nil:
		return nil, sqlError(err, denied)
	}
	return s.sink.take(), nil
}

// MetricError is a widget's failure, its Sentence what the person reads:
// what happened and what they can do.
type MetricError struct {
	Sentence string
	// Limit is whether a bound stopped it (the deadline, the rows or the
	// bytes) rather than the query itself.
	Limit bool
}

func (e *MetricError) Error() string { return e.Sentence }

// sqlError is the sentence of a statement SQLite refused or failed.
func sqlError(err error, denied string) error {
	msg := err.Error()
	if denied != "" {
		return &MetricError{Sentence: "This widget's query " + denied + ", which a metric may not do. Change its query."}
	}
	for _, sign := range []string{"too big", "too many", "too large", "too complex", "too deep"} {
		if strings.Contains(msg, sign) {
			return &MetricError{Sentence: "This widget's query passed one of the limits on a metric (" + sqliteMessage(msg) + "). Make the query smaller.", Limit: true}
		}
	}
	return &MetricError{Sentence: "This widget's query failed: " + sqliteMessage(msg) + ". Change its query."}
}

// sqliteMessage is the message of a modernc error without its code.
func sqliteMessage(msg string) string {
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		msg = msg[:i]
	}
	if i := strings.Index(msg, ": "); i > 0 && strings.HasPrefix(msg, "SQL logic error") {
		msg = msg[i+2:]
	}
	return truncate(msg, 200)
}
