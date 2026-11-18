package attachedbackends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
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
	// pings counts the pings the client sent, over every connection.
	pings atomic.Int32
	mu    sync.Mutex
	conns []*websocket.Conn
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
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OnPingReceived: func(context.Context, []byte) bool {
		p.pings.Add(1)
		return true
	}})
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

// TestAHeldConnectionCarriesEveryCallAfterOnePing: the chunks of one copy
// share one connection, and only the first call on a pooled one pays the
// ping that proves its path; the connection then goes back to the pool.
func TestAHeldConnectionCarriesEveryCallAfterOnePing(t *testing.T) {
	m, p := newThreadPeerManager(t)
	first := query(t, m, p.id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.HoldThreadPeer(ctx, p.id, func(call PeerCall) error {
		for chunk := range 4 {
			var number int32
			if err := call(ctx, "ThreadToolQuery", &number); err != nil {
				return err
			}
			if number != first {
				return fmt.Errorf("chunk %d ran on connection %d, want the held %d", chunk, number, first)
			}
		}
		// The hold keeps the thread tools surface's own allowlist.
		if err := call(ctx, "RemoteCommandStart", nil); err == nil {
			return errors.New("a held connection reached a method outside the thread tools surface")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.pings.Load(); got != 1 {
		t.Fatalf("four calls on a held connection sent %d pings, want the one that proved it", got)
	}
	if got := query(t, m, p.id); got != first {
		t.Fatalf("the call after the hold ran on connection %d, want the pooled %d", got, first)
	}
	if got := p.tickets.Load(); got != 1 {
		t.Fatalf("minted %d tickets, want 1", got)
	}
}

// TestAHeldConnectionThatLosesACallIsReplaced: a call that ends without an
// answer closes the held connection, and the next call dials another.
func TestAHeldConnectionThatLosesACallIsReplaced(t *testing.T) {
	m, p := newThreadPeerManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.HoldThreadPeer(ctx, p.id, func(call PeerCall) error {
		var first int32
		if err := call(ctx, "ThreadToolQuery", &first); err != nil {
			return err
		}
		lost, stop := context.WithCancel(ctx)
		stop()
		if err := call(lost, "ThreadToolQuery", nil); !errors.Is(err, context.Canceled) {
			return fmt.Errorf("a call on an ended context = %v", err)
		}
		if closed := p.nextClosed(t); closed != first {
			return fmt.Errorf("closed connection %d, want the held %d", closed, first)
		}
		var next int32
		if err := call(ctx, "ThreadToolQuery", &next); err != nil {
			return err
		}
		if next == first {
			return errors.New("the call after a lost one ran on the closed connection")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAConnectionOutlastsTheIntervalOfAPeriodicCheck: a status poll or a
// running command's watch asks again five seconds after its last answer,
// and that check reuses the connection the last one left, pinged first,
// instead of paying a ticket, an upgrade and a hello.
func TestAConnectionOutlastsTheIntervalOfAPeriodicCheck(t *testing.T) {
	m, p := newThreadPeerManager(t)
	first := query(t, m, p.id)
	held, err := m.carrier(context.Background(), p.id)
	if err != nil {
		t.Fatal(err)
	}
	// Five seconds of cadence and a wake that came late.
	held.peers.mu.Lock()
	held.peers.idle[0].since = time.Now().Add(-10 * time.Second)
	held.peers.mu.Unlock()
	held.peers.expire()
	if got := query(t, m, p.id); got != first {
		t.Fatalf("the next check ran on connection %d, want the reused %d", got, first)
	}
	if got := p.tickets.Load(); got != 1 {
		t.Fatalf("minted %d tickets for two checks, want 1", got)
	}
}

func TestAConnectionTheFarSideClosedIsNotReused(t *testing.T) {
	m, p := newThreadPeerManager(t)
	first := query(t, m, p.id)
	held, err := m.carrier(context.Background(), p.id)
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
		held, err := m.carrier(context.Background(), p.id)
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
	held, err := m.carrier(context.Background(), p.id)
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
	if held.peers.ended == nil || len(held.peers.open) != 0 || len(held.peers.idle) != 0 || held.peers.expiry != nil {
		t.Fatalf("a removed carrier still pools: ended %v, %d open, %d idle, timer %v", held.peers.ended, len(held.peers.open), len(held.peers.idle), held.peers.expiry)
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
	held, err := m.carrier(context.Background(), p.id)
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

// TestEveryConnectionCheckedOutOfAPairingEndsWithIt: however a pairing's
// carrier leaves, the connections calls hold close with the idle ones, and
// each call on one fails with the pairing's verdict rather than a lost
// connection. Nothing is dialled under that pairing afterwards.
func TestEveryConnectionCheckedOutOfAPairingEndsWithIt(t *testing.T) {
	for _, end := range []struct {
		name  string
		leave func(t *testing.T, m *Manager, id string)
		code  string
	}{
		{"forgotten", func(t *testing.T, m *Manager, id string) {
			if err := m.Remove(id); err != nil {
				t.Error(err)
			}
		}, "remote_not_paired"},
		{"ended by the far side", func(t *testing.T, m *Manager, id string) {
			held, err := m.carrier(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			held.client.Retire()
			if !held.ended(deviceclient.ErrSessionEnded) {
				t.Error("the verdict was not taken")
			}
		}, "remote_pairing_expired"},
		{"replaced on disk", func(t *testing.T, m *Manager, id string) {
			held, err := m.carrier(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			held.client.Retire()
			if again, err := m.carrier(context.Background(), id); err != nil || again == held {
				t.Errorf("the lookup after a retirement = %v, %v; want the profile read again", again, err)
			}
		}, "remote_pairing_replaced"},
		{"closed", func(_ *testing.T, m *Manager, _ string) { m.Close() }, "remote_pairing_unavailable"},
	} {
		t.Run(end.name, func(t *testing.T) {
			m, p := newThreadPeerManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// A call that never gets its answer holds one connection.
			hung := make(chan error, 1)
			go func() { hung <- m.CallThreadPeer(ctx, p.id, "ThreadToolCall", nil) }()
			waitFor(t, "the hung call's connection", func() bool { return p.opened.Load() == 1 })
			err := m.HoldThreadPeer(ctx, p.id, func(call PeerCall) error {
				var held int32
				if err := call(ctx, "ThreadToolQuery", &held); err != nil {
					return err
				}
				end.leave(t, m, p.id)
				closed := map[int32]bool{p.nextClosed(t): true, p.nextClosed(t): true}
				if !closed[1] || !closed[held] {
					return fmt.Errorf("closed connections %v, want the hung call's 1 and the held %d", closed, held)
				}
				return call(ctx, "ThreadToolQuery", nil)
			})
			for name, err := range map[string]error{"the held call": err, "the hung call": <-hung} {
				if code, _, _ := errorsx.PublicDetails(err); code != end.code {
					t.Fatalf("%s after the pairing was %s = %v, want %s", name, end.name, err, end.code)
				}
			}
			if got := p.opened.Load(); got != 2 {
				t.Fatalf("%d connections opened, want none after the pairing was %s", got, end.name)
			}
		})
	}
}

// TestAConnectionThatOpensAfterItsPairingEndedIsClosed: a dial already past
// its ticket when the carrier leaves finishes after it, and is closed then
// rather than handed to its caller.
func TestAConnectionThatOpensAfterItsPairingEndedIsClosed(t *testing.T) {
	m, p := newThreadPeerManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, err := m.carrier(ctx, p.id)
	if err != nil {
		t.Fatal(err)
	}
	// The owner still authorizes, as it does until a replacement retires it.
	held.leave(errPairingReplaced)
	if rpc, err := held.openRPC(ctx, transport.CapabilityThreadTools); !errors.Is(err, errPairingReplaced) {
		t.Fatalf("a connection opened after the carrier left = %v, %v; want the verdict", rpc, err)
	}
	if got := p.nextClosed(t); got != 1 {
		t.Fatalf("closed connection %d, want the one opened after the carrier left", got)
	}
}
