package transport

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ticketOf pulls the ticket out of a minted URL.
func ticketOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	ticket := parsed.Query().Get(previewTicketParam)
	if ticket == "" {
		t.Fatalf("url %q carries no ticket", raw)
	}
	return ticket
}

// The gateway's listener lifecycle. Every listener here binds
// 127.0.0.1:0 — an ephemeral loopback port — and nothing in this file
// reaches the network or another machine.

// stubPreviewSource binds an ephemeral loopback port and ignores the
// port it was asked for. Production binds the SAME number the dev server
// uses; a test cannot, because the dev server it proxies to already
// holds that number on this machine.
type stubPreviewSource struct {
	host string
	err  error

	binds []int
}

func (s *stubPreviewSource) PreviewHost() string { return s.host }

func (s *stubPreviewSource) ListenPreview(port int) (net.Listener, error) {
	s.binds = append(s.binds, port)
	if s.err != nil {
		return nil, s.err
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// testTLSConfig is a certificate source with one self-signed entry, so
// the LAN source has something to serve with. Nothing dials it: the one
// test that uses it is asserting a bind that never succeeds.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	source := NewCertificateSource()
	cert := selfSignedFor(t, "backend.test")
	source.SetSelfSigned(&cert)
	return serverTLSConfig(source)
}

func newTestGateway(t *testing.T, sources ...PreviewListenerSource) *PreviewGateway {
	t.Helper()
	if len(sources) == 0 {
		sources = []PreviewListenerSource{&stubPreviewSource{host: "backend.test"}}
	}
	gw := NewPreviewGateway(PreviewGatewayConfig{
		Sources:     sources,
		SessionLive: func(string) bool { return true },
	})
	t.Cleanup(gw.Close)
	return gw
}

// httpPorts is the preview set for the ordinary case: every one of these
// ports speaks cleartext. The scheme has its own test.
func httpPorts(ports ...int) []PreviewTarget {
	targets := make([]PreviewTarget, 0, len(ports))
	for _, port := range ports {
		targets = append(targets, PreviewTarget{Port: port, Scheme: "http"})
	}
	return targets
}

// previewAddr is the address one served port actually listens on. Only
// a test needs it: every other caller reaches a preview through the URL
// MintURL built.
func previewAddr(t *testing.T, gw *PreviewGateway, port int) string {
	t.Helper()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	listener := gw.ports[port]
	if listener == nil {
		t.Fatalf("port %d is not served", port)
	}
	return listener.ln.Addr().String()
}

func TestSetPortsBindsRetiresAndLeavesTheRestAlone(t *testing.T) {
	source := &stubPreviewSource{host: "backend.test"}
	gw := newTestGateway(t, source)

	gw.SetPorts(httpPorts(5173, 3000))
	if got := gw.Ports(); len(got) != 2 || got[0] != 3000 || got[1] != 5173 {
		t.Fatalf("ports = %v, want [3000 5173]", got)
	}
	held := previewAddr(t, gw, 5173)

	// The unchanged port keeps the listener it had: a reconcile that
	// rebound everything on every tick would drop live HMR sockets three
	// times a second.
	gw.SetPorts(httpPorts(5173, 8080))
	if got := gw.Ports(); len(got) != 2 || got[0] != 5173 || got[1] != 8080 {
		t.Fatalf("ports = %v, want [5173 8080]", got)
	}
	if again := previewAddr(t, gw, 5173); again != held {
		t.Fatalf("port 5173 was rebound: %s then %s", held, again)
	}
	if len(source.binds) != 3 {
		t.Fatalf("binds = %v, want three: two on the first pass and one on the second", source.binds)
	}

	gw.SetPorts(nil)
	if got := gw.Ports(); len(got) != 0 {
		t.Fatalf("ports = %v after emptying the set", got)
	}
}

// A dev server that already bound beyond loopback holds the address the
// gateway wants. That is not a failure to report as one: the page is
// reachable already, and the note says so.
func TestABindCollisionBecomesTheNoteAPersonReads(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = held.Close() }()
	_, portText, _ := net.SplitHostPort(held.Addr().String())
	port, _ := strconv.Atoi(portText)

	gw := NewPreviewGateway(PreviewGatewayConfig{
		Sources: []PreviewListenerSource{&PreviewLANSource{LANIP: func() string { return "127.0.0.1" }, TLS: testTLSConfig(t)}},
	})
	defer gw.Close()

	gw.SetPorts(httpPorts(port))
	if served := gw.Ports(); len(served) != 0 {
		t.Fatalf("ports = %v, want none: the address was already held", served)
	}
	note := gw.Notes()[port]
	if !strings.Contains(note, "already reachable on the LAN") {
		t.Fatalf("note = %q, want the sentence naming what actually happened", note)
	}

	// And the note goes when the port leaves the set, so a stale sentence
	// cannot outlive the question it answered.
	gw.SetPorts(nil)
	if len(gw.Notes()) != 0 {
		t.Fatalf("notes = %v after the port left the set", gw.Notes())
	}
}

// A source with nothing to serve on is skipped and the next one is
// tried. That ordering is the whole reason the gateway takes a list.
func TestTheFirstSourceThatCanServeWins(t *testing.T) {
	absent := &stubPreviewSource{host: ""}
	present := &stubPreviewSource{host: "backend.test"}
	gw := newTestGateway(t, absent, present)

	gw.SetPorts(httpPorts(5173))
	if len(absent.binds) != 0 {
		t.Fatalf("a source with no address to serve on was asked to bind: %v", absent.binds)
	}
	if len(present.binds) != 1 {
		t.Fatalf("the serving source was asked to bind %v", present.binds)
	}
}

// With no source able to serve, the port gets a sentence rather than
// silence: "not shared" and "shared but not reachable" are different
// things on screen.
func TestAPortWithNoAddressAtAllSaysSo(t *testing.T) {
	gw := newTestGateway(t, &stubPreviewSource{host: ""})
	gw.SetPorts(httpPorts(5173))
	if note := gw.Notes()[5173]; !strings.Contains(note, "No tailnet or LAN address") {
		t.Fatalf("note = %q", note)
	}
	if _, err := gw.MintURL("", 5173, "/"); err == nil {
		t.Fatal("a URL was minted for a port nothing is serving")
	}
}

// The minted URL names the source's host, the SAME port number, and the
// path it was handed — and carries the ticket as the one added
// parameter.
func TestMintURLNamesTheHostThePortAndThePath(t *testing.T) {
	gw := newTestGateway(t)
	gw.SetPorts(httpPorts(5173))

	raw, err := gw.MintURL("session-1", 5173, "/app/index.html?tab=2")
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	if !strings.HasPrefix(raw, "https://backend.test:5173/app/index.html?") {
		t.Fatalf("url = %q, want the same port under the source's host", raw)
	}
	if !strings.Contains(raw, "tab=2") {
		t.Fatalf("url = %q lost the caller's own query", raw)
	}
	if !strings.Contains(raw, previewTicketParam+"=") {
		t.Fatalf("url = %q carries no ticket", raw)
	}
}

// A ticket is spent by the FIRST presentation and buys nothing after.
func TestAPreviewTicketIsSpentOnce(t *testing.T) {
	gw := newTestGateway(t)
	gw.SetPorts(httpPorts(5173))

	raw, err := gw.MintURL("session-1", 5173, "/")
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	ticket := ticketOf(t, raw)

	if subject, ok := gw.tickets.consume(ticket); !ok || subject != previewSubject("session-1", 5173) {
		t.Fatalf("first consume = %q/%v", subject, ok)
	}
	if _, ok := gw.tickets.consume(ticket); ok {
		t.Fatal("a spent preview ticket was accepted a second time")
	}
}

// Close ends every listener AND every grant. The spec's rule that a
// backend restart ends every preview is this, plus a process that
// restarted holding no map.
func TestCloseEndsTheListenersAndTheGrants(t *testing.T) {
	gw := NewPreviewGateway(PreviewGatewayConfig{
		Sources:     []PreviewListenerSource{&stubPreviewSource{host: "backend.test"}},
		SessionLive: func(string) bool { return true },
	})
	gw.SetPorts(httpPorts(5173))
	addr := previewAddr(t, gw, 5173)
	gw.storeGrant("token", previewGrant{port: 5173, expiresAtNanos: time.Now().Add(time.Hour).UnixNano()})

	gw.Close()

	if len(gw.Ports()) != 0 {
		t.Fatalf("ports = %v after Close", gw.Ports())
	}
	if _, ok := gw.grant("token"); ok {
		t.Fatal("a grant survived Close")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the listener still accepts after Close")
	}
	// And a second Close is a no-op rather than a panic.
	gw.Close()
}

// A grant that lapsed is refused and dropped, so an unattended device
// stops reaching a dev server without anything having to sweep.
func TestALapsedGrantIsRefusedAndForgotten(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gw := NewPreviewGateway(PreviewGatewayConfig{
		Sources:     []PreviewListenerSource{&stubPreviewSource{host: "backend.test"}},
		SessionLive: func(string) bool { return true },
		Now:         func() time.Time { return now },
	})
	defer gw.Close()

	gw.storeGrant("token", previewGrant{port: 5173, expiresAtNanos: now.Add(time.Minute).UnixNano()})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: previewCookiePrefix + "5173", Value: "token"})

	if !gw.admits(req, 5173, previewCookiePrefix+"5173") {
		t.Fatal("a live grant was refused")
	}
	now = now.Add(2 * time.Minute)
	if gw.admits(req, 5173, previewCookiePrefix+"5173") {
		t.Fatal("a lapsed grant was admitted")
	}
	if _, ok := gw.grant("token"); ok {
		t.Fatal("a lapsed grant was left in the map")
	}
}

// Every preview on this machine shares a host, so the port is checked
// against the grant as well as against the cookie name.
func TestAGrantForAnotherPortIsRefused(t *testing.T) {
	gw := newTestGateway(t)
	gw.storeGrant("token", previewGrant{port: 3000, expiresAtNanos: time.Now().Add(time.Hour).UnixNano()})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: previewCookiePrefix + "5173", Value: "token"})
	if gw.admits(req, 5173, previewCookiePrefix+"5173") {
		t.Fatal("a grant minted on port 3000 admitted a request to port 5173")
	}
}

// The Location rule in one table. Every row is a redirect a dev server
// really answers with; the question each asks is whether the browser is
// sent back to the preview origin or off this machine.
func TestPreviewLocationRewrite(t *testing.T) {
	const upstream = "localhost:5173"

	for _, row := range []struct {
		name string
		in   string
		want string
	}{
		{"a relative Location already names the preview origin", "/app/", "/app/"},
		{"the upstream by the name we dialled", "http://localhost:5173/app/", "/app/"},
		{"the upstream by its own literal", "http://127.0.0.1:5173/app/", "/app/"},
		{"the upstream over IPv6", "http://[::1]:5173/app/", "/app/"},
		{"the upstream over TLS", "https://localhost:5173/app/", "/app/"},
		// An authority with no path names the root. Rewriting it to ""
		// would leave no Location at all: a browser resolves the empty
		// string against the current URL, so the redirect becomes a loop
		// on the page that issued it.
		{"the upstream with no path at all", "http://localhost:5173", "/"},
		{"the upstream with no path but a query", "http://localhost:5173?next=/x", "/?next=/x"},
		{"query and fragment survive", "http://localhost:5173/app/?a=1#top", "/app/?a=1#top"},
		// Another port on the same machine is a different dev server, and
		// somewhere else entirely is somewhere else entirely. Neither is
		// ours to rewrite.
		{"a different port on loopback", "http://localhost:5174/app/", "http://localhost:5174/app/"},
		{"an address that is not this machine", "https://example.test/app/", "https://example.test/app/"},
		{"no Location at all", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if row.in != "" {
				resp.Header.Set("Location", row.in)
			}
			if err := previewRewriteLocation(upstream)(resp); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if got := resp.Header.Get("Location"); got != row.want {
				t.Fatalf("Location = %q, want %q", got, row.want)
			}
		})
	}
}
