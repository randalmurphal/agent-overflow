package transport

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/buildvariant/remotetest"
)

// reusePortBind binds addr the way a second same-uid process sharing the
// port would, with SO_REUSEPORT set, and releases it.
func reusePortBind(addr string) error {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		return setReusePort(raw, 1)
	}}
	ln, err := config.Listen(context.Background(), "tcp4", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

func fixturePort(t *testing.T, f *serverFixture) string {
	t.Helper()
	_, port, err := net.SplitHostPort(f.srv.Addr())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return port
}

// A LAN enable whose wildcard bind fails leaves the loopback socket as
// exclusive as it was.
func TestRebind_FailedLANEnableKeepsThePortExclusive(t *testing.T) {
	remotetest.Require(t)
	f := newServerFixture(t)
	port := fixturePort(t, f)
	// Another loopback address on the port, without SO_REUSEPORT, makes
	// the wildcard bind fail.
	blocker, err := net.Listen("tcp4", "127.0.0.2:"+port)
	if err != nil {
		t.Skipf("cannot hold 127.0.0.2:%s: %v", port, err)
	}
	defer blocker.Close()

	if err := f.srv.Rebind("0.0.0.0:"+port, nil); err == nil {
		t.Fatal("the wildcard bound beside a socket that did not share the port")
	}
	if err := reusePortBind("127.0.0.1:" + port); !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("second binder after the failed enable = %v, want EADDRINUSE", err)
	}
}
