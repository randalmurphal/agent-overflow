package loopback

import (
	"context"
	"net"
	"testing"
	"time"
)

// The dialer's whole job is to IGNORE the host it is handed. These bind
// loopback on an ephemeral port and dial with a host that would go
// somewhere else entirely if it were honoured.

func listenOn(t *testing.T, host string) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("this machine cannot listen on %s: %v", host, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", ln.Addr().String(), err)
	}
	return ln, port
}

func TestDialerIgnoresTheHostAndDialsThisMachine(t *testing.T) {
	_, port := listenOn(t, "127.0.0.1")

	dial := Dialer(2 * time.Second)
	// A name that must never be resolved. If the dialer asked a resolver
	// this would leave the machine or fail; it does neither, because the
	// host is discarded and only the port is used.
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	conn, err = dial(context.Background(), "tcp", net.JoinHostPort("not-a-real-host.invalid", port))
	if err != nil {
		t.Fatalf("dial with a host that should have been discarded: %v", err)
	}
	_ = conn.Close()
}

// A dev server bound to ::1 only is still reached, which is the reason
// both literals are tried rather than one being picked.
func TestDialerFallsBackToTheOtherAddressFamily(t *testing.T) {
	_, port := listenOn(t, "::1")

	conn, err := Dialer(2*time.Second)(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial an IPv6-only listener: %v", err)
	}
	_ = conn.Close()
}

// An address with no port is the caller's bug and is refused, not
// guessed at.
func TestDialerRefusesAnAddressWithNoPort(t *testing.T) {
	if _, err := Dialer(time.Second)(context.Background(), "tcp", "localhost"); err == nil {
		t.Fatal("an address with no port was accepted")
	}
}

// A cancelled context is not spent retrying the second family.
func TestDialerHonoursACancelledContext(t *testing.T) {
	_, port := listenOn(t, "127.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dialer(time.Second)(ctx, "tcp", net.JoinHostPort("localhost", port)); err == nil {
		t.Fatal("a cancelled dial connected anyway")
	}
}
