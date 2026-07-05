package clientmode

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/transport"
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

func TestServe_InjectsBootstrap(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:  "ws://upstream:1234/",
		Token:  "tok-abc",
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(body)

	// The SPA inspects mode === "client" to hide local-only settings
	// panels (Network bind, Remote endpoints) which would otherwise
	// edit the *remote* backend's state.
	wantSnippet := `window.__AO_BOOTSTRAP__ = {"wsUrl":"ws://upstream:1234/","token":"tok-abc","mode":"client"};`
	if !strings.Contains(bodyStr, wantSnippet) {
		t.Fatalf("bootstrap snippet missing.\nwant: %s\nbody: %s", wantSnippet, bodyStr)
	}
	// Snippet must run before the SPA module loads — i.e., it sits
	// between <head> and the <script type="module"> tag in the source.
	bootstrapAt := strings.Index(bodyStr, "__AO_BOOTSTRAP__")
	moduleAt := strings.Index(bodyStr, `type="module"`)
	if bootstrapAt < 0 || moduleAt < 0 || bootstrapAt >= moduleAt {
		t.Fatalf("bootstrap snippet not before module script (bootstrap@%d, module@%d)", bootstrapAt, moduleAt)
	}
}

func TestServe_InjectsClientIDWhenSet(t *testing.T) {
	srv, err := Serve(Config{
		WSURL:    "ws://upstream:1234/",
		Token:    "tok-abc",
		ClientID: "11111111-2222-3333-4444-555555555555",
		Assets:   fakeAssets(),
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// clientId rides the same injected bootstrap so the remote
	// backend's per-client ui_state bucket stays stable across
	// launches. TestServe_InjectsBootstrap covers the omitted case —
	// its expected snippet has no clientId key (json omitempty).
	wantSnippet := `window.__AO_BOOTSTRAP__ = {"wsUrl":"ws://upstream:1234/","token":"tok-abc","mode":"client","clientId":"11111111-2222-3333-4444-555555555555"};`
	if !strings.Contains(string(body), wantSnippet) {
		t.Fatalf("bootstrap snippet missing clientId.\nwant: %s\nbody: %s", wantSnippet, string(body))
	}
}

func TestServe_EscapesScriptTermination(t *testing.T) {
	// Defensive: a token containing "</script>" would break out of the
	// inline tag if rendering preserved the literal angle brackets.
	// Go's encoding/json escapes "<" and ">" as "<" / ">" by
	// default, which already neutralises this — but we test against the
	// rendered HTML directly so a future refactor that switches encoder
	// settings (json.Encoder.SetEscapeHTML(false)) can't silently break
	// the property.
	srv, err := Serve(Config{
		WSURL:  "ws://h/",
		Token:  "evil</script><script>alert(1)</script>",
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
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The literal "</script>" must not appear inside the injected
	// bootstrap. If it does, a hostile token has bracketed the snippet
	// and could inject script.
	bootstrapStart := strings.Index(bodyStr, "__AO_BOOTSTRAP__")
	if bootstrapStart < 0 {
		t.Fatalf("bootstrap missing")
	}
	// Slice from the bootstrap to the next script tag close — which
	// must be our own snippet's </script>, not a token-injected one.
	snippetEnd := strings.Index(bodyStr[bootstrapStart:], "</script>")
	if snippetEnd < 0 {
		t.Fatalf("bootstrap snippet not closed")
	}
	snippet := bodyStr[bootstrapStart : bootstrapStart+snippetEnd]
	if strings.Contains(snippet, "</script>") {
		t.Fatalf("token-injected </script> leaked through escaping: %s", snippet)
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

	resp, err := http.Get(srv.AppURL() + "assets/index-abc.js")
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
	// Asset responses must carry the long-cache header — Vite hashes
	// content into the filename, so they can be cached for a year.
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") {
		t.Fatalf("Cache-Control %q does not include long max-age", got)
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
