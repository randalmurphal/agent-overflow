package trackerdash

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// sealed is a session filled with teamBoard and 3,000 changes, gated.
func sealed(t *testing.T) *session {
	t.Helper()
	ctx := context.Background()
	s, err := openSession(ctx, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	f := teamBoard().exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3000)
		INSERT INTO changes SELECT 't1', 'status', i, i, NULL, '1', '3', NULL, NULL FROM n`)
	if _, _, err := f.fill()(ctx, s.conn); err != nil {
		t.Fatal(err)
	}
	if err := s.gate(ctx); err != nil {
		t.Fatal(err)
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
	return s.run(context.Background(), p, ms(testNow))
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
		{`SELECT a.ticket AS ticket, printf('%.*c', 60000, 'x') AS series FROM changes a`, "passed 4 MiB"},
		{`SELECT ?3 AS ticket`, "failed"},
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

// A query that runs past its deadline is interrupted inside SQLite's
// first step and fails with the limit's sentence.
func TestRunawayQueryStopsAtItsDeadline(t *testing.T) {
	t.Parallel()
	s := sealed(t)
	start := time.Now()
	_, err := runSQL(s, metricOf(`SELECT a.ticket AS ticket FROM changes a, changes b, changes c ORDER BY a.at * b.at * c.at LIMIT 1`))
	elapsed := time.Since(start)
	var me *MetricError
	if !errors.As(err, &me) || !me.Limit || !strings.Contains(me.Sentence, "ran past its limit of 1s") {
		t.Fatalf("runaway query: %v", err)
	}
	if elapsed > metricDeadline+time.Second {
		t.Fatalf("the runaway query stopped after %v; its deadline is %v", elapsed, metricDeadline)
	}
}

// The gate fails closed: a fill that leaves another database attached
// fails the evaluation with the gate's refusal and no answer, its session
// closed and its slot given back; a fill whose detach failed fails with
// that error.
func TestGateRefusesAConnectionHoldingAnotherDatabase(t *testing.T) {
	t.Parallel()
	f := teamBoard()
	leaky := func(ctx context.Context, conn *sql.Conn) (store.MirrorExport, bool, error) {
		export, ok, err := f.fill()(ctx, conn)
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
	failing := func(context.Context, *sql.Conn) (store.MirrorExport, bool, error) {
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
