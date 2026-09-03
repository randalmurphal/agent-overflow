package tailnet

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"tailscale.com/tsnet"
	"tailscale.com/types/logger"

	"agent-overflow/internal/transport"
)

// The two-node story, end to end and off this machine's own stack: one
// node runs the app's real transport on a listener it acquired from the
// tailnet, and a SECOND node — a stand-in for the owner's laptop — reaches
// it by its tailnet address. Everything between them is the production
// path: the real mux, the real Host guard, the real credential, the real
// admission rule.
//
// What it is here to prove is that reaching this backend over the tailnet
// changes nothing about who may do what. The peer address net/http sees is
// the client node's 100.64/10 address, which is not this machine, so the
// §4 rule fires exactly as it does for a LAN browser: the manifest still
// loads (the person holding a share link has to be able to pair) and the
// socket still refuses a launch credential that names no session.
//
// The second node is a bare tsnet.Server rather than a Node, deliberately.
// Node is the shape THIS backend needs; a peer device is not one, and
// giving it an HTTP client for a test's benefit would put a method on the
// production type that production never calls.

const (
	integrationSessionCredential = "tailnet-session-credential"
	integrationSessionID         = "sess-tailnet"
	integrationLaunchToken       = "tailnet-launch-token"
)

// wireStub stands in for the App receiver. Nothing here calls an RPC; the
// dispatcher just needs a receiver to have been registered.
type wireStub struct{}

// Ping is the one method, so registration has something to hash.
func (wireStub) Ping() string { return "pong" }

func TestATailnetPeerReachesTheAppThroughTheAuxiliaryListener(t *testing.T) {
	requireBringUpCapableHost(t)
	ctx := testContext(t)

	controlURL, _ := startControl(t)

	// --- the backend's own node, serving the real transport. ---
	backend := startTestNode(t, controlURL, "agent-overflow")
	served := awaitRunning(t, backend)

	srv := startTestTransport(t)
	_, portText, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("read the listen port: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse the listen port %q: %v", portText, err)
	}

	// The SAME numeric port the main bind uses, which is what keeps the
	// cookie name and every URL derivation uniform across the two ways in.
	ln, err := backend.Listen(port)
	if err != nil {
		t.Fatalf("listen on the tailnet: %v", err)
	}
	aux, err := srv.ServeAuxiliary(ln, func(err error) { t.Errorf("the tailnet listener stopped accepting: %v", err) })
	if err != nil {
		t.Fatalf("attach the tailnet listener: %v", err)
	}
	t.Cleanup(func() { _ = aux.Close() })
	srv.SetAuxiliaryHosts(append(append([]string(nil), served.IPs...), served.DNSName))

	// --- the owner's other device. ---
	peer := startPeerNode(t, ctx, controlURL, "owner-laptop")
	client := peer.HTTPClient()
	client.Timeout = 30 * time.Second

	base := "http://" + net.JoinHostPort(served.IPs[0], portText)

	// The manifest loads for a caller holding the launch credential, on
	// this listener exactly as on the main one.
	status, _ := getOverTailnet(t, ctx, client, base+transport.BootstrapPath, http.Header{
		"Authorization": []string{"Bearer " + integrationLaunchToken},
	})
	if status != http.StatusOK {
		t.Errorf("GET %s over the tailnet = %d, want 200", transport.BootstrapPath, status)
	}
	// And refuses one holding nothing, with the same unfingerprintable 404.
	if status, _ := getOverTailnet(t, ctx, client, base+transport.BootstrapPath, nil); status != http.StatusNotFound {
		t.Errorf("GET %s with no credential = %d, want 404", transport.BootstrapPath, status)
	}

	// The socket: the peer is not on this machine, so the launch
	// credential alone is refused and a named session is admitted.
	wsBase := "ws://" + net.JoinHostPort(served.IPs[0], portText) + transport.WSPath
	if got := dialOverTailnet(t, ctx, client, wsBase+"?token="+integrationLaunchToken, nil); got != http.StatusNotFound {
		t.Errorf("upgrade with the launch credential alone = %d, want 404", got)
	}
	admitted := dialOverTailnet(t, ctx, client, wsBase+"?token="+integrationLaunchToken, http.Header{
		transport.SessionCredentialHeader: []string{integrationSessionCredential},
	})
	if admitted != http.StatusSwitchingProtocols {
		t.Errorf("upgrade naming a live session = %d, want 101", admitted)
	}

	// Detaching withdraws the way in without touching the main listener,
	// which is the isolation the reconciler depends on.
	if err := aux.Close(); err != nil {
		t.Fatalf("detach the tailnet listener: %v", err)
	}
	srv.SetAuxiliaryHosts(nil)
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+transport.HealthPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("the tailnet listener answered %d after being detached", resp.StatusCode)
	}
	if resp, err := http.Get("http://" + srv.Addr() + transport.HealthPath); err != nil {
		t.Errorf("the main listener stopped answering after a tailnet detach: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the main listener answered %d after a tailnet detach, want 200", resp.StatusCode)
		}
	}
}

// startTestTransport builds and starts the real transport server on
// loopback, with the two session hooks the admission rule reads.
func startTestTransport(t *testing.T) *transport.Server {
	t.Helper()
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(&wireStub{}, transport.RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register the wire receiver: %v", err)
	}
	srv, err := transport.New(transport.Config{
		Dispatcher: dispatcher,
		EventBus:   transport.NewEventBus(8),
		Token:      integrationLaunchToken,
		SessionForRequest: func(r *http.Request) (string, bool) {
			switch transport.SessionCredential(r) {
			case "":
				return "", true
			case integrationSessionCredential:
				return integrationSessionID, true
			default:
				return "", false
			}
		},
		SessionLive: func(id string) bool { return id == integrationSessionID },
	})
	if err != nil {
		t.Fatalf("build the transport server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start the transport server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// startPeerNode brings up a plain tsnet node and waits for its home relay,
// so a fast test does not race the DERP handshake.
func startPeerNode(t *testing.T, ctx context.Context, controlURL, hostname string) *tsnet.Server {
	t.Helper()
	logf := logger.Discard
	if verboseNodes {
		logf = t.Logf
	}
	peer := &tsnet.Server{
		Dir:        filepath.Join(t.TempDir(), hostname),
		Hostname:   hostname,
		ControlURL: controlURL,
		Logf:       logf,
		UserLogf:   logf,
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := peer.Up(ctx); err != nil {
		t.Fatalf("bring up the peer node: %v", err)
	}
	lc, err := peer.LocalClient()
	if err != nil {
		t.Fatalf("reach the peer node's local API: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status, err := lc.StatusWithoutPeers(ctx)
		if err == nil && status.Self != nil && status.Self.Relay != "" {
			return peer
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the peer's home relay: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("the peer node never picked a home relay")
	return nil
}

// getOverTailnet issues one GET through the peer node's own dialer,
// retrying while the mesh settles rather than failing the first time a
// packet has nowhere to go yet.
func getOverTailnet(t *testing.T, ctx context.Context, client *http.Client, url string, header http.Header) (int, string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for key, values := range header {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s over the tailnet: %v", url, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// dialOverTailnet performs one WebSocket upgrade through the peer node and
// reports the refusal status, or 101 when the socket opened.
func dialOverTailnet(t *testing.T, ctx context.Context, client *http.Client, url string, header http.Header) int {
	t.Helper()
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: header,
	})
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		t.Fatalf("dial %s over the tailnet: %v (no HTTP response to read a status from)", url, err)
	}
	return resp.StatusCode
}
