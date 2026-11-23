package trackerapi

import (
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
)

// tlsTracker is an HTTP/2 TLS test server that counts the connections
// made to it and records, per request, its Authorization and whether its
// TLS session was resumed.
type tlsTracker struct {
	srv   *httptest.Server
	conns atomic.Int64
	mu    sync.Mutex
	auth  []string
	resum []bool
}

func newTLSTracker(t *testing.T) *tlsTracker {
	t.Helper()
	tr := &tlsTracker{}
	tr.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.mu.Lock()
		tr.auth = append(tr.auth, r.Header.Get("Authorization"))
		tr.resum = append(tr.resum, r.TLS.DidResume)
		tr.mu.Unlock()
		writeString(w, `{}`)
	}))
	tr.srv.EnableHTTP2 = true
	tr.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			tr.conns.Add(1)
		}
	}
	tr.srv.StartTLS()
	t.Cleanup(tr.srv.Close)
	return tr
}

// transport is New's transport, trusting the server's certificate.
func (tr *tlsTracker) transport() *http.Transport {
	t := newTransport(nil)
	t.TLSClientConfig.RootCAs = x509.NewCertPool()
	t.TLSClientConfig.RootCAs.AddCert(tr.srv.Certificate())
	return t
}

func (tr *tlsTracker) requests() (auth []string, resumed []bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.auth...), append([]bool(nil), tr.resum...)
}

func bearerOptions(base, token string) Options {
	opts := testOptions(base)
	opts.Credentials = StaticCredentials(Bearer(secret.New([]byte(token))))
	return opts
}

// A connection made after the idle ones closed resumes the TLS session
// of the one before, with no full handshake.
func TestReconnectResumesTLSSession(t *testing.T) {
	t.Parallel()
	tr := newTLSTracker(t)
	transport := tr.transport()
	c, err := newClientOn(testOptions(tr.srv.URL+"/"), ownConnections(transport))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	for range 2 {
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
		transport.CloseIdleConnections()
	}
	if _, resumed := tr.requests(); len(resumed) != 2 || resumed[0] || !resumed[1] {
		t.Fatalf("resumed per request %v, want a full handshake and then a resumed one", resumed)
	}
	if n := tr.conns.Load(); n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}
}

// Two clients built with NewSharing send over one connection, each with
// its own credential and OnAnswer; a client of its own makes its own.
func TestSharingClientsUseOneConnection(t *testing.T) {
	t.Parallel()
	tr := newTLSTracker(t)
	base := tr.srv.URL + "/"
	a, err := newClientOn(bearerOptions(base, "token-of-client-a"), ownConnections(tr.transport()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	var told atomic.Int64
	opts := bearerOptions(base, "token-of-client-b")
	opts.OnAnswer = func(int, http.Header) { told.Add(1) }
	b, err := NewSharing(a, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for _, c := range []*Client{a, b, a} {
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := tr.conns.Load(); n != 1 {
		t.Fatalf("%d connections for two sharing clients, want 1", n)
	}
	auth, _ := tr.requests()
	if want := []string{"Bearer token-of-client-a", "Bearer token-of-client-b", "Bearer token-of-client-a"}; strings.Join(auth, ",") != strings.Join(want, ",") {
		t.Fatalf("Authorization per request %q, want %q", auth, want)
	}
	if told.Load() != 1 {
		t.Fatalf("b's OnAnswer was told of %d answers, want its own 1", told.Load())
	}
	own, err := newClientOn(testOptions(base), ownConnections(tr.transport()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)
	if _, err := own.Do(t.Context(), Request{Path: "a"}); err != nil {
		t.Fatal(err)
	}
	if n := tr.conns.Load(); n != 2 {
		t.Fatalf("%d connections after a client of its own, want 2", n)
	}
}

// countingConnections is a transport whose idle closes are counted.
func countingConnections(t *http.Transport, closes *atomic.Int64) *connections {
	return &connections{transport: t, users: 1, closeIdle: func() {
		closes.Add(1)
		t.CloseIdleConnections()
	}}
}

// Closing either of two sharing clients ends only its own calls; the idle
// connections close once, with the last of them, in either order.
func TestSharedConnectionsOutliveEitherClose(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": reply(http.StatusOK)})
	for _, order := range []string{"first-closes-first", "sharer-closes-first"} {
		t.Run(order, func(t *testing.T) {
			var closes atomic.Int64
			first, err := newClientOn(testOptions(tr.url()), countingConnections(newTransport(nil), &closes))
			if err != nil {
				t.Fatal(err)
			}
			sharer, err := NewSharing(first, testOptions(tr.url()))
			if err != nil {
				t.Fatal(err)
			}
			closing, staying := first, sharer
			if order == "sharer-closes-first" {
				closing, staying = sharer, first
			}
			closing.Close()
			closing.Close()
			if _, err := closing.Do(t.Context(), Request{Path: "a"}); err == nil {
				t.Fatal("a closed client still sends")
			}
			if n := closes.Load(); n != 0 {
				t.Fatalf("the idle connections closed %d times while a client still shares them", n)
			}
			if _, err := staying.Do(t.Context(), Request{Path: "a"}); err != nil {
				t.Fatalf("the client left open fails: %v", err)
			}
			if _, err := NewSharing(closing, testOptions(tr.url())); err == nil {
				t.Fatal("NewSharing accepted a closed peer")
			}
			staying.Close()
			if n := closes.Load(); n != 1 {
				t.Fatalf("the idle connections closed %d times after both clients closed, want 1", n)
			}
			if _, err := NewSharing(staying, testOptions(tr.url())); err == nil {
				t.Fatal("NewSharing accepted a closed peer whose connections are closed")
			}
		})
	}
}

// NewSharing refuses what would not share one origin's connections.
func TestNewSharingRefusals(t *testing.T) {
	t.Parallel()
	peer, err := New(testOptions("https://example.atlassian.net/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	withProxy := testOptions("https://example.atlassian.net/")
	withProxy.Proxy = http.ProxyFromEnvironment
	for name, tc := range map[string]struct {
		peer *Client
		opts Options
	}{
		"no peer":        {nil, testOptions("https://example.atlassian.net/")},
		"other origin":   {peer, testOptions("https://example.com/")},
		"other port":     {peer, testOptions("https://example.atlassian.net:8443/")},
		"a proxy":        {peer, withProxy},
		"invalid option": {peer, Options{BaseURL: "https://example.atlassian.net/"}},
	} {
		if c, err := NewSharing(tc.peer, tc.opts); err == nil {
			c.Close()
			t.Errorf("%s: NewSharing accepted it", name)
		}
	}
	same := testOptions("https://EXAMPLE.atlassian.net:443/rest/")
	c, err := NewSharing(peer, same)
	if err != nil {
		t.Fatalf("the same origin under another spelling and path: %v", err)
	}
	c.Close()
}
