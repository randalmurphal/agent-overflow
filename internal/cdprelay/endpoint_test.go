package cdprelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

// Everything here talks to 127.0.0.1 listeners this test binary owns. No
// test reaches a real launcher, a real WebView2, or any address off
// loopback — the relay's whole point is that nothing it is told about
// ever becomes a dial target.

const testDeadline = 5 * time.Second

// fixture is one endpoint plus the loopback HTTP server standing in for
// the transport's /browser-cdp route.
type fixture struct {
	t        *testing.T
	endpoint *Endpoint
	server   *httptest.Server
	ctx      context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	endpoint, err := New(Config{Logf: func(format string, args ...any) { t.Logf(format, args...) }})
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	endpoint.openTimeout = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		endpoint.ServeCDPTunnel(ctx, conn)
	}))
	t.Cleanup(server.Close)
	return &fixture{t: t, endpoint: endpoint, server: server, ctx: ctx}
}

// dialLocal is what chromedp does: an ordinary TCP connection to the
// relay's loopback listener.
func (f *fixture) dialLocal() net.Conn {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.endpoint.Addr())
	if err != nil {
		f.t.Fatalf("dial relay listener: %v", err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type datum struct {
	id      uint32
	payload []byte
}

// peer is the fake launcher: it dials the bridge exactly as the real one
// does and speaks the cdpframe protocol, but dials nothing else.
type peer struct {
	t        *testing.T
	conn     *websocket.Conn
	ctx      context.Context
	controls chan webview2host.TunnelControl
	data     chan datum
	readErr  chan error
}

func (f *fixture) connect() *peer {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") + webview2host.CDPTunnelPath
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		f.t.Fatalf("dial tunnel: %v", err)
	}
	conn.SetReadLimit(webview2host.MaxTunnelFrameBytes)
	p := &peer{
		t: f.t, conn: conn, ctx: ctx,
		controls: make(chan webview2host.TunnelControl, 256),
		data:     make(chan datum, 256),
		readErr:  make(chan error, 1),
	}
	f.t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	go p.read()
	return p
}

func (p *peer) read() {
	for {
		kind, payload, err := p.conn.Read(p.ctx)
		if err != nil {
			p.readErr <- err
			return
		}
		switch kind {
		case websocket.MessageText:
			var control webview2host.TunnelControl
			if err := json.Unmarshal(payload, &control); err != nil {
				p.readErr <- err
				return
			}
			p.controls <- control
		case websocket.MessageBinary:
			id, data, err := webview2host.DecodeTunnelData(payload)
			if err != nil {
				p.readErr <- err
				return
			}
			p.data <- datum{id: id, payload: bytes.Clone(data)}
		}
	}
}

func (p *peer) nextControl() webview2host.TunnelControl {
	p.t.Helper()
	select {
	case control := <-p.controls:
		return control
	case err := <-p.readErr:
		p.t.Fatalf("tunnel read: %v", err)
	case <-time.After(testDeadline):
		p.t.Fatal("timed out waiting for a control frame")
	}
	return webview2host.TunnelControl{}
}

// nextOpen asserts the next control frame is an `open` and answers it.
func (p *peer) acceptOpen() uint32 {
	p.t.Helper()
	control := p.nextControl()
	if control.Op != webview2host.TunnelOpen {
		p.t.Fatalf("expected %q, got %q", webview2host.TunnelOpen, control.Op)
	}
	p.send(webview2host.TunnelControl{Op: webview2host.TunnelOpened, StreamID: control.StreamID})
	return control.StreamID
}

func (p *peer) send(control webview2host.TunnelControl) {
	p.t.Helper()
	payload, err := json.Marshal(control)
	if err != nil {
		p.t.Fatalf("encode control: %v", err)
	}
	ctx, cancel := context.WithTimeout(p.ctx, testDeadline)
	defer cancel()
	if err := p.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		p.t.Fatalf("write control: %v", err)
	}
}

func (p *peer) sendData(id uint32, payload []byte) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, testDeadline)
	defer cancel()
	frame := webview2host.EncodeTunnelData(nil, id, payload)
	if err := p.conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		p.t.Fatalf("write data: %v", err)
	}
}

// collect gathers want bytes for one stream, asserting the frame bound
// the backend owes the launcher on every frame it sees.
func (p *peer) collect(id uint32, want int) []byte {
	p.t.Helper()
	var got []byte
	for len(got) < want {
		select {
		case frame := <-p.data:
			if frame.id != id {
				p.t.Fatalf("data for stream %d, want %d", frame.id, id)
			}
			if len(frame.payload)+4 > webview2host.MaxTunnelFrameBytes {
				p.t.Fatalf("frame of %d bytes exceeds the tunnel frame limit", len(frame.payload)+4)
			}
			got = append(got, frame.payload...)
		case err := <-p.readErr:
			p.t.Fatalf("tunnel read: %v", err)
		case <-time.After(testDeadline):
			p.t.Fatalf("timed out with %d of %d bytes", len(got), want)
		}
	}
	return got
}

func readN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func TestRelayPipesBytesBothWays(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	id := p.acceptOpen()

	if _, err := local.Write([]byte("to-the-launcher")); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if got := string(p.collect(id, len("to-the-launcher"))); got != "to-the-launcher" {
		t.Fatalf("launcher saw %q", got)
	}

	p.sendData(id, []byte("to-the-backend"))
	if got := string(readN(t, local, len("to-the-backend"))); got != "to-the-backend" {
		t.Fatalf("local saw %q", got)
	}
}

// The backend owes the launcher this: no data frame may precede the
// launcher's own `opened` answer, because until then there is no socket
// on the far side to write it to.
func TestRelayHoldsDataUntilOpened(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	control := p.nextControl()
	if control.Op != webview2host.TunnelOpen {
		t.Fatalf("expected %q, got %q", webview2host.TunnelOpen, control.Op)
	}
	if _, err := local.Write([]byte("early")); err != nil {
		t.Fatalf("write local: %v", err)
	}

	select {
	case frame := <-p.data:
		t.Fatalf("data frame for stream %d arrived before `opened`", frame.id)
	case err := <-p.readErr:
		t.Fatalf("tunnel read: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	p.send(webview2host.TunnelControl{Op: webview2host.TunnelOpened, StreamID: control.StreamID})
	if got := string(p.collect(control.StreamID, len("early"))); got != "early" {
		t.Fatalf("launcher saw %q after opening", got)
	}
}

func TestRelayRefusesLocalConnectionsPastTheStreamCap(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	locals := make([]net.Conn, 0, webview2host.MaxTunnelStreams)
	for range webview2host.MaxTunnelStreams {
		locals = append(locals, f.dialLocal())
	}
	// Answering every open is what makes the cap observable: all of them
	// are registered once the last `opened` is on the wire.
	for range webview2host.MaxTunnelStreams {
		p.acceptOpen()
	}

	overflow := f.dialLocal()
	if err := overflow.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := overflow.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("overflow connection was not closed: %v", err)
	}
	select {
	case control := <-p.controls:
		t.Fatalf("the refused connection still reached the launcher: %+v", control)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := locals[0].Write([]byte("still live")); err != nil {
		t.Fatalf("an accepted stream died with the refusal: %v", err)
	}
}

func TestRelayDropsTheTunnelOnAnOversizedFrame(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	id := p.acceptOpen()

	oversized := make([]byte, webview2host.MaxTunnelFrameBytes+1)
	p.sendData(id, oversized)

	if err := local.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := local.Read(make([]byte, 1)); err == nil {
		t.Fatal("the stream survived an oversized frame")
	}
	select {
	case <-p.readErr:
	case <-time.After(testDeadline):
		t.Fatal("the tunnel stayed up after an oversized frame")
	}
}

func TestRelayChunksLargeWritesAndTheLauncherReassemblesThem(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	id := p.acceptOpen()

	payload := make([]byte, 4*webview2host.TunnelChunkBytes+1237)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("fill payload: %v", err)
	}
	go func() { _, _ = local.Write(payload) }()

	if got := p.collect(id, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("reassembled %d bytes, want %d identical", len(got), len(payload))
	}
}

func TestRelayReplacesAStaleTunnel(t *testing.T) {
	f := newFixture(t)
	stale := f.connect()
	fresh := f.connect()

	select {
	case <-stale.readErr:
	case <-time.After(testDeadline):
		t.Fatal("the stale tunnel was not retired")
	}

	local := f.dialLocal()
	id := fresh.acceptOpen()
	if _, err := local.Write([]byte("fresh")); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if got := string(fresh.collect(id, len("fresh"))); got != "fresh" {
		t.Fatalf("new tunnel saw %q", got)
	}
}

func TestRelayRefusesLocalConnectionsWithNoTunnel(t *testing.T) {
	f := newFixture(t)
	local := f.dialLocal()
	if err := local.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := local.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a tunnel-less connection was not closed: %v", err)
	}
}

func TestRelayClosesTheLocalConnectionWhenOpenIsNeverAnswered(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	control := p.nextControl()
	if control.Op != webview2host.TunnelOpen {
		t.Fatalf("expected %q, got %q", webview2host.TunnelOpen, control.Op)
	}

	if err := local.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := local.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the unanswered stream stayed open: %v", err)
	}
	// The abandoned stream is retired on the launcher's side too, so a
	// late dial cannot resurrect it.
	if got := p.nextControl(); got.Op != webview2host.TunnelClose || got.StreamID != control.StreamID {
		t.Fatalf("expected a close for stream %d, got %+v", control.StreamID, got)
	}
}

func TestRelayClosesTheLocalConnectionOnAStreamError(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	control := p.nextControl()
	p.send(webview2host.TunnelControl{Op: webview2host.TunnelError, StreamID: control.StreamID, Detail: "connection refused"})

	if err := local.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := local.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the failed stream stayed open: %v", err)
	}
}

func TestRelayClosesTheStreamWhenTheLocalConnectionEnds(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	local := f.dialLocal()
	id := p.acceptOpen()
	_ = local.Close()

	if got := p.nextControl(); got.Op != webview2host.TunnelClose || got.StreamID != id {
		t.Fatalf("expected a close for stream %d, got %+v", id, got)
	}
}

// BrowserWebSocketURL is the one discovery request, and it must go
// THROUGH the tunnel: the fake launcher answers the stream with a
// /json/version document naming a Windows-side address, and what comes
// back names this endpoint's own listener.
func TestBrowserWebSocketURLDiscoversThroughTheTunnelAndRewrites(t *testing.T) {
	f := newFixture(t)
	p := f.connect()

	go func() {
		id := p.acceptOpen()
		request := make([]byte, 0, 512)
		for !bytes.Contains(request, []byte("\r\n\r\n")) {
			select {
			case frame := <-p.data:
				request = append(request, frame.payload...)
			case <-time.After(testDeadline):
				return
			}
		}
		if !bytes.HasPrefix(request, []byte("GET /json/version ")) {
			t.Errorf("launcher saw %q", string(request[:min(len(request), 64)]))
		}
		body := `{"webSocketDebuggerUrl":"ws://127.0.0.1:9333/devtools/browser/abc-123"}`
		response := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " +
			strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
		p.sendData(id, []byte(response))
		p.send(webview2host.TunnelControl{Op: webview2host.TunnelClose, StreamID: id})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	url, err := f.endpoint.BrowserWebSocketURL(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	want := "ws://" + f.endpoint.Addr() + "/devtools/browser/abc-123"
	if url != want {
		t.Fatalf("discovered %q, want %q", url, want)
	}
}

func TestBrowserWebSocketURLWithoutATunnel(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.endpoint.BrowserWebSocketURL(ctx); !errors.Is(err, ErrTunnelDown) {
		t.Fatalf("got %v, want ErrTunnelDown", err)
	}
}

func TestBrowserWebSocketURLAfterClose(t *testing.T) {
	f := newFixture(t)
	f.connect()
	// A live tunnel is what makes the guarded ready-channel close matter:
	// Close must wake waiters without double-closing it.
	if err := f.endpoint.WaitConnected(context.Background()); err != nil {
		t.Fatalf("wait connected: %v", err)
	}
	if err := f.endpoint.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := f.endpoint.BrowserWebSocketURL(context.Background()); !errors.Is(err, ErrRelayClosed) {
		t.Fatalf("got %v, want ErrRelayClosed", err)
	}
}

func TestRewriteDebuggerURL(t *testing.T) {
	const addr = "127.0.0.1:41235"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"windows loopback", "ws://127.0.0.1:9333/devtools/browser/abc", "ws://127.0.0.1:41235/devtools/browser/abc"},
		{"another host entirely", "ws://192.168.1.9:9333/devtools/browser/abc", "ws://127.0.0.1:41235/devtools/browser/abc"},
		{"wss downgrades to the local ws", "wss://example.test/devtools/browser/abc", "ws://127.0.0.1:41235/devtools/browser/abc"},
		{"query survives", "ws://127.0.0.1:9333/devtools/browser/abc?v=1", "ws://127.0.0.1:41235/devtools/browser/abc?v=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteDebuggerURL(tc.raw, addr)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	for _, raw := range []string{"", "http://127.0.0.1:9333/devtools/browser/abc", "ws://127.0.0.1:9333", "ws://127.0.0.1:9333/", "://nonsense"} {
		if got, err := RewriteDebuggerURL(raw, addr); err == nil {
			t.Fatalf("%q was accepted as %q", raw, got)
		}
	}
}
