package attachedbackends

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

// threadPeer is a paired computer serving the thread tools surface. Every
// connection answers ThreadToolQuery with its own number, after an event
// and a heartbeat frame the client must skip. ThreadToolCall is never answered.
type threadPeer struct {
	*httptest.Server
	id      string
	tickets atomic.Int32
	opened  atomic.Int32
	mu      sync.Mutex
	conns   []*websocket.Conn
	// closed receives each connection's number once the client ends it.
	closed chan int32
}

func newThreadPeer(t *testing.T) *threadPeer {
	t.Helper()
	p := &threadPeer{id: uuid.NewString(), closed: make(chan int32, 16)}
	p.Server = httptest.NewServer(http.HandlerFunc(p.route))
	t.Cleanup(p.Close)
	return p
}

func (p *threadPeer) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/auth/ticket" {
		p.tickets.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket"})
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	number := p.opened.Add(1)
	p.mu.Lock()
	p.conns = append(p.conns, conn)
	p.mu.Unlock()
	ctx := context.Background()
	if wsjson.Write(ctx, conn, map[string]any{"type": "hello", "backendId": p.id, "protocolVersion": transport.ProtocolVersion, "capabilities": []string{transport.CapabilityThreadTools}}) != nil {
		return
	}
	for {
		var frame transport.ClientFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			p.closed <- number
			return
		}
		switch frame.Method {
		case "ThreadToolCall":
			continue
		case "ThreadToolResolve":
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "rpc", "id": frame.ID, "error": map[string]string{"code": "thread_not_found", "message": "no such thread"}})
			continue
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "event", "event": "thread:updated", "data": map[string]string{"id": "x"}})
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "ping"})
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "rpc", "id": frame.ID, "result": number})
	}
}

// nextClosed is the number of the next connection the client ended. The
// wait is shorter than the idle window, so expiry cannot stand in for the
// close a test expects.
func (p *threadPeer) nextClosed(t *testing.T) int32 {
	t.Helper()
	select {
	case number := <-p.closed:
		return number
	case <-time.After(peerConnIdle / 2):
		t.Fatal("no connection was closed")
		return 0
	}
}

// closeAll ends every connection from the far side, as a revocation does.
func (p *threadPeer) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		_ = conn.CloseNow()
	}
}

func newThreadPeerManager(t *testing.T) (*Manager, *threadPeer) {
	t.Helper()
	remotetest.Require(t)
	p := newThreadPeer(t)
	m, dir := newManager(t)
	seed(t, dir, deviceclient.Session{BackendID: p.id, Endpoint: p.URL, SessionID: "session", Credential: "credential", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
	return m, p
}

// query makes one call on its own context, cancelled when the call
// returns, the way the thread tools bound each call.
func query(t *testing.T, m *Manager, id string) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var number int32
	if err := m.CallThreadPeer(ctx, id, "ThreadToolQuery", &number); err != nil {
		t.Fatalf("call: %v", err)
	}
	return number
}

func TestAgentCallsInABurstShareOneConnectionAndTicket(t *testing.T) {
	m, p := newThreadPeerManager(t)
	first := query(t, m, p.id)
	// An answered refusal leaves the connection in step for the next call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var remote *rpcclient.Error
	if err := m.CallThreadPeer(ctx, p.id, "ThreadToolResolve", nil); !errors.As(err, &remote) || remote.Code != "thread_not_found" {
		t.Fatalf("refusal: %v", err)
	}
	for range 3 {
		if got := query(t, m, p.id); got != first {
			t.Fatalf("call ran on connection %d, want the reused %d", got, first)
		}
	}
	if got := p.tickets.Load(); got != 1 {
		t.Fatalf("minted %d tickets for one burst, want 1", got)
	}
}

func TestAConnectionTheFarSideClosedIsNotReused(t *testing.T) {
	m, p := newThreadPeerManager(t)
	first := query(t, m, p.id)
	held, err := m.carrier(p.id)
	if err != nil {
		t.Fatal(err)
	}
	p.closeAll()
	waitFor(t, "the idle connection to end", func() bool {
		held.peers.mu.Lock()
		defer held.peers.mu.Unlock()
		return len(held.peers.idle) == 1 && !live(held.peers.idle[0].rpc)
	})
	if got := query(t, m, p.id); got == first {
		t.Fatalf("call ran on the closed connection %d", got)
	}
	if got := p.tickets.Load(); got != 2 {
		t.Fatalf("minted %d tickets, want a second for the new connection", got)
	}
}

func TestACancelledCallEndsOnlyItsOwnConnection(t *testing.T) {
	m, p := newThreadPeerManager(t)
	idle := query(t, m, p.id)
	// The hung call takes the idle connection, so the concurrent query dials its own.
	ctx, cancel := context.WithCancel(context.Background())
	hung := make(chan error, 1)
	go func() { hung <- m.CallThreadPeer(ctx, p.id, "ThreadToolCall", nil) }()
	waitFor(t, "the hung call to take the idle connection", func() bool {
		held, err := m.carrier(p.id)
		if err != nil {
			return false
		}
		held.peers.mu.Lock()
		defer held.peers.mu.Unlock()
		return len(held.peers.idle) == 0
	})
	other := query(t, m, p.id)
	if other == idle {
		t.Fatalf("a concurrent call shared connection %d with the hung one", other)
	}
	cancel()
	if err := <-hung; !errors.Is(err, context.Canceled) {
		t.Fatalf("hung call: %v", err)
	}
	if got := p.nextClosed(t); got != idle {
		t.Fatalf("closed connection %d, want the cancelled call's %d", got, idle)
	}
	if got := query(t, m, p.id); got != other {
		t.Fatalf("call ran on connection %d, want the surviving %d", got, other)
	}
}

func TestIdleConnectionsEndAfterTheirWindowAndWithTheCarrier(t *testing.T) {
	m, p := newThreadPeerManager(t)
	expired := query(t, m, p.id)
	held, err := m.carrier(p.id)
	if err != nil {
		t.Fatal(err)
	}
	held.peers.mu.Lock()
	held.peers.idle[0].since = time.Now().Add(-peerConnIdle)
	held.peers.mu.Unlock()
	held.peers.expire()
	if got := p.nextClosed(t); got != expired {
		t.Fatalf("closed connection %d, want the expired %d", got, expired)
	}
	kept := query(t, m, p.id)
	if err := m.Remove(p.id); err != nil {
		t.Fatal(err)
	}
	if got := p.nextClosed(t); got != kept {
		t.Fatalf("closed connection %d, want %d when the carrier left", got, kept)
	}
	held.peers.mu.Lock()
	defer held.peers.mu.Unlock()
	if !held.peers.closed || len(held.peers.idle) != 0 || held.peers.expiry != nil {
		t.Fatalf("a removed carrier still pools: closed %v, %d idle, timer %v", held.peers.closed, len(held.peers.idle), held.peers.expiry)
	}
}

// pathProxy forwards TCP to a peer. blackhole silently stops carrying the
// connections it holds, as a network change or a NAT rebinding does, while
// new connections still get through.
type pathProxy struct {
	net.Listener
	mu    sync.Mutex
	paths []*atomic.Bool
}

func newPathProxy(t *testing.T, target string) *pathProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pathProxy{Listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			dead := new(atomic.Bool)
			p.mu.Lock()
			p.paths = append(p.paths, dead)
			p.mu.Unlock()
			go carry(server, client, dead)
			go carry(client, server, dead)
		}
	}()
	return p
}

// carry copies until the path dies, then discards what arrives.
func carry(to, from net.Conn, dead *atomic.Bool) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := from.Read(buffer)
		if err != nil {
			return
		}
		if !dead.Load() {
			if _, err := to.Write(buffer[:n]); err != nil {
				return
			}
		}
	}
}

func (p *pathProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, dead := range p.paths {
		dead.Store(true)
	}
}

func TestAConnectionWhosePathDiedSilentlyIsReplaced(t *testing.T) {
	remotetest.Require(t)
	p := newThreadPeer(t)
	proxy := newPathProxy(t, p.Listener.Addr().String())
	m, dir := newManager(t)
	seed(t, dir, deviceclient.Session{BackendID: p.id, Endpoint: "http://" + proxy.Addr().String(), SessionID: "session", Credential: "credential", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
	first := query(t, m, p.id)
	proxy.blackhole()
	// The ticket mint's idle HTTP connections died with the path too, inside
	// deviceclient's idle window (pinnedIdleConnTimeout). They are dropped
	// here to leave only the pooled socket under test.
	held, err := m.carrier(p.id)
	if err != nil {
		t.Fatal(err)
	}
	held.client.RoundTripper().(interface{ CloseIdleConnections() }).CloseIdleConnections()
	// The call's own deadline is longer than the liveness check, so only a
	// call that waited on the dead path fails here.
	ctx, cancel := context.WithTimeout(context.Background(), 2*peerPingTimeout)
	defer cancel()
	var number int32
	if err := m.CallThreadPeer(ctx, p.id, "ThreadToolQuery", &number); err != nil {
		t.Fatalf("call after the path died: %v", err)
	}
	if number == first {
		t.Fatalf("call ran on the dead connection %d", number)
	}
}
