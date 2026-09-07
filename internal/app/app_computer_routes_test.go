package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/backendproxy"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
)

func TestNativeRouteChangesInvalidateOnlyAfterUsableAddressChanges(t *testing.T) {
	b, ctx, conn, cfg := nativeNetworkBackend(t)
	bus := transport.NewEventBus(16)
	b.app.SetEventBus(bus)
	sub := bus.Subscribe()
	defer sub.Close()
	sub.SetChannels([]string{eventchan.ComputerRoutesChanged.String()})
	report := nativeReport(b, cfg)
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.Events():
		if string(event.Data) != "{}" {
			t.Fatalf("invalidation exposed stale route trust: %s", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("new native address did not invalidate connected clients")
	}
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.Events():
		t.Fatalf("unchanged native report invalidated clients: %+v", event)
	default:
	}
	conn.RunCleanups()
	select {
	case <-sub.Events():
	case <-time.After(time.Second):
		t.Fatal("withdrawn native listener left clients unaware")
	}
	if err := b.app.ReportNativeNetworkState(ctx, report); err == nil {
		t.Fatal("late launcher restored routes")
	}
	select {
	case <-sub.Events():
		t.Fatal("rejected report changed advertised routes")
	default:
	}
}

func TestOrdinaryPairedClientsLearnNewRoutesThroughLiveBootstrapAndHello(t *testing.T) {
	var current atomic.Value
	current.Store([]computerroute.Route(nil))
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		cfg.ComputerRoutes = func() []computerroute.Route { return current.Load().([]computerroute.Route) }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invite, link := backend.mintLink(t, "full")
	dir := t.TempDir()
	client, _, err := deviceclient.Pair(ctx, dir, link, "Ordinary desktop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
	ws, err := client.DialURL("")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := backendproxy.New(backendproxy.Config{WSURL: ws, Paired: client})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := proxy.FetchBootstrap(ctx); err != nil || status != http.StatusOK {
		t.Fatalf("initial bootstrap: %d %v", status, err)
	}
	later := []computerroute.Route{{Endpoint: "https://later.test.ts.net"}}
	current.Store(later)
	// A route-change nudge causes this same authenticated refresh while the
	// existing page socket remains open. The proxy updates its Go-owned profile.
	if status, _, err := proxy.FetchBootstrap(ctx); err != nil || status != http.StatusOK {
		t.Fatalf("live bootstrap refresh: %d %v", status, err)
	}
	saved, err := deviceclient.LoadSession(dir, link.BackendID)
	if err != nil || !slices.Equal(saved.Routes, later) {
		t.Fatalf("live route was not remembered: %v %v", saved.Routes, err)
	}
	manager, err := attachedbackends.New(dir, "Ordinary desktop", "linux")
	if err != nil {
		t.Fatal(err)
	}
	newer := computerroute.Route{Endpoint: "https://latest.test.ts.net"}
	current.Store([]computerroute.Route{newer})
	// No membership enrollment or visible frontend is needed to learn the
	// current snapshot on the next authenticated computer RPC connection.
	var result json.RawMessage
	if err := manager.CallOwnDevice(ctx, link.BackendID, "ListOwnDevices", &result); err != nil {
		t.Fatal(err)
	}
	saved, err = deviceclient.LoadSession(dir, link.BackendID)
	if err != nil || len(saved.Routes) != 2 || saved.Routes[0] != newer {
		t.Fatalf("hello route was not remembered: %v %v", saved.Routes, err)
	}
	if saved.OwnDevice {
		t.Fatal("route learning enrolled an ordinary pairing")
	}
}

func TestSurvivingSocketRepairsRoutesAfterListenerMoves(t *testing.T) {
	ownConnectionNetwork(t)
	var app *App
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		cfg.BindAddr = "0.0.0.0"
		cfg.ComputerRoutes = func() []computerroute.Route { return ComputerRoutes(app) }
	})
	app = backend.app
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{BindAll: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	invite, link := backend.mintLink(t, "view-only")
	dir := t.TempDir()
	client, _, err := deviceclient.Pair(ctx, dir, link, "Read-only desktop", "linux", deviceclient.WithDialContext(dialOwnFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
	sessionID := client.Session().SessionID
	ticket, err := client.Ticket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	address, err := client.DialURL(ticket)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: &http.Client{Transport: client.RoundTripper()}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var before []computerroute.Route
	if err := json.Unmarshal(callOverWS(t, ctx, conn, "GetComputerRoutes"), &before); err != nil || len(before) != 1 {
		t.Fatalf("initial route: %v %v", before, err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	if _, err := app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true, ListenPort: port}); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Endpoint()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	unreachable := &http.Client{Transport: client.RoundTripper()}
	if response, err := unreachable.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("retired HTTP listener remained reachable")
	}
	var after []computerroute.Route
	if err := json.Unmarshal(callOverWS(t, ctx, conn, "GetComputerRoutes"), &after); err != nil || len(after) != 1 || after[0].Endpoint == before[0].Endpoint {
		t.Fatalf("surviving socket did not reveal the moved listener: %v %v", after, err)
	}
	if _, err := client.RepairAddress(ctx, after[0].Endpoint); err != nil {
		t.Fatal(err)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, client.Endpoint()+"/healthz", nil)
	response, err := unreachable.Do(request)
	if err != nil {
		t.Fatal("HTTP did not recover after verified address repair", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || client.Session().SessionID != sessionID {
		t.Fatal("repair did not preserve the pairing")
	}
}

func TestEnablingTailnetPreservesActiveNativeLANRelay(t *testing.T) {
	backend, ctx, _, cfg := nativeNetworkBackend(t)
	report := nativeReport(backend, cfg)
	if err := backend.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.app.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true, TailnetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	next, err := backend.app.GetNativeNetworkConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != cfg.Generation || next.Target != cfg.Target {
		t.Fatal("enabling a separate tailnet listener retired the active Windows LAN relay")
	}
	if routes := backend.app.nativeLANStatus(); len(routes.Addresses) != 1 || routes.Addresses[0] != report.Addresses[0] {
		t.Fatalf("tailnet toggle withdrew the current LAN address: %+v", routes)
	}
	if err := backend.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal("healthy relay reports were superseded", err)
	}
}
