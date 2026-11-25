package store

import (
	"container/list"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// stmtCacheSize bounds the statements one connection keeps compiled.
const stmtCacheSize = 128

// textParam is the text parameter param ("?" or "?N") as an expression
// the planner cannot read, for a comparison with a column that a partial
// index pins to a literal. See docs/architecture/sqlite-store.md#connections.
func textParam(param string) string {
	return "CAST(" + param + " AS TEXT)"
}

// boundText is the positional text parameter the planner cannot read.
var boundText = textParam("?")

// sqlTextLiteral is value as an SQL string literal, for a code constant
// the planner must read to choose a partial index.
func sqlTextLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// cachedStmtRan, when set, sees each cached statement after each of its
// runs. Only the package's tests set it (sql_rebind_test.go), to fail
// a statement whose plan reads a bound value.
var cachedStmtRan func(stmt driver.Stmt, query string)

// sqliteConn is the driver connection surface database/sql uses on a
// modernc.org/sqlite connection. The statement cache forwards all of it.
type sqliteConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.ConnBeginTx
	driver.SessionResetter
	driver.Validator
	driver.Pinger
}

// sqliteStmt is the prepared statement surface the cache runs.
type sqliteStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

// stmtCacheConn keeps a connection's most recently used statements compiled.
//
// modernc.org/sqlite compiles every Exec and Query that is not a prepared
// statement and finalizes it after the call. SQLite compiles every trigger a
// write can fire, and every view a read names, into the statement, so the
// compile is most of a short statement's cost. The cache keys compiled
// statements by SQL text and runs them again with new bindings.
//
// A cached statement never runs against an old schema. modernc prepares with
// sqlite3_prepare_v2, whose statements recompile on their next step after any
// schema change from any connection, such as a migration, a deferred phase or
// a replaced view, and after a pragma that changes code generation.
//
// Only statements that read or write rows are cached; DDL, PRAGMA and
// transaction control compile per call as before. A statement whose rows are
// still open is busy, and running the same text again on the connection
// meanwhile compiles it for that call.
type stmtCacheConn struct {
	sqliteConn

	mu      sync.Mutex
	entries map[string]*list.Element
	// order holds *cachedStmt, most recently used first.
	order list.List
	// beforeClose, when set, releases what the connector attached to the
	// connection (the writer's commit hook) before it closes.
	beforeClose func()
	// writes, when set, counts the row writes the connection runs; see
	// Store.WriteCount. Only the writer's connections set it.
	writes *atomic.Uint64
}

type cachedStmt struct {
	query string
	stmt  sqliteStmt
	busy  bool
}

// newStmtCacheConn wraps conn, which must be a modernc.org/sqlite connection
// or a wrapper with its full surface.
func newStmtCacheConn(conn driver.Conn) (*stmtCacheConn, error) {
	full, ok := conn.(sqliteConn)
	if !ok {
		return nil, errors.Join(fmt.Errorf("store: driver connection %T cannot cache statements", conn), conn.Close())
	}
	return &stmtCacheConn{sqliteConn: full, entries: map[string]*list.Element{}}, nil
}

func (c *stmtCacheConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	kw := leadingKeyword(query)
	c.countWriteKeyword(kw, true)
	entry, err := c.acquireKeyword(ctx, query, kw)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return c.sqliteConn.ExecContext(ctx, query, args)
	}
	defer c.release(entry)
	return entry.stmt.ExecContext(ctx, args)
}

func (c *stmtCacheConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	kw := leadingKeyword(query)
	c.countWriteKeyword(kw, false)
	entry, err := c.acquireKeyword(ctx, query, kw)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return c.sqliteConn.QueryContext(ctx, query, args)
	}
	rows, err := entry.stmt.QueryContext(ctx, args)
	if err != nil {
		c.release(entry)
		return nil, err
	}
	return &cachedRows{Rows: rows, conn: c, entry: entry}, nil
}

// PrepareContext prepares query for database/sql's own statements
// (tx.Prepare), which run outside ExecContext and QueryContext, so a row
// write among them is counted on each run.
func (c *stmtCacheConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	prepared, err := c.sqliteConn.PrepareContext(ctx, query)
	if err != nil || c.writes == nil {
		return prepared, err
	}
	stmt, ok := prepared.(sqliteStmt)
	if !ok {
		return nil, errors.Join(fmt.Errorf("store: driver statement %T cannot be counted", prepared), prepared.Close())
	}
	return countedStmt{sqliteStmt: stmt, conn: c, query: query}, nil
}

// countWrite counts query when it writes rows. A WITH statement writes
// rows only when it is executed; run as a query, it reads them.
func (c *stmtCacheConn) countWrite(query string, exec bool) {
	if c.writes == nil {
		return
	}
	c.countWriteKeyword(leadingKeyword(query), exec)
}

func (c *stmtCacheConn) countWriteKeyword(kw string, exec bool) {
	if c.writes == nil {
		return
	}
	switch kw {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		c.writes.Add(1)
	case "WITH":
		if exec {
			c.writes.Add(1)
		}
	}
}

// countedStmt is a statement database/sql prepared on a counting
// connection.
type countedStmt struct {
	sqliteStmt
	conn  *stmtCacheConn
	query string
}

func (s countedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.conn.countWrite(s.query, true)
	return s.sqliteStmt.ExecContext(ctx, args)
}

func (s countedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.conn.countWrite(s.query, false)
	return s.sqliteStmt.QueryContext(ctx, args)
}

// acquireKeyword returns query's cached statement marked busy, compiling and
// caching it on first use. It returns nil when query is not cached or its
// statement is busy; the caller then runs query uncached.
func (c *stmtCacheConn) acquireKeyword(ctx context.Context, query, kw string) (*cachedStmt, error) {
	if !cacheableKeyword(kw) {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[query]; ok {
		entry := el.Value.(*cachedStmt)
		if entry.busy {
			return nil, nil
		}
		entry.busy = true
		c.order.MoveToFront(el)
		return entry, nil
	}
	prepared, err := c.sqliteConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	stmt, ok := prepared.(sqliteStmt)
	if !ok {
		return nil, errors.Join(fmt.Errorf("store: driver statement %T cannot be cached", prepared), prepared.Close())
	}
	c.evictLocked(stmtCacheSize - 1)
	entry := &cachedStmt{query: query, stmt: stmt, busy: true}
	c.entries[query] = c.order.PushFront(entry)
	return entry, nil
}

func (c *stmtCacheConn) release(entry *cachedStmt) {
	if cachedStmtRan != nil {
		cachedStmtRan(entry.stmt, entry.query)
	}
	c.mu.Lock()
	entry.busy = false
	c.mu.Unlock()
}

// evictLocked finalizes least recently used statements that are not busy
// until at most keep remain. Nothing waits on an eviction, so a failure is
// logged.
func (c *stmtCacheConn) evictLocked(keep int) {
	for el := c.order.Back(); el != nil && c.order.Len() > keep; {
		prev := el.Prev()
		entry := el.Value.(*cachedStmt)
		if !entry.busy {
			c.order.Remove(el)
			delete(c.entries, entry.query)
			if err := entry.stmt.Close(); err != nil {
				log.Printf("store: finalize evicted statement: %v", err)
			}
		}
		el = prev
	}
}

// Close finalizes the cached statements, then closes the connection.
// database/sql closes a connection only after every Rows on it has closed, so
// no cached statement is running.
func (c *stmtCacheConn) Close() error {
	if c.beforeClose != nil {
		c.beforeClose()
	}
	c.mu.Lock()
	var errs []error
	for el := c.order.Front(); el != nil; el = el.Next() {
		if err := el.Value.(*cachedStmt).stmt.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: finalize cached statement: %w", err))
		}
	}
	c.entries = map[string]*list.Element{}
	c.order.Init()
	c.mu.Unlock()
	return errors.Join(append(errs, c.sqliteConn.Close())...)
}

// cachedRows releases its statement to the cache when it closes. The driver
// resets the statement on close instead of finalizing it.
type cachedRows struct {
	driver.Rows
	conn  *stmtCacheConn
	entry *cachedStmt
}

func (r *cachedRows) Close() error {
	err := r.Rows.Close()
	if r.entry != nil {
		r.conn.release(r.entry)
		r.entry = nil
	}
	return err
}

// cacheableSQL reports whether query reads or writes rows, the statements a
// connection runs repeatedly.
func cacheableSQL(query string) bool {
	return cacheableKeyword(leadingKeyword(query))
}

func cacheableKeyword(kw string) bool {
	switch kw {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	}
	return false
}

// leadingKeyword is query's first ASCII word, upper-cased. Hot DML/DQL
// keywords return canonical literals without allocating.
func leadingKeyword(query string) string {
	i := 0
	for i < len(query) {
		c := query[i]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		i++
	}
	start := i
	for i < len(query) {
		c := query[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			break
		}
		i++
	}
	word := query[start:i]
	switch len(word) {
	case 4:
		if asciiEqualFold4(word, "WITH") {
			return "WITH"
		}
	case 6:
		switch {
		case asciiEqualFold6(word, "SELECT"):
			return "SELECT"
		case asciiEqualFold6(word, "INSERT"):
			return "INSERT"
		case asciiEqualFold6(word, "UPDATE"):
			return "UPDATE"
		case asciiEqualFold6(word, "DELETE"):
			return "DELETE"
		}
	case 7:
		if asciiEqualFold7(word, "REPLACE") {
			return "REPLACE"
		}
	}
	return strings.ToUpper(word)
}

func asciiEqualFold4(s, upper string) bool {
	return (s[0]&^0x20) == upper[0] && (s[1]&^0x20) == upper[1] &&
		(s[2]&^0x20) == upper[2] && (s[3]&^0x20) == upper[3]
}

func asciiEqualFold6(s, upper string) bool {
	return (s[0]&^0x20) == upper[0] && (s[1]&^0x20) == upper[1] &&
		(s[2]&^0x20) == upper[2] && (s[3]&^0x20) == upper[3] &&
		(s[4]&^0x20) == upper[4] && (s[5]&^0x20) == upper[5]
}

func asciiEqualFold7(s, upper string) bool {
	return (s[0]&^0x20) == upper[0] && (s[1]&^0x20) == upper[1] &&
		(s[2]&^0x20) == upper[2] && (s[3]&^0x20) == upper[3] &&
		(s[4]&^0x20) == upper[4] && (s[5]&^0x20) == upper[5] &&
		(s[6]&^0x20) == upper[6]
}
