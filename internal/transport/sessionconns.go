package transport

import "sync"

// SessionConns is the live-session registry: which upgraded WebSockets are
// carrying which session, and how to tear them down.
//
// It exists because revocation is only real if it reaches connections that
// are already open (docs/specs/remote-access.md §4). A revoked row stops
// the NEXT call; a socket that is mid-stream keeps receiving events until
// something closes it, and that something is CloseSession.
//
// This lives in the transport package because a WebSocket is a transport
// concept. internal/identity, which owns revocation, reaches it through
// the one-method interface it declares for itself, so neither package
// imports the other.
//
// The zero value is not usable; a Server builds one at construction and
// exposes it through SessionConns().
type SessionConns struct {
	mu sync.Mutex
	// bySession maps a session id to its live connections. The inner map
	// is keyed by an opaque handle rather than by anything about the
	// connection, so detaching is O(1) and two connections that look
	// identical stay distinct.
	//
	// Bounded by open sockets: an entry exists only between attach and
	// detach, and detach is registered as a connection cleanup, so it runs
	// on every close — graceful, abrupt, or panicking.
	bySession map[string]map[uint64]func()
	// nextHandle numbers attachments. Monotonic within a process; nothing
	// persists or compares handles across one.
	nextHandle uint64
}

func newSessionConns() *SessionConns {
	return &SessionConns{bySession: make(map[string]map[uint64]func())}
}

// attach records a live connection for sessionID and returns the function
// that removes it. The returned detach is idempotent, which matters
// because it runs from connection cleanup and may race a CloseSession that
// is already tearing the same connection down.
//
// close is invoked by CloseSession and must stop this connection's event
// delivery before it returns — see closeSessionConn in conn.go for why the
// socket alone is not enough.
//
// An empty sessionID attaches nothing and returns a no-op: a connection
// that names no session is the ordinary case today (the launch
// credential), and it must not occupy a slot under the empty key.
func (c *SessionConns) attach(sessionID string, close func()) (detach func()) {
	if c == nil || sessionID == "" || close == nil {
		return func() {}
	}
	c.mu.Lock()
	handle := c.nextHandle
	c.nextHandle++
	conns := c.bySession[sessionID]
	if conns == nil {
		conns = make(map[uint64]func(), 1)
		c.bySession[sessionID] = conns
	}
	conns[handle] = close
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			conns := c.bySession[sessionID]
			delete(conns, handle)
			if len(conns) == 0 {
				// Drop the empty map rather than leaving a shell behind:
				// a session that reconnects for months would otherwise
				// accumulate one entry per session id, forever.
				delete(c.bySession, sessionID)
			}
		})
	}
}

// CloseSession force-closes every connection carrying sessionID and
// returns how many it closed. Satisfies internal/identity's LiveConns.
//
// Synchronous by contract: it returns only after every close callback has
// run, so a caller that has revoked a row and called this knows no socket
// is still streaming under that credential. Callbacks run OUTSIDE the
// registry lock — a teardown that blocked while holding it would stall
// every other attach and detach on the process.
//
// A session with no connections answers 0, which is a normal answer and
// covers both "already disconnected" and "never connected". The registry
// deliberately keeps no tombstone for a revoked session: refusing a later
// reconnection is the database row's job, and a second source of that
// truth would be one that could disagree.
func (c *SessionConns) CloseSession(sessionID string) int {
	if c == nil || sessionID == "" {
		return 0
	}
	c.mu.Lock()
	conns := c.bySession[sessionID]
	closers := make([]func(), 0, len(conns))
	for _, close := range conns {
		closers = append(closers, close)
	}
	// Drop the whole entry now. A connection whose close is running is
	// gone as far as this registry is concerned, and its own detach will
	// find nothing left to remove.
	delete(c.bySession, sessionID)
	c.mu.Unlock()

	for _, close := range closers {
		close()
	}
	return len(closers)
}

// CountForSession reports how many live connections carry sessionID. For
// tests and for a device-management surface that shows "connected now".
func (c *SessionConns) CountForSession(sessionID string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bySession[sessionID])
}

// Sessions reports how many distinct sessions currently hold at least one
// connection. The registry's own bound, readable so a test can assert it
// returns to zero.
func (c *SessionConns) Sessions() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bySession)
}
