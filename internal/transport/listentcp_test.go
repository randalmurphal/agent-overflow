package transport

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

type recordingListen struct {
	addrs []string
	fail  map[int]error // call index -> error
}

func (r *recordingListen) listen(network, addr string) (net.Listener, error) {
	i := len(r.addrs)
	r.addrs = append(r.addrs, addr)
	if err := r.fail[i]; err != nil {
		return nil, err
	}
	return net.Listen(network, addr)
}

func addrInUseError() error {
	return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
}

// Inside WSL a port-0 request ends in an explicit bind of the probed port,
// which is the bind WSL 2.7.x virtioproxy tracks and forwards to Windows.
func TestListenTCPBindsAnExplicitPortInsideWSL(t *testing.T) {
	rec := &recordingListen{}
	ln, err := listenTCP("tcp4", "127.0.0.1:0", true, rec.listen)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if want := []string{"127.0.0.1:0", "127.0.0.1:" + port}; len(rec.addrs) != 2 || rec.addrs[0] != want[0] || rec.addrs[1] != want[1] {
		t.Fatalf("binds = %v, want %v", rec.addrs, want)
	}
}

func TestListenTCPBindsDirectlyOutsideWSLOrForAnExplicitPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		wsl  bool
	}{
		{"port 0 outside WSL", "127.0.0.1:0", false},
		{"explicit port inside WSL", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := tc.addr
			if addr == "" {
				probe, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr = probe.Addr().String()
				probe.Close()
			}
			rec := &recordingListen{}
			ln, err := listenTCP("tcp4", addr, tc.wsl, rec.listen)
			if err != nil {
				t.Fatal(err)
			}
			ln.Close()
			if len(rec.addrs) != 1 || rec.addrs[0] != addr {
				t.Fatalf("binds = %v, want one bind of %s", rec.addrs, addr)
			}
		})
	}
}

// A port taken between the probe and the explicit bind is probed again; any
// other bind failure is the caller's to see.
func TestListenTCPReprobesOnlyAPortTakenInBetween(t *testing.T) {
	rec := &recordingListen{fail: map[int]error{1: addrInUseError()}}
	ln, err := listenTCP("tcp4", "127.0.0.1:0", true, rec.listen)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if len(rec.addrs) != 4 {
		t.Fatalf("binds = %v, want probe, taken, probe, bind", rec.addrs)
	}

	denied := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)}
	rec = &recordingListen{fail: map[int]error{1: denied}}
	if _, err := listenTCP("tcp4", "127.0.0.1:0", true, rec.listen); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want the EACCES from the explicit bind", err)
	}
	if len(rec.addrs) != 2 {
		t.Fatalf("binds = %v, want no re-probe after EACCES", rec.addrs)
	}

	all := map[int]error{}
	for i := 1; i < 2*explicitPortAttempts; i += 2 {
		all[i] = addrInUseError()
	}
	rec = &recordingListen{fail: all}
	if _, err := listenTCP("tcp4", "127.0.0.1:0", true, rec.listen); err == nil {
		t.Fatal("bound after every explicit bind was taken")
	}
	if len(rec.addrs) != 2*explicitPortAttempts {
		t.Fatalf("binds = %d, want %d bounded attempts", len(rec.addrs), 2*explicitPortAttempts)
	}
}

// widenedListen binds as asked and reports the bound address as the
// wildcard, the way WSL virtioproxy has widened a fixed 127.0.0.1 bind.
type widenedListen struct{ inner []net.Listener }

func (w *widenedListen) listen(network, addr string) (net.Listener, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	w.inner = append(w.inner, ln)
	return widened{ln}, nil
}

type widened struct{ net.Listener }

func (w widened) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4zero, Port: w.Listener.Addr().(*net.TCPAddr).Port}
}

// A loopback request the kernel did not bind to loopback is closed and
// refused in every build, on every path through ListenTCP (the server's
// own binds and clientmode's); a wildcard request is not second-guessed.
func TestListenTCPRefusesALoopbackBindThatCameBackWidened(t *testing.T) {
	for _, wsl := range []bool{false, true} {
		w := &widenedListen{}
		if ln, err := listenTCP("tcp4", "127.0.0.1:0", wsl, w.listen); err == nil {
			_ = ln.Close()
			t.Fatalf("wsl=%v: a widened loopback bind was accepted", wsl)
		}
		last := w.inner[len(w.inner)-1]
		if _, err := last.Accept(); err == nil {
			t.Fatalf("wsl=%v: the refused listener was left open", wsl)
		}
	}
	// The wildcard request is served from a loopback socket that reports
	// the wildcard, so the test binds nothing off loopback.
	w := &widenedListen{}
	ln, err := listenTCP("tcp4", "0.0.0.0:0", false, func(network, _ string) (net.Listener, error) {
		return w.listen(network, "127.0.0.1:0")
	})
	if err != nil {
		t.Fatalf("a wildcard request was refused: %v", err)
	}
	_ = ln.Close()
}
