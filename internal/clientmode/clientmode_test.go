package clientmode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/relaysession"
	"agent-overflow/internal/transport"

	"github.com/coder/websocket"
)

// splitAddrForTest mirrors net.SplitHostPort's behaviour but returns a
// typed error so the test surface stays small. Used to extract the
// resolved port from a Server.Addr() string for the Host-header test
// cases that need a concrete port to spell out the loopback target.
func splitAddrForTest(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	return host, port, err
}

// fakeAssets returns a tiny fstest.MapFS with the SPA shell shape the
// production frontend/dist emits: index.html plus an /assets/ child.
func fakeAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte(`<!doctype html>
<html>
  <head>
    <meta charset="UTF-8" />
    <title>Agent Overflow</title>
    <script type="module" src="/assets/index-abc.js"></script>
  </head>
  <body><div id="app"></div></body>
</html>`),
		},
		"assets/index-abc.js": &fstest.MapFile{
			Data: []byte("// fake spa entry"),
		},
		// The bundle's root-level files. boot-theme.js is the
		// first-paint theme stamp, which must load before the deferred
		// module or the frame it exists to paint is already gone;
		// favicon.svg is what index.html's <link rel="icon"> names.
		// Both live beside assets/ in dist rather than inside it.
		"boot-theme.js": &fstest.MapFile{
			Data: []byte("/* fake boot theme */"),
		},
		"favicon.svg": &fstest.MapFile{
			Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		},
	}
}

func TestParseConnectURL_RejectsHTTP(t *testing.T) {
	_, err := ParseConnectURL("http://example.com:8080/?token=abc")
	if err == nil {
		t.Fatal("ParseConnectURL accepted http://, want error")
	}
	if !strings.Contains(err.Error(), "ws:// or wss://") {
		t.Fatalf("error %q does not point at scheme requirement", err)
	}
}

func TestParseConnectURL_RejectsHTTPS(t *testing.T) {
	// Defense-in-depth: https:// is the obvious near-miss for wss://;
	// rejecting it keeps the failure message focused on the right fix.
	_, err := ParseConnectURL("https://example.com:8080/?token=abc")
	if err == nil {
		t.Fatal("ParseConnectURL accepted https://, want error")
	}
}

func TestParseConnectURL_RequiresToken(t *testing.T) {
	_, err := ParseConnectURL("ws://example.com:8080/")
	if err == nil {
		t.Fatal("ParseConnectURL accepted URL with no token, want error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error %q does not mention token", err)
	}
}

func TestParseConnectURL_RequiresHost(t *testing.T) {
	_, err := ParseConnectURL("ws:///?token=abc")
	if err == nil {
		t.Fatal("ParseConnectURL accepted URL with no host, want error")
	}
}

func TestParseConnectURL_StripsTokenFromWSURL(t *testing.T) {
	cfg, err := ParseConnectURL("ws://host:9999/?token=secret-value&other=keep")
	if err != nil {
		t.Fatalf("ParseConnectURL: %v", err)
	}
	if cfg.Token != "secret-value" {
		t.Fatalf("Token = %q, want %q", cfg.Token, "secret-value")
	}
	// The wsClient appends ?token= on its own — leaving the token in
	// the bootstrap URL would double-encode it, so the parse step must
	// strip it. The other params survive.
	if strings.Contains(cfg.WSURL, "secret-value") {
		t.Fatalf("WSURL still contains token: %q", cfg.WSURL)
	}
	if !strings.Contains(cfg.WSURL, "other=keep") {
		t.Fatalf("WSURL dropped other params: %q", cfg.WSURL)
	}
}

func TestParseConnectURL_AcceptsWSS(t *testing.T) {
	cfg, err := ParseConnectURL("wss://remote.example.com/agent?token=abc")
	if err != nil {
		t.Fatalf("ParseConnectURL(wss): %v", err)
	}
	if !strings.HasPrefix(cfg.WSURL, "wss://") {
		t.Fatalf("WSURL = %q, want wss://", cfg.WSURL)
	}
	if cfg.Token != "abc" {
		t.Fatalf("Token = %q, want %q", cfg.Token, "abc")
	}
}

// serveStub boots a stub with the given config, filling in the fake
// assets and registering cleanup.
func serveStub(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Assets = fakeAssets()
	srv, err := Serve(cfg)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// stubBootstrapURL builds the manifest URL a freshly loaded page fetches:
// the stub's own origin, carrying the page ticket the page URL arrived
// with.
func stubBootstrapURL(t *testing.T, srv *Server) string {
	t.Helper()
	parsed, err := url.Parse(srv.AppURL())
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	ticket := parsed.Query().Get(transport.PageTicketParam)
	if ticket == "" {
		t.Fatal("app url carries no page ticket")
	}
	return fmt.Sprintf("http://%s/bootstrap.json?%s=%s", parsed.Host, transport.PageTicketParam, url.QueryEscape(ticket))
}

// TestServe_ServesTheShellVerbatim is the structural half of this
// change: the stub inserts nothing into the page. No injected manifest
// means no credential in the document, and no escaping question about
// what a markup-shaped token does to an inline script tag — the class
// is gone rather than defended, which the markup-shaped token below
// exercises.
func TestServe_ServesTheShellVerbatim(t *testing.T) {
	srv := serveStub(t, Config{
		WSURL: "ws://upstream:1234/",
		Token: "tok</" + "script><" + "script>marker()</" + "script>",
	})

	resp, err := http.Get(srv.AppURL())
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := fakeAssets()["index.html"].Data
	if string(body) != string(want) {
		t.Fatalf("the shell was modified in flight.\ngot:  %s\nwant: %s", body, want)
	}
	if strings.Contains(string(body), "marker()") {
		t.Fatal("the upstream token reached the document")
	}
}

// TestServe_AppURLCarriesTicketModeAndClientID pins what rides the page
// URL: a one-time ticket (never the upstream token), the run mode the
// SPA reads synchronously at module load, and the durable client
// identity. A fresh ticket per call is what lets the reload keybinding
// re-navigate after the first load spent one.
func TestServe_AppURLCarriesTicketModeAndClientID(t *testing.T) {
	srv := serveStub(t, Config{
		WSURL:    "ws://upstream:1234/",
		Token:    "tok-abc",
		ClientID: "11111111-2222-3333-4444-555555555555",
	})

	first, err := url.Parse(srv.AppURL())
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	q := first.Query()
	if q.Get("mode") != "client" {
		t.Errorf("mode = %q, want client", q.Get("mode"))
	}
	if q.Get("cid") != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("cid = %q, want the configured client id", q.Get("cid"))
	}
	ticket := q.Get(transport.PageTicketParam)
	if ticket == "" {
		t.Fatal("app url carries no page ticket")
	}
	if strings.Contains(srv.AppURL(), "tok-abc") {
		t.Fatal("the upstream token reached the page URL")
	}
	second, err := url.Parse(srv.AppURL())
	if err != nil {
		t.Fatalf("parse second app url: %v", err)
	}
	if second.Query().Get(transport.PageTicketParam) == ticket {
		t.Fatal("two AppURL calls handed out the same ticket")
	}
}

// TestServe_ManifestNamesThisOriginAndCarriesNoCredential is what lets
// the SPA be identical in both modes: the stub's manifest names the
// stub's own origin, so the page's same-origin rule holds with no
// exemption, and it carries nothing script could read.
func TestServe_ManifestNamesThisOriginAndCarriesNoCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"wsUrl":"ws://upstream-perspective/ws"}`))
	}))
	t.Cleanup(upstream.Close)
	srv := serveWithUpstream(t, upstream)

	resp, err := http.Get(stubBootstrapURL(t, srv))
	if err != nil {
		t.Fatalf("GET /bootstrap.json: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var manifest struct {
		WSURL    string `json:"wsUrl"`
		LaunchID string `json:"launchId"`
		Remote   bool   `json:"remote"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode manifest %s: %v", body, err)
	}
	if want := "ws://" + srv.Addr() + "/ws"; manifest.WSURL != want {
		t.Errorf("wsUrl = %q, want this stub's own origin %q", manifest.WSURL, want)
	}
	if manifest.LaunchID == "" {
		t.Error("manifest carries no launch id")
	}
	if strings.Contains(string(body), "tok-live") {
		t.Errorf("manifest carries the upstream credential: %s", body)
	}
	// And the exchange issued the page its own cookie.
	var issued *http.Cookie
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "ao_page_") {
			issued = cookie
		}
	}
	if issued == nil {
		t.Fatal("the ticket exchange set no page cookie")
	}
	if !issued.HttpOnly {
		t.Error("page cookie is not HttpOnly")
	}
	if issued.Value == "tok-live" {
		t.Error("the stub handed the page its upstream credential as a cookie")
	}
}

// TestServe_ManifestLocalityFollowsTheUpstream pins which endpoint the
// view-only bit describes. The listener is always loopback; what the SPA
// gates on is whether the backend doing the work is this machine's.
func TestServe_ManifestLocalityFollowsTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		wsURL      string
		wantRemote bool
	}{
		{wsURL: "ws://upstream:1234/ws", wantRemote: true},
		{wsURL: "ws://127.0.0.1:1234/ws", wantRemote: false},
		// The whole 127/8 block is this machine.
		{wsURL: "ws://127.0.0.2:1234/ws", wantRemote: false},
	} {
		t.Run(tc.wsURL, func(t *testing.T) {
			srv := serveStub(t, Config{WSURL: tc.wsURL, Token: "tok-abc"})
			if srv.remote != tc.wantRemote {
				t.Fatalf("remote = %v, want %v", srv.remote, tc.wantRemote)
			}
		})
	}
}

func TestServe_ServesStaticAssets(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// The page URL carries query parameters now, so address the asset by
	// the listener rather than by string-appending to it.
	resp, err := http.Get("http://" + srv.Addr() + "/assets/index-abc.js")
	if err != nil {
		t.Fatalf("GET /assets/...: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "fake spa entry") {
		t.Fatalf("asset body unexpected: %s", body)
	}
	// Asset responses must be no-store: the stub only serves the local
	// embedded webview, which loads the SPA once per process, and a
	// cacheable script pins its decoded text in the renderer's
	// in-memory HTTP cache for the page's lifetime.
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control %q does not include no-store", got)
	}
}

func TestServe_RejectsMissingIndex(t *testing.T) {
	// fstest.MapFS without index.html — buildInjectedIndex must surface
	// the failure rather than starting a half-broken server.
	noIndex := fstest.MapFS{
		"assets/foo.js": &fstest.MapFile{Data: []byte("x")},
	}
	_, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: noIndex,
	})
	if err == nil {
		t.Fatal("Serve accepted asset fs missing index.html, want error")
	}
}

func TestServe_RequiresWSURLAndToken(t *testing.T) {
	if _, err := Serve(Config{Token: "t", Assets: fakeAssets()}); err == nil {
		t.Fatal("Serve accepted empty WSURL, want error")
	}
	if _, err := Serve(Config{WSURL: "ws://h/", Assets: fakeAssets()}); err == nil {
		t.Fatal("Serve accepted empty Token, want error")
	}
	if _, err := Serve(Config{WSURL: "ws://h/", Token: "t"}); err == nil {
		t.Fatal("Serve accepted nil Assets, want error")
	}
}

func TestServe_IndexCacheControl(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	resp, err := http.Get(srv.AppURL())
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	// The shell holds the bootstrap token; a stale cached copy after
	// the operator restarts with a new --connect URL would inject the
	// wrong token. no-store prevents that.
	got := resp.Header.Get("Cache-Control")
	if !strings.Contains(got, "no-store") {
		t.Fatalf("index Cache-Control %q must include no-store", got)
	}
}

func TestParseConnectURL_RejectsUserinfo(t *testing.T) {
	// Userinfo (HTTP basic-auth-style user:pass) is not part of the
	// auth model. Accepting it silently would let an operator paste
	// `ws://user:pass@host?token=t` and get a confusing failure later
	// when the server only honors `?token=`.
	_, err := ParseConnectURL("ws://user:pass@host:9999/?token=tkn")
	if err == nil {
		t.Fatal("ParseConnectURL accepted userinfo, want error")
	}
	if !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("error %q does not mention userinfo", err)
	}
	// User-only (no password) form must be rejected too.
	_, err = ParseConnectURL("ws://user@host:9999/?token=tkn")
	if err == nil {
		t.Fatal("ParseConnectURL accepted user-only userinfo, want error")
	}
}

// TestServe_RejectsRebindHost covers the Host check. The stub binds
// 127.0.0.1, but a DNS name resolving there reaches it too: a page
// navigated to `http://foreign.test:<port>/` arrives with
// `Host: foreign.test`, and without the check the document it received
// — boot script and all — would run under that name's origin. Refuse
// anything that is not a loopback name.
//
// 404 is intentional: it matches the transport server's
// "indistinguishable from a real 404" refusal shape, so the presence of
// the check is not itself a fingerprint.
func TestServe_RejectsRebindHost(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Empty Host is omitted from the table: HTTP/1.1 requires a Host
	// header and Go's http.Client substitutes the URL's authority when
	// Request.Host is "". The check still rejects an empty Host
	// (loopback.HostHeader("") is false, pinned in that package's own
	// test) but the Go client makes the branch unreachable from a real
	// request.
	cases := []struct {
		name string
		path string
		host string
	}{
		{"index foreign name", "/", "foreign.test"},
		{"index foreign name with port", "/", "foreign.test:8080"},
		{"asset foreign name", "/assets/index-abc.js", "foreign.test"},
		{"index public IP", "/", "192.168.1.50"},
		{"asset public IP", "/assets/index-abc.js", "192.168.1.50:54321"},
		{"index ipv6 non-loopback", "/", "[2001:db8::1]:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.AppURL()+strings.TrimPrefix(c.path, "/"), nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Host = c.host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("Host=%q path=%q status = %d, want 404", c.host, c.path, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			// The 404 must NOT contain the bootstrap snippet — the
			// whole point is that the foreign origin reads no
			// credential. Defense-in-depth assertion.
			if strings.Contains(string(body), "__AO_BOOTSTRAP__") {
				t.Fatalf("404 response leaked bootstrap snippet")
			}
		})
	}
}

func TestServe_AcceptsLoopbackHosts(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	_, port, err := splitAddrForTest(srv.Addr())
	if err != nil {
		t.Fatalf("split addr %q: %v", srv.Addr(), err)
	}

	// The Host header values that must succeed. Skip "::1" (the IPv6
	// loopback) because Listen("tcp", "127.0.0.1:0") does not accept
	// IPv6 connections — the v6-loopback path is exercised elsewhere
	// when the bind explicitly opts into it.
	cases := []string{
		"127.0.0.1:" + port,
		"localhost:" + port,
		"127.0.0.1",
		"localhost",
		"LocalHost:" + port, // case-insensitive
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.AppURL(), nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Host = host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Host=%q status = %d, want 200", host, resp.StatusCode)
			}
		})
	}
}

func TestServe_ShutdownIsIdempotent(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "t",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// serveWithUpstream boots a stub whose WSURL points at the given
// upstream httptest server, so /bootstrap.json probes land there.
func serveWithUpstream(t *testing.T, upstream *httptest.Server) *Server {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/ws"
	srv, err := Serve(Config{
		WSURL:  wsURL,
		Token:  "tok-live",
		Assets: fakeAssets(),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// TestHandleBootstrap_RelaysVerdicts pins the whole revalidation
// contract: the stub probes the upstream manifest endpoint with its
// configured token, and maps upstream 200 → the stub's OWN manifest
// (upstream wsUrl would be wrong through a tunnel and carries no
// mode:"client"), refusal → 404, upstream-down → 503.
func TestHandleBootstrap_RelaysVerdicts(t *testing.T) {
	var upstreamStatus int
	var sawToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bootstrap.json" {
			t.Errorf("upstream probed at %q, want /bootstrap.json", r.URL.Path)
		}
		sawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(upstreamStatus)
		if upstreamStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"wsUrl":"ws://upstream-perspective/ws","token":"tok-live"}`))
		}
	}))
	t.Cleanup(upstream.Close)
	srv := serveWithUpstream(t, upstream)

	get := func() (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(stubBootstrapURL(t, srv))
		if err != nil {
			t.Fatalf("GET /bootstrap.json: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, string(body)
	}

	upstreamStatus = http.StatusOK
	resp, body := get()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream 200: stub status = %d, want 200", resp.StatusCode)
	}
	if sawToken != "tok-live" {
		t.Errorf("upstream saw token %q, want the configured tok-live", sawToken)
	}
	if strings.Contains(body, "upstream-perspective") {
		t.Errorf("stub must serve its own manifest, not the upstream's: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	upstreamStatus = http.StatusNotFound
	resp, _ = get()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("upstream 404: stub status = %d, want 404", resp.StatusCode)
	}

	upstreamStatus = http.StatusServiceUnavailable
	resp, _ = get()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upstream 503: stub status = %d, want 503 (transient stays transient)", resp.StatusCode)
	}
}

// TestHandleBootstrap_UnreachableUpstreamIsTransient pins that a dead
// network answers 503, never 404 — the SPA latches its terminal state
// on 404, and "can't reach the backend" is not "the backend refused".
func TestHandleBootstrap_UnreachableUpstreamIsTransient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstream.Close() // deliberately dead
	srv := serveWithUpstream(t, upstream)

	resp, err := http.Get(stubBootstrapURL(t, srv))
	if err != nil {
		t.Fatalf("GET /bootstrap.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dead upstream: stub status = %d, want 503", resp.StatusCode)
	}
}

// exchangeStubCookie walks the page's first contact against the stub and
// returns the session cookie it was issued.
func exchangeStubCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	resp, err := http.Get(stubBootstrapURL(t, srv))
	if err != nil {
		t.Fatalf("GET /bootstrap.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status = %d, want 200", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "ao_page_") {
			return cookie
		}
	}
	t.Fatal("the exchange set no page cookie")
	return nil
}

// TestHandleWS_CarriesTheUpgradeWithTheUpstreamCredential is the
// `--connect` design in one test: the page opens a same-origin socket
// authenticated by the cookie it holds, and this process swaps in the
// upstream credential on the way out. The upstream never sees the page's
// cookie or the stub's origin — a proxy hop is not a browser — and the
// page never sees the upstream's token.
func TestHandleWS_CarriesTheUpgradeWithTheUpstreamCredential(t *testing.T) {
	type observed struct {
		auth   string
		cookie string
		origin string
		query  string
	}
	seen := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bootstrap.json" {
			// The stub probes this before it answers the page's manifest.
			_, _ = w.Write([]byte(`{"wsUrl":"ws://upstream-perspective/ws"}`))
			return
		}
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		seen <- observed{
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			origin: r.Header.Get("Origin"),
			query:  r.URL.RawQuery,
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("upstream accept: %v", err)
			return
		}
		defer conn.CloseNow()
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), typ, data)
	}))
	t.Cleanup(upstream.Close)
	srv := serveWithUpstream(t, upstream)
	cookie := exchangeStubCookie(t, srv)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", "http://"+srv.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx,
		"ws://"+srv.Addr()+"/ws?did=screen-abcdef01&conn=live-abcdef01&nonsense=1",
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the stub's /ws with the page cookie: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("write through the proxy: %v", err)
	}
	_, echoed, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read through the proxy: %v", err)
	}
	if string(echoed) != "ping" {
		t.Fatalf("proxy echoed %q, want ping", echoed)
	}

	got := <-seen
	if got.auth != "Bearer tok-live" {
		t.Errorf("upstream Authorization = %q, want the configured bearer", got.auth)
	}
	if got.cookie != "" {
		t.Errorf("the page's cookie reached the upstream: %q", got.cookie)
	}
	if got.origin != "" {
		t.Errorf("the stub's origin reached the upstream: %q", got.origin)
	}
	// The page's declared identity has to survive the hop: the upstream
	// scopes its ui_state bucket by the connection, so a stub that dropped
	// these two parameters would leave every --connect client with no
	// bucket at all. Nothing else the page put on the URL crosses.
	upstreamQuery, err := url.ParseQuery(got.query)
	if err != nil {
		t.Fatalf("upstream query %q: %v", got.query, err)
	}
	if upstreamQuery.Get("did") != "screen-abcdef01" {
		t.Errorf("upstream did = %q, want the page's device id", upstreamQuery.Get("did"))
	}
	if upstreamQuery.Get("conn") != "live-abcdef01" {
		t.Errorf("upstream conn = %q, want the page's connection id", upstreamQuery.Get("conn"))
	}
	if upstreamQuery.Has("nonsense") {
		t.Errorf("an undeclared page parameter crossed the hop: %q", got.query)
	}
}

// TestUpstreamQuery_OperatorParametersWin — the operator's endpoint owns
// its query and the page cannot see it, so a page parameter that
// overwrote one would be the page reconfiguring the hop.
func TestUpstreamQuery_OperatorParametersWin(t *testing.T) {
	page := url.Values{"did": {"screen-abcdef01"}, "conn": {"live-abcdef01"}}
	got, err := url.ParseQuery(upstreamQuery("did=operator-pinned-id&region=eu", page))
	if err != nil {
		t.Fatalf("parse the assembled query: %v", err)
	}
	if got.Get("did") != "operator-pinned-id" {
		t.Errorf("did = %q, want the operator's value", got.Get("did"))
	}
	if got.Get("region") != "eu" {
		t.Errorf("region = %q, want the operator's value preserved", got.Get("region"))
	}
	if got.Get("conn") != "live-abcdef01" {
		t.Errorf("conn = %q, want the page's value where the operator named none", got.Get("conn"))
	}
}

// TestUpstreamQuery_UnparseableOperatorQueryIsForwardedVerbatim — an
// operator URL we cannot parse is one we must not rewrite from a guess.
func TestUpstreamQuery_UnparseableOperatorQueryIsForwardedVerbatim(t *testing.T) {
	const operator = "%zz"
	page := url.Values{"did": {"screen-abcdef01"}}
	if got := upstreamQuery(operator, page); got != operator {
		t.Fatalf("upstreamQuery = %q, want %q", got, operator)
	}
}

// TestHandleWS_RefusesUnauthenticatedAndForeignOrigin pins the two gates
// on the stub's own upgrade, both the transport's own rules: a page from
// another origin on this host is refused before its credential is
// consulted (cookies are scoped by host, not by port), and a caller with
// no credential is refused with the same unfingerprintable 404.
func TestHandleWS_RefusesUnauthenticatedAndForeignOrigin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bootstrap.json" {
			_, _ = w.Write([]byte(`{"wsUrl":"ws://upstream-perspective/ws"}`))
			return
		}
		t.Error("the upstream was reached by an upgrade the stub should have refused")
	}))
	t.Cleanup(upstream.Close)
	srv := serveWithUpstream(t, upstream)
	cookie := exchangeStubCookie(t, srv)

	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "no credential", header: http.Header{}},
		{name: "foreign origin with a valid cookie", header: http.Header{
			"Cookie": {cookie.Name + "=" + cookie.Value},
			"Origin": {"http://evil.example"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, "ws://"+srv.Addr()+"/ws", &websocket.DialOptions{HTTPHeader: tc.header})
			if err == nil {
				conn.CloseNow()
				t.Fatal("the stub carried an upgrade it should have refused")
			}
			if resp != nil && resp.StatusCode != http.StatusNotFound {
				t.Fatalf("refusal status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// sessionForwardingUpstream is an upstream that plants the backend's local
// page-channel cookie on /bootstrap.json and records the session
// credential each carried upgrade presents.
//
// `accept` decides whether an upgrade is honoured, so one stub covers both
// the forwarding case and the refused-upgrade case that follows it.
type sessionForwardingUpstream struct {
	*httptest.Server
	// credential is the stem of what /bootstrap.json plants; each fetch
	// gets its own suffix so a test can tell a cached value from a
	// refetched one. Read under no lock: set before the server is handed
	// to a stub and never written afterwards.
	credential string
	presented  chan string
	accept     atomic.Bool
	fetches    atomic.Int32
}

func newSessionForwardingUpstream(t *testing.T, credential string) *sessionForwardingUpstream {
	t.Helper()
	up := &sessionForwardingUpstream{
		credential: credential,
		presented:  make(chan string, 4),
	}
	up.accept.Store(true)
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bootstrap.json":
			fetch := up.fetches.Add(1)
			if up.credential != "" {
				http.SetCookie(w, &http.Cookie{
					Name:  relaysession.CookiePrefix + "4321",
					Value: fmt.Sprintf("%s-%d", up.credential, fetch),
					Path:  "/", HttpOnly: true,
				})
			}
			_, _ = w.Write([]byte(`{"wsUrl":"ws://upstream-perspective/ws"}`))
		case "/ws":
			up.presented <- r.Header.Get(relaysession.Header)
			if !up.accept.Load() {
				// The refusal shape the transport uses for a credential it
				// does not honour.
				http.NotFound(w, r)
				return
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Errorf("upstream accept: %v", err)
				return
			}
			conn.CloseNow()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// dialThroughStub opens the stub's /ws the way the page does, with the
// cookie it exchanged and its own origin.
func dialThroughStub(t *testing.T, srv *Server, cookie *http.Cookie) error {
	t.Helper()
	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", "http://"+srv.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+srv.Addr()+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		conn.CloseNow()
	}
	return err
}

// TestHandleWS_ForwardsTheUpstreamSessionCredential — the stub reaches the
// upstream over loopback or the LAN and would otherwise be trusted for its
// topology alone. Presenting the credential the upstream minted for its
// own local page channel is what makes the carried socket an attributable,
// revocable connection.
func TestHandleWS_ForwardsTheUpstreamSessionCredential(t *testing.T) {
	upstream := newSessionForwardingUpstream(t, "ao1.upstream-local")
	srv := serveWithUpstream(t, upstream.Server)
	cookie := exchangeStubCookie(t, srv)

	if err := dialThroughStub(t, srv, cookie); err != nil {
		t.Fatalf("dial through the stub: %v", err)
	}
	first := <-upstream.presented
	if first == "" || !strings.HasPrefix(first, "ao1.upstream-local-") {
		t.Fatalf("upstream saw session credential %q, want the one it planted", first)
	}

	// Cached: a second carried upgrade costs no second bootstrap fetch.
	before := upstream.fetches.Load()
	if err := dialThroughStub(t, srv, cookie); err != nil {
		t.Fatalf("second dial through the stub: %v", err)
	}
	if got := <-upstream.presented; got != first {
		t.Fatalf("the second upgrade presented %q, want the cached %q", got, first)
	}
	if got := upstream.fetches.Load(); got != before {
		t.Fatalf("a cached credential cost %d extra fetches", got-before)
	}
}

// TestHandleWS_ARefusedUpgradeRefreshesTheCredential — a refused upgrade is
// the one signal a forwarded credential has gone dead (the upstream
// restarted, or the session was revoked). Without the refresh the stub
// would replay the dead value on every reconnect until the process was
// restarted.
func TestHandleWS_ARefusedUpgradeRefreshesTheCredential(t *testing.T) {
	upstream := newSessionForwardingUpstream(t, "ao1.upstream-local")
	srv := serveWithUpstream(t, upstream.Server)
	cookie := exchangeStubCookie(t, srv)

	upstream.accept.Store(false)
	if err := dialThroughStub(t, srv, cookie); err == nil {
		t.Fatal("the upstream refused the upgrade and the dial reported success")
	}
	refused := <-upstream.presented
	if refused == "" {
		t.Fatal("the refused upgrade carried no credential to go stale")
	}

	upstream.accept.Store(true)
	if err := dialThroughStub(t, srv, cookie); err != nil {
		t.Fatalf("dial after the refusal: %v", err)
	}
	if got := <-upstream.presented; got == refused {
		t.Fatalf("the next upgrade replayed the refused credential %q", got)
	}
}

// TestHandleWS_ForwardsNoCredentialItDidNotFetch — the page cannot put a
// header on a WebSocket upgrade, but a local non-browser client holding
// this stub's cookie can, and a forwarded one would let it name a session
// it never obtained.
func TestHandleWS_ForwardsNoCredentialItDidNotFetch(t *testing.T) {
	// An upstream with no session core to speak of: nothing to plant, so
	// nothing legitimate should reach it.
	upstream := newSessionForwardingUpstream(t, "")
	srv := serveWithUpstream(t, upstream.Server)
	cookie := exchangeStubCookie(t, srv)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", "http://"+srv.Addr())
	header.Set(relaysession.Header, "ao1.not-ours")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+srv.Addr()+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		conn.CloseNow()
	}
	if err != nil {
		t.Fatalf("dial through the stub: %v", err)
	}
	if got := <-upstream.presented; got != "" {
		t.Fatalf("a caller-supplied session credential crossed the hop: %q", got)
	}
}

// TestServe_DegradesWhenTheUpstreamHasNoCredential — forwarding is an
// improvement in attribution, never a new requirement for the hop to
// carry. An upstream with no session cookie to give leaves the upgrade
// exactly as it was before forwarding existed: the bearer token alone.
func TestServe_DegradesWhenTheUpstreamHasNoCredential(t *testing.T) {
	upstream := newSessionForwardingUpstream(t, "")
	srv := serveWithUpstream(t, upstream.Server)
	cookie := exchangeStubCookie(t, srv)

	if err := dialThroughStub(t, srv, cookie); err != nil {
		t.Fatalf("dial through the stub: %v", err)
	}
	if got := <-upstream.presented; got != "" {
		t.Fatalf("the upgrade named a session the upstream never issued: %q", got)
	}
}
