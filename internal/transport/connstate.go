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
// It also carries the connection's ConnPrincipal — the durable session it
// presented and which screen is on the other end — for handlers that
// attribute a write, let a client recognize the echo of its own change, or
// scope persisted state to the caller rather than to a string the caller
// supplied.
//
// One ConnState per connection. The dispatcher injects it into the
// per-call ctx via WithConnState so handlers can pull it out of their
// receiver method's `context.Context` parameter — see
// ConnStateFromContext.
type ConnState struct {
	mu       sync.Mutex
	cleanups []func()
	closed   bool

	// principal is read at upgrade time and never written again, so it
	// needs no lock: a connection cannot change which session admitted it
	// or which screen it belongs to.
	principal ConnPrincipal
}

// ConnPrincipal is who a connection is, resolved at upgrade time and fixed
// for its lifetime.
//
// A struct rather than loose parameters, because the two string answers
// are not interchangeable: SessionID is what this backend
// ADMITTED (Config.SessionForRequest verified a presented credential),
// while Client is what the peer DECLARED on its upgrade URL. A handler
// scoping durable state wants the first wherever it exists; a handler
// suppressing a client's echo of its own write wants the second. Named
// fields are what stop a call site swapping them.
type ConnPrincipal struct {
	// Client is the screen on the other end. Zero means the peer declared
	// none, which is normal (the harness, the e2e rig, tests).
	Client ClientIdentity
	// SessionID is the durable session this connection presented, empty
	// when it named none — every launch-credential client today.
	SessionID string
	// HostPresent is whether the peer is on this machine. It is what a
	// step-up proof resolves to this phase (transport.stepUpProven), so a
	// bound method whose ARGUMENTS reach something §4 puts behind a fresh
	// proof reads it here rather than re-deriving "local" from a header
	// the peer supplied.
	HostPresent bool
}

type connStateKey struct{}

// WithConnState returns ctx augmented with a fresh ConnState carrying the
// connection's principal. The returned ConnState is the one accessible via
// ConnStateFromContext on any descendant of ctx.
//
// Pass the zero ConnPrincipal for a connection with no session and no screen
// behind it (the harness, tests, in-process bindings).
func WithConnState(ctx context.Context, principal ConnPrincipal) (context.Context, *ConnState) {
	state := &ConnState{principal: principal}
	return context.WithValue(ctx, connStateKey{}, state), state
}

// Client returns the identity the connection declared at upgrade. The zero
// value means anonymous, which every caller must treat as a normal answer.
func (c *ConnState) Client() ClientIdentity {
	if c == nil {
		return ClientIdentity{}
	}
	return c.principal.Client
}

// SessionID returns the durable session the connection presented, or "" when
// it presented none.
//
// Fixed at upgrade, so it answers "which session was admitted here", never
// "is that session still live". A handler that authorizes on it must re-ask
// the session core, because a revocation lands after the upgrade that
// recorded this.
func (c *ConnState) SessionID() string {
	if c == nil {
		return ""
	}
	return c.principal.SessionID
}

// HostPresent reports whether the peer is on this machine, fixed at upgrade
// like the rest of the principal.
func (c *ConnState) HostPresent() bool {
	if c == nil {
		return false
	}
	return c.principal.HostPresent
}

// ClientFromContext is the one-liner handlers use: the identity of the screen
// this call came from, or the zero value when there is none (an in-process
// binding, a background saga, a test).
func ClientFromContext(ctx context.Context) ClientIdentity {
	return ConnStateFromContext(ctx).Client()
}

// SessionFromContext is the session half of the same one-liner: the durable
// session this call's connection presented, or "" when it presented none.
func SessionFromContext(ctx context.Context) string {
	return ConnStateFromContext(ctx).SessionID()
}

// HostPresentFromContext reports whether this call came from a peer on this
// machine. False for a context no transport connection installed, which is
// the honest answer: an in-process caller proves nothing about a peer, and
// every gate that reads this admits such a caller on the session check
// before it ever asks.
func HostPresentFromContext(ctx context.Context) bool {
	return ConnStateFromContext(ctx).HostPresent()
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
