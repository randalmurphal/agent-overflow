package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The preview gateway's contract, against a REAL upstream on loopback.
//
// This route is excluded by name from TestEveryHTTPRouteCarriesThePolicy
// — it writes no security headers, because the bytes are the dev
// server's and a Content-Security-Policy this process invented would
// silently break somebody's application. These are the rules that
// replace that gate: the ticket exchange, the cookie flags, the
// per-request session check, the Host and Origin rewrite, and the
// Location rewrite.
//
// Every header assertion here is a VERIFIED fact from
// docs/references/dev-server-proxy.md (Vite 8.2.2, 2026-09-02). Two of
// them fail in ways that look like success if you get them wrong, which
// is why they are asserted against a real upgrade rather than a ping.

// upstreamSeen is what the dev server observed of one forwarded request.
type upstreamSeen struct {
	mu         sync.Mutex
	host       string
	origin     string
	requestURI string
	cookies    string
	protocols  string
}

func (u *upstreamSeen) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.host = r.Host
	u.origin = r.Header.Get("Origin")
	u.requestURI = r.URL.RequestURI()
	u.cookies = r.Header.Get("Cookie")
	u.protocols = r.Header.Get("Sec-WebSocket-Protocol")
}

func (u *upstreamSeen) read() upstreamSeen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return upstreamSeen{
		host: u.host, origin: u.origin, requestURI: u.requestURI,
		cookies: u.cookies, protocols: u.protocols,
	}
}

// previewRig is a dev server on loopback plus a gateway serving it.
type previewRig struct {
	gw       *PreviewGateway
	port     int
	addr     string
	seen     *upstreamSeen
	live     func(string) bool
	upstream *httptest.Server
}

// newPreviewRig starts an upstream and a gateway in front of it. The
// gateway's own listener is on an ephemeral loopback port; the PROXY
// still targets the upstream's real port, because the handler derives
// its upstream from the port number it was built for.
func newPreviewRig(t *testing.T, handler http.HandlerFunc) *previewRig {
	t.Helper()
	seen := &upstreamSeen{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)

	_, portText, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", upstream.Listener.Addr(), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}

	rig := &previewRig{port: port, seen: seen, upstream: upstream, live: func(string) bool { return true }}
	rig.gw = NewPreviewGateway(PreviewGatewayConfig{
		Sources:     []PreviewListenerSource{&stubPreviewSource{host: "backend.test"}},
		SessionLive: func(sessionID string) bool { return rig.live(sessionID) },
	})
	t.Cleanup(rig.gw.Close)
	rig.gw.SetPorts([]int{port})
	rig.addr = previewAddr(t, rig.gw, port)
	return rig
}

// noRedirectClient follows nothing, so the exchange's 302 is observable
// rather than something the client quietly resolves.
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (r *previewRig) url(path string) string { return "http://" + r.addr + path }

// open spends a freshly minted ticket and returns the cookie it bought.
func (r *previewRig) open(t *testing.T, sessionID, path string) *http.Cookie {
	t.Helper()
	minted, err := r.gw.MintURL(sessionID, r.port, path)
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	ticket := ticketOf(t, minted)

	resp, err := noRedirectClient().Get(r.url(path + "?" + previewTicketParam + "=" + ticket))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer func() { _, _ = readAllAndClose(resp) }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("exchange status = %d, want 302", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == previewCookiePrefix+strconv.Itoa(r.port) {
			return cookie
		}
	}
	t.Fatal("the exchange set no preview cookie")
	return nil
}

func helloHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte("<!doctype html><title>dev</title>"))
}

// The exchange: the first hit spends the ticket, sets the cookie with
// every flag the spec names, and redirects to the SAME address without
// the parameter — so a reload or a shared address bar carries nothing.
func TestTheTicketExchangeSetsTheCookieAndDropsTheTicket(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)

	minted, err := rig.gw.MintURL("session-1", rig.port, "/app/?tab=2")
	if err != nil {
		t.Fatalf("MintURL: %v", err)
	}
	ticket := ticketOf(t, minted)

	resp, err := noRedirectClient().Get(rig.url("/app/?tab=2&" + previewTicketParam + "=" + ticket))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer func() { _, _ = readAllAndClose(resp) }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if strings.Contains(location, previewTicketParam) {
		t.Fatalf("Location = %q still carries the ticket", location)
	}
	if !strings.HasPrefix(location, "/app/") || !strings.Contains(location, "tab=2") {
		t.Fatalf("Location = %q lost the address the person opened", location)
	}

	var cookie *http.Cookie
	for _, candidate := range resp.Cookies() {
		if candidate.Name == previewCookiePrefix+strconv.Itoa(rig.port) {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatalf("no cookie named %s%d was set", previewCookiePrefix, rig.port)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags = HttpOnly:%v Secure:%v SameSite:%v, want all three",
			cookie.HttpOnly, cookie.Secure, cookie.SameSite)
	}
	if cookie.Value == ticket {
		t.Error("the cookie value is the ticket; it must be an opaque token of its own")
	}

	// And the ticket is spent: a second presentation buys nothing, so the
	// request falls through to the no-cookie answer.
	second, err := noRedirectClient().Get(rig.url("/app/?" + previewTicketParam + "=" + ticket))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	body, _ := readAllAndClose(second)
	if second.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "preview session ended") {
		t.Fatalf("replay status = %d body = %q, want the ended-session page", second.StatusCode, body)
	}
}

// A request with no cookie gets one plain sentence and nothing else. Not
// a 404, not a stack trace, and nothing that reads like the dev server's
// own answer.
func TestARequestWithNoGrantGetsTheEndedPage(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)

	resp, err := noRedirectClient().Get(rig.url("/"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := readAllAndClose(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if string(body) != previewEndedPage {
		t.Fatalf("body = %q, want the ended-session sentence", body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if rig.seen.read().host != "" {
		t.Fatal("a request with no grant reached the dev server")
	}
}

// The rewrite rules, on the HTTP path. Host and Origin become
// localhost:<port>; the path and query arrive byte for byte; the preview
// cookie does not arrive at all.
func TestTheProxyRewritesHostAndOriginAndPreservesTheAddress(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)
	cookie := rig.open(t, "session-1", "/")

	req, err := http.NewRequest(http.MethodGet, rig.url("/app/deep%2Fpath?a=1&b=two+words"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: "the_dev_servers_own", Value: "keep-me"})
	req.Header.Set("Origin", "https://backend.test:"+strconv.Itoa(rig.port))
	req.Host = "backend.test:" + strconv.Itoa(rig.port)

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	body, _ := readAllAndClose(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<!doctype html>") {
		t.Fatalf("status = %d body = %q, want the dev server's page", resp.StatusCode, body)
	}

	seen := rig.seen.read()
	want := "localhost:" + strconv.Itoa(rig.port)
	if seen.host != want {
		t.Errorf("upstream saw Host = %q, want %q: Vite refuses anything else with 403", seen.host, want)
	}
	if seen.origin != "http://"+want {
		t.Errorf("upstream saw Origin = %q, want %q", seen.origin, "http://"+want)
	}
	if seen.requestURI != "/app/deep%2Fpath?a=1&b=two+words" {
		t.Errorf("upstream saw %q; the path and query must arrive byte for byte", seen.requestURI)
	}
	if strings.Contains(seen.cookies, previewCookiePrefix) {
		t.Errorf("the preview cookie reached the dev server: %q", seen.cookies)
	}
	if !strings.Contains(seen.cookies, "keep-me") {
		t.Errorf("cookies = %q; the dev server's own cookie was dropped with ours", seen.cookies)
	}
}

// No Origin on the way in means none invented on the way out. Vite reads
// the header's PRESENCE as "this came from a browser", which makes the
// HMR token mandatory; adding one to a request that had none would be a
// different request than the one that was made.
func TestTheProxyDoesNotInventAnOrigin(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)
	cookie := rig.open(t, "session-1", "/")

	req, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	_, _ = readAllAndClose(resp)

	if origin := rig.seen.read().origin; origin != "" {
		t.Fatalf("upstream saw Origin = %q on a request that carried none", origin)
	}
}

// A dev server serving its app under a base path answers `/` with a
// Location naming its own loopback address. Unrewritten, that would send
// the browser to a localhost which is a different machine entirely.
func TestTheProxyRewritesALocationNamingTheUpstream(t *testing.T) {
	var port int
	rig := newPreviewRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:"+strconv.Itoa(port)+"/app/")
		w.WriteHeader(http.StatusFound)
	})
	port = rig.port
	cookie := rig.open(t, "session-1", "/")

	req, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	_, _ = readAllAndClose(resp)

	if got := resp.Header.Get("Location"); got != "/app/" {
		t.Fatalf("Location = %q, want it relative to the preview origin", got)
	}
}

// Revoking a device ends its previews on the NEXT request. Nothing here
// may cache the answer; re-asking is the whole mechanism.
func TestARevokedSessionLosesItsPreviewOnTheNextRequest(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)
	cookie := rig.open(t, "session-1", "/")

	req, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	_, _ = readAllAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want the page", resp.StatusCode)
	}

	rig.live = func(string) bool { return false }

	again, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	again.AddCookie(cookie)
	resp2, err := noRedirectClient().Do(again)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	body, _ := readAllAndClose(resp2)
	if resp2.StatusCode != http.StatusUnauthorized || string(body) != previewEndedPage {
		t.Fatalf("status = %d body = %q, want the ended-session page", resp2.StatusCode, body)
	}
}

// The route writes NO security headers, deliberately. A policy this
// process invented would break somebody's application, and the origin
// separation is what makes that safe rather than the headers.
func TestProxiedResponsesCarryNoPolicyOfOurs(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)
	cookie := rig.open(t, "session-1", "/")

	req, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	_, _ = readAllAndClose(resp)

	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options"} {
		if got := resp.Header.Get(header); got != "" {
			t.Errorf("proxied response carries %s = %q; the bytes are the dev server's", header, got)
		}
	}
}

// The HMR upgrade, through the proxy, for real. A `vite-ping` probe
// bypasses the host check entirely and would pass whatever this code
// did, so the upgrade is exercised with the subprotocol the real client
// uses and the upstream's view of it is asserted.
func TestTheHMRUpgradeReachesTheDevServerAsItself(t *testing.T) {
	upgraded := make(chan struct{})
	rig := newPreviewRig(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:       []string{"vite-hmr"},
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		close(upgraded)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"connected"}`))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	cookie := rig.open(t, "session-1", "/")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", "https://backend.test:"+strconv.Itoa(rig.port))
	conn, _, err := websocket.Dial(ctx, "ws://"+rig.addr+"/base/?token=abc123", &websocket.DialOptions{
		HTTPHeader:   header,
		Subprotocols: []string{"vite-hmr"},
	})
	if err != nil {
		t.Fatalf("upgrade through the proxy: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	select {
	case <-upgraded:
	case <-ctx.Done():
		t.Fatal("the upstream never saw the upgrade")
	}

	seen := rig.seen.read()
	want := "localhost:" + strconv.Itoa(rig.port)
	if seen.host != want {
		t.Errorf("the upgrade reached the dev server with Host = %q, want %q: Vite answers 400 to anything else",
			seen.host, want)
	}
	if seen.requestURI != "/base/?token=abc123" {
		t.Errorf("the upgrade reached the dev server at %q; a changed path hangs the socket with no response",
			seen.requestURI)
	}
	if seen.origin != "http://"+want {
		t.Errorf("the upgrade carried Origin = %q, want it rewritten to %q", seen.origin, "http://"+want)
	}
	if !strings.Contains(seen.protocols, "vite-hmr") {
		t.Errorf("Sec-WebSocket-Protocol = %q, want vite-hmr forwarded unchanged", seen.protocols)
	}
	if conn.Subprotocol() != "vite-hmr" {
		t.Errorf("the negotiated subprotocol came back as %q", conn.Subprotocol())
	}
}

// A dev server that is down is the dev server's condition, said as such.
// The person is looking at somebody's `npm run dev`, and a blank page
// would read as the app being broken.
func TestADevServerThatStoppedSaysSo(t *testing.T) {
	rig := newPreviewRig(t, helloHandler)
	cookie := rig.open(t, "session-1", "/")
	rig.upstream.Close()

	req, _ := http.NewRequest(http.MethodGet, rig.url("/"), nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "did not answer") {
		t.Fatalf("body = %q, want a sentence naming what happened", body)
	}
}
