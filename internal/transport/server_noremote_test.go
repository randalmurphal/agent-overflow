//go:build noremote

package transport

import (
	"errors"
	"net"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// A build without remote access refuses a non-loopback bind at boot.
func TestNoremoteBootRefusesAWildcardBind(t *testing.T) {
	srv, err := New(Config{Dispatcher: NewDispatcher(), EventBus: NewEventBus(4), Token: "t", BindAddr: "0.0.0.0"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	err = srv.Start()
	if err == nil {
		t.Fatalf("Start bound %s in a build without remote access", srv.Addr())
	}
	if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
		t.Fatalf("Start error = %v, want the remote-access refusal", err)
	}
}

// A rebind to every interface is refused and the loopback listener keeps
// serving.
func TestNoremoteRebindRefusesAWildcardBindAndKeepsServing(t *testing.T) {
	f := newServerFixture(t)
	before := f.srv.Addr()
	err := f.srv.Rebind("0.0.0.0:0", nil)
	if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
		t.Fatalf("Rebind error = %v, want the remote-access refusal", err)
	}
	if after := f.srv.Addr(); after != before {
		t.Fatalf("listener moved from %s to %s after a refused rebind", before, after)
	}
	resp := getBootstrap(t, before)
	_ = resp.Body.Close()
}

// rebound reports addr as its bound address whatever it actually bound,
// the way WSL virtioproxy has turned a fixed 127.0.0.1 bind into one on
// every interface.
type rebound struct {
	net.Listener
	addr *net.TCPAddr
}

func (r rebound) Addr() net.Addr { return r.addr }

func TestNoremoteRefusesAListenerTheKernelDidNotBindToLoopback(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := rebound{inner, &net.TCPAddr{IP: net.IPv4zero, Port: 40561}}
	if err := requireLoopbackBound("127.0.0.1:6363", ln); !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
		t.Fatalf("requireLoopbackBound = %v, want the remote-access refusal", err)
	}
	if _, err := inner.Accept(); err == nil {
		t.Fatal("the refused listener was left open")
	}
	loop, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	if err := requireLoopbackBound("127.0.0.1:0", loop); err != nil {
		t.Fatalf("a loopback listener was refused: %v", err)
	}
}

// An auxiliary listener is served only on loopback, which is how local
// clients beside the providers reach this backend (the ::1 listener in
// internal/app). LAN previews are refused.
func TestNoremoteServesAuxiliaryListenersOnlyOnLoopback(t *testing.T) {
	f := newServerFixture(t)
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		aux, err := f.srv.ServeAuxiliary(ln, nil)
		if err != nil {
			t.Fatalf("ServeAuxiliary(%s) = %v, want it served", ln.Addr(), err)
		}
		resp := getBootstrap(t, ln.Addr().String())
		_ = resp.Body.Close()
		if err := aux.Close(); err != nil {
			t.Fatal(err)
		}
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	wildcard := rebound{inner, &net.TCPAddr{IP: net.IPv4zero, Port: 40561}}
	if _, err := f.srv.ServeAuxiliary(wildcard, nil); !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
		t.Fatalf("ServeAuxiliary(%s) = %v, want the remote-access refusal", wildcard.Addr(), err)
	}
	source := &PreviewLANSource{LANIP: func() string { return "127.0.0.1" }}
	if _, err := source.ListenPreview(0); !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
		t.Fatalf("ListenPreview = %v, want the remote-access refusal", err)
	}
}
