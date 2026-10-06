package netisolate

import (
	"net"
	"testing"
)

// TestContainedInterfacesAcceptOnlyTheNamespaceShape: an isolated boot uses
// its LAN as a LAN only when every non-loopback interface is the
// namespace's own. Any other interface, or another address on lan0, could
// reach a real network.
func TestContainedInterfacesAcceptOnlyTheNamespaceShape(t *testing.T) {
	lanAddress := net.IPv4(10, 203, 0, 2)
	lo := net.Interface{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback}
	lan := net.Interface{Index: 2, Name: "lan0", Flags: net.FlagUp | net.FlagMulticast}
	wifi := net.Interface{Index: 3, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}
	linkLocal := net.Interface{Index: 4, Name: "bridge0", Flags: net.FlagUp | net.FlagMulticast}
	ipNet := func(ip string, bits int) net.Addr {
		return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(bits, len(net.ParseIP(ip).To16())*8)}
	}
	addrs := map[string][]net.Addr{
		"lo":      {ipNet("127.0.0.1", 8), ipNet("::1", 128)},
		"lan0":    {ipNet("10.203.0.2", 24), ipNet("fe80::1", 64)},
		"en0":     {ipNet("192.168.1.55", 24)},
		"bridge0": {ipNet("fe80::2", 64)},
	}
	lookup := func(iface net.Interface) ([]net.Addr, error) { return addrs[iface.Name], nil }
	for _, tc := range []struct {
		name   string
		ifaces []net.Interface
		lan0   []net.Addr
		want   bool
	}{
		{"namespace", []net.Interface{lo, lan}, nil, true},
		{"loopback only", []net.Interface{lo}, nil, false},
		{"host interface beside lan0", []net.Interface{lo, lan, wifi}, nil, false},
		{"host interface alone", []net.Interface{lo, wifi}, nil, false},
		{"link-local host interface beside lan0", []net.Interface{lo, lan, linkLocal}, nil, false},
		{"lan0 with an IPv4 link-local address", []net.Interface{lo, lan}, []net.Addr{ipNet("10.203.0.2", 24), ipNet("169.254.1.1", 16)}, false},
		{"lan0 with another address", []net.Interface{lo, lan}, []net.Addr{ipNet("10.203.0.2", 24), ipNet("192.168.1.55", 24)}, false},
		{"lan0 with a global IPv6 address", []net.Interface{lo, lan}, []net.Addr{ipNet("10.203.0.2", 24), ipNet("2001:db8::1", 64)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lan0 != nil {
				saved := addrs["lan0"]
				addrs["lan0"] = tc.lan0
				defer func() { addrs["lan0"] = saved }()
			}
			if got := containedInterfaces(tc.ifaces, lookup, "lan0", lanAddress); got != tc.want {
				t.Fatalf("contained = %v, want %v", got, tc.want)
			}
		})
	}
}
