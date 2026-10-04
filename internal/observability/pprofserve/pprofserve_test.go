package pprofserve

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestDisabledWhenUnsetOrOff(t *testing.T) {
	for _, v := range []string{"", "0", "false", "FALSE"} {
		t.Run(fmt.Sprintf("value=%q", v), func(t *testing.T) {
			t.Setenv(EnvVar, v)
			addr, stop, err := StartIfEnabled()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr != "" {
				stop()
				t.Fatalf("expected disabled, got listener on %s", addr)
			}
			stop() // must be safe to call when disabled
		})
	}
}

func TestRefusesNonLoopbackAndMalformed(t *testing.T) {
	for _, v := range []string{"0.0.0.0:6363", "192.168.1.4:6363", "example.com:6363", "not-an-addr"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvVar, v)
			addr, stop, err := StartIfEnabled()
			if err == nil {
				stop()
				t.Fatalf("expected refusal for %q, got listener on %s", v, addr)
			}
		})
	}
}

func TestServesProfilesOnLoopback(t *testing.T) {
	// Port 0 avoids collisions with a concurrently running backend that
	// has the default port bound.
	t.Setenv(EnvVar, "127.0.0.1:0")
	addr, stop, err := StartIfEnabled()
	if err != nil {
		t.Fatalf("StartIfEnabled: %v", err)
	}
	defer stop()
	if addr == "" {
		t.Fatal("expected a bound address")
	}

	resp, err := http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatalf("GET heap profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heap profile status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "heap profile") {
		t.Fatalf("body does not look like a heap profile: %.120s", body)
	}
}

// A bare enable binds IPv6 loopback: WSL virtioproxy handles fixed IPv4
// loopback binds itself and has left one listening on every interface.
func TestDefaultAddrIsIPv6Loopback(t *testing.T) {
	host, port, err := net.SplitHostPort(DefaultAddr)
	if err != nil || host != "::1" || port != "6363" {
		t.Fatalf("DefaultAddr = %q, want [::1]:6363", DefaultAddr)
	}
}

// rebound reports addr as its bound address whatever it actually bound,
// the way WSL virtioproxy turned a 127.0.0.1:6363 bind into 0.0.0.0:40561.
type rebound struct {
	net.Listener
	addr *net.TCPAddr
}

func (r rebound) Addr() net.Addr { return r.addr }

// The requested address passing the loopback check is not enough; the
// listener the kernel handed back must be loopback too, or nothing serves
// and the listener is closed.
func TestRefusesAListenerThatDidNotBindLoopback(t *testing.T) {
	var inner net.Listener
	listen := func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, "[::1]:0")
		inner = ln
		return rebound{ln, &net.TCPAddr{IP: net.IPv4zero, Port: 40561}}, err
	}
	addr, stop, err := start("127.0.0.1:6363", listen)
	if err == nil {
		stop()
		t.Fatalf("served on %s, a listener that reported 0.0.0.0:40561", addr)
	}
	if !strings.Contains(err.Error(), "0.0.0.0:40561") {
		t.Fatalf("error %q does not name the bound address", err)
	}
	if conn, dialErr := net.Dial("tcp", inner.Addr().String()); dialErr == nil {
		conn.Close()
		t.Fatal("the refused listener is still accepting")
	}
}

func TestBareEnableUsesDefaultAddr(t *testing.T) {
	t.Setenv(EnvVar, "1")
	addr, stop, err := StartIfEnabled()
	if err != nil {
		// The default port may legitimately be taken (e.g. a live
		// backend running with pprof enabled). That is a bind error,
		// not a parse/validation error — accept it.
		if !strings.Contains(err.Error(), "listen") {
			t.Fatalf("unexpected error kind: %v", err)
		}
		return
	}
	defer stop()
	if addr != DefaultAddr {
		t.Fatalf("addr = %q, want %q", addr, DefaultAddr)
	}
}
