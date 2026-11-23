package transport

import (
	"container/list"
	"context"
	"errors"
	"log"
	"sync"
)

// ConnState carries per-connection scratch space that survives the
// lifetime of one WebSocket: from upgrade until the connection closes
// (cleanly or otherwise). Its current job is to let RPC handlers bind
// releases that run when the underlying connection goes away (Bind) —
// the canonical use case is releasing long-lived subscriptions
// (gitwatch, future event streams) so a dropped client does not leak
// server-side resources.
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
	mu sync.Mutex
	// releases holds the bound releases in binding order, each a
	// *registration, so teardown runs the last bound first; byKey indexes
	// it, so a Bind or an Untie touches one entry whatever else is bound.
	releases list.List
	byKey    map[string]*list.Element
	// lastID numbers registrations, so a Tie names the one it made and an
	// Untie of an earlier registration under a reused key removes nothing.
	lastID uint64
	// walked counts the entries visited by walks of releases (each), the
	// one cost that grows with what is bound; the transport tests read it.
	walked int
	closed bool

	// principal is read at upgrade time and never written again, so it
	// needs no lock: a connection cannot change which session admitted it
	// or which screen it belongs to.
	principal ConnPrincipal
}

// registration is one release bound to run when the connection ends.
type registration struct {
	key     string
	id      uint64
	release func()
}

// each visits the bound releases in binding order. Every walk of releases
// goes through it, so walked counts them all. The caller holds c.mu.
func (c *ConnState) each(visit func(*registration)) {
	for el := c.releases.Front(); el != nil; el = el.Next() {
		c.walked++
		visit(el.Value.(*registration))
	}
}

// ConnPrincipal is who a connection is, resolved at upgrade time and fixed
// for its lifetime.
//
// A struct rather than loose parameters, because the two string answers
// are not interchangeable: SessionID is what this backend
// ADMITTED (SessionAuthority.Resolve verified a presented credential),
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
	// HostWindow is whether the connection is the host's own window, the
	// one client that can show the native views the host owns
	// (Server.hostWindow). A browser tab on this machine holding the same
	// launch credential is not.
	HostWindow bool
}

// callerProofKey addresses the per-CALL proof, which is deliberately not on
// the principal above: a principal is fixed for a connection, and step-up
// is answered per RPC (see CallerProof).
type callerProofKey struct{}

// WithCallerProof installs what one call proved about its caller, for the
// argument-dependent rechecks a bound method runs on itself.
//
// The transport resolves it once per RPC and the pre-call gate and the
// in-method recheck read that same value, so the two cannot disagree about
// whether this call was stepped up — which matters because the token is
// spent by the resolution and cannot be presented twice.
//
// A context nothing installed this on answers the zero proof, which is the
// honest answer for an in-process call: it proves nothing about a peer,
// and every gate that reads it admits such a caller on the session check
// before it ever asks.
func WithCallerProof(ctx context.Context, proof CallerProof) context.Context {
	return context.WithValue(ctx, callerProofKey{}, proof)
}

// CallerProofFromContext returns what this call proved about its caller.
func CallerProofFromContext(ctx context.Context) CallerProof {
	proof, _ := ctx.Value(callerProofKey{}).(CallerProof)
	return proof
}

// StepUpProvenFromContext reports whether this call carries a fresh
// step-up proof — host presence or a spent passkey token. The one reader a
// bound method needs; the disjunction itself is argued on stepUpProven.
func StepUpProvenFromContext(ctx context.Context) bool {
	return stepUpProven(CallerProofFromContext(ctx))
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

// HostWindow reports whether the connection is the host's own window. A
// nil state is an in-process caller, which is the host itself.
func (c *ConnState) HostWindow() bool {
	if c == nil {
		return true
	}
	return c.principal.HostWindow
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

// ConnStateFromContext extracts the per-connection ConnState if one was
// installed by the transport. Returns nil when the call came over no
// connection (a unit test, a background task, the Wails runtime route).
// The principal readers above treat nil as anonymous. A handler that ties
// a release to its connection takes the connection from CallerConnection
// instead, which refuses a call that came over none.
func ConnStateFromContext(ctx context.Context) *ConnState {
	state, _ := ctx.Value(connStateKey{}).(*ConnState)
	return state
}

// ErrNoConnection refuses a call that would tie a release to its
// connection when it came over none: nothing would ever run the release.
var ErrNoConnection = errors.New("this call opens something for its client connection to release, and it came over no connection")

// ErrConnectionClosing refuses a tie to a connection whose cleanup pass
// has begun: the release would never run.
var ErrConnectionClosing = errors.New("the connection closed while this call was opening something for it")

// CallerConnection answers the connection ctx's call came over, or
// ErrNoConnection when it came over none. A handler that arms a resource
// for its client calls it first, so a call nothing would release is
// refused before anything is armed or spent.
func CallerConnection(ctx context.Context) (*ConnState, error) {
	state := ConnStateFromContext(ctx)
	if state == nil {
		return nil, ErrNoConnection
	}
	return state, nil
}

// Tie is one registration of a release on a connection. The resource's
// record keeps it, so whoever removes the record, by id from any caller or
// on the resource's own end, unties the connection that holds it. The zero
// Tie is bound to nothing.
type Tie struct {
	conn *ConnState
	key  string
	id   uint64
}

// Bind ties release to the connection under key: the connection's end
// runs it once, unless the Tie it answers is untied first. A key names one
// release for the connection's life, so binding a key that is already
// bound keeps the first release and answers a Tie to it. Bind refuses with
// ErrNoConnection on a nil ConnState and with ErrConnectionClosing once the
// cleanup pass has begun, and binds nothing either way.
//
// Call it inside the critical section that publishes the resource, so a
// release that runs at once finds what it releases. release may take the
// resource's own lock, never c's, and must not block: connection teardown
// waits for it.
func (c *ConnState) Bind(key string, release func()) (Tie, error) {
	if key == "" || release == nil {
		return Tie{}, errors.New("transport: a tie needs a key and a release")
	}
	if c == nil {
		return Tie{}, ErrNoConnection
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Tie{}, ErrConnectionClosing
	}
	if el, ok := c.byKey[key]; ok {
		return Tie{conn: c, key: key, id: el.Value.(*registration).id}, nil
	}
	c.lastID++
	reg := &registration{key: key, id: c.lastID, release: release}
	if c.byKey == nil {
		c.byKey = make(map[string]*list.Element)
	}
	c.byKey[key] = c.releases.PushBack(reg)
	return Tie{conn: c, key: key, id: reg.id}, nil
}

// Untie removes the tie's registration from its connection, so a
// long-lived connection keeps no release for a resource that is gone. A key
// bound again after this tie's registration was removed is another
// registration, which this leaves bound. Idempotent; it does nothing for
// the zero Tie or once the cleanup pass has begun.
func (t Tie) Untie() {
	if t.conn == nil {
		return
	}
	c := t.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[t.key]
	if !ok || el.Value.(*registration).id != t.id {
		return
	}
	c.releases.Remove(el)
	delete(c.byKey, t.key)
}

// HeldBy reports whether the tie binds to conn, which is what makes the
// resource conn's client's. False for the zero Tie and for a nil conn.
func (t Tie) HeldBy(conn *ConnState) bool {
	return t.conn != nil && t.conn == conn
}

// Conn is the connection the tie binds to, nil for the zero Tie.
func (t Tie) Conn() *ConnState { return t.conn }

// Bound reports whether a release is still bound under key. False without
// a connection or once the cleanup pass has begun.
func (c *ConnState) Bound(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.byKey[key]
	return ok
}

// BoundKeys answers the keys releases are bound under, in binding order:
// what the connection's end would release now.
func (c *ConnState) BoundKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, c.releases.Len())
	c.each(func(reg *registration) { keys = append(keys, reg.key) })
	return keys
}

// RunCleanups executes every bound release in LIFO order. Called once by
// the connection handler when the WS shuts down. Later Binds refuse with
// ErrConnectionClosing (they belong to a connection that is already gone).
// Recovers panics so one badly-behaved release can't abort the rest.
// Idempotent: a second call is a no-op.
func (c *ConnState) RunCleanups() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	fns := make([]func(), 0, c.releases.Len())
	c.each(func(reg *registration) { fns = append(fns, reg.release) })
	c.releases.Init()
	c.byKey = nil
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
