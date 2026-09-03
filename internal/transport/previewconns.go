package transport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
)

// The connections one preview listener is currently carrying, and the two
// reasons the gateway has to sever one.
//
// It exists because NOTHING ELSE REACHES AN UPGRADED CONNECTION. net/http
// stops tracking a connection the moment a handler takes it over from
// the server, which is what an UPGRADE does, so Server.Shutdown
// neither waits for it nor ends it, and the listener's own Close only
// stops new accepts. An HMR socket is exactly that shape, and it is the
// connection a preview holds open for hours. So retiring a port left a
// browser still streaming from a port this machine no longer shares, and
// the per-request principal check, the mechanism that ends a revoked
// device's previews, never ran again on a connection that makes no
// further requests.
//
// One registration covers both, because the reverse proxy's handler does
// not return until the copy in both directions is done: a hold taken
// before ServeHTTP and released after it spans the whole life of an
// upgraded connection as well as the milliseconds of an ordinary one.

// previewConnKey addresses the accepted connection on a request context.
// The value is put there by ConnContext, which is the only place net/http
// offers the connection before the handler runs.
type previewConnKey struct{}

// withPreviewConn is the http.Server.ConnContext for a preview listener.
func withPreviewConn(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, previewConnKey{}, conn)
}

// previewConns is one listener's live set: the connection currently
// serving a request, and the token of the grant that admitted it.
//
// Keyed by connection rather than by request, because a connection serves
// one request at a time (http/1.1 is all either listener source offers
// over ALPN) and the sweep acts on connections.
type previewConns struct {
	mu   sync.Mutex
	live map[net.Conn]string
}

// hold registers this request's connection for as long as it is being
// served, and returns the release. The grant's token travels with it, so
// the sweep re-asks every question this request was admitted on (the
// grant still exists, is for this port, has not lapsed, and names a
// principal still live) rather than about whatever cookie the connection
// happens to still carry.
//
// A request with no connection on its context is a test calling the
// handler directly; it holds nothing and releases nothing.
func (c *previewConns) hold(r *http.Request, token string) func() {
	conn, _ := r.Context().Value(previewConnKey{}).(net.Conn)
	if conn == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.live == nil {
		c.live = make(map[net.Conn]string, 4)
	}
	c.live[conn] = token
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.live, conn)
		c.mu.Unlock()
	}
}

// cutAll severs every connection this listener is carrying. Called when
// the port stops being shared, which is what "stop sharing" means: a
// socket that outlived the listener is serving a port nobody chose.
func (c *previewConns) cutAll() {
	c.mu.Lock()
	if len(c.live) == 0 {
		c.mu.Unlock()
		return
	}
	doomed := make([]net.Conn, 0, len(c.live))
	for conn := range c.live {
		doomed = append(doomed, conn)
	}
	clear(c.live)
	c.mu.Unlock()
	for _, conn := range doomed {
		cutPreviewConn(conn)
	}
}

// cutRefused severs every connection whose grant no longer admits it:
// lapsed, dropped, or naming a principal that is no longer live. admits
// is consulted OUTSIDE the lock: it reaches the session store, and
// holding a mutex across somebody else's read is how a sweep becomes a
// stall.
func (c *previewConns) cutRefused(admits func(token string) bool) {
	c.mu.Lock()
	if len(c.live) == 0 {
		c.mu.Unlock()
		return
	}
	held := make([]previewHeldConn, 0, len(c.live))
	for conn, token := range c.live {
		held = append(held, previewHeldConn{conn: conn, token: token})
	}
	c.mu.Unlock()

	for _, entry := range held {
		if admits(entry.token) {
			continue
		}
		c.mu.Lock()
		delete(c.live, entry.conn)
		c.mu.Unlock()
		cutPreviewConn(entry.conn)
	}
}

// previewHeldConn is one snapshot row of the live set.
type previewHeldConn struct {
	conn  net.Conn
	token string
}

// cutPreviewConn severs one connection at once.
//
// A tls.Conn's own Close writes a close_notify record first, which takes
// the write lock whatever is streaming on that connection is holding, so
// a courteous close is one that waits for the thing it is trying to stop.
// The socket underneath owes no such courtesy, and a cut is what both
// callers here mean.
func cutPreviewConn(conn net.Conn) {
	if secure, ok := conn.(*tls.Conn); ok {
		if inner := secure.NetConn(); inner != nil {
			_ = inner.Close()
			return
		}
	}
	_ = conn.Close()
}
