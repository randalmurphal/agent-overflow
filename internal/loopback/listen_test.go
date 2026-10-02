package loopback

import (
	"net"
	"net/netip"
	"testing"
)

func TestListenBindsIPv6Loopback(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if addr.Addr() != netip.IPv6Loopback() || addr.Port() == 0 {
		t.Fatalf("Listen bound %s, want [::1] on an ephemeral port", addr)
	}
	accepted := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- ""
			return
		}
		accepted <- conn.RemoteAddr().String()
		conn.Close()
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if peer := <-accepted; !PeerAddress(peer) {
		t.Fatalf("peer %q is not loopback; clients of Listen must keep loopback trust", peer)
	}
}
