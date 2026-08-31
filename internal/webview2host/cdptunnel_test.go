package webview2host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The tunnel is exercised against an in-test WebSocket server standing in
// for the backend and a loopback TCP listener standing in for WebView2's
// debugging port. Both are 127.0.0.1 only; nothing here crosses a
// machine boundary, and there is no reachability probing.

// echoListener answers every connection by echoing bytes back with a
// prefix, so a test can prove data crossed in both directions. Teardown
// closes the accepted sockets rather than waiting on them: the tunnel
// holds them open for as long as it is up, so a wait here would deadlock
// against its own cleanup.
func echoListener(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						if _, werr := conn.Write(append([]byte("echo:"), buf[:n]...)); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port
}

// backendStub is the far end of the tunnel: it accepts one connection and
// hands the test a way to drive control frames and read what comes back.
type backendStub struct {
	server *httptest.Server

	mu       sync.Mutex
	conn     *websocket.Conn
	connOnce chan struct{}

	control chan TunnelControl
	data    chan []byte
}

func newBackendStub(t *testing.T, token string) *backendStub {
	t.Helper()
	stub := &backendStub{
		connOnce: make(chan struct{}),
		control:  make(chan TunnelControl, 16),
		data:     make(chan []byte, 16),
	}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CDPTunnelPath || r.URL.Query().Get("token") != token {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(MaxTunnelFrameBytes)
		stub.mu.Lock()
		first := stub.conn == nil
		stub.conn = conn
		stub.mu.Unlock()
		if first {
			close(stub.connOnce)
		}
		for {
			kind, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			switch kind {
			case websocket.MessageText:
				var control TunnelControl
				if err := json.Unmarshal(payload, &control); err == nil {
					stub.control <- control
				}
			case websocket.MessageBinary:
				copied := make([]byte, len(payload))
				copy(copied, payload)
				stub.data <- copied
			}
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *backendStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + CDPTunnelPath
}

func (s *backendStub) waitConnected(t *testing.T) {
	t.Helper()
	select {
	case <-s.connOnce:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the launcher to dial the tunnel")
	}
}

func (s *backendStub) sendControl(t *testing.T, control TunnelControl) {
	t.Helper()
	payload, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	s.write(t, websocket.MessageText, payload)
}

func (s *backendStub) sendData(t *testing.T, id uint32, payload []byte) {
	t.Helper()
	s.write(t, websocket.MessageBinary, EncodeTunnelData(nil, id, payload))
}

func (s *backendStub) write(t *testing.T, kind websocket.MessageType, payload []byte) {
	t.Helper()
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("no tunnel connection yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, kind, payload); err != nil {
		t.Fatalf("write to tunnel: %v", err)
	}
}

func (s *backendStub) nextControl(t *testing.T) TunnelControl {
	t.Helper()
	select {
	case control := <-s.control:
		return control
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a control frame from the launcher")
		return TunnelControl{}
	}
}

func startTunnel(t *testing.T, stub *backendStub, cdpPort int) {
	t.Helper()
	// Run keeps logging until it returns, so cleanup joins it before the
	// test frame goes away rather than letting a late t.Logf panic.
	var logMu sync.Mutex
	tunnel, err := NewCDPTunnel(CDPTunnelConfig{
		WSURL:   stub.wsURL(),
		Token:   "tunnel-token",
		CDPPort: cdpPort,
		Logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			t.Logf(format, args...)
		},
		MinBackoff:  time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewCDPTunnel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tunnel.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	stub.waitConnected(t)
}

func TestCDPTunnelPipesBothDirections(t *testing.T) {
	cdpPort := echoListener(t)
	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, cdpPort)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 1})
	if got := stub.nextControl(t); got.Op != TunnelOpened || got.StreamID != 1 {
		t.Fatalf("control = %#v, want opened on stream 1", got)
	}

	stub.sendData(t, 1, []byte(`{"id":1,"method":"Target.getTargets"}`))
	select {
	case frame := <-stub.data:
		id, payload, err := DecodeTunnelData(frame)
		if err != nil {
			t.Fatalf("DecodeTunnelData: %v", err)
		}
		if id != 1 {
			t.Fatalf("stream id = %d, want 1", id)
		}
		if want := `echo:{"id":1,"method":"Target.getTargets"}`; string(payload) != want {
			t.Fatalf("payload = %q, want %q", payload, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the echoed CDP bytes")
	}
}

// The port is fixed at construction and rides no wire field, so a
// compromised backend cannot turn the launcher into a general proxy. The
// closest thing to a test for a negative is proving the wire has no
// address in it and that every stream lands on the registered port.
func TestCDPTunnelDialsOnlyTheRegisteredPort(t *testing.T) {
	cdpPort := echoListener(t)
	otherPort := echoListener(t)
	if cdpPort == otherPort {
		t.Fatal("two listeners took the same port")
	}

	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, cdpPort)

	// The open frame carries no destination. Even one that tries to name
	// the other listener in its detail reaches the registered port.
	stub.sendControl(t, TunnelControl{
		Op:       TunnelOpen,
		StreamID: 5,
		Detail:   fmt.Sprintf("127.0.0.1:%d", otherPort),
	})
	if got := stub.nextControl(t); got.Op != TunnelOpened {
		t.Fatalf("control = %#v, want opened", got)
	}
	stub.sendData(t, 5, []byte("x"))
	select {
	case frame := <-stub.data:
		_, payload, _ := DecodeTunnelData(frame)
		if string(payload) != "echo:x" {
			t.Fatalf("payload = %q", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the echo")
	}
}

func TestCDPTunnelRefusesMoreThanTheStreamCap(t *testing.T) {
	cdpPort := echoListener(t)
	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, cdpPort)

	opened := 0
	for id := uint32(1); id <= MaxTunnelStreams; id++ {
		stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: id})
	}
	for opened < MaxTunnelStreams {
		got := stub.nextControl(t)
		if got.Op != TunnelOpened {
			t.Fatalf("control = %#v, want opened while filling the cap", got)
		}
		opened++
	}

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: MaxTunnelStreams + 1})
	got := stub.nextControl(t)
	if got.Op != TunnelError || got.StreamID != MaxTunnelStreams+1 {
		t.Fatalf("control = %#v, want an error for the over-cap stream", got)
	}
	if !strings.Contains(got.Detail, "too many") {
		t.Fatalf("detail = %q, want it to say the cap was hit", got.Detail)
	}
}

func TestCDPTunnelReportsADuplicateStreamID(t *testing.T) {
	cdpPort := echoListener(t)
	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, cdpPort)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 1})
	if got := stub.nextControl(t); got.Op != TunnelOpened {
		t.Fatalf("control = %#v, want opened", got)
	}
	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 1})
	got := stub.nextControl(t)
	if got.Op != TunnelError || !strings.Contains(got.Detail, "already open") {
		t.Fatalf("control = %#v, want a duplicate-id error", got)
	}
}

func TestCDPTunnelReportsAFailedDial(t *testing.T) {
	// Take a port and immediately release it, so nothing is listening.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, deadPort)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 2})
	got := stub.nextControl(t)
	if got.Op != TunnelError || got.StreamID != 2 {
		t.Fatalf("control = %#v, want an error for the dead port", got)
	}
	if got.Detail == "" {
		t.Fatal("error control frame carried no detail")
	}
}

// A CDP socket that ends must tell the backend, or it waits on a stream
// that will never answer.
func TestCDPTunnelAnnouncesAStreamTheCDPSideClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		// Hang up as soon as the launcher connects.
		_ = conn.Close()
	}()

	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, listener.Addr().(*net.TCPAddr).Port)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 3})
	if got := stub.nextControl(t); got.Op != TunnelOpened {
		t.Fatalf("control = %#v, want opened", got)
	}
	if got := stub.nextControl(t); got.Op != TunnelClose || got.StreamID != 3 {
		t.Fatalf("control = %#v, want close on stream 3", got)
	}
}

func TestCDPTunnelCloseFrameEndsTheStream(t *testing.T) {
	accepted := make(chan net.Conn, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, listener.Addr().(*net.TCPAddr).Port)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 4})
	if got := stub.nextControl(t); got.Op != TunnelOpened {
		t.Fatalf("control = %#v, want opened", got)
	}
	var cdpSide net.Conn
	select {
	case cdpSide = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher never dialed the CDP listener")
	}

	stub.sendControl(t, TunnelControl{Op: TunnelClose, StreamID: 4})
	_ = cdpSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := cdpSide.Read(make([]byte, 1)); err == nil {
		t.Fatal("the CDP socket stayed open after a close frame")
	} else if err != io.EOF && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("unexpected read error: %v", err)
	}
}

func TestCDPTunnelIgnoresUnknownControlOps(t *testing.T) {
	cdpPort := echoListener(t)
	stub := newBackendStub(t, "tunnel-token")
	startTunnel(t, stub, cdpPort)

	// Unknown op, then a real one: the tunnel must survive the first and
	// answer the second, rather than dropping the connection.
	stub.sendControl(t, TunnelControl{Op: "teleport", StreamID: 9})
	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 9})
	if got := stub.nextControl(t); got.Op != TunnelOpened || got.StreamID != 9 {
		t.Fatalf("control = %#v, want opened on stream 9", got)
	}
}

func TestNewCDPTunnelValidatesItsConfig(t *testing.T) {
	base := CDPTunnelConfig{WSURL: "ws://127.0.0.1:1/browser-cdp", Token: "t", CDPPort: 9333}
	for _, tc := range []struct {
		name    string
		mutate  func(*CDPTunnelConfig)
		wantErr string
	}{
		{"no url", func(c *CDPTunnelConfig) { c.WSURL = "" }, "URL is required"},
		{"http scheme", func(c *CDPTunnelConfig) { c.WSURL = "http://127.0.0.1/x" }, "must be ws or wss"},
		{"no token", func(c *CDPTunnelConfig) { c.Token = "" }, "token is required"},
		{"no port", func(c *CDPTunnelConfig) { c.CDPPort = 0 }, "out of range"},
		{"port too high", func(c *CDPTunnelConfig) { c.CDPPort = 70000 }, "out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := base
			tc.mutate(&config)
			_, err := NewCDPTunnel(config)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NewCDPTunnel error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
	if _, err := NewCDPTunnel(base); err != nil {
		t.Fatalf("NewCDPTunnel(valid) = %v", err)
	}
}

func TestCDPTunnelRedactsItsTokenFromErrors(t *testing.T) {
	token := "secret+/token"
	got := redactToken(fmt.Errorf("GET ws://127.0.0.1/browser-cdp?token=secret%%2B%%2Ftoken: refused"), token)
	if strings.Contains(got, token) || strings.Contains(got, "secret%2B%2Ftoken") {
		t.Fatalf("redacted error leaked the token: %q", got)
	}
}

// A close that lands while the stream's dial is still in flight must WIN:
// the finished dial may not resurrect the stream (it would hold a cap slot
// and a socket the backend has forgotten), and a dial that fails after the
// close may not send an error for an id the backend no longer owns.
func TestCDPTunnelCloseDuringDialDoesNotResurrectTheStream(t *testing.T) {
	stub := newBackendStub(t, "tunnel-token")
	var logMu sync.Mutex
	tunnel, err := NewCDPTunnel(CDPTunnelConfig{
		WSURL:       stub.wsURL(),
		Token:       "tunnel-token",
		CDPPort:     1, // never dialed: dialCDP below intercepts
		Logf:        func(format string, args ...any) { logMu.Lock(); defer logMu.Unlock(); t.Logf(format, args...) },
		MinBackoff:  time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewCDPTunnel: %v", err)
	}
	var dialOnce sync.Once
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	tunnel.dialCDP = func(ctx context.Context, _ string) (net.Conn, error) {
		dialOnce.Do(func() { close(dialStarted) })
		select {
		case <-releaseDial:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return local, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); tunnel.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-finished })
	stub.waitConnected(t)

	stub.sendControl(t, TunnelControl{Op: TunnelOpen, StreamID: 9})
	select {
	case <-dialStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the stream dial to start")
	}
	stub.sendControl(t, TunnelControl{Op: TunnelClose, StreamID: 9})
	// The read loop consumes frames in order, so once the dial below is
	// released the close is already in. The sleep only widens the window
	// in which a regressed implementation would adopt the socket; the
	// assertions below are what fail.
	time.Sleep(50 * time.Millisecond)
	close(releaseDial)

	// The launcher must close the dialed socket rather than adopt it.
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := remote.Read(buf); err == nil {
		t.Fatal("dialed socket stayed open after the stream was closed mid-dial")
	}

	// And nothing about stream 9 may reach the backend: no opened for a
	// stream it closed, no error for an id it no longer owns.
	select {
	case control := <-stub.control:
		if control.StreamID == 9 {
			t.Fatalf("stream 9 was resurrected: %#v", control)
		}
	case <-time.After(200 * time.Millisecond):
	}
}
