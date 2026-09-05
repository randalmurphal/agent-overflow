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
// retiring old" pattern fails. We detect an address-in-use bind error,
// close the old listener to release the kernel slot (hijacked WS
// connections are owned by their goroutines and survive listener
// close), and retry. If the retry also fails (the addr is genuinely
// held by a foreign process) we restore the old listener to satisfy the
// state-intact contract.
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

	// Release the old bind before returning. On Darwin the new wildcard
	// listener can overlap the old loopback listener; leaving the latter
	// to asynchronous Shutdown makes a quick toggle back collide with a
	// retired listener that bindRebindListener cannot see. Accepted HTTP
	// requests and hijacked WebSockets survive closing the listener.
	if currentListener != nil {
		if err := currentListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("transport: rebind: close retired listener: %v", err)
		}
	}

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
			ctx, cancel := context.WithTimeout(context.Background(), rebindOldServerShutdownTimeout)
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
// our own existing listener already holds the address (Linux
// self-overlap). On that recovery path the old listener is closed via
// the supplied listener handle. If the retry still fails, we restore the
// old listener at oldAddr so the state-intact contract holds — both
// Addr() and the active http.Server stay observably the same as they
// were on entry.
//
// The recovery path only fires for an address-in-use error whose new and
// old addrs share a port — that's the exact shape of the LAN-bind
// toggle, and the only shape where Linux's address-family overlap rule
// would refuse a bind that our own listener is responsible for.
// Address-in-use on a different port is always a foreign holder; closing
// our own listener wouldn't help, so we propagate the error directly.
//
// Every other bind error — a permission/reservation refusal (EACCES /
// WSAEACCES), an address this host does not own, a malformed one —
// propagates as-is without touching the old listener, because closing a
// working listener cannot change any of those answers. Callers (Rebind)
// keep their state-intact guarantee for all of them.
func (s *Server) bindRebindListener(addr string, oldListener net.Listener, oldAddr string) (net.Listener, error) {
	listener, err := s.bindListener(addr)
	if err == nil {
		return listener, nil
	}
	// Self-overlap can only happen when the new and old listeners share
	// a port. Without that, address-in-use means a foreign holder and
	// our recovery path can't help. addrInUse rather than a bare
	// EADDRINUSE check (Windows reports WSAEADDRINUSE, which the POSIX
	// name does not match) and rather than portUnavailable, whose EACCES
	// half survives closing our own listener.
	if !addrInUse(err) || oldListener == nil || !samePort(addr, oldAddr) {
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
	listener, retryErr := s.bindListener(addr)
	if retryErr == nil {
		return listener, nil
	}
	// Retry failed: a foreign holder owns the address even after we
	// freed our own slot. Restore the old listener so the state-intact
	// contract is honored. If even the rollback bind fails, surface
	// the original error along with the rollback error — the server is
	// genuinely degraded and the caller (and the user) should know.
	rollback, rollbackErr := s.bindListener(oldAddr)
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
// Used by bindRebindListener to scope the address-in-use recovery path
// to the only conflict shape it can resolve (self-overlap on the same
// port). Either addr unparseable yields false — better to propagate
// the original bind error than try a recovery whose preconditions are
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
