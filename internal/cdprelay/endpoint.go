package cdprelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"agent-overflow/internal/loopback"
	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

// Endpoint is the backend end of the CDP tunnel.
//
// Two faces, one object. To the transport it is the consumer of the
// authenticated /browser-cdp WebSocket the Windows launcher dials
// (ServeCDPTunnel). To chromedp it is an ordinary loopback listener inside
// the distro: every connection accepted there becomes one tunnel stream,
// and the launcher pipes it to the pane environment's debugging port.
//
// Exactly ONE tunnel connection is live at a time. The launcher reconnects
// on a backoff ladder after a WSL suspend or a launcher restart, and the
// newest connection wins: a half-dead socket the kernel has not yet
// reported cannot keep the live one out. There is no resume — the codec
// says so (internal/webview2host/cdpframe.go) — so replacing a connection
// drops every stream on it and chromedp reconnects.
type Endpoint struct {
	logf func(string, ...any)

	listener net.Listener
	addr     string
	// openTimeout bounds one stream's wait for `opened`. A field rather
	// than the constant directly so tests can drive the timeout path
	// without spending it.
	openTimeout time.Duration
	// streamBounds bound each stream's writes to its local connection
	// (webview2host.DefaultTunnelStreamBounds); a test changes them.
	streamBounds webview2host.TunnelStreamBounds

	mu      sync.Mutex
	session *session
	// ready is closed when a session is installed and replaced with a
	// fresh open channel when the live one drops, so a waiter parked
	// across a reconnect wakes exactly once per connection.
	ready  chan struct{}
	closed bool

	wg sync.WaitGroup
}

// Config configures an Endpoint. Every field is optional.
type Config struct {
	// Logf receives relay diagnostics. Defaults to log.Printf.
	Logf func(string, ...any)
}

const (
	// streamOpenTimeout bounds the wait for the launcher's `opened`
	// answer. The launcher dials 127.0.0.1 on its own side with a 3s
	// dial timeout, so anything past this is a launcher that stopped
	// reading rather than a slow CDP endpoint.
	streamOpenTimeout = 10 * time.Second
	// writeTimeout bounds one WebSocket write, mirroring the launcher's
	// own 30s bound: a peer that stops draining must not wedge a pump
	// holding the write lock.
	writeTimeout = 30 * time.Second
	// discoveryTimeout bounds the one /json/version request the engine
	// makes through the tunnel before attaching chromedp.
	discoveryTimeout = 15 * time.Second
	// maxVersionBytes bounds the /json/version body. The real document is
	// a few hundred bytes; the cap prevents an oversized endpoint response
	// from making the backend allocate without bound.
	maxVersionBytes = 64 << 10
)

// ErrRelayClosed reports that the endpoint has been shut down.
var ErrRelayClosed = errors.New("cdprelay: endpoint closed")

// ErrTunnelDown reports that no launcher tunnel is connected.
var ErrTunnelDown = errors.New("cdprelay: no browser host tunnel is connected")

// New binds the loopback listener and starts accepting. It opens no
// outbound connection: the launcher dials the backend, never the reverse
// (internal/webview2host/AGENTS.md § The launcher is the only dialer).
func New(config Config) (*Endpoint, error) {
	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}
	listener, err := net.Listen("tcp6", loopback.EphemeralIPv6)
	if err != nil {
		return nil, fmt.Errorf("cdprelay: listen on loopback: %w", err)
	}
	e := &Endpoint{
		logf:         logf,
		listener:     listener,
		addr:         listener.Addr().String(),
		openTimeout:  streamOpenTimeout,
		streamBounds: webview2host.DefaultTunnelStreamBounds,
		ready:        make(chan struct{}),
	}
	e.wg.Add(1)
	go e.accept()
	return e, nil
}

// Addr is the loopback address chromedp connects to, e.g. [::1]:41235.
func (e *Endpoint) Addr() string { return e.addr }

// Close stops accepting, drops the live tunnel, and waits for every pump
// to finish.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	live := e.session
	e.session = nil
	// Wake every WaitConnected. The channel may already be closed (a
	// tunnel was live), so the close is guarded rather than unconditional.
	select {
	case <-e.ready:
	default:
		close(e.ready)
	}
	e.mu.Unlock()
	err := e.listener.Close()
	if live != nil {
		live.shutdown()
	}
	e.wg.Wait()
	return err
}

func (e *Endpoint) accept() {
	defer e.wg.Done()
	for {
		local, err := e.listener.Accept()
		if err != nil {
			e.mu.Lock()
			closed := e.closed
			e.mu.Unlock()
			if !closed {
				e.logf("cdprelay: accept: %v", err)
			}
			return
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.serveLocal(local)
		}()
	}
}

// serveLocal turns one accepted loopback connection into one tunnel
// stream: open, wait for the launcher's `opened`, then pipe until either
// end finishes.
func (e *Endpoint) serveLocal(local net.Conn) {
	defer func() { _ = local.Close() }()
	e.mu.Lock()
	live, closed := e.session, e.closed
	e.mu.Unlock()
	if closed || live == nil {
		// chromedp reached the listener before (or after) a launcher tunnel
		// existed. Closing is the honest answer: there is nothing to relay
		// to, and holding the connection open would look like a live CDP
		// endpoint that never answers.
		e.logf("cdprelay: refusing local connection: %v", ErrTunnelDown)
		return
	}
	st, err := live.openStream(local)
	if err != nil {
		e.logf("cdprelay: open tunnel stream: %v", err)
		return
	}
	live.pump(st)
}

// ServeCDPTunnel consumes one authenticated /browser-cdp connection. It
// returns when the connection ends; the transport route owns the socket's
// lifetime and its authorization.
func (e *Endpoint) ServeCDPTunnel(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(webview2host.MaxTunnelFrameBytes)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &session{endpoint: e, conn: conn, ctx: ctx, streams: make(map[uint32]*stream, 8)}
	if !e.install(s) {
		_ = conn.Close(websocket.StatusGoingAway, "relay closed")
		return
	}
	defer func() {
		e.uninstall(s)
		s.shutdown()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	e.logf("cdprelay: browser host tunnel connected")
	for {
		kind, payload, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				e.logf("cdprelay: browser host tunnel disconnected: %v", err)
			}
			return
		}
		switch kind {
		case websocket.MessageText:
			s.handleControl(payload)
		case websocket.MessageBinary:
			s.handleData(payload)
		}
	}
}

// install makes s the live session, retiring whichever one was there. The
// launcher's reconnect ladder means a stale socket can outlive its
// usefulness by a full TCP timeout; the newest connection is always the
// one that just proved it can reach us.
func (e *Endpoint) install(s *session) bool {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false
	}
	previous := e.session
	e.session = s
	select {
	case <-e.ready:
	default:
		close(e.ready)
	}
	e.mu.Unlock()
	if previous != nil {
		e.logf("cdprelay: replacing a stale browser host tunnel")
		previous.shutdown()
	}
	return true
}

// uninstall clears s only if it is still the live session: a connection
// that was already replaced must not take its successor down with it.
func (e *Endpoint) uninstall(s *session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != s || e.closed {
		return
	}
	e.session = nil
	e.ready = make(chan struct{})
}

// WaitConnected blocks until a launcher tunnel is live, ctx ends, or the
// endpoint closes.
func (e *Endpoint) WaitConnected(ctx context.Context) error {
	for {
		e.mu.Lock()
		live, closed, ready := e.session, e.closed, e.ready
		e.mu.Unlock()
		if closed {
			return ErrRelayClosed
		}
		if live != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrTunnelDown, ctx.Err())
		case <-ready:
		}
	}
}

// BrowserWebSocketURL discovers the pane environment's CDP browser
// endpoint THROUGH the tunnel and returns it addressed at this endpoint's
// own loopback listener.
//
// The rewrite is the security property, not a convenience: /json/version
// answers with a webSocketDebuggerUrl naming 127.0.0.1:<windows port>,
// which inside the distro is a different machine's loopback. Dialing what
// the wire said would be both wrong and a general egress primitive; the
// only address this backend ever dials is its own listener.
func (e *Endpoint) BrowserWebSocketURL(ctx context.Context) (string, error) {
	if err := e.WaitConnected(ctx); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	addr := e.addr
	client := &http.Client{Transport: &http.Transport{
		// No proxy, and the dialer ignores the request's host entirely: the
		// only reachable address is the relay listener, by construction
		// rather than by convention.
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
		DisableKeepAlives: true,
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/version", nil)
	if err != nil {
		return "", fmt.Errorf("cdprelay: build /json/version request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("cdprelay: read /json/version through the tunnel: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxVersionBytes))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cdprelay: /json/version answered %s", response.Status)
	}
	var document struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxVersionBytes)).Decode(&document); err != nil {
		return "", fmt.Errorf("cdprelay: decode /json/version: %w", err)
	}
	return RewriteDebuggerURL(document.WebSocketDebuggerURL, addr)
}

// RewriteDebuggerURL re-addresses a CDP debugger URL onto addr, keeping
// only its path — the per-browser GUID chromedp must present. Scheme and
// host are OURS; nothing that arrived over the wire decides where a socket
// goes.
func RewriteDebuggerURL(raw, addr string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("cdprelay: parse webSocketDebuggerUrl: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("cdprelay: webSocketDebuggerUrl scheme %q is not a WebSocket scheme", parsed.Scheme)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return "", errors.New("cdprelay: webSocketDebuggerUrl carries no browser path")
	}
	rewritten := url.URL{Scheme: "ws", Host: addr, Path: parsed.Path, RawQuery: parsed.RawQuery}
	return rewritten.String(), nil
}
