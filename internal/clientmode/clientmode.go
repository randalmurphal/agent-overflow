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
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/loopback"
	"agent-overflow/internal/transport"
)

// Config configures the local stub server. WSURL is the upstream
// transport endpoint (ws:// or wss://) this stub carries the SPA's
// WebSocket to; Token is that backend's session credential. Both come
// from the operator-supplied --connect URL, and neither is ever served
// to the page.
type Config struct {
	// WSURL is the upstream WebSocket endpoint. Must be ws:// or wss://.
	// Token query params are stripped before this struct is built; the
	// credential travels in Token and is attached by the proxy, not by
	// anything the page can see.
	WSURL string

	// Token is the upstream backend's session credential. Held
	// server-side for the life of the process: the stub presents it when
	// it probes the upstream manifest and when it carries the upgrade.
	Token string

	// ClientID is this installation's durable UI-state client
	// identity, threaded onto the page URL as ?cid= (the same parameter
	// the native shells stamp) so the remote backend's per-client
	// ui_state bucket stays stable across launches — the stub's
	// ephemeral port changes the webview origin, and its localStorage,
	// every run. Optional: when empty the SPA falls back to a
	// best-effort browser-cached identity.
	ClientID string

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

// Server is the running stub server. The Wails webview loads AppURL(),
// which serves the SPA shell plus the static assets backing it, answers
// the SPA's manifest fetch, and carries the SPA's WebSocket to the
// upstream backend.
//
// The upstream credential never leaves this process. The page is served
// this stub's OWN credential — a one-time ticket on the URL, exchanged
// for an HttpOnly cookie — and the stub attaches the upstream token
// itself when it carries the upgrade. That is what makes the SPA
// identical in both modes: one origin, one cookie, one same-origin
// wsUrl, whether the backend is in this process or across a network.
type Server struct {
	cfg      Config
	listener net.Listener
	httpSrv  *http.Server

	// cred is this stub's own page credential, unrelated to the upstream
	// token in cfg. Per launch, like the transport's.
	cred *transport.Credential

	// launchID identifies this stub launch in the manifest, the same
	// non-credential marker the transport publishes.
	launchID string

	// indexHTML caches the SPA shell bytes read at Serve time, so each
	// request is a memcpy rather than an fs.Read. Plain []byte; never
	// mutated after Serve returns.
	indexHTML []byte

	// remote records whether the upstream endpoint is off this machine,
	// the same locality bit the transport's own manifest carries.
	remote bool

	// upstreamBootstrapURL is the upstream backend's own /bootstrap.json
	// (derived from Config.WSURL), which the probe hits with the
	// configured token to learn whether that token is still honoured.
	upstreamBootstrapURL string

	// wsProxy carries the SPA's WebSocket to the upstream /ws, adding
	// the upstream credential on the way out. Built once at Serve time.
	wsProxy *httputil.ReverseProxy

	// probeClient bounds the upstream probe. Overridable in tests.
	probeClient *http.Client

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// ParseConnectURL parses the operator-supplied --connect URL. The URL
// must be ws:// or wss:// and carry the upstream's session token as a
// `token` query parameter. That is the operator's own composition: a
// page URL carries a one-time ticket, not the token, so the token comes
// from the upstream host (network.Settings.Token) rather than from a
// copied share link. The credential is split out into Config.Token and
// the stripped URL is returned in Config.WSURL, so the endpoint this
// process dials and logs is free of it and the proxy is the only thing
// that re-attaches it.
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

	// Strip the token from the URL. The proxy rewrite is the only
	// thing that re-attaches it (as a bearer header), so the endpoint
	// string this process stores, dials and logs is free of it: any
	// future log of WSURL cannot carry the credential into stack
	// traces or telemetry.
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
// Three routes, all on one loopback origin: the SPA bundle,
// /bootstrap.json (which exchanges this stub's one-time page ticket for
// its HttpOnly cookie and reports the upstream's verdict on the
// configured token — see handleBootstrap), and /ws, which carries the
// SPA's WebSocket to the upstream with the upstream credential attached
// here rather than in the page (see handleWS).
//
// Still no RPC dispatch, no event bus, no method table: the proxy is a
// byte carrier that adds one header, not a second backend.
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

	parsedWSURL, err := url.Parse(cfg.WSURL)
	if err != nil {
		return nil, fmt.Errorf("clientmode: parse websocket URL: %w", err)
	}
	indexHTML, err := readIndexHTML(cfg.Assets)
	if err != nil {
		return nil, fmt.Errorf("clientmode: read index.html: %w", err)
	}
	upstreamBootstrap, err := upstreamBootstrapURL(cfg.WSURL)
	if err != nil {
		return nil, fmt.Errorf("clientmode: derive upstream bootstrap URL: %w", err)
	}
	wsProxy, err := newWSProxy(cfg.WSURL, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("clientmode: build websocket proxy: %w", err)
	}
	cred, err := transport.NewCredential("")
	if err != nil {
		return nil, fmt.Errorf("clientmode: mint page credential: %w", err)
	}
	launchID, err := transport.NewToken()
	if err != nil {
		return nil, fmt.Errorf("clientmode: mint launch id: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("clientmode: listen %s: %w", addr, err)
	}

	s := &Server{
		cfg:                  cfg,
		listener:             listener,
		cred:                 cred,
		launchID:             launchID,
		indexHTML:            indexHTML,
		remote:               !loopback.EndpointAuthority(parsedWSURL.Host),
		upstreamBootstrapURL: upstreamBootstrap,
		wsProxy:              wsProxy,
		probeClient:          &http.Client{Timeout: bootstrapProbeTimeout},
	}

	mux := http.NewServeMux()
	// "/{$}" is the exact root and nothing else, so every other path
	// reaches the file server. The bundle's root-level files —
	// boot-theme.js, favicon.svg — live beside /assets/ in dist and are
	// unreachable if the shell answers for them: served as text/html
	// under X-Content-Type-Options: nosniff, the boot script would be
	// refused and the first-paint theme stamp would silently stop
	// applying on this origin only. Serving the whole bundle from one
	// file server also matches what the transport does with the same FS.
	mux.HandleFunc("/{$}", s.handleIndex)
	mux.Handle("/", withSecurityHeaders(http.FileServerFS(cfg.Assets)))
	mux.HandleFunc("/bootstrap.json", s.handleBootstrap)
	mux.HandleFunc("/ws", s.handleWS)

	// loopbackOnly wraps every route with a Host-header check so a site
	// whose DNS resolves to 127.0.0.1 cannot navigate the user's browser
	// to http://someone-elses-name:<our-port>/ and have the SPA run under
	// that name. Browsers do not apply same-origin to a cross-origin Host
	// header alone — the check has to live in the response.

	s.httpSrv = &http.Server{
		Handler:           loopbackOnly(mux),
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
	}

	s.wg.Go(func() {
		if err := s.httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("clientmode: http serve: %v", err)
		}
	})

	return s, nil
}

// AppURL returns the loopback URL the Wails webview should load, with a
// freshly minted one-time page ticket. Empty before Serve completes.
//
// Every call mints its own ticket, which is what makes the reload
// keybinding (main_desktop.go hands this method itself to
// uikeys.BrowserWithReload) reload a page whose first ticket was already
// spent.
//
// Two more parameters ride the URL, both facts about the local shell
// rather than credentials, and both surviving the SPA's scrub of the
// ticket:
//
//   - mode=client tells the SPA it is attached to a remote backend, so
//     the settings panels whose RPCs would edit the REMOTE machine's
//     state render a placeholder. It travels on the URL rather than in
//     the manifest because the SPA reads it once at module load, before
//     any fetch resolves.
//   - cid=<client id> is this installation's durable ui_state identity,
//     the same parameter the native shells stamp (main.go
//     appURLWithClientID); the stub's ephemeral port changes the page
//     origin, and with it browser storage, on every launch.
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
	ticket, err := s.cred.MintPageTicket()
	if err != nil {
		log.Printf("clientmode: mint page ticket: %v", err)
		return ""
	}
	pageURL := fmt.Sprintf("http://%s:%s/?%s=%s&mode=client", host, port, transport.PageTicketParam, ticket)
	if s.cfg.ClientID != "" {
		pageURL += "&cid=" + url.QueryEscape(s.cfg.ClientID)
	}
	return pageURL
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

// handleIndex serves index.html for the exact root and nothing else.
// The SPA has no client-side router — it is loaded once, at "/", and
// only ever rewrites its own query string — so every other path is a
// bundle file or a 404, and answering one with the shell would just
// mislabel it as HTML.
//
// The shell is served verbatim: nothing is injected into it any more.
// The SPA learns where its socket is from /bootstrap.json, exactly as it
// does when the transport serves it, which is the whole point of the
// stub owning an origin of its own.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Only GET / HEAD. The static SPA shell has no reason to receive
	// POST/PUT/etc, and refusing outright keeps the surface tight.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Same headers the transport package puts on the SPA shell. Keeps
	// the security posture identical between the embedded-webview path
	// (transport.go) and the remote-client path (this).
	transport.WriteSecurityHeaders(h, transport.CSPProduction)
	// no-store, matching the transport's policy for the entry shell: a
	// deploy must never be shadowed by a cached shell, and the local
	// webview loads it once per process so nothing is gained by keeping
	// it around.
	h.Set("Cache-Control", "no-store, max-age=0")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(s.indexHTML)
}

// handleWS carries the SPA's WebSocket to the upstream backend.
//
// The page authenticates to this stub with its own cookie, and the
// upstream credential is attached on the way out by the proxy's rewrite
// (newWSProxy), so it exists only in this process's memory. Both checks
// are the transport's own: the same origin rule, because the page cookie
// reaches this listener from any page on this host, and the same
// Authenticate that every other route on both servers uses.
//
// A refusal is the transport's 404, which a browser surfaces as a bare
// socket failure; the SPA's reconnect ladder and its /bootstrap.json
// revalidation own the verdict from there, unchanged.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !transport.OriginAllowed(r, nil) || !s.cred.Authenticate(r) {
		http.NotFound(w, r)
		return
	}
	s.wsProxy.ServeHTTP(w, r)
}

// bootstrapProbeTimeout bounds one upstream credential probe. The SPA
// treats a probe failure as transient and stays on its reconnect
// ladder, so a slow answer only delays the verdict — it never wedges
// anything.
const bootstrapProbeTimeout = 5 * time.Second

// handleBootstrap serves the stub's own manifest, and is where the page
// exchanges its one-time ticket for this stub's HttpOnly cookie. It has
// two callers, both the SPA: the first load, and the reconnect path that
// refetches after consecutive connect failures to learn whether its
// session is still honoured.
//
// Before answering, it asks the upstream whether the configured token is
// still good. The webview page cannot ask (a cross-origin fetch dies on
// the read rules, and a refused upgrade is a bare 1006), but this Go
// process can, which is what turns "backend restarted, token dead" from
// an indefinite reconnect loop into the SPA's terminal unauthorized
// state. The upstream still answers a wrong token with its
// unfingerprintable 404, and only this loopback stub — which already
// holds the token — observes it.
//
// The response body is the stub's OWN manifest, not the upstream's: the
// wsUrl must name THIS origin, since that is where the page's socket
// goes and the only wsUrl the SPA accepts. Status mapping mirrors the
// upstream transport's semantics (transport/AGENTS.md § "Credentials and
// refusal shapes"): refusal maps to 404, everything transient — network
// failure included — to 503 so the SPA keeps its ladder.
//
// The ticket exchange runs BEFORE the probe, so a page load during an
// upstream outage still lands its cookie and its retry costs no ticket.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !transport.OriginAllowed(r, nil) {
		http.NotFound(w, r)
		return
	}
	if !s.cred.Exchange(w, r) {
		http.NotFound(w, r)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstreamBootstrapURL, nil)
	if err != nil {
		log.Printf("clientmode: build bootstrap probe request: %v", err)
		http.Error(w, "bootstrap probe unavailable", http.StatusServiceUnavailable)
		return
	}
	// Header, not query: the upstream's `?t=` slot now takes a one-time
	// page ticket, and this is a client that is not a browser presenting
	// the session token it was configured with.
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.probeClient.Do(req)
	if err != nil {
		// Unreachable upstream is indistinguishable from a mid-outage
		// backend; the SPA's ladder owns retrying.
		http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
		return
	}
	// The verdict is the status code; the body is drained only so the
	// connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// The token is no longer honoured. Same set the SPA's
		// CREDENTIAL_REFUSED_STATUSES treats as definitive.
		http.NotFound(w, r)
		return
	default:
		// 503 readiness gate, 500 startup failure, anything unexpected:
		// transient by the upstream's own contract.
		http.Error(w, "backend not ready", http.StatusServiceUnavailable)
		return
	}

	manifest, err := s.manifestJSON(r)
	if err != nil {
		log.Printf("clientmode: build bootstrap manifest: %v", err)
		http.Error(w, "bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	transport.WriteSecurityHeaders(h, transport.CSPProduction)
	// Same no-store posture as the shell: a manifest from a previous
	// launch names a socket that no longer exists.
	h.Set("Cache-Control", "no-store, max-age=0")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(manifest)
}

// upstreamBootstrapURL maps the configured WS endpoint onto the upstream
// transport's manifest endpoint: ws→http / wss→https, and a trailing
// /ws path segment (the transport's upgrade route) replaced by
// /bootstrap.json so a reverse-proxy path prefix survives.
func upstreamBootstrapURL(wsURL string) (string, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse ws url: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	prefix := strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), "/ws")
	parsed.Path = prefix + "/bootstrap.json"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// loopbackOnly requires the request's Host to name loopback, which the
// bind address alone does not establish. The stub binds 127.0.0.1, but a
// DNS name that resolves to 127.0.0.1 reaches it just as well: a page
// navigated to http://that.name:<our-port>/ arrives here over the
// loopback interface, and the document it gets back — including the
// boot script that fetches the WS URL — would run under that name's
// origin rather than ours. Browsers send the requested Host verbatim,
// so refusing a non-loopback one refuses the whole shape.
// loopback.HostHeader is the strict predicate that does it, and it
// refuses names rather than classifying them for exactly this reason.
//
// 404 (not 403) is the deliberate response code: the rest of the
// transport / clientmode surface returns NotFound for both credential
// failures and missing paths, so probing cannot tell a guarded route
// from an absent one, or this server from any other service on
// 127.0.0.1.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopback.HostHeader(r.Host) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders wraps the static asset handler with the same
// security headers the Wails-managed path uses. Cache-Control is
// no-store for the same reason the transport server uses it for
// loopback peers (see transport.withAssetHeaders): this stub only ever
// serves the local embedded webview, which loads the SPA once per
// process — caching can't pay off, but cacheable scripts pin their
// decoded text in the renderer's in-memory HTTP cache for the page's
// lifetime.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		transport.WriteSecurityHeaders(h, transport.CSPProduction)
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// manifestJSON marshals the manifest this stub answers /bootstrap.json
// with. Built per request rather than once at Serve time, because wsUrl
// is derived from the request's own Host header — the same rule the
// transport uses (transport.DeriveWSURL), and the reason the SPA can
// require a same-origin wsUrl on every path with no exemptions.
//
// It carries no credential. The page's credential is the cookie the
// exchange just issued, and the upstream's token never leaves this
// process. It carries no client identity either: that rides the page URL
// as ?cid=, the same parameter the native shells stamp, because the SPA
// reads it synchronously at module load before any fetch resolves.
//
// The locality bit is derived from the UPSTREAM endpoint, not from this
// loopback listener: what the SPA gates on it is whether the backend
// doing the work is this machine's, which is exactly what a browser
// attached straight to that backend would have been told.
//
// KNOWN LIMITATION — the manifest carries no `harness` bit. The
// transport bootstrap has one (transport.Bootstrap.Harness) and the SPA
// keys its harness bridge on it, so `agent-overflow --connect` pointed
// at a harness or soak instance gets a page whose bridge never arms:
// `ui snapshot`, `perf` and `bench` go unanswered on that page even over
// loopback, where the delivery would otherwise work. Forwarding it means
// reading the upstream's own manifest body here and deciding, field by
// field, which of its answers describe a backend the page may believe
// (the store identity deliberately must NOT: absence disables the
// client-side replica rather than pointing it at another backend's
// database). That is its own change; the workflow is served today by
// opening the harness URL directly.
func (s *Server) manifestJSON(r *http.Request) ([]byte, error) {
	manifest, err := json.Marshal(struct {
		WSURL    string `json:"wsUrl"`
		LaunchID string `json:"launchId,omitempty"`
		Remote   bool   `json:"remote,omitempty"`
	}{
		WSURL:    transport.DeriveWSURL(r),
		LaunchID: s.launchID,
		Remote:   s.remote,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal bootstrap: %w", err)
	}
	return manifest, nil
}

// readIndexHTML reads the SPA shell once at Serve time so each request
// is a memcpy. Nothing is inserted into it: the page learns everything
// it needs from /bootstrap.json on its own origin.
func readIndexHTML(assets fs.FS) ([]byte, error) {
	indexFile, err := assets.Open("index.html")
	if err != nil {
		return nil, fmt.Errorf("open index.html: %w", err)
	}
	defer indexFile.Close()
	raw, err := io.ReadAll(indexFile)
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}
	return raw, nil
}

// newWSProxy builds the carrier for the SPA's WebSocket. The rewrite is
// the whole of it:
//
//   - Address the upstream's own /ws, keeping any path prefix the
//     operator's URL carried (a reverse proxy in front of the backend).
//     Host is cleared so the request goes out naming the upstream, which
//     is what the upstream's own loopback host guard expects when the
//     endpoint is reached through an SSH tunnel.
//   - Attach the upstream credential as a bearer header. This is the
//     only place it appears on a wire, and it replaces the page's own
//     credential rather than travelling beside it.
//   - Drop the browser's Cookie and Origin. This stub's cookie means
//     nothing upstream, and this stub's origin is not one the upstream
//     serves — an Origin it does not recognise is refused, correctly,
//     because a proxy hop is not a browser. Removing it presents the
//     stub as what it is: a client that is not a browser.
//
// Everything else is byte-transparent: the handshake key, the requested
// subprotocols and the compression extension all pass through, so the
// browser and the upstream negotiate with each other and this process
// only carries frames.
//
// httputil.ReverseProxy handles the 101 itself — it hijacks the
// connection and splices both directions, which also clears the HTTP
// server's write deadline (net/http hijackLocked), so the stub's request
// timeouts cannot cut a healthy long-lived socket.
func newWSProxy(wsURL, token string) (*httputil.ReverseProxy, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parse ws url: %w", err)
	}
	target := *parsed
	switch parsed.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	default:
		return nil, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	if target.Path == "" || target.Path == "/" {
		target.Path = "/ws"
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawQuery = target.RawQuery
			pr.Out.Host = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// An unreachable upstream is the SPA's ordinary outage: its
			// reconnect ladder retries and its manifest refetch owns the
			// "credential is dead" verdict. Log once, answer the shape a
			// refused upgrade already has.
			log.Printf("clientmode: websocket proxy: %v", err)
			http.Error(w, "backend unreachable", http.StatusServiceUnavailable)
		},
	}, nil
}
