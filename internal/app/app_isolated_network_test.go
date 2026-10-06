package app

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"testing"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/nativenetwork"
	"agent-overflow/internal/nearby"
	"agent-overflow/internal/netisolate"
	"agent-overflow/internal/network"
	"agent-overflow/internal/settings"
)

// TestIsolationConfinesTheNetworkOutsideTheNamespace: an isolated boot uses
// the host network only inside the test network namespace, whose LAN
// reaches nothing. Anywhere else (macOS, a bare go test) it is confined to
// loopback.
func TestIsolationConfinesTheNetworkOutsideTheNamespace(t *testing.T) {
	t.Parallel()
	a := &App{}
	ConfigureIsolation(a, IsolationConfig{})
	if got, want := a.netReach.LoopbackOnly(), !netisolate.Contained(); got != want {
		t.Fatalf("isolated boot LoopbackOnly = %v, want %v (contained = %v)", got, want, netisolate.Contained())
	}
}

// TestLoopbackReachLANToggleStaysOnLoopback: turning on LAN access in a
// loopback-confined instance keeps the listener on 127.0.0.1 and reports a
// loopback URL, so no socket is opened on the developer's LAN.
func TestLoopbackReachLANToggleStaysOnLoopback(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, srv := newNetworkTestApp(t)
	a.netReach = network.LoopbackReach()
	before := srv.Addr()
	got, err := a.SetNetworkSettings(atTheMachine(), network.Settings{BindAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr() != before {
		t.Fatalf("LAN toggle moved the listener from %s to %s", before, srv.Addr())
	}
	if host, _, _ := net.SplitHostPort(srv.Addr()); host != "127.0.0.1" {
		t.Fatalf("LAN toggle bound %s, want 127.0.0.1", srv.Addr())
	}
	if u, err := url.Parse(got.URL); err != nil || u.Host != before {
		t.Fatalf("reported URL %q, want one on %s", got.URL, before)
	}
	read, err := a.GetNetworkSettings(atTheMachine())
	if err != nil {
		t.Fatal(err)
	}
	if u, err := url.Parse(read.URL); err != nil || u.Host != before {
		t.Fatalf("read URL %q, want one on %s", read.URL, before)
	}
}

// TestLoopbackReachSendsNoMulticast: with LAN access on, a loopback-confined
// instance routes and pairs over 127.0.0.1, advertises nothing, discovers nothing by
// multicast, keeps the Windows launcher's LAN relay off and serves no LAN
// preview listeners. Each refusal is the explicit state the UI already
// shows. Serial: it replaces the interface seams, which must never be read.
func TestLoopbackReachSendsNoMulticast(t *testing.T) {
	remotetest.Require(t)
	a := identityApp(t)
	a.netReach = network.LoopbackReach()
	srv := servePairedApp(t, a).srv
	if _, err := a.settings.SetNetwork(settings.NetworkSettings{BindAll: true}); err != nil {
		t.Fatal(err)
	}
	previousInterfaces, previousAddrs, previousNearby := network.Interfaces, network.InterfaceAddrs, nearby.Interfaces
	t.Cleanup(func() {
		network.Interfaces, network.InterfaceAddrs, nearby.Interfaces = previousInterfaces, previousAddrs, previousNearby
	})
	network.Interfaces = func() ([]net.Interface, error) {
		t.Error("a loopback reach read the host's interfaces")
		return nil, nil
	}
	network.InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		t.Error("a loopback reach read the host's addresses")
		return nil, nil
	}
	nearby.Interfaces = func() ([]net.Interface, error) {
		t.Error("a loopback reach opened multicast")
		return nil, nil
	}

	origin := "127.0.0.1:" + strconv.Itoa(portFromAddr(srv.Addr()))
	if routes := ComputerRoutes(a); len(routes) != 1 || routes[0].Endpoint != "https://"+origin {
		t.Fatalf("computer routes %v, want only https://%s", routes, origin)
	}
	for _, choice := range []string{"", "lan"} {
		page, _, _, err := a.pairingPageURL(choice)
		if err != nil {
			t.Fatalf("pairing link on %q: %v", choice, err)
		}
		if u, err := url.Parse(page); err != nil || u.Host != origin {
			t.Fatalf("pairing link on %q is %q, want one on %s", choice, page, origin)
		}
	}
	w, err := a.OpenComputerPairing(context.Background(), "lan", "full")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.CloseComputerPairing(w.ID) })
	if w.Address != "https://"+origin {
		t.Fatalf("pairing address %q, want https://%s", w.Address, origin)
	}
	status, err := a.ComputerPairingStatus(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "waiting" || status.DiscoveryError != nearby.ErrIsolated.Error() {
		t.Fatalf("status = %+v, want an open window saying discovery is off", status)
	}
	a.computerPairing.mu.Lock()
	adv := a.computerPairing.advertiser
	a.computerPairing.mu.Unlock()
	if adv != nil {
		t.Fatal("a loopback reach started the multicast responders")
	}

	manager, err := attachedbackends.New(t.TempDir(), "Host", "test")
	if err != nil {
		t.Fatal(err)
	}
	SetAttachedBackends(a, manager)
	if _, err := a.DiscoverComputers(context.Background()); !errors.Is(err, nearby.ErrIsolated) {
		t.Fatalf("DiscoverComputers error = %v, want %v", err, nearby.ErrIsolated)
	}

	cfg, err := a.GetNativeNetworkConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.Target != "" {
		t.Fatalf("launcher config %+v, want the LAN relay off", cfg)
	}
	if err := a.ReportNativeNetworkState(context.Background(), nativenetwork.State{}); !errors.Is(err, errNativeNetworkIsolated) {
		t.Fatalf("ReportNativeNetworkState error = %v, want %v", err, errNativeNetworkIsolated)
	}
	a.nativeNetwork.mu.Lock()
	seen := a.nativeNetwork.seen
	a.nativeNetwork.mu.Unlock()
	if seen {
		t.Fatal("the launcher became this instance's LAN ingress")
	}
	if ip := a.previewLANIP(); ip != "" {
		t.Fatalf("previewLANIP = %q, want no LAN preview listeners", ip)
	}
}

// TestLoopbackReachJoinsNoTailnet: a loopback-confined instance with
// Tailscale turned on starts no node, so nothing reaches a coordination
// server, STUN or DERP, and the tailnet status says why.
func TestLoopbackReachJoinsNoTailnet(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a := &App{settings: settings.NewService(t.TempDir())}
	a.netReach = network.LoopbackReach()
	// No state directory: a regression past the guard fails on that rather
	// than starting a node that would contact the coordination server.
	if _, err := a.settings.SetNetwork(settings.NetworkSettings{TailnetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	a.reconcileTailnet()
	a.tailnet.mu.Lock()
	node, lastErr := a.tailnet.node, a.tailnet.lastErr
	a.tailnet.mu.Unlock()
	if node != nil {
		t.Fatal("a loopback reach started a tailnet node")
	}
	if lastErr != errTailnetIsolated.Error() {
		t.Fatalf("tailnet failure %q, want %q", lastErr, errTailnetIsolated)
	}
}

// TestLoopbackReachOrdersNoCertificate: a loopback-confined instance with a
// canonical domain and a DNS hook orders nothing from the certificate
// authority, and the TLS status says why.
func TestLoopbackReachOrdersNoCertificate(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a := &App{settings: settings.NewService(t.TempDir())}
	a.netReach = network.LoopbackReach()
	// No certificate directory: a regression past the guard fails on that
	// rather than placing an order.
	if _, err := a.settings.SetNetwork(settings.NetworkSettings{CanonicalDomain: "ao.example.test", ACMEDNSHook: []string{"/bin/false"}}); err != nil {
		t.Fatal(err)
	}
	a.reconcileDomainCertificate(context.Background())
	a.domainCert.mu.Lock()
	lastErr := a.domainCert.lastErr
	a.domainCert.mu.Unlock()
	if lastErr != errDomainCertIsolated.Error() {
		t.Fatalf("certificate failure %q, want %q", lastErr, errDomainCertIsolated)
	}
}

// TestLoopbackReachBuildsNoPushSender: a loopback-confined instance installs
// no real push sender, pasted or stored, so no send reaches Google.
func TestLoopbackReachBuildsNoPushSender(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	a, _ := pushApp(t)
	a.installPushSender(nil, "", "")
	// Stored while unconfined, as a soak data root could hold it.
	if err := a.SetPushSenderCredential(serviceAccountJSON()); err != nil {
		t.Fatal(err)
	}
	a.installPushSender(nil, "", "")
	a.netReach = network.LoopbackReach()
	if err := a.SetPushSenderCredential(serviceAccountJSON()); !errors.Is(err, errPushIsolated) {
		t.Fatalf("SetPushSenderCredential error = %v, want %v", err, errPushIsolated)
	}
	a.loadPushSender()
	if a.currentPushSender() != nil {
		t.Fatal("a loopback reach installed a push sender")
	}
}
