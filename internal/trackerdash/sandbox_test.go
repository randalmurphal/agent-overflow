package trackerdash

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/store"

	sqlite "modernc.org/sqlite"
)

// sealed is a session filled with teamBoard and 3,000 changes, gated,
// closed when the test ends.
func sealed(t *testing.T) *session {
	t.Helper()
	s := sealedOpen(t)
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// sealedOpen is sealed's session, which the caller closes.
func sealedOpen(t *testing.T) *session {
	t.Helper()
	ctx := context.Background()
	s, err := openSession(ctx, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	f := teamBoard().exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3000)
		INSERT INTO changes SELECT 't1', 'status', i, i, NULL, '1', '3', NULL, NULL FROM n`)
	if _, _, err := f.fill()(ctx, s.conn, nil); err != nil {
		t.Fatal(errors.Join(err, s.close()))
	}
	if err := s.gate(ctx); err != nil {
		t.Fatal(errors.Join(err, s.close()))
	}
	return s
}

func metricOf(sql string) Metric {
	return Metric{Name: "probe", Title: "Probe", Words: []string{"A probe."}, SQL: sql, Reduce: ReduceCount, Unit: UnitTickets}
}

func runSQL(s *session, m Metric) ([]contribution, error) {
	p, err := s.prepare(m)
	if err != nil {
		return nil, err
	}
	return s.run(context.Background(), p, ms(testNow), 0)
}

// A metric's SQL reads the board's relations and calls the allowed
// functions, and nothing else: each hostile query fails with the sentence
// of what it tried, and the connection still answers afterwards.
func TestHostileQueriesAreRefused(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	for _, c := range []struct{ sql, want string }{
		{`ATTACH 'file::memory:' AS x`, "attaches or detaches a database"},
		{`SELECT name AS ticket FROM sqlite_schema`, "reads sqlite_master, which is not one of the board's relations"},
		{`SELECT name AS ticket FROM temp.sqlite_master`, "reads temp.sqlite_temp_master, outside the board's relations"},
		{`SELECT name AS ticket FROM pragma_table_info('tickets')`, "which a metric may not do"},
		{`PRAGMA query_only = 0`, "runs PRAGMA query_only"},
		{`SELECT name AS ticket FROM pragma_database_list`, "which a metric may not do"},
		{`WITH RECURSIVE x(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM x) SELECT n AS ticket FROM x`, "is recursive"},
		{`SELECT random() AS ticket`, "calls random()"},
		{`SELECT randomblob(8) AS ticket`, "calls randomblob()"},
		{`SELECT date('now') AS ticket`, "calls date()"},
		{`SELECT load_extension('x') AS ticket`, "calls load_extension()"},
		{`SELECT json('{}') AS ticket`, "calls json()"},
		{`SELECT bounded_rows(1) AS ticket`, "calls bounded_rows()"},
		{`SELECT trim(key, 'W') AS ticket FROM tickets`, "calls trim()"},
		{`SELECT ltrim(key) AS ticket FROM tickets`, "calls ltrim()"},
		{`SELECT rtrim(key, '0') AS ticket FROM tickets`, "calls rtrim()"},
		{`INSERT INTO people VALUES ('x', 'y')`, "writes people"},
		{`DELETE FROM tickets`, "writes tickets"},
		{`BEGIN`, "opens or ends a transaction"},
		{`SELECT 1 AS ticket; DELETE FROM tickets`, "holds more than one statement"},
		{`SELECT 1 AS ticket) SELECT 1 --`, "closes a parenthesis it never opened"},
		{`SELECT (1 AS ticket`, "leaves a parenthesis open"},
		{`SELECT 1 AS ticket /*`, "has a comment that is never closed"},
		{`SELECT 'x AS ticket`, "is never closed"},
		{`SELECT id FROM tickets`, `returns a column named "id"`},
		{`SELECT key AS note FROM tickets`, "returns no ticket column"},
		{`SELECT printf('%.*c', 100000, 'x') AS ticket`, "passed one of the limits on a metric"},
		{`SELECT id AS ticket, key AS value FROM tickets`, "column value holds text"},
		{`SELECT a.ticket AS ticket FROM changes a, changes b LIMIT 30000`, "more than 20000 rows, its limit"},
		{`SELECT a.ticket AS ticket, printf('%.*c', 60000, 'x') AS person FROM changes a`, "passed 4 MiB"},
		{`SELECT ?3 AS ticket`, "failed"},
		{`SELECT 'WEB-999' AS ticket`, `returned ticket "WEB-999", which is not one of this board's tickets`},
		{`SELECT id AS ticket, 'Doing' AS series FROM tickets`, "names no series, so its query returns no series column"},
		{`SELECT id AS ticket, 1e308 * 10 AS value FROM tickets`, `ticket "t1"'s value is +Inf, past 1e+12 either way`},
		{`SELECT id AS ticket, -2e12 AS value FROM tickets`, `ticket "t1"'s value is -2e+12, past 1e+12 either way`},
		{`SELECT id AS ticket, -1 AS at FROM tickets`, `ticket "t1"'s at is -1, not an instant in milliseconds from 1970 to 2286`},
	} {
		_, err := runSQL(s, metricOf(c.sql))
		var me *MetricError
		if !errors.As(err, &me) || !strings.Contains(me.Sentence, c.want) {
			t.Errorf("%s: %v; want a sentence with %q", c.sql, err, c.want)
		}
	}
	rows, err := runSQL(s, metricOf(`SELECT id AS ticket FROM tickets`))
	if err != nil || len(rows) != 10 {
		t.Fatalf("after the refusals: %d rows, %v", len(rows), err)
	}
}

// A row's series is one the metric declares, and the sentence says which
// it returned and which the metric names.
func TestRowsNameDeclaredSeries(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	m := Metric{Name: "probe", Title: "Probe", Words: []string{"A probe."}, Reduce: ReduceCount, Unit: UnitTickets,
		Series: []string{"To do", "Doing"}, SQL: `SELECT id AS ticket, iif(id = 't1', 'Doing', 'Blocked') AS series FROM tickets`}
	_, err := runSQL(s, m)
	var me *MetricError
	if !errors.As(err, &me) || !strings.Contains(me.Sentence, `returned series "Blocked", which its metric does not name (it names To do, Doing)`) {
		t.Fatalf("an undeclared series: %v", err)
	}
	m.SQL = `SELECT id AS ticket, iif(id = 't1', 'Doing', 'To do') AS series FROM tickets`
	if rows, err := runSQL(s, m); err != nil || len(rows) != 10 {
		t.Fatalf("declared series: %d rows, %v", len(rows), err)
	}
}

// A query that runs past its deadline is interrupted inside SQLite's
// first step and fails with the limit's sentence: the sort of 27 billion
// rows would otherwise run for hours, so the deadline's error is the
// proof, and the test takes no reading of the clock.
func TestRunawayQueryStopsAtItsDeadline(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	_, err := runSQL(s, metricOf(`SELECT a.ticket AS ticket FROM changes a, changes b, changes c ORDER BY a.at * b.at * c.at LIMIT 1`))
	var me *MetricError
	if !errors.As(err, &me) || !me.Limit || !strings.Contains(me.Sentence, "ran past its limit of 1s") {
		t.Fatalf("runaway query: %v", err)
	}
}

// A metric's deadline began at its prepare: the prepare records the time
// it took (within the call's), and a prepare that took the whole deadline
// leaves the run none, so a query that answers at once fails with the
// limit's sentence.
func TestTheDeadlineCountsThePrepare(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	start := time.Now()
	p, err := s.prepare(metricOf(`SELECT id AS ticket FROM tickets`))
	call := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if p.took <= 0 || p.took > call {
		t.Fatalf("the prepare recorded %v of a call of %v; want the time it took", p.took, call)
	}
	if rows, err := s.run(context.Background(), p, ms(testNow), 0); err != nil || len(rows) != 10 {
		t.Fatalf("the query after its own prepare: %d rows, %v", len(rows), err)
	}
	p.took = metricDeadline
	_, err = s.run(context.Background(), p, ms(testNow), 0)
	var me *MetricError
	if !errors.As(err, &me) || !me.Limit {
		t.Fatalf("the query after a prepare that took the whole deadline: %v; want the limit's sentence", err)
	}
}

// A LIKE or GLOB pattern past 256 bytes is refused, so one call, which
// SQLite cannot interrupt, stays short: 256 bytes against a 60,000-byte
// subject finishes inside the deadline, which a call past it would meet
// as the deadline's error at the statement's next step.
func TestLikePatternsAreBounded(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	for _, op := range []string{"LIKE '%' || printf('%.*c', 30000, 'a') || 'b'", "GLOB '*' || printf('%.*c', 30000, 'a') || 'b'"} {
		_, err := runSQL(s, metricOf(`SELECT 't1' AS ticket WHERE printf('%.*c', 60000, 'a') `+op))
		var me *MetricError
		if !errors.As(err, &me) || !me.Limit || !strings.Contains(me.Sentence, "passed one of the limits") {
			t.Errorf("a 30,000-byte pattern (%.4s): %v; want the limits sentence", op, err)
		}
	}
	rows, err := runSQL(s, metricOf(`SELECT 't1' AS ticket WHERE printf('%.*c', 60000, 'a') LIKE '%' || printf('%.*c', 250, 'a') || 'b'`))
	if err != nil || len(rows) != 0 {
		t.Fatalf("a 252-byte pattern: %d rows, %v", len(rows), err)
	}
}

// The gate fails closed: a fill that leaves another database attached
// fails the evaluation with the gate's refusal and no answer, its session
// closed and its slot given back; a fill whose detach failed fails with
// that error.
func TestGateRefusesAConnectionHoldingAnotherDatabase(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	leaky := func(ctx context.Context, conn *sql.Conn, _ []string) (store.MirrorExport, bool, error) {
		export, ok, err := f.fill()(ctx, conn, nil)
		if err != nil {
			return export, ok, err
		}
		if _, err := conn.ExecContext(ctx, `ATTACH 'file::memory:' AS mirror_file`); err != nil {
			return export, ok, err
		}
		_, err = conn.ExecContext(ctx, `CREATE TABLE mirror_file.leak(x); INSERT INTO mirror_file.leak VALUES (1)`)
		return export, ok, err
	}
	e := evaluator()
	_, err := e.Evaluate(context.Background(), Board{Fill: leaky, Epics: true}, utcSprints3)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != "dashboard_unsealed" || !strings.Contains(err.Error(), `still holds database "mirror_file"`) {
		t.Fatalf("evaluate with a database left attached = %v; want the gate's refusal", err)
	}
	failing := func(context.Context, *sql.Conn, []string) (store.MirrorExport, bool, error) {
		return store.MirrorExport{}, false, errors.New("store: detach the database after exporting board 10: busy")
	}
	if _, err := e.Evaluate(context.Background(), Board{Fill: failing}, utcSprints3); err == nil || !strings.Contains(err.Error(), "detach") {
		t.Fatalf("evaluate after a failed detach = %v; want its error", err)
	}
	// Both sessions closed: the slots are free for two evaluations at once.
	if len(e.slots) != 0 {
		t.Fatalf("%d slots held after the refusals", len(e.slots))
	}
}

// The gate's seals hold beneath the authorizer: with it taken off a
// sealed connection, an ATTACH passes the attached-database limit of 0,
// a write meets query_only, and every lowered limit reads back as set.
func TestGateSealsHoldWithoutTheAuthorizer(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	ctx := context.Background()
	err := s.conn.Raw(func(dc any) error {
		return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.conn.ExecContext(ctx, `ATTACH 'file::memory:' AS x`); err == nil || !strings.Contains(err.Error(), "too many attached databases") {
		t.Errorf("ATTACH without the authorizer = %v; want the attached-database limit's refusal", err)
	}
	if _, err := s.conn.ExecContext(ctx, `INSERT INTO people VALUES ('x', 'y')`); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Errorf("INSERT without the authorizer = %v; want query_only's refusal", err)
	}
	for _, l := range limits {
		if n, err := sqlite.Limit(s.conn, l.id, -1); err != nil || n != l.value {
			t.Errorf("limit %s = %d, %v; want %d", l.name, n, err, l.value)
		}
	}
}

// oneStatement cuts a trailing semicolon and keeps semicolons, quotes and
// parentheses inside strings, quoted names and comments.
func TestOneStatementReadsStringsAndComments(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"SELECT 1 AS ticket;":                       "SELECT 1 AS ticket",
		"SELECT 1 AS ticket; -- done\n":             "SELECT 1 AS ticket",
		"SELECT ');(' AS ticket":                    "SELECT ');(' AS ticket",
		`SELECT 1 AS "ti)ck;et"`:                    `SELECT 1 AS "ti)ck;et"`,
		"SELECT 1 AS [a;)] -- ) ;\n":                "SELECT 1 AS [a;)] -- ) ;\n",
		"SELECT /* ) ; */ 1 AS ticket":              "SELECT /* ) ; */ 1 AS ticket",
		"SELECT 'it''s' AS ticket":                  "SELECT 'it''s' AS ticket",
		"SELECT (SELECT 1) AS ticket /* end */ ;  ": "SELECT (SELECT 1) AS ticket /* end */ ",
	} {
		got, err := oneStatement(in)
		if err != nil || got != want {
			t.Errorf("oneStatement(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// The functions a session uses are registered once per zone and outlive
// no session's state: across opens, counts, failed fills, refused gates
// and closes in two zones, each zone's driver registered its four
// functions once and no session's sink is left in the registry. A key
// that names no open session fails the statement that uses it. Not
// parallel: it reads the registry, which parallel tests' sessions fill.
func TestSessionsLeaveNoRegistrationBehind(t *testing.T) {
	ctx := context.Background()
	york, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	e := evaluator()
	failing := func(context.Context, *sql.Conn, []string) (store.MirrorExport, bool, error) {
		return store.MirrorExport{}, false, errors.New("the fill failed")
	}
	unsealed := func(ctx context.Context, conn *sql.Conn, _ []string) (store.MirrorExport, bool, error) {
		export, ok, err := teamBoard().fill()(ctx, conn, nil)
		if err == nil {
			_, err = conn.ExecContext(ctx, `ATTACH 'file::memory:' AS left_open`)
		}
		return export, ok, err
	}
	for i := range 50 {
		zone := []string{"UTC", "America/New_York"}[i%2]
		if _, err := e.Evaluate(ctx, teamBoard().board(), Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: zone}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Evaluate(ctx, Board{Fill: failing}, utcSprints3); err == nil {
			t.Fatal("an evaluation whose fill failed answered")
		}
		if _, err := e.Evaluate(ctx, Board{Fill: unsealed}, utcSprints3); err == nil {
			t.Fatal("an evaluation whose gate refused answered")
		}
		if _, err := e.Drill(ctx, teamBoard().board(), DrillRequest{Request: utcSprints3, Widget: "stuck_list"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Dashboards(ctx, Kind{PlansSprints: true}, Own{}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := openSession(ctx, york)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	zoneDrivers.mu.Lock()
	zones, registrations := len(zoneDrivers.m), zoneDrivers.registrations
	_, hasUTC := zoneDrivers.m["UTC"]
	_, hasYork := zoneDrivers.m["America/New_York"]
	zoneDrivers.mu.Unlock()
	if !hasUTC || !hasYork || registrations != 4*zones {
		t.Errorf("%d registrations over %d zone drivers (UTC %v, New York %v); want 4 a zone", registrations, zones, hasUTC, hasYork)
	}
	sessionSinks.mu.Lock()
	left := len(sessionSinks.m)
	sessionSinks.mu.Unlock()
	if left != 0 {
		t.Errorf("%d session sinks left in the registry after every session closed", left)
	}
	// A statement naming a key no open session holds fails.
	s, err = openSession(ctx, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	}()
	var n int64
	err = s.conn.QueryRowContext(ctx, `SELECT bounded_rows('0123456789abcdef0123456789abcdef', 't1')`).Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "names no open dashboard session") {
		t.Errorf("bounded_rows under a key no session holds = %d, %v", n, err)
	}
}

// A process holds at most maxZoneDrivers zones' drivers, one a zone name
// as the count spells it: Europe/Paris, Europe/./Paris and Europe//Paris
// are three. A count that names one more is refused, saying that
// restarting the app clears it, and a name already held still counts.
// Not parallel: it fills the registry, and takes out what it added.
func TestZoneDriversStopAtTheirBound(t *testing.T) {
	ctx := context.Background()
	zoneDrivers.mu.Lock()
	before := map[string]bool{}
	for name := range zoneDrivers.m {
		before[name] = true
	}
	zoneDrivers.mu.Unlock()
	t.Cleanup(func() {
		zoneDrivers.mu.Lock()
		defer zoneDrivers.mu.Unlock()
		for name := range zoneDrivers.m {
			if !before[name] {
				delete(zoneDrivers.m, name)
				zoneDrivers.registrations -= 4
			}
		}
	})
	held := func() int {
		zoneDrivers.mu.Lock()
		defer zoneDrivers.mu.Unlock()
		return len(zoneDrivers.m)
	}
	e := evaluator()
	count := func(zone string) error {
		_, err := e.Evaluate(ctx, teamBoard().board(), Request{Dashboard: builtinSprintsID, Range: RangeSprints3, TimeZone: zone})
		return err
	}
	// A kind's first count prepares its metrics on a session in UTC.
	if err := count("UTC"); err != nil {
		t.Fatal(err)
	}
	n := held()
	for _, zone := range []string{"Europe/Paris", "Europe/./Paris", "Europe//Paris"} {
		if err := count(zone); err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
	}
	if got := held() - n; got != 3 {
		t.Fatalf("three spellings of Paris made %d drivers; each spelling is its own", got)
	}
	for i := 1; held() < maxZoneDrivers; i++ {
		if err := count("Europe/" + strings.Repeat("./", i+1) + "Paris"); err != nil {
			t.Fatalf("zone %d of %d: %v", held()+1, maxZoneDrivers, err)
		}
	}
	err := count("Europe/Berlin")
	var r *Refusal
	if !errors.As(err, &r) || r.Code != "dashboard_too_many_zones" || !strings.Contains(r.Error(), "32 time zones") || !strings.Contains(r.Error(), "restart") {
		t.Fatalf("a 33rd zone = %v; want the bound's refusal, which says restarting the app clears it", err)
	}
	if held() != maxZoneDrivers {
		t.Fatalf("%d drivers after the refusal; want %d", held(), maxZoneDrivers)
	}
	if err := count("Europe//Paris"); err != nil {
		t.Fatalf("a zone already held, after the bound: %v", err)
	}
}

// A session whose connection cannot be had leaves nothing behind: its
// sink's key is taken back and its database closed, so neither the
// registry nor the database's connection opener outlives the failure.
// Not parallel: it reads the registry, which parallel tests' sessions
// fill. In a bubble, so the opener's exit is waited for, not polled.
func TestASessionThatCannotConnectLeavesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := openSession(ctx, time.UTC); err == nil || !strings.Contains(err.Error(), "open the dashboard's database") {
			t.Fatalf("a session opened under a canceled context = %v; want its connection's failure", err)
		}
		sessionSinks.mu.Lock()
		left := len(sessionSinks.m)
		sessionSinks.mu.Unlock()
		if left != 0 {
			t.Errorf("%d session sinks left in the registry after the session failed to open", left)
		}
		synctest.Wait()
		// The bubble's own goroutines: an earlier test's closed database
		// can still be ending its opener outside it.
		buf := make([]byte, 1<<20)
		n := 0
		for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(g, "synctest bubble") && strings.Contains(g, "database/sql.(*DB).connectionOpener") {
				n++
			}
		}
		if n != 0 {
			t.Errorf("%d database connection openers run after the session failed to open; its database was not closed", n)
		}
	})
}
