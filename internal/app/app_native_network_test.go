package app

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/nativenetwork"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/transport"
)

func nativeNetworkBackend(t *testing.T) (*pairedBackend, context.Context, *transport.ConnState, nativenetwork.Config) {
	t.Helper()
	oldInterfaces, oldAddrs := network.Interfaces, network.InterfaceAddrs
	network.Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "wsl", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	network.InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("172.20.0.2"), Mask: net.CIDRMask(16, 32)}}, nil
	}
	t.Cleanup(func() { network.Interfaces, network.InterfaceAddrs = oldInterfaces, oldAddrs })
	b := newPairedBackend(t, func(cfg *transport.Config) { cfg.BindAddr = "0.0.0.0" })
	if _, err := b.app.settings.SetNetwork(settings.NetworkSettings{BindAll: true}); err != nil {
		t.Fatal(err)
	}
	ctx, conn := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	t.Cleanup(conn.RunCleanups)
	cfg, err := b.app.GetNativeNetworkConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return b, ctx, conn, cfg
}

func nativeReport(b *pairedBackend, cfg nativenetwork.Config) nativenetwork.State {
	return nativenetwork.State{Generation: cfg.Generation, Addresses: []string{fmt.Sprintf("https://192.168.1.55:%d", portFromAddr(b.srv.Addr()))}, ScanID: cfg.ScanID}
}

func TestNativeNetworkOwnerHandoverRetiresOldObservations(t *testing.T) {
	b, ctx, old, cfg := nativeNetworkBackend(t)
	report := nativeReport(b, cfg)
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	if got := b.app.nativeLANStatus(); len(got.Addresses) != 1 {
		t.Fatal("valid Windows ingress missing")
	}
	// Config always targets the actual WSL listener, never the Windows
	// forwarding address that Report publishes to remote clients.
	if !strings.HasPrefix(cfg.Target, "https://172.20.0.2:") {
		t.Fatal("launcher was directed back to its own ingress", cfg.Target)
	}
	newCtx, newConn := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	defer newConn.RunCleanups()
	newCfg, err := b.app.GetNativeNetworkConfig(newCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.app.nativeLANStatus().Addresses) != 0 {
		t.Fatal("replacement launcher inherited stale ready address")
	}
	if err := b.app.ReportNativeNetworkState(ctx, report); err == nil {
		t.Fatal("superseded launcher restored its ingress")
	}
	old.RunCleanups()
	if err := b.app.ReportNativeNetworkState(newCtx, nativeReport(b, newCfg)); err != nil {
		t.Fatal("old cleanup removed new owner", err)
	}
	newConn.RunCleanups()
	if status := b.app.nativeLANStatus(); len(status.Addresses) != 0 || status.Error == "" {
		t.Fatal("disconnected launcher still appeared ready")
	}
}

func TestNativeNetworkRejectsMalformedOrSupersededReports(t *testing.T) {
	b, ctx, _, cfg := nativeNetworkBackend(t)
	good := nativeReport(b, cfg)
	for name, mutate := range map[string]func(*nativenetwork.State){
		"public ingress": func(s *nativenetwork.State) { s.Addresses = []string{"https://8.8.8.8:443"} },
		"loopback":       func(s *nativenetwork.State) { s.Addresses = []string{"https://127.0.0.1:443"} },
		"credentials":    func(s *nativenetwork.State) { s.Addresses = []string{"https://secret@192.168.1.55:443"} },
		"wrong port":     func(s *nativenetwork.State) { s.Addresses = []string{"https://192.168.1.55:1"} },
		"address count":  func(s *nativenetwork.State) { s.Addresses = make([]string, 17) },
		"error size":     func(s *nativenetwork.State) { s.Error = strings.Repeat("x", 1025) },
		"nearby count":   func(s *nativenetwork.State) { s.Nearby = make([]nearby.Host, 65) },
		"nearby credential": func(s *nativenetwork.State) {
			s.Nearby = []nearby.Host{{BackendID: "peer", Name: "PC", Address: "https://user@192.168.1.2:443"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := good
			mutate(&bad)
			if err := b.app.ReportNativeNetworkState(ctx, bad); err == nil {
				t.Fatal("malformed report accepted")
			}
		})
	}
	if err := b.app.ReportNativeNetworkState(ctx, good); err != nil {
		t.Fatal(err)
	}
	b.app.invalidateNativeNetwork()
	if err := b.app.ReportNativeNetworkState(ctx, good); err == nil {
		t.Fatal("old configuration generation restored readiness")
	}
	next, err := b.app.GetNativeNetworkConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation == cfg.Generation {
		t.Fatal("configuration invalidation reused generation")
	}
	if err := b.app.ReportNativeNetworkState(ctx, nativeReport(b, next)); err != nil {
		t.Fatal(err)
	}
	b.app.nativeNetwork.mu.Lock()
	b.app.nativeNetwork.addresses = []string{"https://192.168.1.55:1"}
	b.app.nativeNetwork.mu.Unlock()
	if len(b.app.nativeLANStatus().Addresses) != 0 {
		t.Fatal("retired listener port was published")
	}
}

func startNativeScan(t *testing.T, a *App, owner context.Context) (uint64, <-chan []attachedbackends.DiscoveredComputer) {
	t.Helper()
	done := make(chan []attachedbackends.DiscoveredComputer, 1)
	go func() { done <- a.discoverNativeComputers(t.Context()) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cfg, err := a.GetNativeNetworkConfig(owner)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ScanID != 0 {
			return cfg.ScanID, done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("native scan did not start")
	return 0, done
}

func TestNativeDiscoveryKeepsResultsWithTheirScanAndRetiresOnOwnerLoss(t *testing.T) {
	b, ctx, conn, cfg := nativeNetworkBackend(t)
	firstID, first := startNativeScan(t, b.app, ctx)
	report := nativeReport(b, cfg)
	report.ScanID = firstID
	report.Nearby = []nearby.Host{{BackendID: "d4445647-1d5e-479e-b6ee-8936ac5210e7", Name: "First", Address: "https://192.168.1.2:443"}}
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	secondID, second := startNativeScan(t, b.app, ctx)
	if secondID == firstID {
		t.Fatal("new scan reused old generation")
	}
	// A late result may update ingress, but cannot complete the newer scan.
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
		t.Fatal("stale report completed new scan")
	default:
	}
	report.ScanID = secondID
	report.Nearby[0].Name = "Second"
	if err := b.app.ReportNativeNetworkState(ctx, report); err != nil {
		t.Fatal(err)
	}
	if got := <-first; len(got) != 1 || got[0].Name != "First" {
		t.Fatal("later scan replaced earlier result", got)
	}
	if got := <-second; len(got) != 1 || got[0].Name != "Second" {
		t.Fatal("new scan lost its own result", got)
	}
	_, pending := startNativeScan(t, b.app, ctx)
	conn.RunCleanups()
	select {
	case got := <-pending:
		if len(got) != 0 {
			t.Fatal("disconnected owner returned stale scan")
		}
	case <-time.After(time.Second):
		t.Fatal("owner disconnect stranded scan")
	}
}

func TestForwardedLANAddressDoesNotFallBackToWSLWhenUnavailable(t *testing.T) {
	if got := network.LANIP(network.Settings{}, "172.20.0.2"); got != "172.20.0.2" {
		t.Fatal("ordinary host lost native interface")
	}
	s := network.Settings{LAN: &network.LANStatus{}}
	if got := network.LANIP(s, "172.20.0.2"); got != "" {
		t.Fatal("unready Windows forwarding advertised unreachable WSL IP", got)
	}
	s.LAN.Addresses = []string{"https://192.168.1.55:6000"}
	if got := network.LANIP(s, "172.20.0.2"); got != "192.168.1.55" {
		t.Fatal("Windows ingress not selected", got)
	}
}

func TestNativeDiscoveryTimeoutAllowsFreshScan(t *testing.T) {
	b, ctx, conn, _ := nativeNetworkBackend(t)
	firstID, first := startNativeScan(t, b.app, ctx)
	select {
	case got := <-first:
		if len(got) != 0 {
			t.Fatal("unanswered scan returned computers")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("unanswered native scan did not expire")
	}
	nextID, next := startNativeScan(t, b.app, ctx)
	if nextID == firstID {
		t.Fatal("retry reused expired scan")
	}
	conn.RunCleanups()
	<-next
}

func TestNativeNetworkDoesNotAdvertiseAnExplicitLoopbackListener(t *testing.T) {
	b, ctx, _, cfg := nativeNetworkBackend(t)
	if err := b.app.ReportNativeNetworkState(ctx, nativeReport(b, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Rebind(net.JoinHostPort("127.0.0.1", strconv.Itoa(portFromAddr(b.srv.Addr()))), nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := b.app.GetNativeNetworkConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.Target != "" {
		t.Fatalf("loopback listener enabled a broken LAN relay: %+v", cfg)
	}
	status := b.app.nativeLANStatus()
	if len(status.Addresses) != 0 || !strings.Contains(status.Error, "listening only on localhost") {
		t.Fatalf("actual loopback bind did not override stale ready report: %+v", status)
	}
}
