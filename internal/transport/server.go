package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"agent-overflow/internal/loopback"
	"agent-overflow/internal/pagehost"
)

// Bootstrap is the JSON document the SPA fetches at /bootstrap.json on
// page load. It tells the client where the WS endpoint lives; it carries
// no credential, because the request that fetched it either arrived with
// the page cookie or was answered with a Set-Cookie, and the browser
// presents that cookie on the upgrade without page script ever holding
// it. The wsUrl is built from the request's Host header so a LAN bind
// serves a LAN-reachable URL, not the loopback string the server
// resolved at bind time.
type Bootstrap struct {
	WSURL string `json:"wsUrl"`
	// LaunchID identifies this backend launch. The SPA scopes its
	// notification replay checkpoint by it, since a sequence number from
	// a previous launch means nothing to this one. Opaque, not a
	// credential, and safe for page script to read.
	LaunchID string `json:"launchId,omitempty"`
	Remote   bool   `json:"remote,omitempty"`
	// Harness marks a backend booted as the agent test harness or the
	// soak rig (Config.Harness). The SPA keys its harness bridge on it —
	// the bridge module ships in every bundle but is only imported when
	// this is true, so an ordinary boot pays nothing and a production
	// binary can serve a harness without a frontend rebuild. Never a
	// capability: it announces a mode, it does not grant one.
	Harness bool `json:"harness,omitempty"`
	// PageMarker is a per-instance marker carried by the harness page URL.
	// CDP consumers require it in addition to the origin, so another page on
	// the same debugger endpoint cannot be mistaken for this instance.
	PageMarker string `json:"pageMarker,omitempty"`
	// BackendID and ReplicaGeneration identify the backend's history
	// store for the client-side thread replica
	// (docs/architecture/thread-replica-sync.md §3.3): the first keys the
	// client's replica database per backend, the second invalidates it
	// wholesale when the backend's history counters lose continuity (a
	// database restore). Both are empty when the store is not open yet —
	// the client refetches this manifest on every connect, so an early
	// connection simply gets them on its next one, and MUST treat empty
	// as "no replica keying available" rather than as a generation.
	BackendID         string `json:"backendId,omitempty"`
	ReplicaGeneration string `json:"replicaGeneration,omitempty"`
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

	// EphemeralPortFallback lets Start retry once on an ephemeral port
	// when the requested Port is unavailable (in use, or privileged).
	// Only meaningful alongside a non-zero Port, and only for callers
	// that treat the port as a preference rather than a requirement —
	// main.go's persisted per-install port (a stable page origin for
	// origin-scoped browser storage) sets it; an operator's explicit
	// --listen host:port does not, because silently landing somewhere
	// else would be a lie.
	//
	// The retry is scoped to port-attributable bind failures. A bind
	// address the host cannot honor still fails loudly, since retrying
	// it on port 0 would fail identically and only obscure the cause.
	EphemeralPortFallback bool

	// Token is this launch's session credential. Empty asks Server.New
	// to generate one (recommended). Browsers never see it — they hold
	// the page cookie Server.AppURL's one-time ticket buys them — so it
	// is the credential for clients that are not browsers: the WSL
	// launcher's probe and notification socket, the `ao-harness` CLI,
	// the `--connect` stub dialling this server. See credential.go.
	Token string

	// DecoratePageURL threads whatever else the boot puts on a page URL
	// (the durable client id, the harness page marker) onto a base this
	// package built. Served by the PageURLPath route.
	//
	// A decorator supplied by the boot, not a whole assembler: the ticket
	// half differs per consumer — a browser's rides `?t=`, a webview
	// host's is delivered by injection and the URL is bare — and only
	// this package can mint one either way. The boot's extra parameters
	// are the same in both cases, so it contributes those and nothing
	// else. Optional; nil leaves the base untouched.
	DecoratePageURL func(base string) string

	// Dispatcher hosts the registered RPC methods. Required.
	Dispatcher *Dispatcher

	// EventBus pushes server-initiated events. Required.
	EventBus *EventBus

	// Version is the semantic version this binary reports on /healthz and
	// in logs. Injected because the string is stamped into package main
	// at link time; this package never reads a build variable. Empty is
	// valid (tests, unstamped builds) and reports as empty rather than
	// inventing a number.
	Version string

	// AssetHandler serves the SPA assets. Optional — when nil, the
	// HTTP server returns 404 for non-RPC paths. main.go wires this
	// to http.FileServer(http.FS(embeddedAssets)) so the same process
	// hosts the bundle. Pass http.FS-backed handlers only; http.Dir
	// would expose path traversal of the local filesystem.
	AssetHandler http.Handler

	// BackendIdentity reports the history store's backend id and replica
	// generation for the bootstrap manifest. A getter, not a value, because
	// the store opens during the App's startup, which runs AFTER New() — a
	// snapshot taken at config time would always be empty. This package never
	// learns what a store is; app wiring supplies the two strings.
	//
	// Optional — when nil, the manifest carries no identity and the
	// client keeps its replica disabled.
	BackendIdentity func() (backendID, replicaGeneration string)

	// SessionForRequest resolves the durable session a request presents,
	// if any, and says whether the request may proceed at all.
	//
	// The seam between this package and internal/identity, in the
	// direction that keeps transport store-free: the boot passes a
	// closure over the session core, and this package never learns what a
	// session row is. A false `ok` refuses the upgrade with the same
	// http.NotFound shape a bad launch credential gets, which is what
	// makes a reconnection on a revoked credential fail rather than
	// silently downgrade to an unattributed connection.
	//
	// Optional. Nil means every request proceeds naming no session, which
	// is the launch-credential behavior this server has always had — and
	// handleWS then admits such a connection only from a loopback peer,
	// because a server that cannot resolve a session cannot admit a
	// session-naming one either.
	SessionForRequest func(r *http.Request) (sessionID string, ok bool)

	// SessionLive reports whether a session id still admits work. The
	// same seam as SessionForRequest, taking an ID rather than a request
	// because its two callers do not have one: the upgrade that spent a
	// WebSocket ticket (the ticket names the session; nothing about the
	// request does), and the per-connection re-validation that runs long
	// after the request is gone.
	//
	// Optional. Nil means an established connection is never re-checked
	// and a ticket's subject is taken as live, which is the behavior
	// before any client presents a session.
	SessionLive func(sessionID string) bool

	// SessionScopes resolves the capability grants a session holds RIGHT
	// NOW, or refuses it outright.
	//
	// The third hook over the same seam, and the one the per-RPC gate
	// reads (authorize.go). It answers a scope set and an empty refusal
	// when the session still admits work, and a non-empty refusal — one
	// spelling from internal/identity's closed set, which this package
	// carries without interpreting — when it does not. That second answer
	// is why the gate needs no separate liveness call: a revoked session
	// stops authorizing on the very next RPC rather than at the next
	// watchdog tick.
	//
	// Optional. Nil means a connection's scopes are never consulted, which
	// is the pre-enforcement behavior: the origin gate alone decides, as it
	// does for every launch-credential client.
	SessionScopes func(sessionID string) (scopes []string, refusal string)

	// PageSessionCredential returns the session credential to plant on
	// the page as an HttpOnly cookie during the bootstrap exchange, or ""
	// when there is none.
	//
	// A getter for the reason BackendIdentity is one: the local page
	// channel's session is minted during the App's startup, which runs
	// after New(). It is also what lets the app re-issue a credential
	// approaching its expiry without this package knowing a window
	// exists.
	//
	// Optional — when nil, no session cookie is written and local clients
	// name no session, exactly as before.
	PageSessionCredential func() string

	// AuthEndpoints backs the device-facing credential routes (pairing
	// redemption and token rotation). The app satisfies it with an
	// adapter over internal/identity.
	// Optional — when nil, neither route is registered.
	AuthEndpoints AuthEndpoints

	// ScopedTokens resolves an `ao` CLI credential to the caller scope it
	// was minted for. The App owns the registry (a token lives exactly as
	// long as the provider session it belongs to); this package owns the
	// authorization table and the ScopedRPCPath route.
	// Optional — when nil, the scoped RPC route is not registered.
	ScopedTokens ScopedTokens

	// ReadLimit caps the byte size of a single inbound WS message.
	// Zero defaults to DefaultReadLimit (75 MiB).
	ReadLimit int64

	// MaxConcurrentRPCs caps how many in-flight RPC handlers a single
	// connection can have at once. Zero defaults to
	// DefaultMaxConcurrentRPCs (64).
	MaxConcurrentRPCs int

	// KeepaliveInterval overrides the per-connection heartbeat cadence.
	// Zero defaults to defaultKeepaliveInterval (10s). The frontend's
	// stale-socket watchdog threshold is sized as 3× the default —
	// production code should not change this; it exists so tests can
	// exercise the keepalive loop without multi-second sleeps.
	KeepaliveInterval time.Duration
	// KeepalivePongTimeout overrides the protocol-ping pong wait. Zero
	// defaults to defaultKeepalivePongTimeout (10s). Test knob, like
	// KeepaliveInterval.
	KeepalivePongTimeout time.Duration

	// SessionRecheckInterval is how often an established connection
	// re-asks whether the session it named is still live
	// (docs/specs/remote-access.md §4). Zero defaults to
	// defaultSessionRecheck (60s); negative disables the re-check.
	//
	// It is a floor on how long a connection can outlive its credential
	// by a route nothing else covers: revocation force-closes sockets
	// synchronously, so this catches the two cases revocation does not —
	// a session that simply EXPIRED, and a revocation this process did
	// not perform (another replica, a direct database edit).
	SessionRecheckInterval time.Duration

	// MaxRemoteConnLifetime caps how long one non-loopback connection
	// naming a session may stay open, forcing a periodic re-ticket. Zero
	// defaults to defaultRemoteConnLifetime (12h); negative disables the
	// cap.
	//
	// Loopback connections are deliberately exempt. The cap exists so a
	// credential that travels a network is re-presented periodically; the
	// local page's session is re-minted at boot and has no network to
	// travel, so capping it would buy nothing and cost the webview a
	// visible reconnect.
	MaxRemoteConnLifetime time.Duration

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

	// Harness announces harness/soak mode in the bootstrap manifest.
	// main.go sets it exactly where it registers the Harness RPC
	// receiver, so "the manifest says harness" and "the harness methods
	// exist on this wire" can never disagree. It is a mode announcement,
	// not an authorization: the receiver stays LocalOnly regardless.
	Harness bool
	// PageMarker authenticates the harness page identity. Empty is valid for
	// ordinary boots, which do not expose the harness bridge.
	PageMarker string

	// CrossOriginIsolate makes every asset response carry
	// cross-origin isolation headers (COOP/COEP/CORP) so the SPA runs
	// crossOriginIsolated and measureUserAgentSpecificMemory works.
	// Diagnostic opt-in (AGENT_OVERFLOW_RENDERER_DIAG) — COEP blocks
	// remote subresources while on. See WriteCrossOriginIsolationHeaders.
	CrossOriginIsolate bool

	// DevAssetProxy marks a boot whose AssetHandler forwards to a live
	// Vite dev server instead of serving the embedded bundle. It picks
	// CSPDevServer over CSPProduction, once, in New — the strict/relaxed
	// split is a boot-mode decision, never a per-request one. main.go
	// sets it from the same condition that built the proxy handler, so
	// the two can never disagree about which bundle is being served.
	DevAssetProxy bool

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

	// cred is this launch's page credential: the session token clients
	// that are not browsers present, plus the one-time page tickets that
	// buy a browser its HttpOnly cookie. See credential.go.
	cred *Credential

	// launchID identifies this boot to a client that must not reuse
	// state minted against a previous one (the SPA's notification
	// replay checkpoint). Opaque and non-credential: it is published in
	// the manifest precisely so nothing else has to be.
	launchID string

	// csp is the one Content-Security-Policy every response on this
	// server carries, resolved from Config.DevAssetProxy in New and
	// immutable afterwards. Rebind does not revisit it: swapping the
	// listener changes where the bundle is reachable from, never which
	// bundle it is.
	csp ContentSecurityPolicy

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

	// sessionConns is the live-session registry: which upgraded sockets
	// carry which durable session, and how to close them. Built at New
	// and never replaced, because a Rebind must not lose track of the
	// connections it is deliberately keeping alive.
	sessionConns *SessionConns

	// Per-peer request budgets for the credential surfaces. Built at New
	// and never replaced: a Rebind changes which address the server
	// answers on, not how much work one peer may ask for, and rebuilding
	// them would hand every peer a fresh burst on a LAN-bind toggle.
	// Deliberately absent for /healthz and the SPA assets — see
	// ratelimit.go.
	bootstrapLimit *rateLimiter
	pageURLLimit   *rateLimiter
	scopedRPCLimit *rateLimiter
	authLimit      *rateLimiter

	// wsTickets holds the single-use tickets minted at AuthTicketPath and
	// spent on the upgrade. Server-owned rather than Credential-owned
	// because a ticket names a session, and a session outlives no launch
	// but belongs to none either.
	wsTickets *ticketBook

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
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("transport: Config.Port %d out of range", cfg.Port)
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
	cred, err := NewCredential(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("transport: generate credential: %w", err)
	}
	launchID, err := NewToken()
	if err != nil {
		return nil, fmt.Errorf("transport: generate launch id: %w", err)
	}
	// Copy origin patterns so a caller mutating the Config slice after
	// New can't reach into the live allow-list. The mu-guarded mirror
	// is the only source of truth at handshake time; Config.OriginPatterns
	// is read once here and never again.
	var originPatterns []string
	if len(cfg.OriginPatterns) > 0 {
		originPatterns = append(originPatterns, cfg.OriginPatterns...)
	}
	csp := CSPProduction
	if cfg.DevAssetProxy {
		csp = CSPDevServer
	}
	s := &Server{
		cfg:            cfg,
		cred:           cred,
		launchID:       launchID,
		csp:            csp,
		originPatterns: originPatterns,
		serveErr:       make(chan error, 1),
		sessionConns:   newSessionConns(),
		bootstrapLimit: newRateLimiter("/bootstrap.json", bootstrapRateLimit),
		pageURLLimit:   newRateLimiter(PageURLPath, pageURLRateLimit),
		scopedRPCLimit: newRateLimiter(ScopedRPCPath, scopedRPCRateLimit),
		authLimit:      newRateLimiter("/auth", authRateLimit),
		wsTickets:      newTicketBook(maxOutstandingWSTickets, wsTicketTTL),
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
	listener, err := s.listen()
	if err != nil {
		return err
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

// listen binds the configured address. When the caller asked for a
// specific port as a *preference* (Config.EphemeralPortFallback) and
// the host refuses that port specifically, it retries exactly once on
// an ephemeral port and says so in the log. There is no loop: the
// second bind either succeeds or the whole Start fails carrying both
// errors. The caller learns where it actually landed from Addr() —
// main.go uses that to re-persist the pinned port.
func (s *Server) listen() (net.Listener, error) {
	addr := net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener, nil
	}
	if !s.cfg.EphemeralPortFallback || s.cfg.Port == 0 || !portUnavailable(err) {
		return nil, fmt.Errorf("transport: listen %s: %w", addr, err)
	}

	ephemeral := net.JoinHostPort(s.cfg.BindAddr, "0")
	log.Printf("transport: listen %s: %v — retrying on an ephemeral port", addr, err)
	listener, retryErr := net.Listen("tcp", ephemeral)
	if retryErr != nil {
		return nil, fmt.Errorf("transport: listen %s: %w (after %s: %v)", ephemeral, retryErr, addr, err)
	}
	log.Printf("transport: bound %s instead of %s", listener.Addr(), addr)
	return listener, nil
}

// portUnavailable reports whether a bind error is attributable to the
// port rather than the bind address: the port is held by someone else
// (EADDRINUSE) or reserved to a privileged process (EACCES). Every
// other failure — an address this host does not own, a malformed one —
// would fail the same way on port 0, so retrying it would only bury
// the real cause. The errno set is per-GOOS (porterr_unix.go /
// porterr_windows.go): Windows surfaces WSAEADDRINUSE/WSAEACCES, which
// errors.Is does not match against the POSIX names.
func portUnavailable(err error) bool {
	return matchesErrno(err, portUnavailableErrnos)
}

// addrInUse reports whether a bind error is "somebody already holds this
// address" (EADDRINUSE / WSAEADDRINUSE) — the strictly narrower half of
// portUnavailable. It exists for the one decision that must not confuse
// the two: bindRebindListener cures a bind failure by CLOSING our own
// live listener and retrying, which can only ever help when the address
// was in use. A permission/reservation refusal (EACCES, WSAEACCES) is
// identical after the close, so treating it as recoverable would tear
// down a working listener for nothing.
func addrInUse(err error) bool {
	return matchesErrno(err, addrInUseErrnos)
}

func matchesErrno(err error, errnos []syscall.Errno) bool {
	for _, errno := range errnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// buildHTTPServer constructs an http.Server with the same handler /
// timeouts / BaseContext used at boot. Extracted so Rebind can spin up
// a parallel server pointing at a fresh listener while the old one
// keeps draining hijacked WS connections.
//
// Loopback paths (/bootstrap.json, /ws) are wrapped in a Host-header
// guard that fires when the live origin allow-list is empty (loopback
// mode). Loopback binding is not by itself a boundary: a foreign origin
// whose DNS name resolves to 127.0.0.1 can navigate the user to
// http://that.name:<our-port>/bootstrap.json, and the request arrives
// over the loopback interface like any other, carrying that name as its
// Host and its origin as the document's. Requiring a loopback Host
// (loopback.HostHeader, which refuses names precisely for this) makes
// the guard about who is asking rather than about which interface the
// packets crossed. On LAN bind (origin allow-list non-empty) it is a
// pass-through — origin validation already covers the WS handshake, and
// HTTP callers from the LAN reach the server by its LAN host.
func (s *Server) buildHTTPServer() *http.Server {
	mux := http.NewServeMux()
	// Rate limiting sits OUTSIDE the host guard on the three credential
	// surfaces, so a peer over budget is refused before any other work is
	// done for it. /ws is not limited here: a WebSocket is one upgrade per
	// long-lived connection, and the request that carries the credential
	// is the same one that starts the connection — the budget that matters
	// for it is the ticket exchange that precedes it. /healthz and the
	// assets are never limited (ratelimit.go says why).
	mux.HandleFunc("/bootstrap.json",
		rateLimited(s.bootstrapLimit, s.loopbackHostGuard(s.handleBootstrap)))
	mux.HandleFunc("/ws", s.loopbackHostGuard(s.handleWS))
	mux.HandleFunc(PageURLPath,
		rateLimited(s.pageURLLimit, s.loopbackHostGuard(s.handlePageURL)))
	mux.HandleFunc(HealthPath, s.loopbackHostGuard(s.handleHealthz))
	if s.cfg.ScopedTokens != nil {
		mux.HandleFunc(ScopedRPCPath,
			rateLimited(s.scopedRPCLimit, s.loopbackHostGuard(s.handleScopedRPC)))
	}
	// The credential routes share ONE budget, deliberately: they are
	// alternative ways for the same peer to ask this backend for a
	// credential, so a peer that has exhausted its patience on one must
	// not simply move to the next.
	if s.cfg.AuthEndpoints != nil {
		mux.HandleFunc(AuthPairPath,
			rateLimited(s.authLimit, s.loopbackHostGuard(s.handleAuthPair)))
		mux.HandleFunc(AuthTokenPath,
			rateLimited(s.authLimit, s.loopbackHostGuard(s.handleAuthToken)))
	}
	// The ticket route needs no AuthEndpoints — it mints from the session
	// the caller already holds — so it is registered whenever a session
	// can be resolved at all.
	if s.cfg.SessionForRequest != nil {
		mux.HandleFunc(AuthTicketPath,
			rateLimited(s.authLimit, s.loopbackHostGuard(s.handleAuthTicket)))
	}
	assetH := s.cfg.AssetHandler
	if assetH == nil {
		assetH = http.NotFoundHandler()
	}
	assetFinal := withAssetHeaders(assetH, s.csp)
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
			if !loopback.HostHeader(r.Host) {
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

// Token returns this launch's session credential — the carrier for
// clients that are not browsers. A browser never receives it: it holds
// the page cookie instead (see credential.go).
func (s *Server) Token() string { return s.cred.Token() }

// LaunchID returns the opaque identifier for this boot that the manifest
// publishes. Not a credential.
func (s *Server) LaunchID() string { return s.launchID }

// PageMarker returns the authenticated harness-page marker, if this server
// is serving a harness. It is intentionally read-only and immutable after
// construction so page URLs and bootstrap manifests cannot drift.
func (s *Server) PageMarker() string { return s.cfg.PageMarker }

// SessionConns is the live-session registry: the connections currently
// carrying each durable session, and the teardown a revocation runs.
//
// Handed to internal/identity's session core at boot, which reaches it
// through its own one-method interface so neither package imports the
// other. Never nil for a server built by New.
func (s *Server) SessionConns() *SessionConns { return s.sessionConns }

// sessionStillLive asks the session core whether a session id admits work
// right now. A nil hook answers yes, which is the pre-session behavior:
// nothing has told this server that sessions exist, so nothing may refuse
// on their behalf.
func (s *Server) sessionStillLive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if check := s.cfg.SessionLive; check != nil {
		return check(sessionID)
	}
	return true
}

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

// Origin returns this server's own origin ("http://host:port"), with no
// credential on it. Callers that want a page to navigate to want AppURL;
// this is for the callers that only need to name the server — the `ao`
// CLI's endpoint, the harness's page-origin check.
//
// Empty before Start, for the same reason AppURL is.
func (s *Server) Origin() string {
	host, port, ok := s.hostPort()
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// AppURL returns the HTTP URL a browser should load, carrying a FRESHLY
// MINTED one-time page ticket. Every call mints its own, so a caller
// that hands out two URLs has handed out two independently usable ones,
// and the reload path (main_desktop.go's Ctrl+R getter) always produces
// a URL that works even if the page it replaces already spent its
// ticket.
//
// The ticket is not the session token. It buys the browser one HttpOnly
// cookie at /bootstrap.json and is spent doing so; the SPA then scrubs
// it from the URL. See credential.go.
//
// Pre-Start (no listener bound yet) returns "" so callers can detect
// the not-ready state. main.go's `runDesktop` asserts non-empty before
// passing the URL to Wails, turning that case into a loud boot error
// rather than letting Wails fall through to its built-in scheme.
func (s *Server) AppURL() string {
	host, port, ok := s.hostPort()
	if !ok {
		return ""
	}
	ticket, err := s.cred.MintPageTicket()
	if err != nil {
		log.Printf("transport: AppURL: mint page ticket: %v", err)
		return ""
	}
	return fmt.Sprintf("http://%s:%s/?%s=%s", host, port, PageTicketParam, ticket)
}

// WebviewPageURL is AppURL's answer for a host that owns the window it
// is about to load: a BARE page URL, no ticket on it, marked as
// webview-hosted so the SPA waits for one to be injected instead
// (internal/pagehost).
//
// It mints nothing, and that is the difference that matters beside
// AppURL. A ticketed URL is single-use, so producing one is producing a
// credential; a bare one is just an address, so a host may re-read it on
// every reload without churning the ticket book. The credential comes
// from MintPageTicket, once per document, at the moment that document
// asks for it.
//
// Pre-Start returns "" for the same reason AppURL does.
func (s *Server) WebviewPageURL() string {
	host, port, ok := s.hostPort()
	if !ok {
		return ""
	}
	return pagehost.MarkWebview(fmt.Sprintf("http://%s:%s/", host, port))
}

// MintPageTicket hands out a one-time page ticket for a URL this package
// does not build itself — the LAN share URL, which names a discovered
// interface address rather than the listener's own host — and for a
// window host re-ticketing a document it did not navigate (uiwindow's
// per-load delivery).
func (s *Server) MintPageTicket() (string, error) { return s.cred.MintPageTicket() }

// decoratePageURL applies the boot's extra page-URL parameters, if it
// supplied any. The empty base passes through, since every caller
// already reads "" as "no page to open yet".
func (s *Server) decoratePageURL(base string) string {
	if base == "" || s.cfg.DecoratePageURL == nil {
		return base
	}
	return s.cfg.DecoratePageURL(base)
}

// hostPort resolves the host and port a page URL should name, reporting
// false when no listener is bound.
//
// Post-Start, if the cached addr string fails to parse for any reason,
// we fall back to the live listener address (net.Listener.Addr() is
// always well-formed). Either way the result points at this server,
// never at port 80 (which an earlier fallback path produced when
// SplitHostPort failed).
func (s *Server) hostPort() (string, string, bool) {
	addr := s.Addr()
	if addr == "" {
		// Pre-Start: no listener exists yet, so there is no port we
		// could plausibly point a webview at. Report not-ready and let
		// the caller (typically main.go) decide that's a fatal boot
		// condition — better than silently emitting a port-less URL
		// that hits port 80 on first navigation.
		return "", "", false
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
		log.Printf("transport: page URL: split %q: %v (falling back to listener.Addr() %q)", addr, err, live)
		if live == "" {
			return "", "", false
		}
		host, port, err = net.SplitHostPort(live)
		if err != nil {
			// Both addr forms unparseable — report not-ready rather than
			// emit a port-less URL.
			log.Printf("transport: page URL: live addr %q also unparseable: %v", live, err)
			return "", "", false
		}
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return host, port, true
}

// PageURLPath serves a freshly minted page URL to a client that already
// holds the session token: the Windows launcher pointing its WebView2 at
// the WSL backend and re-pointing it on reload, `ao-harness open` /
// `attach`, the e2e rig opening one browser context per test. Each of
// those navigates more than once over a backend's life, and a page URL
// is single-use by design, so the URL is minted on demand rather than
// stored.
//
// It grants nothing the caller does not already have: presenting the
// session token is already full access to this wire.
//
// Two answer shapes, one per consumer class. A caller pointing a BROWSER
// at this backend gets the plain-text ticketed URL, because a URL is the
// only channel a browser has. A caller that owns the WINDOW it is about
// to navigate asks with `?host=webview` and gets a bare URL and the
// ticket as separate JSON fields, so nothing credential-shaped reaches
// the URL at all — see WebviewPageURL and internal/pagehost.
const PageURLPath = "/pageurl"

// handlePageURL answers PageURLPath. The default shape is one URL and a
// newline: plain text because every browser-pointing consumer wants
// exactly the string, and the two non-Go ones (a shell, the e2e rig)
// should not need a parser.
func (s *Server) handlePageURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if !OriginAllowed(r, s.currentOriginPatterns()) || !s.cred.Authenticate(r) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get(pagehost.Param) == pagehost.Webview {
		s.writeWebviewPageURL(w, r)
		return
	}
	pageURL := s.decoratePageURL(s.AppURL())
	if pageURL == "" {
		http.Error(w, "page url unavailable", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.WriteString(w, pageURL+"\n")
}

// writeWebviewPageURL answers the same route for a caller that owns the
// window it is about to navigate: the bare URL and the ticket, as JSON,
// because the two have to arrive as separate strings and a plain-text
// answer would need a delimiter nobody else wants.
//
// The two callers on this branch are Go (the Windows launcher, which
// decodes pagehost.Answer without linking this package), so the parser
// the plain-text form exists to spare a shell is not needed here.
func (s *Server) writeWebviewPageURL(w http.ResponseWriter, r *http.Request) {
	pageURL := s.decoratePageURL(s.WebviewPageURL())
	if pageURL == "" {
		http.Error(w, "page url unavailable", http.StatusServiceUnavailable)
		return
	}
	ticket, err := s.cred.MintPageTicket()
	if err != nil {
		log.Printf("transport: page url: mint page ticket: %v", err)
		http.Error(w, "page url unavailable", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = json.NewEncoder(w).Encode(pagehost.Answer{URL: pageURL, Ticket: ticket})
}

// handleBootstrap answers the SPA's manifest fetch and is the one place
// a page ticket is exchanged for the page cookie.
//
// The origin check runs first and for the same reason it does on the
// upgrade: this route sets a cookie, and a request another origin
// initiated must not be able to spend a ticket or be handed a session.
// A same-origin GET carries no Origin header at all, so the SPA's own
// fetch passes without one.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !OriginAllowed(r, s.currentOriginPatterns()) {
		http.NotFound(w, r)
		return
	}
	// Exchange writes the Set-Cookie for a request that paid with a
	// ticket, and must run before anything writes a status — including
	// the readiness and startup-failure paths below, where issuing the
	// cookie is exactly right: the credential was good, the backend just
	// is not serving yet, and the retry should not need another ticket.
	pageAuthed := s.cred.Exchange(w, r)
	if !pageAuthed {
		// The page credential is not the only door: a durable session —
		// a paired device presenting its credential header, or a session
		// cookie that outlived the launch credential that planted it —
		// already admits the /ws upgrade (handleWS's non-ticket arm), so
		// the manifest, whose whole job is to hand out wsUrl, must not
		// be stricter than the socket it describes. Without this arm a
		// paired page whose one-time ?t= is long spent can never load
		// the manifest again after a backend restart.
		if !s.sessionAdmitsRequest(r) {
			// Indistinguishable from "no such path" so a LAN scanner
			// can't fingerprint the agent-overflow server vs other 404
			// responses.
			http.NotFound(w, r)
			return
		}
	}

	// CORS not strictly needed (same origin), but emit no-store so a
	// manifest from a previous launch is never replayed from a cache.
	// Security headers match the asset handler's so the bootstrap
	// response can't be framed, sniffed, or referrer-leaked from a
	// foreign page.
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	WriteSecurityHeaders(h, s.csp)
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
	// The local page's session credential rides the SAME exchange the
	// page credential does, so a local client acquires both in one round
	// trip and neither needs a route of its own. Written after the
	// readiness checks above because the credential does not exist until
	// the App's startup has minted it, and a page that arrives early gets
	// it on the refetch its reconnect already performs.
	//
	// Two gates, and they refuse different requests. The PAGE credential:
	// a request the session fallback admitted holds a device-bound
	// session, and planting the local channel's credential on it would
	// hand that device the one session this surface refuses to revoke.
	// The PEER: that credential is `loopback-only` by class, so handing it
	// to an off-host page would mint a credential its own class does not
	// let it present — the share URL loads the page (deliberately, so the
	// person holding it sees the pairing prompt) and gets no local
	// channel with it. The presentation side refuses such a credential
	// anyway (internal/app bindingAdmitsPeer); not planting it is the
	// other end of the same rule, so a page is never handed a credential
	// that would be refused the moment it used one.
	if issue := s.cfg.PageSessionCredential; issue != nil && pageAuthed && loopback.PeerAddress(r.RemoteAddr) {
		WriteSessionCookie(w, r, issue())
	}
	backendID, replicaGeneration := "", ""
	if s.cfg.BackendIdentity != nil {
		backendID, replicaGeneration = s.cfg.BackendIdentity()
	}
	_ = json.NewEncoder(w).Encode(Bootstrap{
		// Build the wsUrl from the request's Host header so a LAN
		// client gets a LAN-reachable URL even though the server's
		// internal addr might say "0.0.0.0:port" (unconnectable).
		WSURL:    deriveWSURL(r),
		LaunchID: s.launchID,
		// Use the exact predicate captured by handleWS before upgrade so
		// the client posture cannot disagree with LocalOnly enforcement.
		Remote:            !loopback.PeerAddress(r.RemoteAddr),
		Harness:           s.cfg.Harness,
		PageMarker:        s.cfg.PageMarker,
		BackendID:         backendID,
		ReplicaGeneration: replicaGeneration,
	})
}

// sessionAdmitsRequest reports whether the request presents a durable
// session credential the session core verifies as live. The resolver
// treats "no credential presented" as ok-with-empty-id, so the id check
// is what distinguishes an anonymous request from an authenticated one.
func (s *Server) sessionAdmitsRequest(r *http.Request) bool {
	resolve := s.cfg.SessionForRequest
	if resolve == nil {
		return false
	}
	id, ok := resolve(r)
	return ok && id != ""
}

// HealthPath is the one route on this listener that consults no
// credential. Its two consumers — the SPA's pre-WS compatibility check
// and the update watchdog — run precisely when no valid credential is
// held, so gating it would answer 404 for a restarted backend, which is
// indistinguishable from down and is the exact condition it exists to
// detect. Reasoning and posture: internal/surfaces.
const HealthPath = "/healthz"

// Health is the /healthz document: what backend this is and what version
// it runs. Deliberately two fields. It answers "is the thing I expect
// still there, and is it still the build I was talking to", which is all
// the pre-WS compatibility check and the update watchdog need; readiness
// keeps its own channel (/bootstrap.json's 503) rather than being folded
// in here, because a health probe that conflates "booting" with
// "unreachable" is the failure mode both consumers are trying to avoid.
//
// Additive-only, like every other wire shape: a field may be appended,
// never repurposed.
type Health struct {
	// Version is Config.Version, the semantic version stamped at link
	// time. Empty on an unstamped build, which reads as unknown.
	Version string `json:"version"`
	// BackendID identifies this backend. Empty until the history store
	// opens — the same "unknown, never a wildcard" rule the bootstrap
	// manifest carries.
	BackendID string `json:"backendId,omitempty"`
}

// handleHealthz serves the unauthenticated health document. The posture
// decision and its reasoning live on the route's row in
// internal/surfaces; the enforcement here is: GET or HEAD only, no
// credential consulted, no CORS header (so a foreign page may issue the
// request but can never read the answer), no-store, and the same
// security headers every other route sends.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store, max-age=0")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	backendID := ""
	if s.cfg.BackendIdentity != nil {
		backendID, _ = s.cfg.BackendIdentity()
	}
	h.Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Health{
		Version:   s.cfg.Version,
		BackendID: backendID,
	})
}

// withAssetHeaders wraps the asset handler with caching + security
// headers. Cache headers are content- and peer-aware: Vite-hashed asset
// paths (/assets/*) get a year of immutable caching for remote peers
// but no-store for loopback ones (see below), while index.html and "/"
// go out as no-cache so a fresh deploy isn't shadowed by a stale shell.
//
// Security headers come from WriteSecurityHeaders so the rule set stays
// in sync between this server and clientmode's stub.
func withAssetHeaders(next http.Handler, csp ContentSecurityPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		WriteSecurityHeaders(h, csp)
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			if loopback.PeerAddress(r.RemoteAddr) {
				// The only loopback consumer is the embedded webview,
				// which loads the SPA once per process and never
				// renavigates — a cached asset can never be reused.
				// Cacheable scripts DO cost: Blink's in-memory HTTP
				// cache retains their decoded text for the page's
				// lifetime (~6 MB measured 2026-07-21, renderer
				// web_cache/Script_resources). no-store keeps assets
				// out of that cache; the rare manual reload refetches
				// embedded bytes over loopback.
				h.Set("Cache-Control", "no-store")
				break
			}
			// Vite emits hashed filenames under /assets/, so the response
			// is content-addressable forever — a content change ships a
			// new path, never overwrites this one. Remote clients reload
			// across sessions, so the cache genuinely pays off there.
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
// every response of the wrapped handler.
func withCrossOriginIsolation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteCrossOriginIsolationHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

// DeriveWSURL turns the inbound HTTP request's Host into the matching
// ws:// URL on the same authority. Falls back to a loopback path only
// when r.Host is empty (synthetic test requests).
//
// Exported for internal/clientmode, whose stub answers a manifest for
// the page it serves and must name its own /ws the same way this server
// names its own — the SPA requires a same-origin wsUrl on every path.
func DeriveWSURL(r *http.Request) string { return deriveWSURL(r) }

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
	isLoopback := loopback.PeerAddress(r.RemoteAddr)

	// Resolve the durable session BEFORE the upgrade, so a credential
	// that has been revoked is refused with the same unfingerprintable
	// 404 a bad launch credential gets. Refusing after the upgrade would
	// hand the client a live socket first and close it a moment later,
	// which a reconnect ladder reads as a flaky network rather than as a
	// dead credential.
	//
	// Nil hook means no client presents a session yet, and every request
	// proceeds naming none — which the peer rule below then admits only
	// from this machine.
	//
	// A ticket on the URL takes precedence, and is spent whether or not
	// the upgrade goes on to succeed — that is what single use means. It
	// names the session; it does not authorize the connection, so the
	// session it names is re-checked for liveness before it is believed.
	// A ticket for a session revoked during the seconds it was in flight
	// must not resurrect it.
	sessionID := ""
	ticketProven := false
	if ticket := r.URL.Query().Get(WSTicketParam); ticket != "" {
		subject, spent := s.wsTickets.consume(ticket)
		if !spent || !s.sessionStillLive(subject) {
			http.NotFound(w, r)
			return
		}
		sessionID = subject
		// The spent ticket authenticates this connection: it was minted
		// moments ago by a request presenting the session's credential,
		// and a paired device holds no page credential after a backend
		// restart. The ambient-cookie arm below does NOT get this waiver
		// — a cookie is the browser's default behavior, not a deliberate
		// per-connection proof.
		ticketProven = true
	} else if resolve := s.cfg.SessionForRequest; resolve != nil {
		id, ok := resolve(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		sessionID = id
	}

	// A peer that is not on this machine must NAME a session (spec §4,
	// "Local clients"). The launch credential says which BACKEND LAUNCH a
	// client belongs to and nothing about WHICH client it is, so a
	// connection carrying only that credential is unattributable and
	// unrevocable — CloseSession has no id to reach it by, and the
	// per-RPC gate has no grant set to read. That is tolerable exactly
	// while the peer is one of this host's own processes (the embedded
	// webview, ao-harness, the e2e rig, the WSL launcher's notification
	// socket, a --connect stub carrying its page's socket), and it is not
	// tolerable off-host.
	//
	// This narrows what a sessionless credentialled connection may be; it
	// loosens nothing. A session-naming peer still had to clear
	// SessionForRequest above, and the launch credential is still
	// demanded by upgrade() wherever it was demanded before.
	//
	// The refusal is the same unfingerprintable http.NotFound a bad
	// credential and a missing route both get: a LAN scanner learns
	// nothing from which of the three refused it.
	if !isLoopback && sessionID == "" {
		http.NotFound(w, r)
		return
	}

	// Read the live (post-rebind) allow-list, not Config's static value.
	// A LAN-bind toggle rotates the allow-list under the same mu-guarded
	// swap as the listener; the upgrader must see whichever policy was
	// in effect when this handshake began.
	conn, err := upgrade(w, r, s.cred, s.currentOriginPatterns(), !isLoopback, ticketProven)
	if err != nil {
		// upgrade has already written the HTTP error code.
		return
	}

	profile := connProfile{
		isLoopback: isLoopback,
		remoteAddr: r.RemoteAddr,
		sessionID:  sessionID,
		// Read from the pre-upgrade request: websocket.Conn does not
		// re-expose the handshake URL, and the identity has to be in place
		// before the first RPC is dispatched.
		client: ParseClientIdentity(r.URL.Query()),
	}

	if !isLoopback {
		s.remoteConns.Add(1)
		defer s.remoteConns.Add(-1)
	}

	backendID := ""
	if s.cfg.BackendIdentity != nil {
		backendID, _ = s.cfg.BackendIdentity()
	}

	// Use the server's root context so Shutdown can cancel us promptly,
	// not r.Context() which net/http only cancels on connection close.
	runConnHandler(s.rootCtx, conn, s.cfg.Dispatcher, s.cfg.EventBus, connSettings{
		readLimit:         s.cfg.ReadLimit,
		maxConcurrentRPCs: s.cfg.MaxConcurrentRPCs,
		keepaliveInterval: s.cfg.KeepaliveInterval,
		pongTimeout:       s.cfg.KeepalivePongTimeout,
		sessionConns:      s.sessionConns,
		sessionLive:       s.cfg.SessionLive,
		sessionScopes:     s.cfg.SessionScopes,
		sessionRecheck:    s.cfg.SessionRecheckInterval,
		maxLifetime:       s.cfg.MaxRemoteConnLifetime,
		hello: helloFrame{
			Capabilities: serverCapabilities,
			BackendID:    backendID,
			// Sampled per accept: the field's whole purpose is letting a
			// client measure its own skew against this backend, which a
			// value cached at boot would silently corrupt by the process
			// uptime.
			ServerTimeMs: time.Now().UnixMilli(),
		},
	}, profile)
	// Best-effort close. Read errors above already represent a closed
	// connection; any normal-closure send here is for the other half
	// of the bidirectional handshake.
	_ = conn.Close(1000, "")
}
