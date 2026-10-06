package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// rebindOldServerShutdownTimeout bounds the graceful shutdown of the
// retired http.Server after a Rebind. Hijacked WebSockets are
// untouched (their goroutines own the connection lifetime); this only
// covers in-flight HTTP handlers. 5s matches the upper bound a fresh
// HTTP request would tolerate before the client gives up.
const rebindOldServerShutdownTimeout = 5 * time.Second

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

// Rebind moves the server to addr and returns once addr is bound; the
// caller can read Addr() to confirm. Existing WebSocket connections are
// untouched: their goroutines are owned by handleWS on the shared
// rootCtx. A retired listener's http.Server is Closed for real on
// Server.Shutdown.
//
// A socket already bound to an address the move still needs is kept, not
// rebound. Closing a listening socket resets every connection the kernel
// has queued on it but the server has not yet accepted, so the LAN toggle
// never closes the loopback socket that the embedded webview and local
// browsers connect to: a move to 0.0.0.0:P binds the wildcard beside the
// existing 127.0.0.1:P socket, and a move back retires only the
// wildcard. Loopback connections reach the more specific 127.0.0.1
// socket whenever both exist. A move to another port binds the new
// sockets before retiring the old ones; clients of the old port have to
// move anyway.
//
// When opts.OriginPatterns is non-nil, the live allow-list is updated
// atomically with the listener swap so the new bind enforces the new
// origin policy from its first accept. A nil opts (or nil
// OriginPatterns) leaves the existing allow-list in place. New
// connections after the swap pass through the post-rebind allow-list;
// already-upgraded WebSockets are unaffected (origin is a handshake-
// time check).
//
// Atomicity: on any error (listen failure, racing Shutdown), the
// server's observable state is unchanged — Addr(), origin allow-list,
// and the serving listeners are exactly what they were before the
// call. Callers may retry without compounding rollback complexity.
//
// Concurrency: Rebind is safe to call concurrently with Shutdown —
// the rebind drops if Shutdown wins. Sequential Rebind calls are
// serialised via rebindMu so two toggles can't race each other into
// a torn state. A no-op (addr equals current Addr() and OriginPatterns
// nil) short-circuits with nil error so callers don't need to compare.
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
	held := []*endpoint{s.main, s.loopback}
	s.mu.Unlock()
	if currentAddr != "" && opts == nil && addrsEquivalent(currentAddr, addr) {
		return nil
	}

	main, loopback, bound, err := s.bindAt(addr, held)
	if err != nil {
		return fmt.Errorf("transport: rebind listen %s: %w", addr, err)
	}
	for _, ep := range bound {
		ep.srv = s.buildHTTPServer()
	}

	var evicted []*http.Server

	s.mu.Lock()
	// shutDown could have flipped between the load above and acquiring
	// mu — verify under the lock so we don't leak the new listeners.
	if s.shutDown.Load() {
		s.mu.Unlock()
		if closeErr := closeEndpoints(bound); closeErr != nil {
			return fmt.Errorf("transport: server is shut down (releasing %s: %v)", addr, closeErr)
		}
		return fmt.Errorf("transport: server is shut down")
	}
	s.main = main
	s.loopback = loopback
	s.addr = main.tcp.Addr().String()
	if opts != nil && opts.OriginPatterns != nil {
		// Defensive copy: the caller's slice could mutate after Rebind
		// returns. The live allow-list is read on every WS upgrade, so
		// we don't want a downstream append to corrupt it.
		s.originPatterns = append([]string(nil), opts.OriginPatterns...)
	}
	// held now lists only the endpoints the move does not keep.
	for _, ep := range held {
		if ep == nil {
			continue
		}
		s.formerSrvs = append(s.formerSrvs, ep.srv)
		// Hard cap: a rebind storm must not accumulate http.Servers
		// past the bound. Pop the oldest, schedule a force-close
		// outside the lock so we don't block other accessors.
		if len(s.formerSrvs) > MaxRetainedFormerSrvs {
			evicted = append(evicted, s.formerSrvs[0])
			s.formerSrvs = s.formerSrvs[1:]
		}
	}
	s.mu.Unlock()

	// Start the new serve loops before retiring the old ones so there is
	// no window where new accepts have nowhere to go.
	for _, ep := range bound {
		s.serve(ep)
	}
	for _, ep := range held {
		if ep != nil {
			s.retire(ep)
		}
	}

	// Force-close any evicted entry from the cap pop. Done on a fresh
	// goroutine because Close() can block on hijacked WS sockets and we
	// don't want to extend Rebind's wall-clock for a slow shutdown.
	for _, srv := range evicted {
		go func() {
			if err := srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("transport: rebind: force-close evicted former server: %v", err)
			}
		}()
	}
	return nil
}

// bindAt returns the endpoints that serve addr, reusing any of held
// already bound to an address addr needs and binding the rest. For the
// IPv4 wildcard that is two sockets: 127.0.0.1 on the port, then the
// wildcard beside it. Reused entries are cleared from held, so on success
// held is left with the endpoints the move retires. bound lists what this
// call bound; on error it has closed them.
func (s *Server) bindAt(addr string, held []*endpoint) (main, loopback *endpoint, bound []*endpoint, err error) {
	keep := func(want string) *endpoint {
		for i, ep := range held {
			if ep != nil && addrsEquivalent(ep.tcp.Addr().String(), want) {
				held[i] = nil
				return ep
			}
		}
		return nil
	}
	bind := func(addr string, beside *endpoint) (*endpoint, error) {
		ep, err := s.bindListener(addr, beside)
		if err != nil {
			if closeErr := closeEndpoints(bound); closeErr != nil {
				return nil, fmt.Errorf("%w (releasing a new listener also failed: %v)", err, closeErr)
			}
			return nil, err
		}
		bound = append(bound, ep)
		return ep, nil
	}

	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil || !wildcardIPv4(host) {
		if main = keep(addr); main == nil {
			if main, err = bind(addr, nil); err != nil {
				return nil, nil, nil, err
			}
		}
		return main, nil, bound, nil
	}
	loopbackAddr := net.JoinHostPort(loopbackIPv4, port)
	if loopback = keep(loopbackAddr); loopback == nil {
		if loopback, err = bind(loopbackAddr, nil); err != nil {
			return nil, nil, nil, err
		}
	}
	// A port-0 request is settled by the loopback bind.
	wildcardAddr := net.JoinHostPort(host, listenerPort(loopback))
	if main = keep(wildcardAddr); main == nil {
		if main, err = bind(wildcardAddr, loopback); err != nil {
			return nil, nil, nil, err
		}
	}
	return main, loopback, bound, nil
}

// retire stops ep accepting and lets its server drain.
func (s *Server) retire(ep *endpoint) {
	// Release the old bind before returning, so a quick toggle back does
	// not collide with a retired listener. Accepted HTTP requests and
	// hijacked WebSockets survive closing the listener.
	if err := ep.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("transport: rebind: close retired listener %s: %v", ep.tcp.Addr(), err)
	}
	// A closed listener refuses new dials, but connections it already
	// accepted keep serving requests until the retired server stops. Close
	// the idle keep-alive connections now and let in-flight requests close
	// theirs after responding, so no request reaches the old address once
	// Rebind returns. The server speaks only HTTP/1.1 (serverTLSConfig), so
	// this covers every connection net/http still tracks.
	ep.srv.SetKeepAlivesEnabled(false)

	// Gracefully retire the old server. Shutdown returns once
	// in-flight HTTP handlers complete; hijacked WS conns are not
	// affected (their goroutines own the connection lifetime). A bg
	// context bounds the call — Server.Shutdown's eventual Close()
	// will sever any conns still holding on at process exit. When the
	// graceful shutdown completes, drop the entry from formerSrvs so
	// the slice naturally drains under steady-state churn.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), rebindOldServerShutdownTimeout)
		defer cancel()
		// net.ErrClosed is the listener closed above, closed again.
		if err := ep.srv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, net.ErrClosed) {
			log.Printf("transport: rebind: shutdown old server: %v", err)
		}
		s.removeFormerSrv(ep.srv)
	}()
}

// closeEndpoints closes listeners nothing has served yet.
func closeEndpoints(eps []*endpoint) error {
	var errs []error
	for _, ep := range eps {
		if err := ep.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close %s: %w", ep.tcp.Addr(), err))
		}
	}
	return errors.Join(errs...)
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
