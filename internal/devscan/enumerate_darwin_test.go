//go:build darwin

package devscan

import (
	"net/netip"
	"testing"
)

// lsof prints both wildcards as `*:port`; the `t` field of the same file
// record is what says which family a wildcard is.
func TestParseLSOFCarriesTheAddressThatReachesEachSocket(t *testing.T) {
	output := "p100\ncnode\nf10\ntIPv4\nn*:3000\nf11\ntIPv6\nn*:3001\nf12\ntIPv4\nn127.0.0.1:3002\nf13\ntIPv6\nn[::1]:3003\nf14\ntIPv4\nn192.168.1.2:3004\n"
	want := map[int]netip.Addr{
		3000: netip.MustParseAddr("127.0.0.1"),
		3001: netip.IPv6Loopback(),
		3002: netip.MustParseAddr("127.0.0.1"),
		3003: netip.IPv6Loopback(),
	}
	got := parseLSOF(output)
	if len(got) != len(want) {
		t.Fatalf("listeners = %+v, want ports %v", got, want)
	}
	for _, l := range got {
		if l.PID != 100 || l.Comm != "node" || l.Addr != want[l.Port] {
			t.Errorf("listener = %+v, want pid 100, node, addr %v", l, want[l.Port])
		}
	}
}
