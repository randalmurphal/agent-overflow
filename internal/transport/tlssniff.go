package transport

import (
	"crypto/tls"
	"io"
	"net"
	"sync"
	"time"
)

// Same-port TLS.
//
// When a certificate is configured (Config.TLSCertificate), this
// listener answers a TLS client and a cleartext HTTP client on the ONE
// address it binds. A second https port was the alternative, and it
// would have made the pinned endpoint a different authority from the one
// the share URL and the pairing payload name — every consumer would then
// have to learn which of the two it was handed, and the cookie names,
// the origin allow-list and the port pin are all derived from a single
// authority today.
//
// Classification is the first byte, because the two protocols disagree
// about it unambiguously: every TLS connection opens with a handshake
// record (content type 0x16), and no HTTP request line can start with
// that byte. Nothing here parses further, and nothing decides per bind:
// loopback and LAN behave identically, so there is no mode in which
// pinning silently stops being available.
//
// Who this is for: a client that owns its TLS configuration and pins the
// fingerprint the pairing payload carried (docs/specs/remote-access.md
// §7). A browser cannot pin, so it keeps speaking cleartext on the same
// port and the share URL stays http://.
const tlsRecordTypeHandshake = 0x16

// tlsSniffListener classifies each accepted connection before handing it
// to http.Server.
//
// The classification runs on its own goroutine per connection rather
// than inline in Accept. Inline, one peer that connects and then says
// nothing would hold up every other client for the whole sniff window,
// repeatedly, at no cost to itself — on a listener the user can point at
// the LAN. Off the accept path, a silent connection costs one parked
// goroutine until its deadline and nothing else.
type tlsSniffListener struct {
	// The bound listener. Close also releases goroutines waiting to
	// deliver a classified connection or a temporary accept error.
	net.Listener

	tlsConfig *tls.Config

	// sniffTimeout bounds how long one connection may stay silent. It is
	// the server's own header-read budget: the byte being waited for is
	// the first byte of what would have been the request, so borrowing
	// that budget keeps one number where there would otherwise be two to
	// hold in sync — and matches what net/http would have done with a
	// connection that never spoke.
	sniffTimeout time.Duration

	// ready carries classified connections to Accept. Unbuffered on
	// purpose: a connection is either in a caller's hands or still owned
	// by the goroutine that can close it, never parked in a channel
	// nobody will drain.
	ready chan sniffAcceptResult

	// dead is closed on Close or a terminal accept error, after acceptErr is set.
	// Reading acceptErr is safe only after the close, which is the
	// happens-before this pair exists for.
	dead      chan struct{}
	acceptErr error
	endOnce   sync.Once
}

type sniffAcceptResult struct {
	conn net.Conn
	err  error
}

// serverTLSConfig turns the configured certificate source into what an
// accepted TLS connection is served with. Nil in, nil out: no source
// means no TLS half, and the sniff wrapper is never installed.
//
// Resolution is PER HANDSHAKE (GetCertificate) rather than a fixed
// Certificates slice, which is what lets a certificate arrive, renew or
// be replaced while the listener keeps running. A source that holds
// nothing yet is still a source: the wrapper installs, and the first
// handshake before a certificate lands is refused cleanly rather than
// requiring a restart once one does.
//
// The two settings are the protocol constraints this SERVER has, which
// is why they live here rather than with whoever minted the certificate:
//
//   - http/1.1 only. The `/ws` upgrade is an HTTP/1.1 mechanism and
//     needs the raw-connection takeover h2 does not offer, so ALPN must
//     never land on h2 — and net/http only wires an h2 handler for a
//     server that went through ServeTLS, which this one deliberately
//     does not.
//   - TLS 1.2 floor. Everything that pins this certificate is a current
//     Go process or engine; older peers would fail on the certificate
//     itself long before the protocol version mattered.
func serverTLSConfig(source *CertificateSource) *tls.Config {
	if source == nil {
		return nil
	}
	return &tls.Config{
		GetCertificate: source.certificateFor,
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"http/1.1"},
	}
}

// sniffTLS wraps a bound listener so it terminates TLS in-app. Returns
// the listener unchanged when no certificate is configured, which is
// what every boot without one — and every test that does not ask for
// TLS — keeps running on.
func sniffTLS(inner net.Listener, tlsConfig *tls.Config, sniffTimeout time.Duration) net.Listener {
	if tlsConfig == nil {
		return inner
	}
	l := &tlsSniffListener{
		Listener:     inner,
		tlsConfig:    tlsConfig,
		sniffTimeout: sniffTimeout,
		ready:        make(chan sniffAcceptResult),
		dead:         make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

// acceptLoop drains the bound listener and hands each connection to its
// own classification. Temporary errors are delivered once so net/http
// retains its retry/backoff behavior; caching one would permanently stop
// accepting while established WebSockets continued to work.
func (l *tlsSniffListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				select {
				case l.ready <- sniffAcceptResult{err: err}:
					continue
				case <-l.dead:
					return
				}
			}
			l.end(err)
			return
		}
		go l.classify(conn)
	}
}

func (l *tlsSniffListener) Accept() (net.Conn, error) {
	select {
	case result := <-l.ready:
		return result.conn, result.err
	case <-l.dead:
		return nil, l.acceptErr
	}
}

func (l *tlsSniffListener) end(err error) {
	l.endOnce.Do(func() {
		l.acceptErr = err
		close(l.dead)
	})
}

func (l *tlsSniffListener) Close() error {
	err := l.Listener.Close()
	l.end(net.ErrClosed)
	return err
}

// classify reads the one byte that decides what this connection is, then
// hands it on with that byte put back.
func (l *tlsSniffListener) classify(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Now().Add(l.sniffTimeout)); err != nil {
		_ = conn.Close()
		return
	}
	peeked := &peekedConn{Conn: conn}
	if _, err := io.ReadFull(conn, peeked.first[:]); err != nil {
		// A connection that produced nothing inside its window is
		// closed, and only it: the deadline is per-connection state, so
		// a peer that stalls cannot reach any other connection or the
		// accept loop.
		_ = conn.Close()
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return
	}
	var classified net.Conn = peeked
	if peeked.first[0] == tlsRecordTypeHandshake {
		// A real *tls.Conn, not a wrapper that happens to decrypt.
		// net/http keys the handshake, its deadline handling and r.TLS
		// off this exact type, and r.TLS is what the Secure cookie and
		// the wss:// manifest URL are derived from.
		classified = tls.Server(peeked, l.tlsConfig)
	}
	select {
	case l.ready <- sniffAcceptResult{conn: classified}:
	case <-l.dead:
		_ = classified.Close()
	}
}

// peekedConn replays the byte the sniff consumed, and is otherwise the
// socket underneath: deadlines, Close and the addresses a handler
// reports all have to keep reaching it, and the TLS layer sets its own
// read deadlines through here.
//
// It does hide *net.TCPConn's ReadFrom, so net/http cannot take its
// sendfile path on a cleartext response. Nothing this server writes
// comes from a file — embedded assets, JSON and WebSocket frames are all
// already in memory — so that path was never on this listener to lose.
type peekedConn struct {
	net.Conn
	first    [1]byte
	consumed bool
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if c.consumed {
		return c.Conn.Read(p)
	}
	if len(p) == 0 {
		return 0, nil
	}
	// One byte, then out: the caller loops (bufio and crypto/tls both
	// do), and filling the rest of p here would mean blocking for bytes
	// the peer may not have sent yet.
	p[0] = c.first[0]
	c.consumed = true
	return 1, nil
}
