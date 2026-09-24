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
)

// stmtCacheSize bounds the statements one connection keeps compiled.
const stmtCacheSize = 64

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
	entry, err := c.acquire(ctx, query)
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
	entry, err := c.acquire(ctx, query)
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

// acquire returns query's cached statement marked busy, compiling and caching
// it on first use. It returns nil when query is not cached or its statement is
// busy; the caller then runs query uncached.
func (c *stmtCacheConn) acquire(ctx context.Context, query string) (*cachedStmt, error) {
	if !cacheableSQL(query) {
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
	query = strings.TrimLeft(query, " \t\r\n")
	end := strings.IndexFunc(query, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
	if end < 0 {
		end = len(query)
	}
	switch strings.ToUpper(query[:end]) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	}
	return false
}
