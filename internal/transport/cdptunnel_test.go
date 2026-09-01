package transport

import (
	"context"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/coder/websocket"
)

// The /browser-cdp route carries the Windows launcher's CDP tunnel. It is
// the same credential and the same locality rule as every other entry
// point here, and this file pins both: the route must never become a way
// to reach the pane environment from anywhere but this host.

type recordingTunnel struct {
	served chan struct{}
}

func (r *recordingTunnel) ServeCDPTunnel(ctx context.Context, conn *websocket.Conn) {
	select {
	case r.served <- struct{}{}:
	default:
	}
	// Hold the socket until the client goes away, exactly as the real
	// endpoint's read loop does.
	_, _, _ = conn.Read(ctx)
}

func newCDPTunnelServer(t *testing.T, tunnel CDPTunnelEndpoint) *Server {
	t.Helper()
	d := NewDispatcher()
	d.Register(&fakeApp{}, RegisterOptions{Package: "main", TypeName: "App"})
	srv, err := New(Config{Dispatcher: d, EventBus: NewEventBus(4), Token: "test-token", CDPTunnel: tunnel})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func TestCDPTunnelRouteAcceptsTheLaunchToken(t *testing.T) {
	tunnel := &recordingTunnel{served: make(chan struct{}, 1)}
	srv := newCDPTunnelServer(t, tunnel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+srv.Addr()+webview2host.CDPTunnelPath+"?token=test-token", nil)
	if err != nil {
		t.Fatalf("dial the tunnel route: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	select {
	case <-tunnel.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the route never handed the connection to the endpoint")
	}
}

func TestCDPTunnelRouteRefusesAWrongToken(t *testing.T) {
	tunnel := &recordingTunnel{served: make(chan struct{}, 1)}
	srv := newCDPTunnelServer(t, tunnel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, query := range []string{"", "?token=", "?token=wrong"} {
		if _, _, err := websocket.Dial(ctx, "ws://"+srv.Addr()+webview2host.CDPTunnelPath+query, nil); err == nil {
			t.Fatalf("%q was upgraded", query)
		}
	}
	select {
	case <-tunnel.served:
		t.Fatal("an unauthenticated dial reached the endpoint")
	default:
	}
}

// With no endpoint the route does not exist at all, which is what keeps a
// non-WSL build from answering on it.
func TestCDPTunnelRouteIsAbsentWithoutAnEndpoint(t *testing.T) {
	srv := newCDPTunnelServer(t, nil)

	request, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+webview2host.CDPTunnelPath+"?token=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("answered %s, want 404", response.Status)
	}
}

// The Host header guard is the DNS-rebinding defence the other routes
// carry; the tunnel route must not be the one that skips it.
func TestCDPTunnelRouteRefusesANonLoopbackHostHeader(t *testing.T) {
	tunnel := &recordingTunnel{served: make(chan struct{}, 1)}
	srv := newCDPTunnelServer(t, tunnel)

	request, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+webview2host.CDPTunnelPath+"?token=test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "browser-pane.example.test"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("answered %s, want 404", response.Status)
	}
	select {
	case <-tunnel.served:
		t.Fatal("a rebound host reached the endpoint")
	default:
	}
}
