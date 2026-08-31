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
	// a few hundred bytes; this only exists so a wedged or hostile
	// endpoint cannot make the backend allocate without bound.
	maxVersionBytes = 64 << 10
)

// ErrRelayClosed reports that the endpoint has been shut down.
var ErrRelayClosed = errors.New("cdprelay: endpoint closed")

// ErrTunnelDown reports that no launcher tunnel is connected.
var ErrTunnelDown = errors.New("cdprelay: no browser host tunnel is connected")

// New binds the loopback listener and starts accepting. It opens no
// outbound connection: the launcher dials the backend, never the reverse
// (internal/webview2host/AGENTS.md § Direction is the security property).
func New(config Config) (*Endpoint, error) {
	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cdprelay: listen on loopback: %w", err)
	}
	e := &Endpoint{
		logf:        logf,
		listener:    listener,
		addr:        listener.Addr().String(),
		openTimeout: streamOpenTimeout,
		ready:       make(chan struct{}),
	}
	e.wg.Add(1)
	go e.accept()
	return e, nil
}

// Addr is the loopback address chromedp connects to, e.g. 127.0.0.1:41235.
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

// session is one tunnel connection's stream table. It dies with the
// connection: there is no resume, so nothing here outlives ServeCDPTunnel.
type session struct {
	endpoint *Endpoint
	conn     *websocket.Conn
	ctx      context.Context

	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*stream
	nextID  uint32
	closed  bool
}

// stream is one loopback connection bound to one tunnel stream id.
type stream struct {
	id    uint32
	local net.Conn

	openOnce sync.Once
	opened   chan struct{}
	doneOnce sync.Once
	done     chan struct{}

	failMu sync.Mutex
	fail   string
}

func (st *stream) markOpened() { st.openOnce.Do(func() { close(st.opened) }) }

// finish closes the local socket, which is what unblocks the pump's Read.
func (st *stream) finish(detail string) {
	st.doneOnce.Do(func() {
		st.failMu.Lock()
		st.fail = detail
		st.failMu.Unlock()
		close(st.done)
		_ = st.local.Close()
	})
}

func (st *stream) failure() string {
	st.failMu.Lock()
	defer st.failMu.Unlock()
	return st.fail
}

// openStream reserves an id, asks the launcher to dial, and waits for
// `opened`. The local socket is bound to the stream BEFORE the request
// goes out, so a launcher that answers and immediately pumps cannot race
// its first bytes against the binding.
func (s *session) openStream(local net.Conn) (*stream, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrTunnelDown
	}
	if len(s.streams) >= webview2host.MaxTunnelStreams {
		s.mu.Unlock()
		return nil, fmt.Errorf("cdprelay: %d tunnel streams already open", webview2host.MaxTunnelStreams)
	}
	s.nextID++
	st := &stream{id: s.nextID, local: local, opened: make(chan struct{}), done: make(chan struct{})}
	s.streams[st.id] = st
	s.mu.Unlock()

	if err := s.sendControl(webview2host.TunnelControl{Op: webview2host.TunnelOpen, StreamID: st.id}); err != nil {
		s.retire(st)
		return nil, err
	}

	timer := time.NewTimer(s.endpoint.openTimeout)
	defer timer.Stop()
	select {
	case <-st.opened:
		return st, nil
	case <-st.done:
		detail := st.failure()
		if detail == "" {
			detail = "tunnel closed the stream"
		}
		s.retire(st)
		return nil, fmt.Errorf("cdprelay: stream %d: %s", st.id, detail)
	case <-s.ctx.Done():
		s.retire(st)
		return nil, ErrTunnelDown
	case <-timer.C:
		s.retire(st)
		s.closeRemote(st.id)
		return nil, fmt.Errorf("cdprelay: stream %d: launcher did not answer open within %s", st.id, s.endpoint.openTimeout)
	}
}

// pump copies the local socket into data frames until either end finishes,
// then tells the launcher the stream is over.
//
// Writes are chunked at TunnelChunkBytes, comfortably under the 1MiB frame
// limit both ends enforce — the backend owes that bound just as the
// launcher does (internal/webview2host/AGENTS.md).
func (s *session) pump(st *stream) {
	buf := make([]byte, webview2host.TunnelChunkBytes)
	frame := make([]byte, 0, 4+webview2host.TunnelChunkBytes)
	for {
		n, err := st.local.Read(buf)
		if n > 0 {
			frame = webview2host.EncodeTunnelData(frame, st.id, buf[:n])
			if writeErr := s.write(websocket.MessageBinary, frame); writeErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	st.finish("local connection ended")
	if s.retire(st) {
		s.closeRemote(st.id)
	}
}

// retire removes st from the table, but only when the slot still holds it.
// Reports whether this call is the one that removed it, which is what
// decides who owes the launcher a close frame.
func (s *session) retire(st *stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[st.id] != st {
		return false
	}
	delete(s.streams, st.id)
	return !s.closed
}

func (s *session) lookup(id uint32) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) handleControl(payload []byte) {
	var control webview2host.TunnelControl
	if err := json.Unmarshal(payload, &control); err != nil {
		s.endpoint.logf("cdprelay: ignore malformed control frame: %v", err)
		return
	}
	switch control.Op {
	case webview2host.TunnelOpened:
		if st := s.lookup(control.StreamID); st != nil {
			st.markOpened()
		}
	case webview2host.TunnelError:
		if st := s.lookup(control.StreamID); st != nil {
			st.finish(webview2host.TruncateDetail(control.Detail))
		}
	case webview2host.TunnelClose:
		if st := s.lookup(control.StreamID); st != nil {
			st.finish("launcher closed the stream")
		}
	default:
		// `open` is the backend's own verb and anything else is version
		// skew. Both are dropped rather than guessed at: the tunnel's whole
		// safety story is that no control frame names an address, and
		// acting on an unknown op is how that stops being true.
		s.endpoint.logf("cdprelay: ignore control op %q on stream %d", control.Op, control.StreamID)
	}
}

func (s *session) handleData(payload []byte) {
	id, data, err := webview2host.DecodeTunnelData(payload)
	if err != nil {
		s.endpoint.logf("cdprelay: %v", err)
		return
	}
	st := s.lookup(id)
	if st == nil {
		// A late frame for a stream that just ended is ordinary: the close
		// raced the launcher's last write.
		return
	}
	if _, err := st.local.Write(data); err != nil {
		// The pump owns the stream's lifecycle and will send the close;
		// closing the socket here is what wakes it.
		st.finish("local write failed")
	}
}

func (s *session) sendControl(control webview2host.TunnelControl) error {
	payload, err := json.Marshal(control)
	if err != nil {
		return fmt.Errorf("cdprelay: encode control %q: %w", control.Op, err)
	}
	return s.write(websocket.MessageText, payload)
}

func (s *session) closeRemote(id uint32) {
	if err := s.sendControl(webview2host.TunnelControl{Op: webview2host.TunnelClose, StreamID: id}); err != nil {
		s.endpoint.logf("cdprelay: close stream %d: %v", id, err)
	}
}

func (s *session) write(kind websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(ctx, kind, payload)
}

// shutdown drops every stream on this connection. Idempotent: a replaced
// session is shut down by whoever replaced it and again by its own reader
// when it notices.
func (s *session) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	streams := s.streams
	s.streams = make(map[uint32]*stream)
	s.mu.Unlock()
	for _, st := range streams {
		st.finish("tunnel disconnected")
	}
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}
