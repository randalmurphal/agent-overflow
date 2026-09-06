package app

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/backendproxy"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
)

func alternatePairedListener(t *testing.T, backend *pairedBackend) computerroute.Route {
	t.Helper()
	material, err := servercert.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{material.Certificate}})
	if err != nil {
		t.Fatal(err)
	}
	aux, err := backend.srv.ServeAuxiliary(listener, func(err error) { t.Errorf("auxiliary listener failed: %v", err) })
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { aux.Close() })
	return computerroute.Route{Endpoint: "https://" + listener.Addr().String(), CertFingerprint: material.Fingerprint}
}

// Both listeners terminate independent TLS certificates but share the real
// transport, identity and SQLite store. The proxy target never changes; its
// paired client must carry both HTTP and WS over the newly verified route.
func TestPairedProxySwitchesListenersWithoutChangingComputerOrSession(t *testing.T) {
	var advertised atomic.Value
	advertised.Store([]computerroute.Route(nil))
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		cfg.ComputerRoutes = func() []computerroute.Route { return advertised.Load().([]computerroute.Route) }
	})
	alternate := alternatePairedListener(t, backend)
	advertised.Store([]computerroute.Route{alternate})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))
	client, pairing, err := deviceclient.Pair(ctx, t.TempDir(), link, "route test", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
	wsURL, err := client.WebSocketURL()
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := backendproxy.New(backendproxy.Config{WSURL: wsURL, Paired: client})
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := carrier.FetchBootstrap(ctx)
	if err != nil || status != 200 {
		t.Fatalf("initial manifest: %d, %v", status, err)
	}
	if len(client.Session().Routes) != 1 {
		t.Fatal("trusted bootstrap did not teach the alternate listener")
	}
	proxy := httptest.NewServer(http.HandlerFunc(carrier.CarryUpgrade))
	defer proxy.Close()
	dial := func() {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		callOverWS(t, ctx, conn, "ListProjects")
	}
	dial()
	if err := backend.srv.Rebind("127.0.0.1:0", nil); err != nil {
		t.Fatal(err)
	}
	// The old listener is now gone. The first failed request is surfaced;
	// the next attempt may select an alternative, never replay that request.
	if _, _, err := carrier.FetchBootstrap(ctx); err == nil {
		t.Fatal("retired original listener still answered")
	}
	status, _, err = carrier.FetchBootstrap(ctx)
	if err != nil || status != 200 {
		t.Fatalf("alternate manifest: %d, %v", status, err)
	}
	dial()
	held := client.Session()
	if held.SessionID != pairing.SessionID || held.BackendID != link.BackendID || held.LastEndpoint != alternate.Endpoint {
		t.Fatal("listener switch changed pairing or failed to remember the working endpoint")
	}
}
