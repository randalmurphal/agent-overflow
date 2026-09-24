package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

// sqlUse is what a driver connection was asked to do with a statement.
type sqlUse int

const (
	// sqlPrepared compiles a statement and keeps it prepared.
	sqlPrepared sqlUse = iota
	// sqlDirect runs a statement without a prepared statement; the driver
	// compiles and finalizes it on every call.
	sqlDirect
	// sqlStmtRun runs a prepared statement.
	sqlStmtRun
	// sqlStmtClose finalizes a prepared statement.
	sqlStmtClose
)

// sqlObserver sees each statement an observed connection handles, with the
// arguments of a run.
type sqlObserver func(conn *observedConn, use sqlUse, query string, args []driver.NamedValue)

// observedConnector wraps a modernc connector so a test sees every statement
// its connections compile, run and finalize. Put it under gatedConnector to
// observe the statement cache's use of the driver.
type observedConnector struct {
	driver.Connector
	observe sqlObserver
}

func (c observedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &observedConn{Conn: conn, observe: c.observe}, nil
}

// observedConn forwards every interface database/sql and the statement cache
// use on a modernc connection, so the pool behaves as it does in production.
type observedConn struct {
	driver.Conn
	observe sqlObserver
}

func (c *observedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.observe(c, sqlPrepared, query, nil)
	return &observedStmt{Stmt: stmt, conn: c, query: query}, nil
}

func (c *observedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.observe(c, sqlDirect, query, args)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *observedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.observe(c, sqlDirect, query, args)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *observedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *observedConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *observedConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c *observedConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

type observedStmt struct {
	driver.Stmt
	conn  *observedConn
	query string
}

func (s *observedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.conn.observe(s.conn, sqlStmtRun, s.query, args)
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *observedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.conn.observe(s.conn, sqlStmtRun, s.query, args)
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}

func (s *observedStmt) Close() error {
	s.conn.observe(s.conn, sqlStmtClose, s.query, nil)
	return s.Stmt.Close()
}

// openObservedPool opens a pool the way openPool does, with each connection
// reporting to observe.
func openObservedPool(t *testing.T, path string, pragmas []connPragma, conns int, observe sqlObserver) *sql.DB {
	t.Helper()
	base, err := sqlite.NewConnector(poolDSN(path, pragmas))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(gatedConnector{Connector: observedConnector{Connector: base, observe: observe}})
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	return db
}

type sqlUseKey struct {
	conn  *observedConn
	use   sqlUse
	query string
}

// sqlCounts counts each use of each statement text per connection.
type sqlCounts struct {
	mu   sync.Mutex
	uses map[sqlUseKey]int
}

func (c *sqlCounts) observe(conn *observedConn, use sqlUse, query string, _ []driver.NamedValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uses[sqlUseKey{conn: conn, use: use, query: query}]++
}

// total sums use of query over every connection.
func (c *sqlCounts) total(use sqlUse, query string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for key, count := range c.uses {
		if key.use == use && key.query == query {
			n += count
		}
	}
	return n
}

// snapshot copies the counts.
func (c *sqlCounts) snapshot() map[sqlUseKey]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[sqlUseKey]int, len(c.uses))
	for key, count := range c.uses {
		out[key] = count
	}
	return out
}

// openObservedStore opens a migrated store whose writer and read pool report
// every statement they compile, run and finalize.
func openObservedStore(t *testing.T) (*Store, *sqlCounts) {
	t.Helper()
	path := newTestStorePath(t)
	counts := &sqlCounts{uses: map[sqlUseKey]int{}}
	s := &Store{
		db:   openObservedPool(t, path, writerConnPragmas, 1, counts.observe),
		read: openObservedPool(t, path, readerConnPragmas, readPoolConns, counts.observe),
		path: path,
		gate: &connGate{},
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close observed store: %v", err)
		}
	})
	return s, counts
}

// cacheOf returns the statement cache of one connection of db.
func cacheOf(t *testing.T, conn *sql.Conn) *stmtCacheConn {
	t.Helper()
	var cache *stmtCacheConn
	if err := conn.Raw(func(dc any) error {
		var ok bool
		cache, ok = dc.(*stmtCacheConn)
		if !ok {
			return fmt.Errorf("driver connection is %T, not the statement cache", dc)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return cache
}

// TestStatementCacheCompilesOncePerConnection runs the hot item, payload and
// stamp writes, the unseal copies and the by-id read, and requires every
// statement that reads or writes rows to be compiled once per connection and
// run prepared after that.
func TestStatementCacheCompilesOncePerConnection(t *testing.T) {
	s, counts := openObservedStore(t)
	const thread = "hot"

	// Item inserts, payload upserts with their chunk and edit-snapshot
	// clears, payload appends with the owner touch, and plain history bumps.
	localHistoryFixture(t, s, thread, 30)
	for i := range 2 {
		item := Item{ID: fmt.Sprintf("with-payload-%d", i), ThreadID: thread, TurnIndex: 9, ItemIndex: i,
			Kind: "tool_call", Role: "assistant", Status: "completed"}
		payload := Payload{ID: fmt.Sprintf("inserted-%d", i), Kind: "tool_call_result", Meta: "{}", Data: []byte("out")}
		if err := s.InsertItemWithPayload(item, payload); err != nil {
			t.Fatal(err)
		}
	}
	// Streaming appends and the update that settles the row.
	streaming := Item{ID: "streaming", ThreadID: thread, TurnIndex: 10, Kind: "assistant_text", Role: "assistant", Status: "streaming"}
	if _, err := s.AppendItem(streaming); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []string{"one ", "two ", "three"} {
		if _, err := s.AppendItemSummary(thread, streaming.ID, delta, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendItemSummaryTail(thread, streaming.ID, delta, 8, 2); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"streaming", "completed"} {
		streaming.Status, streaming.Summary = status, "settled"
		if _, err := s.UpsertItem(streaming, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Item touches, as the proposed-plan mutators issue them.
	for range 2 {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := bumpHistoryRevForItemTx(tx, thread, streaming.ID, "test touch"); err != nil {
			t.Fatal(errors.Join(err, tx.Rollback()))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// The unseal copies, one run per piece: 20 rows are two pieces of
	// different sizes.
	sealItemsForTest(t, s, thread, "row-000", "row-001", "row-002", "row-003", "row-004", "row-005",
		"row-006", "row-007", "row-008", "row-009", "row-010", "row-011", "row-012", "row-013",
		"row-014", "row-015", "row-016", "row-017", "row-018", "row-019")
	if stats, err := s.UnsealThreadHistory(context.Background(), thread, nil); err != nil || stats.Rows != 20 {
		t.Fatalf("unseal: %+v, %v", stats, err)
	}
	// By-id reads on the read pool.
	for range 8 {
		if _, found, err := s.GetThreadItem(thread, "row-005"); err != nil || !found {
			t.Fatalf("GetThreadItem: %v, %v", found, err)
		}
	}

	uses := counts.snapshot()
	compiled := map[*observedConn]int{}
	runs := map[string]int{}
	for key, n := range uses {
		switch key.use {
		case sqlPrepared:
			compiled[key.conn]++
			if n != 1 {
				t.Errorf("compiled %d times on one connection:\n%s", n, key.query)
			}
		case sqlDirect:
			if cacheableSQL(key.query) {
				t.Errorf("ran %d times without the cache:\n%s", n, key.query)
			}
		case sqlStmtRun:
			runs[key.query] += n
		}
	}
	for conn, n := range compiled {
		if n >= stmtCacheSize {
			t.Fatalf("connection %p compiled %d statements, which the cache cannot hold; the check proves nothing", conn, n)
		}
	}
	hot := map[string]string{
		"item insert":       itemInsertSQL,
		"history_rev bump":  `UPDATE threads SET history_rev = history_rev + 1 WHERE id = ?`,
		"item touch":        `UPDATE items SET rev = rev WHERE thread_id = ? AND id = ?`,
		"payload insert":    payloadInsertSQL,
		"summary append":    `UPDATE items SET summary = summary || ?, updated_at = ? WHERE thread_id = ? AND id = ? AND status = 'streaming'`,
		"payload owner rev": touchPayloadOwnerRowsSQL,
	}
	for name, query := range hot {
		if runs[query] < 2 {
			t.Errorf("%s ran %d times prepared, want at least 2", name, runs[query])
		}
	}
	byID, unsealItems := 0, 0
	for query, n := range runs {
		if strings.Contains(query, "SELECT ? AS id") {
			byID += n
		}
		if strings.HasPrefix(strings.TrimSpace(query), "INSERT INTO items") && strings.Contains(query, "json_each(?3)") {
			unsealItems += n
		}
	}
	if byID < 8 {
		t.Errorf("by-id read ran %d times prepared, want 8", byID)
	}
	if unsealItems != 2 {
		t.Errorf("unseal item copy ran %d times prepared, want once per piece (2)", unsealItems)
	}
}

// TestStatementCacheFollowsSchemaChanges pins that a cached statement never
// runs against the schema it was compiled for once another connection has
// changed it: a replaced view, a renamed column and an added trigger.
func TestStatementCacheFollowsSchemaChanges(t *testing.T) {
	s, counts := openObservedStore(t)
	mustCreateThread(t, s, "t")
	other, err := sql.Open("sqlite", poolDSN(s.path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mustExec(t, other, `CREATE TABLE cache_probe (a INTEGER)`)
	mustExec(t, other, `INSERT INTO cache_probe (a) VALUES (7)`)
	mustExec(t, other, `CREATE VIEW cache_probe_view AS SELECT 1 AS x`)

	conn, err := s.read.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readOne := func(query string) (int, error) {
		var v int
		err := conn.QueryRowContext(context.Background(), query).Scan(&v)
		return v, err
	}
	const viewRead = `SELECT x FROM cache_probe_view`
	if v, err := readOne(viewRead); err != nil || v != 1 {
		t.Fatalf("view read = %d, %v; want 1", v, err)
	}
	mustExec(t, other, `DROP VIEW cache_probe_view`)
	mustExec(t, other, `CREATE VIEW cache_probe_view AS SELECT 2 AS x`)
	if v, err := readOne(viewRead); err != nil || v != 2 {
		t.Fatalf("view read after the view was replaced = %d, %v; want 2", v, err)
	}

	const columnRead = `SELECT a FROM cache_probe`
	if v, err := readOne(columnRead); err != nil || v != 7 {
		t.Fatalf("column read = %d, %v; want 7", v, err)
	}
	mustExec(t, other, `ALTER TABLE cache_probe RENAME COLUMN a TO b`)
	if _, err := readOne(columnRead); err == nil || !strings.Contains(err.Error(), "no such column: a") {
		t.Fatalf("column read after the column was renamed: %v, want no such column", err)
	}
	if n := counts.total(sqlPrepared, viewRead); n != 1 {
		t.Fatalf("view read compiled %d times by the cache, want 1: SQLite recompiles it in place", n)
	}

	insert := func(id string) error {
		return s.InsertItem(Item{ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: len(id),
			Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: id})
	}
	if err := insert("a"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, other, `CREATE TRIGGER refuse_ccc BEFORE INSERT ON items WHEN NEW.id = 'ccc'
		BEGIN SELECT RAISE(ABORT, 'refused by a trigger added after compile'); END`)
	if err := insert("ccc"); err == nil || !strings.Contains(err.Error(), "refused by a trigger added after compile") {
		t.Fatalf("insert after another connection added a trigger: %v, want the trigger's refusal", err)
	}
	mustExec(t, other, `DROP TRIGGER refuse_ccc`)
	if err := insert("ccc"); err != nil {
		t.Fatalf("insert after the trigger was dropped: %v", err)
	}
	if n := counts.total(sqlPrepared, itemInsertSQL); n != 1 {
		t.Fatalf("item insert compiled %d times by the cache, want 1", n)
	}
}

// TestStatementCacheFollowsForeignKeyPragma pins the code-generation pragma
// a table rebuild relies on: a cached insert compiled with foreign keys
// enforced stops enforcing them while the rebuild's connection has them off,
// and enforces them again once they are back on.
func TestStatementCacheFollowsForeignKeyPragma(t *testing.T) {
	s, counts := openObservedStore(t)
	mustExec(t, s.db, `CREATE TABLE cache_parent (id TEXT PRIMARY KEY)`)
	mustExec(t, s.db, `CREATE TABLE cache_child (parent_id TEXT NOT NULL REFERENCES cache_parent(id))`)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const insert = `INSERT INTO cache_child (parent_id) VALUES (?)`
	orphan := func() error {
		_, err := conn.ExecContext(ctx, insert, "missing")
		return err
	}
	if err := orphan(); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("orphan insert with foreign keys on: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if err := orphan(); err != nil {
		t.Fatalf("orphan insert with foreign keys off: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM cache_child`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if err := orphan(); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("orphan insert with foreign keys back on: %v", err)
	}
	if n := counts.total(sqlPrepared, insert); n != 1 {
		t.Fatalf("insert compiled %d times by the cache, want 1", n)
	}
}

// TestStatementCacheBusyStatementRunsUncached opens the same query twice on
// one connection: the nested run compiles its own statement, both read every
// row, and the cached statement is reused once free.
func TestStatementCacheBusyStatementRunsUncached(t *testing.T) {
	s, counts := openObservedStore(t)
	localHistoryFixture(t, s, "t", 3)
	const query = `SELECT id FROM items WHERE thread_id = ? ORDER BY id`
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	readAll := func(rows *sql.Rows) ([]string, error) {
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			ids = append(ids, id)
		}
		return ids, errors.Join(rows.Err(), rows.Close())
	}
	outer, err := tx.Query(query, "t")
	if err != nil {
		t.Fatal(err)
	}
	var outerIDs []string
	for outer.Next() {
		var id string
		if err := outer.Scan(&id); err != nil {
			t.Fatal(err)
		}
		outerIDs = append(outerIDs, id)
		inner, err := tx.Query(query, "t")
		if err != nil {
			t.Fatal(err)
		}
		innerIDs, err := readAll(inner)
		if err != nil || len(innerIDs) != 3 {
			t.Fatalf("nested read = %v, %v", innerIDs, err)
		}
	}
	if err := errors.Join(outer.Err(), outer.Close()); err != nil {
		t.Fatal(err)
	}
	if len(outerIDs) != 3 {
		t.Fatalf("outer read = %v", outerIDs)
	}
	if direct := counts.total(sqlDirect, query); direct != 3 {
		t.Fatalf("nested runs compiled per call %d times, want 3", direct)
	}
	again, err := tx.Query(query, "t")
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := readAll(again); err != nil || len(ids) != 3 {
		t.Fatalf("read after release = %v, %v", ids, err)
	}
	if prepared, runs := counts.total(sqlPrepared, query), counts.total(sqlStmtRun, query); prepared != 1 || runs != 2 {
		t.Fatalf("query compiled %d times and ran prepared %d times, want 1 and 2", prepared, runs)
	}
}

// TestStatementCacheEvictsLeastRecentlyUsed fills one connection past the
// bound: the least recently used statements are finalized, the cache never
// holds more than stmtCacheSize, and an evicted statement compiles again.
func TestStatementCacheEvictsLeastRecentlyUsed(t *testing.T) {
	s, counts := openObservedStore(t)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const extra = 10
	query := func(i int) string { return fmt.Sprintf("SELECT %d AS probe", i) }
	run := func(i int) {
		var v int
		if err := conn.QueryRowContext(context.Background(), query(i)).Scan(&v); err != nil || v != i {
			t.Fatalf("%s = %d, %v", query(i), v, err)
		}
	}
	for i := range stmtCacheSize + extra {
		run(i)
	}
	cache := cacheOf(t, conn)
	cache.mu.Lock()
	held := cache.order.Len()
	cache.mu.Unlock()
	if held != stmtCacheSize {
		t.Fatalf("cache holds %d statements, want %d", held, stmtCacheSize)
	}
	for i := range extra {
		if n := counts.total(sqlStmtClose, query(i)); n != 1 {
			t.Fatalf("%s finalized %d times on eviction, want 1", query(i), n)
		}
	}
	if n := counts.total(sqlStmtClose, query(extra)); n != 0 {
		t.Fatalf("%s finalized while still among the most recent", query(extra))
	}
	run(0)
	if n := counts.total(sqlPrepared, query(0)); n != 2 {
		t.Fatalf("evicted statement compiled %d times, want 2", n)
	}
	run(stmtCacheSize + extra - 1)
	if n := counts.total(sqlPrepared, query(stmtCacheSize+extra-1)); n != 1 {
		t.Fatalf("recent statement compiled %d times, want 1", n)
	}
}

// TestStatementCacheReusesAfterFailures runs cached statements into a
// constraint failure, a failing first step and a read cancelled part way;
// each time the next run of the same statement succeeds on the compiled
// statement.
func TestStatementCacheReusesAfterFailures(t *testing.T) {
	s, counts := openObservedStore(t)
	mustCreateThread(t, s, "t")
	insert := func(id string) error {
		return s.InsertItem(Item{ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: len(id),
			Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: id})
	}
	if err := insert("a"); err != nil {
		t.Fatal(err)
	}
	if err := insert("a"); err == nil {
		t.Fatal("duplicate item insert succeeded")
	}
	if err := insert("bb"); err != nil {
		t.Fatalf("insert after a constraint failure: %v", err)
	}

	const parse = `SELECT json(?) AS v`
	for _, arg := range []string{`{"a":1}`, `not json`, `[2]`} {
		var v string
		if err := s.db.QueryRow(parse, arg).Scan(&v); (arg == `not json`) != (err != nil) {
			t.Fatalf("json(%s) = %q, %v", arg, v, err)
		}
	}

	const count = `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100000) SELECT i FROM n`
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := s.db.QueryContext(ctx, count)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("first row: %v", rows.Err())
	}
	cancel()
	for rows.Next() {
	}
	if err := rows.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancelled part way: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.db.Query(count)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || n != 100000 {
		t.Fatalf("read after cancellation = %d rows, %v", n, err)
	}

	for _, want := range []struct {
		query string
		runs  int
	}{{itemInsertSQL, 3}, {parse, 3}, {count, 2}} {
		prepared, direct, runs := counts.total(sqlPrepared, want.query), counts.total(sqlDirect, want.query), counts.total(sqlStmtRun, want.query)
		if prepared != 1 || direct != 0 || runs != want.runs {
			t.Errorf("compiled %d times, ran %d times uncached and %d times prepared, want 1, 0 and %d:\n%s",
				prepared, direct, runs, want.runs, want.query)
		}
	}
}

// TestStatementCacheFinalizesWithItsConnection retires the writer
// connection the way ConvertToIncrementalVacuum does and closes the store:
// every compiled statement is finalized with its connection, and the
// replacement connection compiles its own.
func TestStatementCacheFinalizesWithItsConnection(t *testing.T) {
	path := newTestStorePath(t)
	counts := &sqlCounts{uses: map[sqlUseKey]int{}}
	s := &Store{
		db:   openObservedPool(t, path, writerConnPragmas, 1, counts.observe),
		read: openObservedPool(t, path, readerConnPragmas, readPoolConns, counts.observe),
		path: path,
		gate: &connGate{},
	}
	mustCreateThread(t, s, "t")
	insert := func(id string) error {
		return s.InsertItem(Item{ID: id, ThreadID: "t", TurnIndex: 0, ItemIndex: len(id),
			Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: id})
	}
	if err := insert("a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetThreadItem("t", "a"); err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("retire writer: %v", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	if n := counts.total(sqlStmtClose, itemInsertSQL); n != 1 {
		t.Fatalf("item insert finalized %d times with the retired writer, want 1", n)
	}
	if err := insert("bb"); err != nil {
		t.Fatalf("insert on the replacement writer: %v", err)
	}
	if n := counts.total(sqlPrepared, itemInsertSQL); n != 2 {
		t.Fatalf("item insert compiled %d times across two writer connections, want 2", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	uses := counts.snapshot()
	for key, n := range uses {
		if key.use != sqlPrepared {
			continue
		}
		if closed := uses[sqlUseKey{conn: key.conn, use: sqlStmtClose, query: key.query}]; closed != n {
			t.Errorf("compiled %d times and finalized %d times on one connection:\n%s", n, closed, key.query)
		}
	}
}

func TestCacheableSQL(t *testing.T) {
	for query, want := range map[string]bool{
		"SELECT 1":                             true,
		"\n\t  select id FROM items":           true,
		"INSERT INTO items VALUES (?)":         true,
		"UPDATE items SET rev = rev":           true,
		"DELETE FROM items":                    true,
		"REPLACE INTO t VALUES (1)":            true,
		"WITH x AS (SELECT 1) SELECT * FROM x": true,
		"PRAGMA foreign_keys = OFF":            false,
		"CREATE TABLE t (a)":                   false,
		"DROP VIEW v":                          false,
		"BEGIN IMMEDIATE":                      false,
		"ATTACH DATABASE ? AS restore_src":     false,
		"SELECTED":                             false,
		"":                                     false,
	} {
		if got := cacheableSQL(query); got != want {
			t.Errorf("cacheableSQL(%q) = %v, want %v", query, got, want)
		}
	}
}
