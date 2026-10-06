package transport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
)

// acceptGate holds a listener's accepts, so connections a client opens
// stay queued in the kernel as they do while a busy server has not yet
// accepted them.
type acceptGate struct {
	*net.TCPListener
	mu   sync.Mutex
	open chan struct{}
}

func newAcceptGate(ln *net.TCPListener) *acceptGate {
	g := &acceptGate{TCPListener: ln, open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *acceptGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = make(chan struct{})
}

func (g *acceptGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}

func (g *acceptGate) Accept() (net.Conn, error) {
	g.mu.Lock()
	open := g.open
	g.mu.Unlock()
	<-open
	return g.TCPListener.Accept()
}

// startGatedServer starts a server whose first loopback socket is gated.
// Tests stay off the LAN, so a loopback socket on another port stands in
// for every wildcard bind.
func startGatedServer(t *testing.T, bindAddr string) (*Server, *acceptGate) {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&fakeApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv, err := New(Config{Dispatcher: d, EventBus: NewEventBus(20), Token: "test-token", BindAddr: bindAddr})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	var gate *acceptGate
	srv.bindTCP = func(network, addr string, sharePort bool) (net.Listener, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if wildcardIPv4(host) {
			return listenTCPSharing(network, "127.0.0.1:0", sharePort)
		}
		ln, err := listenTCPSharing(network, addr, sharePort)
		if err != nil || gate != nil {
			return ln, err
		}
		gate = newAcceptGate(ln.(*net.TCPListener))
		return gate, nil
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	// Runs before the shutdown above, so no accept stays parked.
	t.Cleanup(gate.release)
	return srv, gate
}

// queueRequests opens n connections to addr and writes a request on each
// while the server is not accepting.
func queueRequests(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	conns := make([]net.Conn, 0, n)
	for i := range n {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d to %s: %v", i, addr, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", HealthPath, addr)
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	return conns
}

func requireServed(t *testing.T, step string, conns []net.Conn) {
	t.Helper()
	for i, conn := range conns {
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("%s: set deadline: %v", step, err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%s: connection %d queued before the move was not served: %v", step, i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: connection %d answered %d", step, i, resp.StatusCode)
		}
	}
}

// moveWithQueue queues requests on the loopback socket, moves the server
// to addr, then lets the server accept and requires every request served.
func moveWithQueue(t *testing.T, srv *Server, gate *acceptGate, addr string) {
	t.Helper()
	gate.hold()
	conns := queueRequests(t, gate.Addr().String(), 8)
	if err := srv.Rebind(addr, nil); err != nil {
		t.Fatalf("rebind to %s: %v", addr, err)
	}
	gate.release()
	requireServed(t, "move to "+addr, conns)
}

func requireRefused(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("retired wildcard socket %s still accepts", addr)
	}
}

// A LAN toggle must not reset the connections a loopback client already
// opened: closing a listening socket resets everything queued on it.
func TestRebind_ServesConnectionsQueuedOnTheLoopbackSocket(t *testing.T) {
	remotetest.Require(t)

	t.Run("booted on loopback", func(t *testing.T) {
		srv, gate := startGatedServer(t, "127.0.0.1")
		loopbackAddr := srv.Addr()
		_, port, err := net.SplitHostPort(loopbackAddr)
		if err != nil {
			t.Fatal(err)
		}
		moveWithQueue(t, srv, gate, net.JoinHostPort("0.0.0.0", port))
		wildcard := srv.Addr()
		if wildcard == loopbackAddr {
			t.Fatalf("Addr still %s after the move to the wildcard", wildcard)
		}
		moveWithQueue(t, srv, gate, loopbackAddr)
		if got := srv.Addr(); got != loopbackAddr {
			t.Fatalf("Addr = %s after the move back, want %s", got, loopbackAddr)
		}
		requireRefused(t, wildcard)
	})

	t.Run("booted on the wildcard", func(t *testing.T) {
		srv, gate := startGatedServer(t, "0.0.0.0")
		wildcard := srv.Addr()
		loopbackAddr := gate.Addr().String()
		_, port, err := net.SplitHostPort(loopbackAddr)
		if err != nil {
			t.Fatal(err)
		}
		moveWithQueue(t, srv, gate, loopbackAddr)
		requireRefused(t, wildcard)
		moveWithQueue(t, srv, gate, net.JoinHostPort("0.0.0.0", port))
	})
}
