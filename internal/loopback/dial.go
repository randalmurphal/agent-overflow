package loopback

import (
	"context"
	"net"
	"time"
)

// Dialer returns a net/http DialContext that IGNORES the address it is
// handed and dials this machine, 127.0.0.1 first and ::1 second, on the
// port from that address.
//
// It is for one shape of caller: a client whose request URL this process
// built with the literal host `localhost`, talking to something on this
// machine's own port. The devscan probe and the dev-server preview proxy
// are both exactly that, and both had their own copy of these ten lines.
//
// Discarding the host is the whole point, and it buys two things:
//
//   - The target is deterministic. `localhost` is a name, and a name is
//     resolved by configuration this process does not own — /etc/hosts,
//     a search domain, a resolver. Neither the probe's verdict nor the
//     proxy's upstream may be steerable by that.
//   - A dev server bound to only one address family is still reached.
//     Plenty bind ::1 only, or 127.0.0.1 only; one resolver answer picks
//     one of them and the other looks dead.
//
// The timeout bounds each attempt, so a host where 127.0.0.1 blackholes
// costs at most twice it.
//
// Deliberately NOT used by internal/devserverprobe, which asks a
// different question: it takes a URL from outside, validates that the URL
// is loopback at all, and preserves the literal the URL named rather than
// discarding it. Folding the two together would mean either dropping that
// validation or bolting URL parsing onto a dialer.
func Dialer(timeout time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		if err == nil {
			return conn, nil
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
	}
}
