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
	"testing"
	"testing/fstest"
	"time"

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
// what a hostile token could do to an inline script tag — the class is
// gone rather than defended.
func TestServe_ServesTheShellVerbatim(t *testing.T) {
	srv := serveStub(t, Config{
		WSURL: "ws://upstream:1234/",
		Token: "evil</" + "script><" + "script>alert(1)</" + "script>",
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
	if strings.Contains(string(body), "alert(1)") {
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

// TestServe_RejectsRebindHost guards against a DNS-rebinding attack:
// the stub server binds to 127.0.0.1, but a hostile page whose DNS
// resolves to 127.0.0.1 could direct the user's browser to
// `http://attacker.tld:<port>/` — the request would arrive at our
// server with `Host: attacker.tld`, and without a Host check the
// bootstrap-injected page (carrying the WS URL + token) would execute
// in the attacker's origin context. Reject anything that isn't a
// loopback name.
//
// 404 is intentional: it matches the existing transport server's
// "indistinguishable from a real 404" auth-failure shape so the
// presence of this defense isn't a fingerprint.
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
	// Request.Host is "". The defense still rejects an empty Host
	// (isLoopbackHost("") returns false) but the Go client makes that
	// branch unreachable from a real request.
	cases := []struct {
		name string
		path string
		host string
	}{
		{"index attacker.tld", "/", "attacker.tld"},
		{"index attacker.tld with port", "/", "attacker.tld:8080"},
		{"asset attacker.tld", "/assets/index-abc.js", "attacker.tld"},
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
			// whole point is that the attacker's origin can't read
			// the token. Defense-in-depth assertion.
			if strings.Contains(string(body), "__AO_BOOTSTRAP__") {
				t.Fatalf("404 response leaked bootstrap snippet")
			}
		})
	}
}

// TestIsLoopbackHost covers the rebind-defense decision function
// directly. The integration test (TestServe_RejectsRebindHost) can't
// exercise the empty-Host branch because Go's http.Client substitutes
// the URL's authority for Request.Host == "", but a hand-crafted
// raw-TCP client can — make sure the decision function rejects it.
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},
		{"127.0.0.1", true},
		{"127.0.0.1:54321", true},
		{"localhost", true},
		{"localhost:8080", true},
		{"LocalHost", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"::1", false}, // unbracketed IPv6 is malformed for HTTP Host
		{"attacker.tld", false},
		{"attacker.tld:80", false},
		{"192.168.1.5", false},
		{"127.0.0.1.attacker.tld", false}, // string-prefix isn't enough
		{"localhost.attacker.tld", false},
		{"127.0.0.2", false},         // any other 127.x.x.x is rejected
		{"[fe80::1234]:8080", false}, // non-loopback IPv6 with port
	}
	for _, c := range cases {
		if got := transport.IsLoopbackHost(c.host); got != c.want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
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

func TestUpstreamBootstrapURL(t *testing.T) {
	tests := []struct {
		wsURL string
		want  string
	}{
		{"ws://host:1234/ws", "http://host:1234/bootstrap.json"},
		{"wss://host/ws", "https://host/bootstrap.json"},
		{"wss://host/ao/ws", "https://host/ao/bootstrap.json"},
		{"ws://host:1234/", "http://host:1234/bootstrap.json"},
		{"ws://host:1234", "http://host:1234/bootstrap.json"},
	}
	for _, tt := range tests {
		got, err := upstreamBootstrapURL(tt.wsURL)
		if err != nil {
			t.Errorf("upstreamBootstrapURL(%q): %v", tt.wsURL, err)
			continue
		}
		if got != tt.want {
			t.Errorf("upstreamBootstrapURL(%q) = %q, want %q", tt.wsURL, got, tt.want)
		}
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
	conn, _, err := websocket.Dial(ctx, "ws://"+srv.Addr()+"/ws", &websocket.DialOptions{HTTPHeader: header})
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
