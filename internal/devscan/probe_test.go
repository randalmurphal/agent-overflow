package devscan

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
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
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			probe := newProber(time.Now)
			got := probe.answersLikeAPage(context.Background(), loopbackPort(t, srv), 0)
			if got != tc.want {
				t.Fatalf("answersLikeAPage = %v, want %v", got, tc.want)
			}
		})
	}
}

// A dev server serving TLS on loopback is answered on the second attempt.
// The certificate is httptest's own and nothing can verify it, which is
// exactly the shape a real one has.
func TestProbeFallsBackToHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	probe := newProber(time.Now)
	if !probe.answersLikeAPage(context.Background(), loopbackPort(t, srv), 0) {
		t.Fatal("an https dev server on loopback was not recognized")
	}
}

// A port nothing is on is a false verdict, never a hang: both dials fail
// immediately on loopback.
func TestProbeRefusesADeadPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	port := loopbackPort(t, srv)
	srv.Close()

	probe := newProber(time.Now)
	if probe.answersLikeAPage(context.Background(), port, 0) {
		t.Fatal("a closed port answered like a page")
	}
}

// The verdict memo is keyed by port AND pid, and it expires. Both halves
// matter: a cached answer is what makes the 3s cadence free, and a port
// that changed hands must not inherit the previous occupant's verdict.
func TestProbeVerdictCacheIsKeyedAndExpires(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	port := loopbackPort(t, srv)

	now := time.Unix(1_700_000_000, 0)
	probe := newProber(func() time.Time { return now })

	if !probe.answersLikeAPage(context.Background(), port, 11) || hits != 1 {
		t.Fatalf("first probe: hits = %d, want 1", hits)
	}
	if !probe.answersLikeAPage(context.Background(), port, 11) || hits != 1 {
		t.Fatalf("second probe within the TTL dialled again: hits = %d, want 1", hits)
	}
	if !probe.answersLikeAPage(context.Background(), port, 22) || hits != 2 {
		t.Fatalf("a different pid reused the verdict: hits = %d, want 2", hits)
	}
	now = now.Add(probeVerdictTTL + time.Second)
	if !probe.answersLikeAPage(context.Background(), port, 11) || hits != 3 {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html>"))
	}))
	defer srv.Close()
	port := loopbackPort(t, srv)

	probe := newProber(time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if probe.answersLikeAPage(ctx, port, 7) {
		t.Fatal("a cancelled probe returned a verdict")
	}
	if _, ok := probe.cached(strconv.Itoa(port) + "/7"); ok {
		t.Fatal("a cancelled probe was remembered as a verdict")
	}

	// And the next pass asks for real.
	if !probe.answersLikeAPage(context.Background(), port, 7) {
		t.Fatal("the port was not probed again after a cancelled attempt")
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 (the cancelled attempt never reached it)", hits)
	}
}
