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
	"syscall"
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

	// ReadLimit caps the byte size of a single inbound WS message.
	// Zero defaults to DefaultReadLimit (16 MiB).
	ReadLimit int64

	// MaxConcurrentRPCs caps how many in-flight RPC handlers a single
	// connection can have at once. Zero defaults to
	// DefaultMaxConcurrentRPCs (64).
	MaxConcurrentRPCs int

	// OriginPatterns is the WS origin allow-list. Empty (default) uses
	// InsecureSkipVerify — appropriate for loopback. Phase E (LAN bind
	// toggle) populates this with the configured remote origins.
	OriginPatterns []string

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
	rootCancel context.CancelFunc

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
	return &Server{
		cfg:            cfg,
		token:          token,
		originPatterns: originPatterns,
		serveErr:       make(chan error, 1),
	}, nil
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
	s.rootCtx, s.rootCancel = context.WithCancel(context.Background())

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
	if s.cfg.AssetHandler != nil {
		mux.Handle("/", withAssetHeaders(s.cfg.AssetHandler))
	} else {
		mux.Handle("/", withAssetHeaders(http.NotFoundHandler()))
	}
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
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := srv.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return
		}
		log.Printf("transport: http serve: %v", err)
		select {
		case s.serveErr <- err:
		default:
		}
	}()
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
			s.rootCancel()
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

// RebindOptions carries optional per-rebind configuration. A nil value
// (or zero-valued struct) leaves the corresponding field unchanged —
// today only OriginPatterns is settable, but new fields can be added
// without breaking callers that pass nil for "no overrides".
type RebindOptions struct {
	// OriginPatterns replaces the live WS origin allow-list under the
	// same mu-guarded swap as the listener. Pass nil to leave the
	// existing allow-list in place. An explicit empty slice (length 0,
	// non-nil) clears the allow-list — equivalent to "loopback /
	// InsecureSkipVerify" mode.
	OriginPatterns []string
}

// Rebind opens a new listener on addr, swaps it in as the active
// listener for new accepts, and gracefully shuts down the old
// listener+server. Existing WebSocket connections remain on the old
// http.Server (their goroutines are owned by handleWS, which uses the
// shared rootCtx — they're untouched by the rebind) and continue to
// drain naturally as users close tabs. The retired http.Server is
// Closed for real on Server.Shutdown.
//
// When opts.OriginPatterns is non-nil, the live allow-list is updated
// atomically with the listener swap so the new bind enforces the new
// origin policy from its first accept. A nil opts (or nil
// OriginPatterns) leaves the existing allow-list in place. New
// connections after the swap pass through the post-rebind allow-list;
// already-upgraded WebSockets are unaffected (origin is a handshake-
// time check).
//
// Used by the LAN-bind toggle: switching between 127.0.0.1 and
// 0.0.0.0 should not interrupt an in-flight session. Returns when
// the new listener is bound — caller can read Addr() to confirm.
//
// Atomicity: on any error (listen failure, racing Shutdown), the
// server's observable state is unchanged — Addr(), origin allow-list,
// and the active http.Server are exactly what they were before the
// call. Callers may retry without compounding rollback complexity.
//
// Concurrency: Rebind is safe to call concurrently with Shutdown —
// the rebind drops if Shutdown wins. Sequential Rebind calls are
// serialised via rebindMu so two toggles can't race each other into
// a torn state. A no-op (addr equals current Addr() and OriginPatterns
// nil) short-circuits with nil error so callers don't need to compare.
//
// Linux address-family overlap: BSD/Darwin allow a fresh 0.0.0.0:N
// bind while 127.0.0.1:N is still held by the same process; Linux
// rejects it with EADDRINUSE. The LAN-bind toggle keeps the same port
// across the host flip, so on Linux the optimistic "bind new before
// retiring old" pattern fails. We detect EADDRINUSE on the bind, close
// the old listener to release the kernel slot (hijacked WS connections
// are owned by their goroutines and survive listener close), and retry.
// If the retry also fails (the addr is genuinely held by a foreign
// process) we restore the old listener to satisfy the state-intact
// contract.
func (s *Server) Rebind(addr string, opts *RebindOptions) error {
	s.rebindMu.Lock()
	defer s.rebindMu.Unlock()

	if s.shutDown.Load() {
		return fmt.Errorf("transport: server is shut down")
	}

	// No-op shortcut: if the caller asks for the same addr and isn't
	// rotating origin patterns, there's nothing to do. Callers don't
	// need to compare addresses themselves — they can fire Rebind
	// blindly when settings change.
	s.mu.Lock()
	currentAddr := s.addr
	currentListener := s.listener
	s.mu.Unlock()
	if currentAddr != "" && opts == nil && addrsEquivalent(currentAddr, addr) {
		return nil
	}

	listener, err := s.bindRebindListener(addr, currentListener, currentAddr)
	if err != nil {
		return err
	}

	newSrv := s.buildHTTPServer()

	var evicted *http.Server

	s.mu.Lock()
	// shutDown could have flipped between the load above and acquiring
	// mu — verify under the lock so we don't leak the new listener.
	if s.shutDown.Load() {
		s.mu.Unlock()
		_ = listener.Close()
		return fmt.Errorf("transport: server is shut down")
	}
	oldSrv := s.srv
	s.listener = listener
	s.srv = newSrv
	s.addr = listener.Addr().String()
	if opts != nil && opts.OriginPatterns != nil {
		// Defensive copy: the caller's slice could mutate after Rebind
		// returns. The live allow-list is read on every WS upgrade, so
		// we don't want a downstream append to corrupt it.
		s.originPatterns = append([]string(nil), opts.OriginPatterns...)
	}
	if oldSrv != nil {
		s.formerSrvs = append(s.formerSrvs, oldSrv)
		// Hard cap: a rebind storm must not accumulate http.Servers
		// past the bound. Pop the oldest, schedule a force-close
		// outside the lock so we don't block other accessors.
		if len(s.formerSrvs) > MaxRetainedFormerSrvs {
			evicted = s.formerSrvs[0]
			s.formerSrvs = s.formerSrvs[1:]
		}
	}
	s.mu.Unlock()

	// Start the new serve loop before retiring the old one so there is
	// no window where new accepts have nowhere to go.
	s.serve(newSrv, listener)

	// Force-close any evicted entry from the cap pop. Done on a fresh
	// goroutine because Close() can block on hijacked WS sockets and we
	// don't want to extend Rebind's wall-clock for a slow shutdown.
	if evicted != nil {
		go func() {
			if err := evicted.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("transport: rebind: force-close evicted former server: %v", err)
			}
		}()
	}

	// Gracefully retire the old server. Shutdown returns once
	// in-flight HTTP handlers complete; hijacked WS conns are not
	// affected (their goroutines own the connection lifetime). A bg
	// context bounds the call — Server.Shutdown's eventual Close()
	// will sever any conns still holding on at process exit. When the
	// graceful shutdown completes, drop the entry from formerSrvs so
	// the slice naturally drains under steady-state churn.
	if oldSrv != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := oldSrv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("transport: rebind: shutdown old server: %v", err)
			}
			s.removeFormerSrv(oldSrv)
		}()
	}

	return nil
}

// bindRebindListener acquires the listener at addr, falling back to a
// close-old-then-retry sequence when the kernel rejects the bind because
// our own existing listener already holds the port (Linux self-overlap).
// On EADDRINUSE recovery, the old listener is closed via the supplied
// listener handle. If the retry still fails, we restore the old listener
// at oldAddr so the state-intact contract holds — both Addr() and the
// active http.Server stay observably the same as they were on entry.
//
// The recovery path only fires when the new and old addrs share a port
// — that's the exact shape of the LAN-bind toggle, and the only shape
// where Linux's address-family overlap rule would refuse a bind that
// our own listener is responsible for. EADDRINUSE on a different port
// is always a foreign holder; closing our own listener wouldn't help,
// so we propagate the error directly.
//
// All non-EADDRINUSE errors propagate as-is without touching the old
// listener; callers (Rebind) keep their state-intact guarantee for
// invalid / unresolvable addrs.
func (s *Server) bindRebindListener(addr string, oldListener net.Listener, oldAddr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener, nil
	}
	// Self-overlap can only happen when the new and old listeners share
	// a port. Without that, EADDRINUSE means a foreign holder and our
	// recovery path can't help.
	if !errors.Is(err, syscall.EADDRINUSE) || oldListener == nil || !samePort(addr, oldAddr) {
		return nil, fmt.Errorf("transport: rebind listen %s: %w", addr, err)
	}
	// Linux refuses 0.0.0.0:N while 127.0.0.1:N (or vice versa) is held
	// by this process, even though BSD/Darwin allow the overlap. Close
	// the old listener to release the kernel slot, then retry. Hijacked
	// WS connections live on independent goroutines and survive the
	// listener close — the in-flight session keeps working.
	if closeErr := oldListener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		log.Printf("transport: rebind: close old listener for retry: %v", closeErr)
	}
	listener, retryErr := net.Listen("tcp", addr)
	if retryErr == nil {
		return listener, nil
	}
	// Retry failed: a foreign holder owns the address even after we
	// freed our own slot. Restore the old listener so the state-intact
	// contract is honored. If even the rollback bind fails, surface
	// the original error along with the rollback error — the server is
	// genuinely degraded and the caller (and the user) should know.
	rollback, rollbackErr := net.Listen("tcp", oldAddr)
	if rollbackErr != nil {
		return nil, fmt.Errorf("transport: rebind listen %s: %w (state-intact rollback to %s also failed: %v)", addr, retryErr, oldAddr, rollbackErr)
	}
	s.mu.Lock()
	s.listener = rollback
	s.addr = rollback.Addr().String()
	s.mu.Unlock()
	s.serve(s.activeSrv(), rollback)
	return nil, fmt.Errorf("transport: rebind listen %s: %w", addr, retryErr)
}

// samePort reports whether two host:port strings name the same port.
// Used by bindRebindListener to scope the EADDRINUSE recovery path to
// the only conflict shape it can resolve (self-overlap on the same
// port). Either addr unparseable yields false — better to propagate
// the original EADDRINUSE than try a recovery whose preconditions are
// uncertain.
func samePort(a, b string) bool {
	_, pa, errA := net.SplitHostPort(a)
	if errA != nil {
		return false
	}
	_, pb, errB := net.SplitHostPort(b)
	if errB != nil {
		return false
	}
	return pa == pb
}

// activeSrv returns the live http.Server under mu. Used by the rollback
// path in bindRebindListener so the restored listener serves through the
// same handler the user already had.
func (s *Server) activeSrv() *http.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// removeFormerSrv drops the matching server from formerSrvs if it's
// still there. Called from the deferred Shutdown goroutine in Rebind so
// a successful graceful shutdown doesn't leave a phantom entry behind.
// Safe if the entry was already evicted by a concurrent rebind storm
// (e.g. cap-pop on a later Rebind beat us to it) — the linear search
// just no-ops.
func (s *Server) removeFormerSrv(target *http.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, srv := range s.formerSrvs {
		if srv == target {
			s.formerSrvs = append(s.formerSrvs[:i], s.formerSrvs[i+1:]...)
			return
		}
	}
}

// addrsEquivalent reports whether two "host:port" addresses represent
// the same listen target. The post-listener Addr is canonicalised by
// the kernel (port 0 becomes the resolved port, host names become IPs);
// callers typically pass an "intent" addr that needs the same rendering
// before comparison. For now we compare strings directly — the
// no-op shortcut is opportunistic, and a missed match falls through to
// a real rebind which is still correct, just slower.
func addrsEquivalent(a, b string) bool {
	return a == b
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
	conn, err := upgrade(w, r, s.token, s.currentOriginPatterns())
	if err != nil {
		// upgrade has already written the HTTP error code.
		return
	}

	// Use the server's root context so Shutdown can cancel us promptly,
	// not r.Context() which net/http only cancels on connection close.
	runConnHandler(s.rootCtx, conn, s.cfg.Dispatcher, s.cfg.EventBus, s.cfg.ReadLimit, s.cfg.MaxConcurrentRPCs, isLoopback)
	// Best-effort close. Read errors above already represent a closed
	// connection; any normal-closure send here is for the other half
	// of the bidirectional handshake.
	_ = conn.Close(1000, "")
}
