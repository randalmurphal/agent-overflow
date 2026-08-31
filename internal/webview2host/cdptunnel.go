package webview2host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// CDPTunnel is the launcher half of the CDP relay.
//
// Direction is the whole point. The WSL backend never dials the Windows
// host and nothing listens across the boundary: the LAUNCHER dials the
// backend's existing loopback URL — the hop that already works for the
// notification bridge, with the same launch token — and multiplexes TCP
// streams to 127.0.0.1:<cdp port> on the Windows side. The only sockets
// are one outbound WebSocket and N loopback dials.
//
// It is NOT a proxy. The port is fixed at construction and every stream
// dials exactly it; the wire carries no address, so a compromised backend
// gains no reach it did not already have.
type CDPTunnel struct {
	wsURL   string
	token   string
	cdpAddr string
	logf    func(string, ...any)
	minWait time.Duration
	maxWait time.Duration
	dialTO  time.Duration
	// dialCDP is the one seam the race tests need: a directive-paced fake
	// dial. nil everywhere but tests; the production path is dialStream.
	dialCDP func(ctx context.Context, addr string) (net.Conn, error)
}

func (t *CDPTunnel) dialStream(ctx context.Context, addr string) (net.Conn, error) {
	if t.dialCDP != nil {
		return t.dialCDP(ctx, addr)
	}
	dialer := net.Dialer{Timeout: t.dialTO}
	return dialer.DialContext(ctx, "tcp", addr)
}

// CDPTunnelConfig configures a tunnel. WSURL, Token and CDPPort are
// required; the rest have production defaults.
type CDPTunnelConfig struct {
	// WSURL is the backend's tunnel route, e.g.
	// ws://127.0.0.1:41235/browser-cdp. The token rides as a query
	// parameter, exactly as the notification bridge does.
	WSURL string
	Token string
	// CDPPort is the ONE loopback port streams may reach: the
	// --remote-debugging-port the pane environment was created with.
	CDPPort int
	Logf    func(string, ...any)

	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	DialTimeout time.Duration
}

const (
	defaultTunnelMinBackoff  = 250 * time.Millisecond
	defaultTunnelMaxBackoff  = 5 * time.Second
	defaultTunnelDialTimeout = 3 * time.Second
	// tunnelWriteTimeout bounds one WebSocket write so a backend that
	// stops draining cannot wedge a stream pump holding the write lock.
	tunnelWriteTimeout = 30 * time.Second
)

// NewCDPTunnel validates the configuration. It opens nothing; Run does.
func NewCDPTunnel(config CDPTunnelConfig) (*CDPTunnel, error) {
	if config.WSURL == "" {
		return nil, errors.New("cdp tunnel websocket URL is required")
	}
	parsed, err := url.Parse(config.WSURL)
	if err != nil {
		return nil, fmt.Errorf("parse cdp tunnel websocket URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("cdp tunnel websocket URL scheme %q must be ws or wss", parsed.Scheme)
	}
	if config.Token == "" {
		return nil, errors.New("cdp tunnel token is required")
	}
	if config.CDPPort <= 0 || config.CDPPort > 65535 {
		return nil, fmt.Errorf("cdp tunnel port %d is out of range", config.CDPPort)
	}
	logf := config.Logf
	if logf == nil {
		logf = log.Printf
	}
	minWait := config.MinBackoff
	if minWait <= 0 {
		minWait = defaultTunnelMinBackoff
	}
	maxWait := config.MaxBackoff
	if maxWait <= 0 {
		maxWait = defaultTunnelMaxBackoff
	}
	if maxWait < minWait {
		maxWait = minWait
	}
	dialTO := config.DialTimeout
	if dialTO <= 0 {
		dialTO = defaultTunnelDialTimeout
	}
	return &CDPTunnel{
		wsURL:   parsed.String(),
		token:   config.Token,
		cdpAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(config.CDPPort)),
		logf:    logf,
		minWait: minWait,
		maxWait: maxWait,
		dialTO:  dialTO,
	}, nil
}

// Run reconnects until ctx is cancelled, on the same backoff ladder the
// notification bridge uses: a connection that survived long enough to be
// useful resets the wait, so a healthy relay that is torn down by a WSL
// suspend reconnects promptly while a backend that is simply absent is
// not hammered.
func (t *CDPTunnel) Run(ctx context.Context) {
	wait := t.minWait
	for ctx.Err() == nil {
		started := time.Now()
		connected, err := t.runConnection(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			t.logf("browser cdp tunnel: disconnected: %s", redactToken(err, t.token))
		}
		if connected && time.Since(started) >= 5*time.Second {
			wait = t.minWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		if wait < t.maxWait {
			wait *= 2
			if wait > t.maxWait {
				wait = t.maxWait
			}
		}
	}
}

func (t *CDPTunnel) runConnection(ctx context.Context) (bool, error) {
	parsed, err := url.Parse(t.wsURL)
	if err != nil {
		return false, err
	}
	query := parsed.Query()
	query.Set("token", t.token)
	parsed.RawQuery = query.Encode()

	conn, _, err := websocket.Dial(ctx, parsed.String(), nil)
	if err != nil {
		return false, fmt.Errorf("connect to cdp tunnel: %s", redactToken(err, t.token))
	}
	conn.SetReadLimit(MaxTunnelFrameBytes)

	session := &tunnelSession{
		conn:    conn,
		tunnel:  t,
		streams: make(map[uint32]*tunnelStream, 8),
	}
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		// Cancel before waiting: an in-flight stream dial aborts on the
		// context rather than holding the reconnect for its full timeout.
		cancel()
		session.closeAll()
		_ = conn.Close(websocket.StatusNormalClosure, "")
		session.pumps.Wait()
	}()

	t.logf("browser cdp tunnel: connected, relaying to %s", t.cdpAddr)
	for {
		kind, payload, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		switch kind {
		case websocket.MessageText:
			session.handleControl(ctx, payload)
		case websocket.MessageBinary:
			session.handleData(payload)
		}
	}
}

// tunnelSession is one connection's stream table. It dies with the
// connection: there is no resume, so nothing here outlives runConnection.
type tunnelSession struct {
	conn   *websocket.Conn
	tunnel *CDPTunnel

	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*tunnelStream
	closed  bool

	pumps sync.WaitGroup
}

type tunnelStream struct {
	id   uint32
	conn net.Conn
	once sync.Once
}

func (s *tunnelStream) close() {
	s.once.Do(func() {
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}

func (s *tunnelSession) handleControl(ctx context.Context, payload []byte) {
	var control TunnelControl
	if err := json.Unmarshal(payload, &control); err != nil {
		s.tunnel.logf("browser cdp tunnel: ignore malformed control frame: %v", err)
		return
	}
	switch control.Op {
	case TunnelOpen:
		s.openStream(ctx, control.StreamID)
	case TunnelClose:
		s.closeStream(control.StreamID)
	default:
		// opened/error are the launcher's own answers, and anything else
		// is version skew. Dropping is right in both cases: acting on a
		// guess here would dial a socket nobody asked for.
		s.tunnel.logf("browser cdp tunnel: ignore control op %q on stream %d", control.Op, control.StreamID)
	}
}

func (s *tunnelSession) openStream(ctx context.Context, id uint32) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, exists := s.streams[id]; exists {
		s.mu.Unlock()
		s.sendControl(ctx, TunnelControl{Op: TunnelError, StreamID: id, Detail: "stream id already open"})
		return
	}
	if len(s.streams) >= MaxTunnelStreams {
		s.mu.Unlock()
		s.tunnel.logf("browser cdp tunnel: refusing stream %d, %d already open", id, MaxTunnelStreams)
		s.sendControl(ctx, TunnelControl{Op: TunnelError, StreamID: id, Detail: "too many concurrent streams"})
		return
	}
	// Reserve the slot before the dial so a burst of opens cannot race
	// past the cap while each one is waiting on the network. The
	// placeholder is THIS open's identity: a close (or a close-and-reopen)
	// racing the dial replaces it, and the finished dial recognises that
	// its reservation is gone rather than resurrecting a stream the
	// backend has already forgotten.
	placeholder := &tunnelStream{id: id}
	s.streams[id] = placeholder
	s.mu.Unlock()

	// The dial itself leaves the read loop: a dead CDP endpoint takes the
	// full dial timeout to fail, and holding the loop for it would stall
	// every other stream's data and close frames behind one corpse.
	s.pumps.Add(1)
	go s.finishOpen(ctx, id, placeholder)
}

// finishOpen dials the registered CDP port for one reserved stream and
// either registers the result or unwinds the reservation — but only its
// own: the placeholder is the proof the id still means this open.
func (s *tunnelSession) finishOpen(ctx context.Context, id uint32, placeholder *tunnelStream) {
	defer s.pumps.Done()
	conn, err := s.tunnel.dialStream(ctx, s.tunnel.cdpAddr)
	if err != nil {
		s.mu.Lock()
		mine := s.streams[id] == placeholder
		if mine {
			delete(s.streams, id)
		}
		s.mu.Unlock()
		if mine {
			s.sendControl(ctx, TunnelControl{Op: TunnelError, StreamID: id, Detail: TruncateDetail(err.Error())})
		}
		return
	}

	stream := &tunnelStream{id: id, conn: conn}
	s.mu.Lock()
	if s.closed || s.streams[id] != placeholder {
		s.mu.Unlock()
		stream.close()
		return
	}
	s.streams[id] = stream
	s.mu.Unlock()

	s.sendControl(ctx, TunnelControl{Op: TunnelOpened, StreamID: id})
	s.pumps.Add(1)
	go s.pump(ctx, stream)
}

// pump copies CDP -> WebSocket until the socket ends, then tells the
// backend the stream is finished. One goroutine and one reusable buffer
// per stream.
func (s *tunnelSession) pump(ctx context.Context, stream *tunnelStream) {
	defer s.pumps.Done()
	buf := make([]byte, TunnelChunkBytes)
	frame := make([]byte, 0, 4+TunnelChunkBytes)
	for {
		n, err := stream.conn.Read(buf)
		if n > 0 {
			frame = EncodeTunnelData(frame, stream.id, buf[:n])
			if writeErr := s.writeFrame(ctx, websocket.MessageBinary, frame); writeErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	stream.close()
	s.mu.Lock()
	// Settle only OUR entry: after a close-and-reopen of the same id the
	// slot may already hold the next open's reservation, which is not
	// this pump's to delete or to announce dead.
	mine := s.streams[stream.id] == stream
	if mine {
		delete(s.streams, stream.id)
	}
	closed := s.closed
	s.mu.Unlock()
	if mine && !closed {
		s.sendControl(ctx, TunnelControl{Op: TunnelClose, StreamID: stream.id})
	}
}

func (s *tunnelSession) handleData(payload []byte) {
	id, data, err := DecodeTunnelData(payload)
	if err != nil {
		s.tunnel.logf("browser cdp tunnel: %v", err)
		return
	}
	s.mu.Lock()
	stream := s.streams[id]
	s.mu.Unlock()
	if stream == nil || stream.conn == nil {
		// A late frame for a stream that just died is normal, not an
		// error: the close raced the backend's last write. A frame while
		// the dial is still in flight (conn == nil) is a backend that did
		// not wait for opened; those bytes have nowhere to go either.
		return
	}
	if _, err := stream.conn.Write(data); err != nil {
		// Let the pump goroutine report the close: it owns the stream's
		// lifecycle and would otherwise send a second close frame.
		stream.close()
	}
}

func (s *tunnelSession) closeStream(id uint32) {
	s.mu.Lock()
	stream := s.streams[id]
	delete(s.streams, id)
	s.mu.Unlock()
	if stream != nil {
		stream.close()
	}
}

func (s *tunnelSession) closeAll() {
	s.mu.Lock()
	s.closed = true
	streams := s.streams
	s.streams = make(map[uint32]*tunnelStream)
	s.mu.Unlock()
	for _, stream := range streams {
		if stream != nil {
			stream.close()
		}
	}
}

func (s *tunnelSession) sendControl(ctx context.Context, control TunnelControl) {
	payload, err := json.Marshal(control)
	if err != nil {
		s.tunnel.logf("browser cdp tunnel: encode control %q: %v", control.Op, err)
		return
	}
	if err := s.writeFrame(ctx, websocket.MessageText, payload); err != nil {
		s.tunnel.logf("browser cdp tunnel: write control %q: %v", control.Op, err)
	}
}

func (s *tunnelSession) writeFrame(ctx context.Context, kind websocket.MessageType, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, tunnelWriteTimeout)
	defer cancel()
	return s.conn.Write(ctx, kind, payload)
}

// redactToken keeps the launch token out of launcher.log, the same rule
// the notification bridge's error path follows.
func redactToken(err error, token string) string {
	if err == nil {
		return "unknown error"
	}
	message := err.Error()
	if token == "" {
		return message
	}
	message = strings.ReplaceAll(message, token, "[redacted]")
	return strings.ReplaceAll(message, url.QueryEscape(token), "[redacted]")
}
