package transport

import (
	"context"
	"log"
	"sync"
)

// ConnState carries per-connection scratch space that survives the
// lifetime of one WebSocket: from upgrade until the connection closes
// (cleanly or otherwise). Its current job is to let RPC handlers
// register cleanup callbacks that fire when the underlying connection
// goes away — the canonical use case is releasing long-lived
// subscriptions (gitwatch, future event streams) so a dropped client
// does not leak server-side resources.
//
// One ConnState per connection. The dispatcher injects it into the
// per-call ctx via WithConnState so handlers can pull it out of their
// receiver method's `context.Context` parameter — see
// ConnStateFromContext.
type ConnState struct {
	mu       sync.Mutex
	cleanups []func()
	closed   bool
}

type connStateKey struct{}

// WithConnState returns ctx augmented with a fresh ConnState. The
// returned ConnState is the one accessible via ConnStateFromContext on
// any descendant of ctx.
func WithConnState(ctx context.Context) (context.Context, *ConnState) {
	state := &ConnState{}
	return context.WithValue(ctx, connStateKey{}, state), state
}

// ConnStateFromContext extracts the per-connection ConnState if one was
// installed by the transport. Returns nil when called from a context
// not derived from a transport connection (e.g. unit tests, Wails
// in-process bindings) — handlers should treat nil as "no per-conn
// cleanup is available; skip the registration".
func ConnStateFromContext(ctx context.Context) *ConnState {
	state, _ := ctx.Value(connStateKey{}).(*ConnState)
	return state
}

// RegisterCleanup adds fn to the list of callbacks that will run when
// the connection ends. Returns true if registered, false if the
// connection is already closing — in which case the caller should run
// fn itself (or no-op, depending on semantics) since the cleanup pass
// has already started or finished.
//
// fn must not block — connection teardown waits for it.
func (c *ConnState) RegisterCleanup(fn func()) bool {
	if fn == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.cleanups = append(c.cleanups, fn)
	return true
}

// RunCleanups executes every registered callback in LIFO order. Called
// once by the connection handler when the WS shuts down. Subsequent
// RegisterCleanup calls return false (they belong to a connection that
// is already gone). Recovers panics so one badly-behaved cleanup can't
// abort the rest. Idempotent: a second call is a no-op.
func (c *ConnState) RunCleanups() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	fns := c.cleanups
	c.cleanups = nil
	c.mu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		fn := fns[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("transport: cleanup panic: %v", r)
				}
			}()
			fn()
		}()
	}
}
