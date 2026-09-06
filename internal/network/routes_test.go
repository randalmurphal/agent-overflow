package network

import (
	"crypto/tls"
	"net"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/computerroute"
)

func TestComputerRoutesFollowActualListenersAndTrust(t *testing.T) {
	srv := shareURLServer(t)
	s := Settings{BindAll: true, CanonicalDomain: "backend.example", TLS: TLSStatus{SelfSignedFingerprint: "sha256:" + strings.Repeat("a", 64)},
		Tailnet: TailnetStatus{Running: true, HTTPS: true, DNSName: "gpu.test.ts.net"}}
	tailnet := computerroute.Route{Endpoint: "https://gpu.test.ts.net"}
	// The setting preceding the actual rebind cannot advertise loopback.
	if got := ComputerRoutes(srv, s, "192.168.1.2"); !reflect.DeepEqual(got, []computerroute.Route{tailnet}) {
		t.Fatalf("loopback advertised: %v", got)
	}
	if err := srv.Rebind("0.0.0.0:0", nil); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	lan := computerroute.Route{Endpoint: "https://" + net.JoinHostPort("192.168.1.2", port), CertFingerprint: s.TLS.SelfSignedFingerprint}
	if got := ComputerRoutes(srv, s, "192.168.1.2"); !reflect.DeepEqual(got, []computerroute.Route{tailnet, lan}) {
		t.Fatalf("wrong listener/pin: %v", got)
	}
	srv.Certificates().SetDomain(s.CanonicalDomain, &tls.Certificate{})
	domain := computerroute.Route{Endpoint: "https://" + net.JoinHostPort(s.CanonicalDomain, port)}
	if got := ComputerRoutes(srv, s, "192.168.1.2"); !reflect.DeepEqual(got, []computerroute.Route{tailnet, lan, domain}) {
		t.Fatalf("domain trust: %v", got)
	}
	s.BindAll = false
	s.Tailnet.HTTPS = false
	if got := ComputerRoutes(srv, s, "192.168.1.2"); len(got) != 0 {
		t.Fatalf("disabled LAN or cleartext tailnet advertised: %v", got)
	}
	s.BindAll = true
	s.CanonicalDomain = "unloaded.example"
	for _, ip := range []string{"", "127.0.0.1", "0.0.0.0", "8.8.8.8", "not-an-ip"} {
		if got := ComputerRoutes(srv, s, ip); len(got) != 0 {
			t.Fatalf("bad discovery %q advertised: %v", ip, got)
		}
	}
	s.Tailnet.HTTPS = true
	s.Tailnet.Running = false
	s.TLS.SelfSignedFingerprint = ""
	if got := ComputerRoutes(srv, s, "192.168.1.2"); len(got) != 0 {
		t.Fatalf("no TLS or stopped tailnet advertised: %v", got)
	}
}

func TestComputerRouteAdvertisementsNeverSpendPageTickets(t *testing.T) {
	srv := shareURLServer(t)
	ticket, err := srv.MintPageTicket()
	if err != nil {
		t.Fatal(err)
	}
	s := Settings{Tailnet: TailnetStatus{Running: true, HTTPS: true, DNSName: "gpu.test.ts.net"}}
	for range 100 {
		ComputerRoutes(srv, s, "")
	}
	if !ticketOutstanding(t, srv, ticket) {
		t.Fatal("route advertisements evicted a pairing page ticket")
	}
}
