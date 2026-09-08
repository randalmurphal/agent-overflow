package network

import (
	"net"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/computerroute"
)

func TestForwardedWindowsIngressDrivesPairingAndRouteTrust(t *testing.T) {
	previousInterfaces, previousAddrs := Interfaces, InterfaceAddrs
	t.Cleanup(func() { Interfaces, InterfaceAddrs = previousInterfaces, previousAddrs })
	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "wsl", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	const wsl = "172.20.0.2"
	InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(wsl), Mask: net.CIDRMask(16, 32)}}, nil
	}
	srv := shareURLServer(t)
	if err := srv.Rebind("0.0.0.0:0", nil); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	windows := net.JoinHostPort("192.168.1.55", port)
	second := net.JoinHostPort("10.1.2.3", port)
	pin := "sha256:" + strings.Repeat("a", 64)
	tailnet := computerroute.Route{Endpoint: "https://desktop.test.ts.net"}
	for _, ready := range []bool{true, false} {
		name := "forwarded listener ready"
		if !ready {
			name = "forwarded listener unavailable"
		}
		t.Run(name, func(t *testing.T) {
			s := Settings{BindAll: true, TLS: TLSStatus{SelfSignedFingerprint: pin}, Tailnet: TailnetStatus{Running: true, HTTPS: true, DNSName: "desktop.test.ts.net"}, LAN: &LANStatus{}}
			wantRoutes := []computerroute.Route{tailnet}
			if ready {
				s.LAN.Addresses = []string{"https://" + windows, "https://" + second}
				wantRoutes = append(wantRoutes, computerroute.Route{Endpoint: "https://" + windows, CertFingerprint: pin}, computerroute.Route{Endpoint: "https://" + second, CertFingerprint: pin})
			}
			address, addressErr := PairingAddressOnNetwork(srv, s, "lan")
			invite, invitePin, inviteErr := PairingURLOnNetwork(srv, s, "lan")
			if ready {
				if addressErr != nil || address != "https://"+windows {
					t.Fatal("native setup did not use Windows ingress", address, addressErr)
				}
				u, err := url.Parse(invite)
				if inviteErr != nil || err != nil || u.Scheme != "http" || u.Host != windows || invitePin != pin {
					t.Fatal("QR invitation did not use Windows authority and backend pin", inviteErr)
				}
			} else if addressErr == nil || inviteErr == nil || address != "" || invite != "" || invitePin != "" {
				t.Fatal("unavailable Windows ingress fell back to inaccessible WSL address")
			}
			if got := ComputerRoutes(srv, s, wsl); !reflect.DeepEqual(got, wantRoutes) {
				t.Fatalf("wrong route authorities/trust: got %+v want %+v", got, wantRoutes)
			}
			// A native LAN failure must not hide or change the independent
			// tailnet listener, whose certificate uses WebPKI rather than a pin.
			address, err := PairingAddressOnNetwork(srv, s, "tailnet")
			if err != nil || address != tailnet.Endpoint {
				t.Fatal("tailnet setup changed with Windows ingress", err)
			}
			invite, invitePin, err = PairingURLOnNetwork(srv, s, "tailnet")
			u, parseErr := url.Parse(invite)
			if err != nil || parseErr != nil || u.Scheme+"://"+u.Host != tailnet.Endpoint || invitePin != "" {
				t.Fatal("tailnet invitation gained the LAN pin or changed authority", err)
			}
		})
	}
}
