package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Bootstrap is the JSON document the SPA fetches at /bootstrap.json on
// page load. It tells the client where the WS endpoint lives and what
// token to present. The wsUrl is built from the request's Host header
// so a LAN bind serves a LAN-reachable URL, not the loopback string
// the server resolved at bind time.
type Bootstrap struct {
	WSURL string `json:"wsUrl"`
	Token string `json:"token"`
}

// MaxRetainedFormerSrvs caps how many retired http.Servers Rebind keeps
// around for in-flight WS draining. Beyond this we force-Close the
// oldest so a pathological rebind loop (e.g. a UI that toggles BindAll
// rapidly) can't accumulate http.Server instances indefinitely. Real
// users hit zero retained servers at steady state — graceful shutdown
// drains the slice naturally on its 5s timeout.
const MaxRetainedFormerSrvs = 4

// errServerShutdown is the cancel-cause attached to rootCtx when
// Shutdown begins. Lets per-connection handlers distinguish "we shut
// down" from "client disconnected" via context.Cause. Internal only.
var errServerShutdown = errors.New("transport: server shut down")

// Config configures Server. Zero values default to safe choices —
// loopback bind, ephemeral port, freshly-generated token.
type Config struct {
	// BindAddr is the listen interface. Defaults to "127.0.0.1" so the
	// server is unreachable from the LAN unless the user opts in. Set
	// to "0.0.0.0" (or a specific interface IP) to expose.
	BindAddr string

	// Port is the listen port. Zero asks the OS for an ephemeral port —
	// the resolved port is available via Server.Addr() after Start.
	Port int

	// Token is the auth secret presented as ?token=<value> on WS
	// upgrade. Empty asks Server.New to generate one (recommended).
	Token string

	// Dispatcher hosts the registered RPC methods. Required.
	Dispatcher *Dispatcher

	// EventBus pushes server-initiated events. Required.
	EventBus *EventBus

	// AssetHandler serves the SPA assets. Optional — when nil, the
	// HTTP server returns 404 for non-RPC paths. main.go wires this
	// to http.FileServer(http.FS(embeddedAssets)) so the same process
	// hosts the bundle. Pass http.FS-backed handlers only; http.Dir
	// would expose path traversal of the local filesystem.
	AssetHandler http.Handler

	// DesignHandler is a late-bound lookup for the per-thread design
	// file server, registered at the /design/ prefix. It's a getter
	// rather than a plain http.Handler because the underlying handler
	// is constructed during the App's ServiceStartup lifecycle, which
	// runs AFTER bootTransport calls New() — a value snapshot taken at
	// config time would always be nil and the route would never
	// register. The mux registration consults the getter per-request
	// so a handler that becomes available mid-flight is picked up
	// without restarting the server.
	//
	// Optional — when this field is nil, no /design/ route registers
	// and design requests fall through to the asset handler (returning
	// the SPA shell with X-Frame-Options: DENY, the iframe-display
	// failure mode we explicitly want to avoid; see DesignPreviewPanel
	// for the gating that protects against this on the client side).
	//
	// When the getter returns nil at request time the route returns
	// 404, which the iframe handles cleanly (the parent receives an
	// onerror event and re-tries on the next workdir-ready signal).
	//
	// main.go wires this to App.DesignServer (the bound method, no
	// parens) which returns design.FileHandler(designBaseDir) once
	// initSubsystems has run. That handler already strips the /design
	// prefix and injects the diagnostic-capture script into HTML
	// responses. The route is wrapped by loopbackHostGuard so design
	// dirs are unreachable from a hostile rebound DNS origin — the
	// bytes the agent writes can include user material.
	DesignHandler func() http.Handler

	// ReadLimit caps the byte size of a single inbound WS message.
	// Zero defaults to DefaultReadLimit (75 MiB).
	ReadLimit int64

	// MaxConcurrentRPCs caps how many in-flight RPC handlers a single
	// connection can have at once. Zero defaults to
	// DefaultMaxConcurrentRPCs (64).
	MaxConcurrentRPCs int

	// OriginPatterns is the WS origin allow-list. Empty (default) uses
	// InsecureSkipVerify — appropriate for loopback. Phase E (LAN bind
	// toggle) populates this with the configured remote origins.
	OriginPatterns []string

	// RequireReadyForBootstrap makes /bootstrap.json return 503 until
	// MarkReady is called. Default false preserves the normal desktop
	// path, where the frontend may connect while Wails is still running
	// ServiceStartup. Headless launchers use this to publish the port
	// early but hold navigation until the backend is actually ready.
	RequireReadyForBootstrap bool

	// CrossOriginIsolate makes every asset and design response carry
	// cross-origin isolation headers (COOP/COEP/CORP) so the SPA runs
	// crossOriginIsolated and measureUserAgentSpecificMemory works.
	// Diagnostic opt-in (AGENT_OVERFLOW_RENDERER_DIAG) — COEP blocks
	// remote subresources while on. See WriteCrossOriginIsolationHeaders.
	CrossOriginIsolate bool

	// HTTPReadHeaderTimeout, HTTPReadTimeout, HTTPWriteTimeout,
	// HTTPIdleTimeout map onto net/http.Server fields. Zero values
	// pick safe defaults documented in New().
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration
}

// Server is the HTTP+WS transport. The HTTP side serves the SPA + a
// bootstrap manifest; the WS side handles RPC and event push.
type Server struct {
	cfg Config

	// mu guards listener / srv / addr / formerSrvs / originPatterns.
	// Held briefly during Rebind so a concurrent Addr() reader sees a
	// coherent value.
	mu       sync.Mutex
	listener net.Listener
	srv      *http.Server
	addr     string

	// originPatterns is the live WS origin allow-list, mirroring
	// Config.OriginPatterns at boot and updated atomically by Rebind so
	// a LAN-bind toggle can flip the allow-list at the same moment as
	// the listener swap. Empty means "InsecureSkipVerify" — appropriate
	// for loopback (no browser origin to compare against). Guarded by mu.
	originPatterns []string

	// formerSrvs are http.Servers retired by Rebind that are still
	// draining hijacked WS connections. We keep them around so existing
	// clients keep working after the bind toggle, and Close them on
	// final Server.Shutdown to bound the leak window. The slice is
	// guarded by mu.
	//
	// Two bounding mechanisms keep the slice from unbounded growth:
	//  - on Rebind, if the slice exceeds MaxRetainedFormerSrvs we
	//    force-Close the oldest entry and remove it. Hard cap.
	//  - the deferred Shutdown goroutine that retires an old server
	//    naturally drops it from the slice when the graceful shutdown
	//    completes — so a healthy app drains the slice without ever
	//    hitting the cap.
	formerSrvs []*http.Server

	// rebindMu serialises Rebind calls so two concurrent toggles can't
	// race each other into a torn listener+server pair. Separate from
	// mu so Addr() reads aren't blocked behind a slow rebind.
	rebindMu sync.Mutex

	token string

	// rootCtx + rootCancel scope every connection's lifetime to the
	// server. Shutdown cancels rootCtx so live readLoops exit
	// promptly even when the WS itself is idle. Rebind deliberately
	// reuses this context so a re-bind doesn't kill existing WS
	// connections — only Shutdown should terminate them.
	rootCtx    context.Context
	rootCancel context.CancelCauseFunc

	// serveErr surfaces async errors from the listener goroutine.
	// Buffered 1 so a single failure can be observed without a
	// reader; subsequent errors drop.
	serveErr chan error

	wg        sync.WaitGroup
	stopOnce  sync.Once
	startOnce sync.Once
	startErr  error
	// shutDown flips true once Shutdown begins. Rebind checks this so
	// a rebind racing a concurrent Shutdown drops cleanly rather than
	// installing a fresh listener that nobody will ever close.
	shutDown atomic.Bool

	ready atomic.Bool

	startupFailed atomic.Bool

	// remoteConns counts live non-loopback WebSocket connections.
	// Feeds HasRemoteClient, which gates work that only benefits
	// remote viewers (the highlight seed push). Note tunneled remotes
	// (SSH local forward) arrive AS loopback and are invisible here —
	// they get the ordinary RPC path, today's behavior.
	remoteConns atomic.Int64
}

// New constructs a Server. Generates a token if one wasn't provided.
// Does not start listening — caller invokes Start.
func New(cfg Config) (*Server, error) {
	if cfg.Dispatcher == nil {
		return nil, fmt.Errorf("transport: Config.Dispatcher is required")
	}
	if cfg.EventBus == nil {
		return nil, fmt.Errorf("transport: Config.EventBus is required")
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
	if cfg.HTTPIdleTimeout == 0 {
		cfg.HTTPIdleTimeout = 120 * time.Second
	}
	token := cfg.Token
	if token == "" {
		t, err := NewToken()
		if err != nil {
			return nil, fmt.Errorf("transport: generate token: %w", err)
		}
		token = t
	}
	// Copy origin patterns so a caller mutating the Config slice after
	// New can't reach into the live allow-list. The mu-guarded mirror
	// is the only source of truth at handshake time; Config.OriginPatterns
	// is read once here and never again.
	var originPatterns []string
	if len(cfg.OriginPatterns) > 0 {
		originPatterns = append(originPatterns, cfg.OriginPatterns...)
	}
	s := &Server{
		cfg:            cfg,
		token:          token,
		originPatterns: originPatterns,
		serveErr:       make(chan error, 1),
	}
	if !cfg.RequireReadyForBootstrap {
		s.ready.Store(true)
	}
	return s, nil
}

// Start begins listening. Returns when the listener is bound — caller
// can then read Addr() to discover the resolved address. The HTTP
// server itself is served on a goroutine; async listener errors are
// surfaced via ServeErr().
//
// Start is a one-shot: a second call (after a successful or failed
// first) returns an error rather than installing a parallel listener.
// Use a fresh Server to re-bind.
func (s *Server) Start() error {
	called := false
	s.startOnce.Do(func() {
		called = true
		s.startErr = s.start()
	})
	if !called {
		return fmt.Errorf("transport: Server.Start already called")
	}
	return s.startErr
}

func (s *Server) start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("transport: listen %s: %w", addr, err)
	}
	s.rootCtx, s.rootCancel = context.WithCancelCause(context.Background())

	srv := s.buildHTTPServer()
	s.mu.Lock()
	s.listener = listener
	s.srv = srv
	s.addr = listener.Addr().String()
	s.mu.Unlock()

	s.serve(srv, listener)
	return nil
}

// buildHTTPServer constructs an http.Server with the same handler /
// timeouts / BaseContext used at boot. Extracted so Rebind can spin up
// a parallel server pointing at a fresh listener while the old one
// keeps draining hijacked WS connections.
//
// Loopback paths (/bootstrap.json, /ws) are wrapped in a Host-header
// guard that fires when the live origin allow-list is empty (loopback
// mode). A hostile site whose DNS resolves to 127.0.0.1 can otherwise
// navigate the user to http://attacker.tld:<our-port>/bootstrap.json
// and read the bootstrap token; rejecting non-loopback Hosts in
// loopback mode closes that vector. On LAN bind (origin allow-list
// non-empty) the guard is a pass-through — origin validation already
// covers cross-origin attacks for the WS handshake, and HTTP-side
// callers from the LAN need to reach the server by its LAN host.
func (s *Server) buildHTTPServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/bootstrap.json", s.loopbackHostGuard(s.handleBootstrap))
	mux.HandleFunc("/ws", s.loopbackHostGuard(s.handleWS))
	if s.cfg.DesignHandler != nil {
		// Wrap in the same loopback host guard as /bootstrap.json:
		// design files can include user material the agent put in the
		// working dir, and a hostile DNS-rebound origin shouldn't be
		// able to read them. /design/ must be registered before /
		// because mux longest-match-wins routing already handles it,
		// but listing it here makes the priority obvious.
		//
		// The route is registered unconditionally on a getter; if the
		// underlying handler isn't ready yet (App.ServiceStartup races
		// the first iframe load) we serve 404 from the design route
		// rather than falling through to the SPA shell. Falling
		// through used to be the failure mode: the SPA's
		// X-Frame-Options: DENY would block iframe display and leave
		// the preview stuck on chrome-error://, masking what was
		// really a startup-order bug.
		//
		// StripPrefix lets the FileHandler resolve "{threadId}/main/..."
		// against its baseDir directly. Without it, http.FileServer
		// would look for files at "{baseDir}/design/{threadId}/main/..."
		// — a path the agent never writes to.
		designH := http.StripPrefix("/design", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := s.cfg.DesignHandler()
			if h == nil {
				http.NotFound(w, r)
				return
			}
			h.ServeHTTP(w, r)
		}))
		// On LAN bind the loopbackHostGuard becomes a pass-through and
		// /design/ has no token check, so we additionally refuse from
		// non-loopback peers to avoid leaking agent-rendered content
		// (which can include user material) over LAN. LAN-served design
		// previews are a separate feature — pick it up via a deliberate
		// token-validation pass when we want them.
		// Diag-mode isolation headers must cover /design/ too: COEP
		// applies to nested documents, so the preview iframe fails to
		// load under the isolated shell unless its responses carry
		// CORP/COEP themselves.
		var designFinal http.Handler = s.loopbackHostGuard(s.designLoopbackOnly(designH).ServeHTTP)
		if s.cfg.CrossOriginIsolate {
			designFinal = withCrossOriginIsolation(designFinal)
		}
		mux.Handle("/design/", designFinal)
	}
	assetH := s.cfg.AssetHandler
	if assetH == nil {
		assetH = http.NotFoundHandler()
	}
	assetFinal := withAssetHeaders(assetH)
	if s.cfg.CrossOriginIsolate {
		assetFinal = withCrossOriginIsolation(assetFinal)
	}
	mux.Handle("/", assetFinal)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: s.cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       s.cfg.HTTPReadTimeout,
		WriteTimeout:      s.cfg.HTTPWriteTimeout,
		IdleTimeout:       s.cfg.HTTPIdleTimeout,
		// Per-conn ctx so Shutdown can cancel handlers in flight.
		// Reused across Rebind: a re-bound server shares the same
		// rootCtx so existing handlers don't see a spurious cancel.
		BaseContext: func(_ net.Listener) context.Context { return s.rootCtx },
	}
}

// designLoopbackOnly refuses /design/* requests from non-loopback peers
// even when the server is bound to a LAN interface. The agent-rendered
// HTML in the design workdir can include user-prompted material; until
// we add explicit token validation, LAN peers don't get the design
// surface. Returns 404 so a LAN scanner can't fingerprint that the
// route exists.
func (s *Server) designLoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.currentOriginPatterns()) > 0 && !remoteAddrIsLoopback(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHostGuard returns a wrapper that rejects non-loopback Host
// headers when the server is in loopback mode (origin allow-list
// empty). This is a DNS-rebinding defence: without it, a hostile site
// resolving to 127.0.0.1 could probe /bootstrap.json and harvest the
// token. Returns 404 (not 403) so a LAN scanner can't fingerprint the
// agent-overflow server vs an arbitrary 127.0.0.1 service.
//
// The mode check reads the live origin allow-list under mu so a
// post-Rebind LAN bind reaches this function with patterns set and
// stops gating on Host. Rebinding back to loopback re-enables the
// guard for the next request.
func (s *Server) loopbackHostGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only enforce in loopback mode. LAN bind has its own
		// origin-validation story for /ws; HTTP /bootstrap.json on LAN
		// must be reachable from any LAN host the user shares.
		if len(s.currentOriginPatterns()) == 0 {
			if !IsLoopbackHost(r.Host) {
				http.NotFound(w, r)
				return
			}
		}
		next(w, r)
	}
}

// serve runs srv.Serve(listener) on a tracked goroutine. The first
// async failure is published to serveErr; subsequent failures drop.
// Used both by Start and Rebind.
//
// http.ErrServerClosed (graceful Shutdown) and net.ErrClosed (the
// underlying listener was closed directly — used by Rebind's Linux
// EADDRINUSE recovery path) are both expected lifecycles, not failures,
// so they're silently dropped.
func (s *Server) serve(srv *http.Server, listener net.Listener) {
	s.wg.Go(func() {
		err := srv.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return
		}
		log.Printf("transport: http serve: %v", err)
		select {
		case s.serveErr <- err:
		default:
		}
	})
}

// ServeErr returns a channel that delivers async listener failures
// (e.g. unexpected Serve return). Buffered 1; subsequent errors after
// the first are logged but dropped.
func (s *Server) ServeErr() <-chan error { return s.serveErr }

// Shutdown stops accepting new connections, drains in-flight requests
// up to ctx's deadline, and closes the event bus. Idempotent.
//
// Order: cancel root ctx (so live read loops exit promptly), then
// http.Server.Shutdown (drains in-flight HTTP), then force-close any
// http.Servers retired by Rebind (their hijacked WS conns), then
// close the event bus (signals subscribers), then wait for the serve
// goroutine to return.
func (s *Server) Shutdown(ctx context.Context) error {
	var shutdownErr error
	s.stopOnce.Do(func() {
		s.shutDown.Store(true)
		if s.rootCancel != nil {
			s.rootCancel(errServerShutdown)
		}
		s.mu.Lock()
		current := s.srv
		former := s.formerSrvs
		s.formerSrvs = nil
		s.mu.Unlock()
		if current != nil {
			shutdownErr = current.Shutdown(ctx)
		}
		// Former servers are already mid-Shutdown from Rebind, but
		// hijacked WS connections on them ignore Shutdown — Close()
		// is what severs those underlying TCP connections so the
		// process can exit. The error is logged but not aggregated;
		// the active server's Shutdown error is what callers care about.
		for _, prev := range former {
			if err := prev.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("transport: close former http server: %v", err)
			}
		}
		if s.cfg.EventBus != nil {
			s.cfg.EventBus.Close()
		}
		s.wg.Wait()
	})
	return shutdownErr
}

// SetOriginPatterns rotates the WS origin allow-list without binding a
// new listener. Useful when a frontend lifecycle event (e.g. user
// changes the trusted-origins setting) needs to update the policy
// without disrupting the listener. Pass an empty slice to clear the
// allow-list — equivalent to "loopback / InsecureSkipVerify" mode.
//
// Already-upgraded WebSockets are unaffected; origin is a handshake-
// time check, so the new policy applies to subsequent upgrades.
func (s *Server) SetOriginPatterns(patterns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if patterns == nil {
		s.originPatterns = nil
		return
	}
	s.originPatterns = append([]string(nil), patterns...)
}

// currentOriginPatterns returns a snapshot of the live allow-list
// under mu. The returned slice is safe for the caller to read but must
// not be mutated — the in-server slice is shared.
func (s *Server) currentOriginPatterns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.originPatterns
}

// Addr returns the resolved listen address (e.g. "127.0.0.1:54321").
// Empty before Start. Updated atomically by Rebind so concurrent
// readers see either the pre-rebind or post-rebind value, never a
// torn string.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Token returns the auth token in use.
func (s *Server) Token() string { return s.token }

// HasRemoteClient reports whether at least one non-loopback WebSocket
// connection is currently attached. Producers of remote-only event
// channels (see event_visibility.go) consult this to skip the work
// entirely when nobody would receive it.
func (s *Server) HasRemoteClient() bool { return s.remoteConns.Load() > 0 }

// MarkReady releases a readiness-gated bootstrap endpoint.
func (s *Server) MarkReady() { s.ready.Store(true) }

// MarkStartupFailed makes a readiness-gated bootstrap endpoint return
// a terminal startup failure instead of "still booting".
func (s *Server) MarkStartupFailed() { s.startupFailed.Store(true) }

// Ready reports whether /bootstrap.json is allowed to return the
// manifest. Servers without RequireReadyForBootstrap start ready.
func (s *Server) Ready() bool { return s.ready.Load() }

// AppURL returns the HTTP URL the webview should load. The query
// parameter primes the bootstrap fetch — the SPA reads ?t= and presents
// it to the WS upgrade.
//
// Pre-Start (no listener bound yet) returns "" so callers can detect
// the not-ready state. main.go's `runDesktop` asserts non-empty before
// passing the URL to Wails, turning that case into a loud boot error
// rather than letting Wails fall through to its built-in scheme.
//
// Post-Start, if the cached addr string fails to parse for any reason,
// we fall back to the live listener address (net.Listener.Addr() is
// always well-formed). Either way the URL points at this server with
// the live token, never at port 80 (which an earlier fallback path
// produced when SplitHostPort failed).
func (s *Server) AppURL() string {
	addr := s.Addr()
	if addr == "" {
		// Pre-Start: no listener exists yet, so there is no port we
		// could plausibly point a webview at. Return "" and let the
		// caller (typically main.go) decide that's a fatal boot
		// condition — better than silently emitting a port-less URL
		// that hits port 80 on first navigation.
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Cached addr is malformed — should be impossible because we
		// only ever store listener.Addr() into it, which net/http
		// guarantees parseable. Fall back to the live listener Addr()
		// directly so we keep a real port and don't accidentally produce
		// a port 80 URL.
		s.mu.Lock()
		live := ""
		if s.listener != nil {
			live = s.listener.Addr().String()
		}
		s.mu.Unlock()
		log.Printf("transport: AppURL: split %q: %v (falling back to listener.Addr() %q)", addr, err, live)
		if live == "" {
			return ""
		}
		host, port, err = net.SplitHostPort(live)
		if err != nil {
			// Both addr forms unparseable — return "" rather than
			// emit a port-less URL.
			log.Printf("transport: AppURL: live addr %q also unparseable: %v", live, err)
			return ""
		}
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/?t=%s", host, port, s.token)
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	supplied := r.URL.Query().Get("t")
	if err := ConstantTimeEqual(s.token, supplied); err != nil {
		// Indistinguishable from "no such path" so a LAN scanner can't
		// fingerprint the agent-overflow server vs other 404 responses.
		http.NotFound(w, r)
		return
	}

	// CORS not strictly needed (same origin), but emit no-cache so a
	// stale token never gets reused after a server restart. Security
	// headers match the asset handler's so the bootstrap response can't
	// be framed, sniffed, or referrer-leaked from a foreign page.
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	WriteSecurityHeaders(h)
	if s.startupFailed.Load() {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "backend startup failed", http.StatusInternalServerError)
		return
	}
	if !s.Ready() {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "backend not ready", http.StatusServiceUnavailable)
		return
	}
	h.Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Bootstrap{
		// Build the wsUrl from the request's Host header so a LAN
		// client gets a LAN-reachable URL even though the server's
		// internal addr might say "0.0.0.0:port" (unconnectable).
		WSURL: deriveWSURL(r),
		Token: s.token,
	})
}

// withAssetHeaders wraps the asset handler with caching + security
// headers. Cache headers are content-aware: Vite-hashed asset paths
// (/assets/*) get a year of immutable caching because their filename
// already encodes their content hash, while index.html and "/" go out
// as no-cache so a fresh deploy isn't shadowed by a stale shell.
//
// Security headers come from WriteSecurityHeaders so the rule set stays
// in sync between this server and clientmode's stub.
func withAssetHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		WriteSecurityHeaders(h)
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			// Vite emits hashed filenames under /assets/, so the response
			// is content-addressable forever — a content change ships a
			// new path, never overwrites this one.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// "/" and "/index.html" are the SPA entry shell; a deploy
			// must invalidate them immediately so users don't run an
			// old shell that fetches new asset URLs from a stale manifest.
			h.Set("Cache-Control", "no-cache, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}

// withCrossOriginIsolation stamps the diagnostic isolation headers on
// every response of the wrapped handler. Kept separate from
// withAssetHeaders because it also wraps the /design/ route, which has
// its own header stack.
func withCrossOriginIsolation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteCrossOriginIsolationHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

// deriveWSURL turns the inbound HTTP request's Host into the matching
// ws:// URL. Falls back to a loopback path only when r.Host is empty
// (synthetic test requests).
func deriveWSURL(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s/ws", scheme, strings.TrimSpace(host))
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Track BEFORE upgrade so a slow handshake also blocks Shutdown.
	s.wg.Add(1)
	defer s.wg.Done()

	// Capture the peer's loopback-ness BEFORE upgrade — the WebSocket
	// connection wraps the underlying net.Conn and websocket.Conn doesn't
	// re-expose the original RemoteAddr. r.RemoteAddr is the kernel-
	// reported peer address; we only mark a connection loopback when
	// that address sits on a loopback interface.
	isLoopback := remoteAddrIsLoopback(r.RemoteAddr)

	// Read the live (post-rebind) allow-list, not Config's static value.
	// A LAN-bind toggle rotates the allow-list under the same mu-guarded
	// swap as the listener; the upgrader must see whichever policy was
	// in effect when this handshake began.
	conn, err := upgrade(w, r, s.token, s.currentOriginPatterns(), !isLoopback)
	if err != nil {
		// upgrade has already written the HTTP error code.
		return
	}

	profile := connProfile{
		isLoopback: isLoopback,
	}

	if !isLoopback {
		s.remoteConns.Add(1)
		defer s.remoteConns.Add(-1)
	}

	// Use the server's root context so Shutdown can cancel us promptly,
	// not r.Context() which net/http only cancels on connection close.
	runConnHandler(s.rootCtx, conn, s.cfg.Dispatcher, s.cfg.EventBus, s.cfg.ReadLimit, s.cfg.MaxConcurrentRPCs, profile)
	// Best-effort close. Read errors above already represent a closed
	// connection; any normal-closure send here is for the other half
	// of the bidirectional handshake.
	_ = conn.Close(1000, "")
}
