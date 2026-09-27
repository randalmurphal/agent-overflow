package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"

	sqlite "modernc.org/sqlite"
)

// readPoolConns bounds the read-only pool: historyReadSlots and one more.
// Small enough that the file swap can prove the pool is empty quickly.
const readPoolConns = 5

// historyReadSlots bounds the read-pool connections held by reads whose
// duration grows with a thread's history (historyReadSnapshot) and by
// checkpoints, and fits the history reads one screen issues at once. A
// page read over a long activity run holds its connection for hundreds of
// milliseconds; without the bound a few panes opening at once hold the
// whole pool, and provider event handling, whose per-event reads are
// single index lookups, queues behind them while the live stream stalls.
const historyReadSlots = readPoolConns - 1

// connGate holds new physical connections off the database file.
//
// database/sql opens a replacement connection whenever a pooled one is
// discarded or the pool grows, and it does so from its own goroutine
// with no hook of its own. ConvertToIncrementalVacuum replaces the file
// under both pools: between the moment it proves no connection is left
// on the old file and the moment the rename lands, a connection opened
// by any other goroutine would attach to the file about to be renamed
// away, and its writes would be lost with it. Holding the gate makes
// that window unreachable instead of merely unlikely.
//
// The zero value is open. Only one holder at a time is supported, which
// fileMu already guarantees.
type connGate struct {
	mu   sync.Mutex
	held chan struct{}
}

// hold closes the gate and returns the release function. Callers must
// release it, including on every failure path.
func (g *connGate) hold() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held != nil {
		// fileMu serializes the only caller; a second holder would mean
		// two swaps in flight, which the release below cannot express.
		panic("store: connection gate already held")
	}
	held := make(chan struct{})
	g.held = held
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.held = nil
			g.mu.Unlock()
			close(held)
		})
	}
}

// wait blocks until the gate is open or ctx is done.
func (g *connGate) wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		held := g.held
		g.mu.Unlock()
		if held == nil {
			return nil
		}
		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// gatedConnector is the driver connector both pools open through. It
// waits on the gate, then opens the driver's connection behind a statement
// cache.
type gatedConnector struct {
	driver.Connector
	gate *connGate
	// commits, when set, is signalled by every commit on the connections
	// this connector opens: the writer's, for the checkpointer.
	commits commitSignal
}

// commitHooker is the modernc.org/sqlite connection's commit hook surface.
type commitHooker interface {
	RegisterCommitHook(sqlite.CommitHookFn)
}

func (c gatedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.gate != nil {
		if err := c.gate.wait(ctx); err != nil {
			return nil, err
		}
	}
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	cached, err := newStmtCacheConn(conn)
	if err != nil {
		return nil, err
	}
	if c.commits != nil {
		hooked, ok := conn.(commitHooker)
		if !ok {
			return nil, errors.Join(fmt.Errorf("store: driver connection %T has no commit hook", conn), cached.Close())
		}
		hooked.RegisterCommitHook(c.commits.hook)
		// The driver keeps each registered hook in a process-wide map
		// until it is unregistered, so a closed connection would
		// otherwise leave its entry behind.
		cached.beforeClose = func() { hooked.RegisterCommitHook(nil) }
	}
	return cached, nil
}

// openPool opens one pool against dbPath with the given connection
// pragmas, routed through gate. A non-nil commits is signalled by every
// commit on the pool's connections.
//
// sql.OpenDB rather than sql.Open: the gate has to sit in front of the
// physical open, and interposing on Connect is the supported way to do
// that (modernc.org/sqlite documents NewConnector for exactly this).
// The DSN is unchanged, so the pragmas still ride every connection.
func openPool(dbPath string, pragmas []connPragma, gate *connGate, commits commitSignal) (*sql.DB, error) {
	base, err := sqlite.NewConnector(poolDSN(dbPath, pragmas))
	if err != nil {
		return nil, fmt.Errorf("store: connector for %s: %w", dbPath, err)
	}
	return sql.OpenDB(gatedConnector{Connector: base, gate: gate, commits: commits}), nil
}
