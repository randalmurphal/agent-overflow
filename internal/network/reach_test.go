package network

import (
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/computerroute"
)

// TestLoopbackReachPublishesOnlyLoopback: an isolated instance on a shared
// host network pairs, advertises routes and shares URLs on 127.0.0.1 even
// when the machine has a LAN address, and never reads the interfaces.
func TestLoopbackReachPublishesOnlyLoopback(t *testing.T) {
	previousInterfaces, previousAddrs := Interfaces, InterfaceAddrs
	t.Cleanup(func() { Interfaces, InterfaceAddrs = previousInterfaces, previousAddrs })
	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagRunning | net.FlagMulticast}}, nil
	}
	InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		t.Error("a loopback Reach discovered the machine's LAN address")
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.55"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	reach := LoopbackReach()
	srv := shareURLServer(t)
	if err := srv.Rebind(reach.BindHost(true)+":0", nil); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(srv.Addr())
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("LAN bind listens on %q, want 127.0.0.1", srv.Addr())
	}
	pin := "sha256:" + strings.Repeat("b", 64)
	s := Settings{BindAll: true, TLS: TLSStatus{SelfSignedFingerprint: pin}}
	loopback := net.JoinHostPort("127.0.0.1", port)

	if address, err := PairingAddressOnNetwork(srv, s, reach, "lan"); err != nil || address != "https://"+loopback {
		t.Fatalf("pairing address = %q, %v; want https://%s", address, err, loopback)
	}
	if link, linkPin, err := PairingURLOnNetwork(srv, s, reach, "lan"); err != nil || !strings.HasPrefix(link, "http://"+loopback+"/?") || linkPin != pin {
		t.Fatalf("LAN invitation = %q, %q, %v; want http://%s with the listener pin", link, linkPin, err, loopback)
	}
	if link, _ := PairingURL(srv, s, reach); !strings.HasPrefix(link, "http://"+loopback+"/?") {
		t.Fatalf("pairing URL = %q, want http://%s", link, loopback)
	}
	if got := FromServer(srv, s, reach).URL; !strings.HasPrefix(got, "http://"+loopback+"/?") {
		t.Fatalf("share URL = %q, want http://%s", got, loopback)
	}
	want := []computerroute.Route{{Endpoint: "https://" + loopback, CertFingerprint: pin}}
	if got := ComputerRoutes(srv, s, reach); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}

	// The toggle still means something: with LAN access off nothing is
	// published as a LAN address.
	s.BindAll = false
	if got := ComputerRoutes(srv, s, reach); len(got) != 0 {
		t.Fatalf("LAN access off advertised %+v", got)
	}
	if _, err := PairingAddressOnNetwork(srv, s, reach, "lan"); err == nil {
		t.Fatal("LAN access off still offered a LAN pairing address")
	}
}

// TestHostReachNeverPublishesLoopback: on the host network a loopback
// listener or a loopback discovery is never a LAN address for another
// computer.
func TestHostReachNeverPublishesLoopback(t *testing.T) {
	srv := shareURLServer(t)
	s := Settings{BindAll: true, TLS: TLSStatus{SelfSignedFingerprint: "sha256:" + strings.Repeat("c", 64)}}
	reach := discovered("127.0.0.1")
	if got := ComputerRoutes(srv, s, reach); len(got) != 0 {
		t.Fatalf("host Reach advertised its loopback listener: %+v", got)
	}
	if address, err := PairingAddressOnNetwork(srv, s, reach, "lan"); err == nil {
		t.Fatalf("host Reach offered loopback %q as a LAN pairing address", address)
	}
	if link, _, err := PairingURLOnNetwork(srv, s, reach, "lan"); err == nil {
		t.Fatalf("host Reach offered loopback %q as a LAN invitation", link)
	}
}

// TestOriginPatternsNameEachHostOnce: a loopback Reach's LAN address is
// already one of the loopback entries.
func TestOriginPatternsNameEachHostOnce(t *testing.T) {
	got := OriginPatterns(true, "127.0.0.1", "", 4242)
	want := []string{"http://127.0.0.1:4242", "https://127.0.0.1:4242", "http://localhost:4242", "https://localhost:4242"}
	if !slices.Equal(got, want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
}
