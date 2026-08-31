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
// It also carries the connection's ClientIdentity — which screen is on the
// other end — for handlers that attribute a write or let a client recognize
// the echo of its own change.
//
// One ConnState per connection. The dispatcher injects it into the
// per-call ctx via WithConnState so handlers can pull it out of their
// receiver method's `context.Context` parameter — see
// ConnStateFromContext.
type ConnState struct {
	mu       sync.Mutex
	cleanups []func()
	closed   bool

	// client is read at upgrade time and never written again, so it needs
	// no lock: a connection cannot change which screen it belongs to.
	client ClientIdentity
}

type connStateKey struct{}

// WithConnState returns ctx augmented with a fresh ConnState carrying the
// connection's client identity. The returned ConnState is the one accessible
// via ConnStateFromContext on any descendant of ctx.
//
// Pass the zero ClientIdentity for a connection with no screen behind it (the
// harness, tests, in-process bindings).
func WithConnState(ctx context.Context, client ClientIdentity) (context.Context, *ConnState) {
	state := &ConnState{client: client}
	return context.WithValue(ctx, connStateKey{}, state), state
}

// Client returns the identity the connection declared at upgrade. The zero
// value means anonymous, which every caller must treat as a normal answer.
func (c *ConnState) Client() ClientIdentity {
	if c == nil {
		return ClientIdentity{}
	}
	return c.client
}

// ClientFromContext is the one-liner handlers use: the identity of the screen
// this call came from, or the zero value when there is none (an in-process
// binding, a background saga, a test).
func ClientFromContext(ctx context.Context) ClientIdentity {
	return ConnStateFromContext(ctx).Client()
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
