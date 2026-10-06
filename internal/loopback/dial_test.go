package loopback

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

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

// localhost is never resolved, and a host that is neither localhost nor a
// loopback literal is refused rather than rewritten.
func TestDialerDialsOnlyThisMachine(t *testing.T) {
	_, port := listenOn(t, "127.0.0.1")

	dial := Dialer(2 * time.Second)
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	// A cancelled context: a regression that dialed would fail with the
	// context's error, never with the refusal, and sends nothing.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, host := range []string{"not-a-real-host.invalid", "192.0.2.1", "::", "fe80::1%lo0"} {
		_, err := dial(cancelled, "tcp", net.JoinHostPort(host, port))
		if err == nil || !strings.Contains(err.Error(), "refusing to dial") {
			t.Fatalf("dial %s = %v, want a refusal", host, err)
		}
	}
}

// A literal loopback address reaches that socket, not another process on
// the other family at the same port.
func TestDialerHonoursALoopbackLiteral(t *testing.T) {
	v6, port := listenOn(t, "::1")
	v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("127.0.0.1:%s is taken by another process: %v", port, err)
	}
	defer v4.Close()
	for _, want := range []net.Listener{v4, v6} {
		host, _, _ := net.SplitHostPort(want.Addr().String())
		conn, err := Dialer(2*time.Second)(context.Background(), "tcp", net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("dial %s: %v", host, err)
		}
		got, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		_ = conn.Close()
		if got != host {
			t.Fatalf("dialed %s, want %s", got, host)
		}
	}
}

func TestAuthority(t *testing.T) {
	for _, tc := range []struct {
		addr netip.Addr
		want string
	}{
		{netip.Addr{}, "localhost:5173"},
		{netip.MustParseAddr("127.0.0.1"), "127.0.0.1:5173"},
		{netip.MustParseAddr("::1"), "[::1]:5173"},
		{netip.MustParseAddr("::ffff:127.0.0.1"), "127.0.0.1:5173"},
	} {
		if got := Authority(tc.addr, 5173); got != tc.want {
			t.Fatalf("Authority(%v) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// A dev server bound to 127.0.0.1 only is still reached, which is the
// reason both literals are tried rather than one being picked.
func TestDialerFallsBackToTheOtherAddressFamily(t *testing.T) {
	_, port := listenOn(t, "127.0.0.1")

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

// A server on both families is reached on ::1, which stays in the Linux
// kernel under WSL virtioproxy where IPv4 loopback is relayed.
func TestDialerPrefersIPv6Loopback(t *testing.T) {
	_, port := listenOn(t, "::1")
	ipv4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("127.0.0.1:%s is taken by another process: %v", port, err)
	}
	defer ipv4.Close()
	conn, err := Dialer(2*time.Second)(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if host, _, _ := net.SplitHostPort(conn.RemoteAddr().String()); host != "::1" {
		t.Fatalf("dialed %s, want ::1 first", conn.RemoteAddr())
	}
}
