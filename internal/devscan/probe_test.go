package devscan

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/loopback/loopbacktest"
)

// Every server here is httptest's own: loopback, an ephemeral port, and
// shut down with the test. Nothing in this file reaches the network or
// spawns a process.

func loopbackPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", srv.Listener.Addr(), err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return number
}

// pageOf keeps the rows below reading as the yes/no question they are
// asking. The scheme has its own tests.
func pageOf(p *prober, ctx context.Context, port, pid int) bool {
	_, ok := p.pageScheme(ctx, port, pid, netip.Addr{})
	return ok
}

// samePortServers serves v4 on 127.0.0.1 and v6 on ::1 at one port: two
// processes that share a port number on different loopback families.
func samePortServers(t *testing.T, v4, v6 http.Handler) int {
	t.Helper()
	ln6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	port := ln6.Addr().(*net.TCPAddr).Port
	ln4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = ln6.Close()
		t.Skipf("127.0.0.1:%d is taken by another process: %v", port, err)
	}
	for _, pair := range []struct {
		ln      net.Listener
		handler http.Handler
	}{{ln4, v4}, {ln6, v6}} {
		srv := &http.Server{Handler: pair.handler, ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(pair.ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	return port
}

func htmlHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte("<!doctype html>"))
}

func jsonHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// The probe asks the socket discovery named, not whatever holds the same
// port on the other family, and still sends Host: localhost.
func TestProbeAsksTheDiscoveredAddress(t *testing.T) {
	var hosts []string
	var mu sync.Mutex
	record := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hosts = append(hosts, r.Host)
			mu.Unlock()
			next(w, r)
		}
	}
	port := samePortServers(t, record(htmlHandler), record(jsonHandler))
	probe := newProber(time.Now)
	if _, ok := probe.pageScheme(context.Background(), port, 1, netip.MustParseAddr("127.0.0.1")); !ok {
		t.Fatal("the page on 127.0.0.1 was judged by the API on ::1")
	}
	if _, ok := probe.pageScheme(context.Background(), port, 1, netip.IPv6Loopback()); ok {
		t.Fatal("the API on ::1 was judged by the page on 127.0.0.1")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, host := range hosts {
		if host != "localhost:"+strconv.Itoa(port) {
			t.Fatalf("Host = %q, want localhost:%d", host, port)
		}
	}
}

func TestProbeAcceptsHTMLAndRedirectsAndRefusesTheRest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    bool
	}{
		{"html", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>dev</title>"))
		}, true},
		{"xhtml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xhtml+xml")
			w.WriteHeader(http.StatusOK)
		}, true},
		{"redirect to the app base", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/app/")
			w.WriteHeader(http.StatusFound)
		}, true},
		{"json api", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}, false},
		{"redirect with nowhere to go", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusFound)
		}, false},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := loopbacktest.NewServer(t, tc.handler)
			defer srv.Close()
			probe := newProber(time.Now)
			got := pageOf(probe, context.Background(), loopbackPort(t, srv), 0)
			if got != tc.want {
				t.Fatalf("pageScheme answered %v, want %v", got, tc.want)
			}
		})
	}
}

// A dev server serving TLS on loopback is answered on the second attempt.
// The certificate is httptest's own and nothing can verify it, which is
// exactly the shape a real one has.
func TestProbeFallsBackToHTTPS(t *testing.T) {
	srv := loopbacktest.NewTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	probe := newProber(time.Now)
	if !pageOf(probe, context.Background(), loopbackPort(t, srv), 0) {
		t.Fatal("an https dev server on loopback was not recognized")
	}
}

// A port nothing is on is a false verdict, never a hang: both dials fail
// immediately on loopback.
func TestProbeRefusesADeadPort(t *testing.T) {
	srv := loopbacktest.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := loopbackPort(t, srv)
	srv.Close()

	probe := newProber(time.Now)
	if pageOf(probe, context.Background(), port, 0) {
		t.Fatal("a closed port answered like a page")
	}
}

// The verdict memo is keyed by port AND pid, and it expires. Both halves
// matter: a cached answer is what makes the 3s cadence free, and a port
// that changed hands must not inherit the previous occupant's verdict.
func TestProbeVerdictCacheIsKeyedAndExpires(t *testing.T) {
	hits := 0
	srv := loopbacktest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	port := loopbackPort(t, srv)

	now := time.Unix(1_700_000_000, 0)
	probe := newProber(func() time.Time { return now })

	if !pageOf(probe, context.Background(), port, 11) || hits != 1 {
		t.Fatalf("first probe: hits = %d, want 1", hits)
	}
	if !pageOf(probe, context.Background(), port, 11) || hits != 1 {
		t.Fatalf("second probe within the TTL dialled again: hits = %d, want 1", hits)
	}
	if !pageOf(probe, context.Background(), port, 22) || hits != 2 {
		t.Fatalf("a different pid reused the verdict: hits = %d, want 2", hits)
	}
	now = now.Add(probeVerdictTTL + time.Second)
	if !pageOf(probe, context.Background(), port, 11) || hits != 3 {
		t.Fatalf("a lapsed verdict was reused: hits = %d, want 3", hits)
	}
}

// A scan that ran out of time did not learn anything, and must not
// record that it did. The bound is real — a handful of ports that accept
// and say nothing ends a pass mid-probe — so a stored "not a page" here
// would blind the next fifteen seconds of scans to a port nothing ever
// asked about.
func TestACancelledProbeIsNotAVerdict(t *testing.T) {
	hits := 0
	srv := loopbacktest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	port := loopbackPort(t, srv)

	probe := newProber(time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if pageOf(probe, ctx, port, 7) {
		t.Fatal("a cancelled probe returned a verdict")
	}
	if _, ok := probe.cached(strconv.Itoa(port) + "/7/" + netip.Addr{}.String()); ok {
		t.Fatal("a cancelled probe was remembered as a verdict")
	}

	// And the next pass asks for real.
	if !pageOf(probe, context.Background(), port, 7) {
		t.Fatal("the port was not probed again after a cancelled attempt")
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 (the cancelled attempt never reached it)", hits)
	}
}

func TestDialAddrReachesTheBoundSocket(t *testing.T) {
	for _, tc := range []struct{ bound, want string }{
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.2", "127.0.0.2"},
		{"::1", "::1"},
		{"0.0.0.0", "127.0.0.1"},
		{"::", "::1"},
		{"::ffff:127.0.0.1", "127.0.0.1"},
	} {
		if got := dialAddr(netip.MustParseAddr(tc.bound)); got != netip.MustParseAddr(tc.want) {
			t.Errorf("dialAddr(%s) = %v, want %s", tc.bound, got, tc.want)
		}
	}
}

// sniLocalhostServer is a TLS page that refuses a handshake whose SNI is
// not localhost, the shape of a dev server selecting its certificate by
// name.
func sniLocalhostServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(htmlHandler))
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName != "localhost" {
			return nil, fmt.Errorf("SNI %q, want localhost", hello.ServerName)
		}
		return nil, nil
	}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// An https dev server reached at a discovered literal address still gets
// SNI localhost.
func TestProbeSendsLocalhostSNIToADiscoveredAddress(t *testing.T) {
	srv := sniLocalhostServer(t)
	addr := srv.Listener.Addr().(*net.TCPAddr)
	probe := newProber(time.Now)
	scheme, ok := probe.pageScheme(context.Background(), addr.Port, 1, netip.MustParseAddr(addr.IP.String()))
	if !ok || scheme != "https" {
		t.Fatalf("pageScheme = %q, %v; want https", scheme, ok)
	}
}
