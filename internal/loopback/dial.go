package loopback

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Dialer returns a net/http DialContext that only ever dials this machine.
//
// A literal loopback address (127.0.0.1, ::1) is dialed as given. The
// caller named one socket, and a server on the other family at the same
// port is a different process: dialing it would send the request, and any
// credential in it, to the wrong server. The devscan probe and the
// dev-server preview proxy pass the address discovery found the server
// bound to; the transfer client passes the one in its offer.
//
// The name `localhost` is never resolved. It is the spelling for "this
// machine, address unknown" (a hand-named port nothing is listening on
// yet, an offer that named the host rather than an address), and it dials
// ::1 first and 127.0.0.1 second on the given port:
//
//   - The target is deterministic. A name is resolved by configuration
//     this process does not own (/etc/hosts, a search domain, a
//     resolver), and neither the probe's verdict nor the proxy's upstream
//     may be steerable by that.
//   - A server bound to only one family is still reached.
//
// ::1 goes first because WSL's virtioproxy (consomme) networking relays
// in-distro IPv4 loopback through Windows, which is slower and refuses
// bursts (see EphemeralIPv6); one on 127.0.0.1 only costs an immediate
// refusal on ::1 first. The timeout bounds each attempt, so a host where
// one family blackholes costs at most twice it.
//
// Any other host is refused: this dialer never leaves the machine.
func Dialer(timeout time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if !addr.Unmap().IsLoopback() || addr.Zone() != "" {
				return nil, fmt.Errorf("loopback: refusing to dial %s: not a loopback address", address)
			}
			return dialer.DialContext(ctx, network, address)
		}
		if !strings.EqualFold(host, "localhost") {
			return nil, fmt.Errorf("loopback: refusing to dial %s: only localhost or a loopback address", address)
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
		if err == nil {
			return conn, nil
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
}

// Authority returns the URL host that makes Dialer reach addr on port:
// the bracketed literal for a discovered loopback address, or
// `localhost:port` when addr is the zero Addr because nothing was
// discovered. A request that must still carry `localhost` in its Host
// header sets that separately.
func Authority(addr netip.Addr, port int) string {
	host := "localhost"
	if addr.IsValid() {
		host = addr.Unmap().String()
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}
