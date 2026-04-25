package clientmode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/transport"
)

// Config configures the local stub server. WSURL is the upstream
// transport endpoint (ws:// or wss://) the SPA's wsClient connects to;
// Token is the per-launch auth secret presented as ?token=<value> on
// upgrade. Both come from the operator-supplied --connect URL.
type Config struct {
	// WSURL is the upstream WebSocket endpoint. Must be ws:// or wss://.
	// Token query params are stripped before this struct is built; the
	// stripped form is what gets injected into the SPA bootstrap so the
	// browser-side wsClient appends `?token=` itself.
	WSURL string

	// Token is the auth secret the SPA presents on the WS upgrade.
	Token string

	// Assets is the embedded SPA filesystem rooted at the directory
	// containing index.html. Required.
	Assets fs.FS

	// BindAddr is the loopback bind for the stub server. Defaults to
	// "127.0.0.1" — clientmode never binds publicly because the SPA is
	// the entire surface, and a remote client tab loaded over HTTP from
	// some other machine would defeat the point.
	BindAddr string

	// Port is the listen port. Zero asks the OS for an ephemeral port —
	// the resolved port is available via Server.AppURL after Serve.
	Port int

	// HTTPReadHeaderTimeout, HTTPReadTimeout, HTTPWriteTimeout map onto
	// net/http.Server fields. Zero values pick safe defaults.
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
}

// Server is the running stub server. The Wails webview loads
// AppURL() which serves the SPA shell (index.html with the bootstrap
// snippet injected) plus the static assets backing it.
type Server struct {
	cfg      Config
	listener net.Listener
	httpSrv  *http.Server

	// indexHTML caches the bootstrap-injected index.html bytes. Built
	// once at Serve time so each request is a memcpy rather than an
	// fs.Read + string mutation. Plain []byte; never mutated after
	// Serve returns.
	indexHTML []byte

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// ParseConnectURL parses the operator-supplied --connect URL. The URL
// must be ws:// or wss:// and carry the token as a `token` query
// parameter. The token-stripped URL is returned in Config.WSURL so
// the wsClient downstream can append the token itself (and so the
// injected bootstrap doesn't double-encode the token in the
// connection string).
//
// Out of scope for v1: an interactive token prompt. If the operator
// hasn't included one in the URL, ParseConnectURL returns an error
// pointing at the missing query param.
func ParseConnectURL(raw string) (Config, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Config{}, errors.New("clientmode: --connect URL cannot be empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return Config{}, fmt.Errorf("clientmode: parse --connect URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return Config{}, fmt.Errorf("clientmode: --connect URL scheme must be ws:// or wss://, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return Config{}, errors.New("clientmode: --connect URL missing host")
	}
	if parsed.User != nil {
		// HTTP basic-auth-style userinfo is not part of the auth model
		// — the server only honours `?token=<value>`. Accepting it
		// silently would let an operator paste a user:pass URL and get
		// an opaque "missing token" failure later; rejecting up front
		// points at the right fix. Don't echo the userinfo in the
		// error message — even a partial is more sensitive than the
		// rest of the URL.
		return Config{}, errors.New("clientmode: --connect URL userinfo not supported; use ?token=<value>")
	}

	q := parsed.Query()
	token := strings.TrimSpace(q.Get("token"))
	if token == "" {
		return Config{}, errors.New("clientmode: --connect URL missing required ?token=<value>")
	}

	// Strip the token from the URL before injecting into the SPA. The
	// wsClient appends `?token=<value>` on its own at upgrade time, so
	// keeping it on the bootstrap URL would double-encode it in the
	// rendered connection URL and confuse log output for very little
	// benefit. We also strip from a defensive cache-poisoning angle:
	// any future log of WSURL won't accidentally leak the token in
	// stack traces or telemetry.
	q.Del("token")
	parsed.RawQuery = q.Encode()

	return Config{
		WSURL: parsed.String(),
		Token: token,
	}, nil
}

// Serve starts the stub HTTP server. Returns when the listener is
// bound — caller can read AppURL() to discover the resolved address
// and hand it to the Wails webview.
//
// The server is single-purpose: serve the embedded SPA bundle with
// the bootstrap manifest injected into index.html. There is no WS
// upgrade, no RPC dispatch, no /bootstrap.json endpoint; the SPA's
// wsClient reads window.__AO_BOOTSTRAP__ on first call and never
// hits the loopback HTTP server again after the initial page load.
func Serve(cfg Config) (*Server, error) {
	if cfg.Assets == nil {
		return nil, errors.New("clientmode: Config.Assets is required")
	}
	if cfg.WSURL == "" {
		return nil, errors.New("clientmode: Config.WSURL is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("clientmode: Config.Token is required")
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1"
	}
	if cfg.HTTPReadHeaderTimeout == 0 {
		cfg.HTTPReadHeaderTimeout = 10 * time.Second
	}
	if cfg.HTTPReadTimeout == 0 {
		cfg.HTTPReadTimeout = 60 * time.Second
	}
	if cfg.HTTPWriteTimeout == 0 {
		cfg.HTTPWriteTimeout = 60 * time.Second
	}

	indexHTML, err := buildInjectedIndex(cfg.Assets, cfg.WSURL, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("clientmode: build injected index: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("clientmode: listen %s: %w", addr, err)
	}

	s := &Server{
		cfg:       cfg,
		listener:  listener,
		indexHTML: indexHTML,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/assets/", withSecurityHeaders(http.FileServerFS(cfg.Assets)))

	// loopbackOnly wraps every route with a Host-header check so an
	// attacker site whose DNS resolves to 127.0.0.1 can't navigate the
	// user's browser to http://attacker.tld:<our-port>/ and have the
	// injected bootstrap script execute under the attacker's origin.
	// Browsers won't apply same-origin to a cross-origin Host header
	// alone — the defense has to live in the response.

	s.httpSrv = &http.Server{
		Handler:           loopbackOnly(mux),
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("clientmode: http serve: %v", err)
		}
	}()

	return s, nil
}

// AppURL returns the loopback URL the Wails webview should load.
// Empty before Serve completes.
func (s *Server) AppURL() string {
	if s.listener == nil {
		return ""
	}
	addr := s.listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/", host, port)
}

// Addr returns the resolved listen address (e.g. "127.0.0.1:54321").
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Shutdown gracefully stops the stub server. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		if s.httpSrv != nil {
			err = s.httpSrv.Shutdown(ctx)
		}
		s.wg.Wait()
	})
	return err
}

// handleIndex serves the bootstrap-injected index.html for "/" and
// "/index.html". Anything else under "/" that isn't /assets/ falls
// through to the SPA shell so client-side routing keeps working
// (the SPA's router handles in-app paths from the rendered shell).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Defensive: only accept GET / HEAD. The static SPA shell has no
	// reason to receive POST/PUT/etc, and rejecting outright keeps the
	// surface tight against hostile probing.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Same headers the transport package puts on the SPA shell. Keeps
	// the security posture identical between the embedded-webview path
	// (transport.go) and the remote-client path (this).
	transport.WriteSecurityHeaders(h)
	// no-cache: the bootstrap holds the token. A stale shell from a
	// disk cache would re-inject a no-longer-valid token, leading to
	// auth failures the user can't easily debug. Forcing a re-fetch on
	// every load also keeps the URL token + injected token in sync if
	// the operator restarts the binary with a new --connect URL.
	h.Set("Cache-Control", "no-store, max-age=0")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(s.indexHTML)
}

// loopbackOnly is a DNS-rebinding defense. The stub binds to 127.0.0.1,
// but a hostile site whose DNS resolves to 127.0.0.1 could navigate the
// user to http://attacker.tld:<our-port>/ and the embedded bootstrap
// script (carrying the WS URL + token) would execute under the
// attacker's origin. Browsers attach the requested Host header to those
// requests verbatim, so we reject anything that isn't a loopback name.
//
// 404 (not 403) is the deliberate response code: the rest of the
// transport / clientmode surface returns NotFound for both auth
// failures and missing paths, so an attacker can't fingerprint the
// server vs an arbitrary web service running on 127.0.0.1 by probing
// rebind-protected vs open URLs.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !transport.IsLoopbackHost(r.Host) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders wraps the static asset handler with the same
// security headers the Wails-managed path uses. Unlike the SPA shell
// (which holds the bootstrap token), Vite-hashed asset paths are
// content-addressable forever, so they get a long max-age.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		transport.WriteSecurityHeaders(h)
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// bootstrapTag is the placeholder marker used inside the injection
// snippet. We do NOT modify the SPA source — the snippet is prepended
// verbatim before the existing <head> contents, immediately after the
// <head> tag, so it executes before any module preload completes.
const bootstrapTag = "<head>"

// buildInjectedIndex reads index.html from the embedded fs and
// inserts the bootstrap script immediately after the opening <head>
// tag. The script declares window.__AO_BOOTSTRAP__ so wsClient.ts
// reads it on its first call and skips the /bootstrap.json fetch.
//
// JSON marshalling of the {wsUrl, token} pair is the single source of
// the literal we inject — manual string concatenation would risk
// quote-escaping bugs against tokens or URLs containing arbitrary
// bytes.
func buildInjectedIndex(assets fs.FS, wsURL, token string) ([]byte, error) {
	indexFile, err := assets.Open("index.html")
	if err != nil {
		return nil, fmt.Errorf("open index.html: %w", err)
	}
	defer indexFile.Close()
	raw, err := io.ReadAll(indexFile)
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	// mode: "client" tells the SPA it is attached to a remote backend
	// rather than a local Wails-embedded transport. Local-only settings
	// panels (Network bind toggle, Remote endpoints list) inspect this
	// field and render a "edit from your local install" placeholder
	// instead of letting the user mutate the *remote* server's settings
	// through it. The transport's Bootstrap (server.go) does NOT carry
	// this field; defaultBootstrap in wsClient.ts treats absence as
	// "local".
	bootstrapJSON, err := json.Marshal(struct {
		WSURL string `json:"wsUrl"`
		Token string `json:"token"`
		Mode  string `json:"mode"`
	}{
		WSURL: wsURL,
		Token: token,
		Mode:  "client",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal bootstrap: %w", err)
	}

	// </script>-safe rendering. encoding/json's default (escapeHTML=true)
	// already turns "<" / ">" / "&" into "<" etc., so a token
	// literal like "</script>" comes out as "</script>" and
	// can't break out of the inline tag. The defensive ReplaceAll below
	// is a belt-and-braces measure: if a future refactor sets
	// SetEscapeHTML(false) for some other reason, this still keeps the
	// </script> exit blocked. Same trick browsers' own
	// JSON.stringify-into-script-tag callers use.
	bootstrapJSONStr := strings.ReplaceAll(string(bootstrapJSON), "</", "<\\/")

	snippet := fmt.Sprintf(`<script>window.__AO_BOOTSTRAP__ = %s;</script>`, bootstrapJSONStr)

	idx := strings.Index(string(raw), bootstrapTag)
	if idx < 0 {
		return nil, fmt.Errorf("index.html missing %q tag", bootstrapTag)
	}
	insertAt := idx + len(bootstrapTag)

	out := make([]byte, 0, len(raw)+len(snippet))
	out = append(out, raw[:insertAt]...)
	out = append(out, snippet...)
	out = append(out, raw[insertAt:]...)
	return out, nil
}
