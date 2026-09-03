package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Auxiliary listeners: a second (or third) way in, answered by exactly
// the same server (docs/specs/remote-access.md §7, "Multi-listener, one
// session store").
//
// The tailnet node is the caller this exists for. It hands over a
// listener it acquired itself — netstack, not net.Listen — and this
// package serves it with the SAME mux, the same credential checks, the
// same origin and Host rules, the same per-RPC scope gate and the same
// session registry the main bind uses. There is no second API, no second
// credential class, and no route that exists on one listener and not the
// other. A session revoked from the settings pane stops working on both
// in the same instant, because both consult the one live-session
// registry.
//
// Three properties are the whole design:
//
//   - THE CALLER OWNS THE LISTENER. This package accepts on it and stops
//     when told to; it never binds one, never rebinds one, and never
//     reopens one that failed. Whatever produced it decides when it
//     exists (internal/app's reconciler, for the tailnet).
//
//   - PER-LISTENER ISOLATION. An auxiliary accept loop that ends is
//     reported to the CALLER's sink, never to Server.ServeErr, and never
//     touches the main listener. The spec names the failure this avoids
//     by name: one integration failing to start must degrade that
//     listener only. Reporting it on the shared channel would make a
//     tailnet that could not accept read as the app's own transport
//     dying.
//
//   - ITS OWN http.Server, NOT THE ACTIVE ONE. Rebind retires the active
//     server and shuts it down, and http.Server.Shutdown closes every
//     listener it is serving — so an auxiliary listener attached to the
//     active server would be closed by an unrelated LAN-bind toggle, and
//     the only symptom would be a tailnet that silently stopped
//     answering. The instance is different; the handler, the timeouts
//     and the connection context are the same object graph, built by the
//     same buildHTTPServer that builds the main one.
//
// No TLS sniffing is installed on an auxiliary listener, deliberately.
// The sniff wrapper belongs to bindListener because a bind acquires a
// raw TCP socket that a pinning client may speak TLS to; a tailnet
// listener's bytes are already encrypted and authenticated by WireGuard
// before this package sees them, and the node's own HTTPS listener
// arrives here as a listener that has ALREADY terminated TLS (tsnet
// resolves that certificate itself). Wrapping either would classify a
// first byte that is not ours to classify.

// AuxListener is one attached listener. Close detaches it; the caller
// holds it for exactly that.
type AuxListener struct {
	server *Server
	srv    *http.Server
	ln     net.Listener

	closeOnce sync.Once
	closeErr  error
}

// auxShutdownTimeout bounds the graceful shutdown of a detached
// auxiliary server. Same budget a rebind gives the listener it retires,
// and for the same reason: it covers in-flight HTTP handlers only. An
// upgraded WebSocket has taken its connection off net/http's books and
// owns its own lifetime, so the final Close is what severs it.
const auxShutdownTimeout = 5 * time.Second

// ServeAuxiliary serves ln with this server's routes and credentials
// until the returned handle is closed.
//
// fail receives the accept loop's terminal error, once, if it ended for
// a reason that is not a shutdown. It may be nil, which means the caller
// does not want to know — but a caller that publishes the listener's
// state to a person should pass one, because a listener that stopped
// accepting is not otherwise observable from here.
func (s *Server) ServeAuxiliary(ln net.Listener, fail func(error)) (*AuxListener, error) {
	if ln == nil {
		return nil, fmt.Errorf("transport: ServeAuxiliary needs a listener")
	}
	if s.shutDown.Load() {
		return nil, fmt.Errorf("transport: server is shut down")
	}

	s.mu.Lock()
	// A handler's connection context is the server's root context, which
	// exists only after Start. Serving before then would hand net/http a
	// nil BaseContext result.
	if s.rootCtx == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("transport: server has not started, so it cannot serve an auxiliary listener yet")
	}
	if s.shutDown.Load() {
		s.mu.Unlock()
		return nil, fmt.Errorf("transport: server is shut down")
	}
	aux := &AuxListener{server: s, srv: s.buildHTTPServer(), ln: ln}
	s.auxListeners = append(s.auxListeners, aux)
	s.mu.Unlock()

	s.wg.Go(func() {
		err := aux.srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return
		}
		log.Printf("transport: auxiliary listener %s: %v", ln.Addr(), err)
		if fail != nil {
			fail(err)
		}
	})
	return aux, nil
}

// Close detaches the listener and stops serving it. Idempotent, and safe
// to race with Server.Shutdown.
//
// The listener itself is closed here — the caller handed it over, and a
// detach that left it accepting would leave connections arriving at a
// server that is going away.
func (a *AuxListener) Close() error {
	a.closeOnce.Do(func() {
		a.server.dropAuxListener(a)
		ctx, cancel := context.WithTimeout(context.Background(), auxShutdownTimeout)
		defer cancel()
		err := a.srv.Shutdown(ctx)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			a.closeErr = err
		}
		// Shutdown closes the listener it was serving, but a Serve that
		// never started (an attach that raced a shutdown) leaves it open.
		if lnErr := a.ln.Close(); lnErr != nil && !errors.Is(lnErr, net.ErrClosed) && a.closeErr == nil {
			a.closeErr = lnErr
		}
	})
	return a.closeErr
}

// dropAuxListener removes a handle from the registry. Called by Close,
// and tolerant of a handle Shutdown already took.
func (s *Server) dropAuxListener(target *AuxListener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, aux := range s.auxListeners {
		if aux == target {
			s.auxListeners = append(s.auxListeners[:i], s.auxListeners[i+1:]...)
			return
		}
	}
}

// closeAuxListeners detaches everything still attached. Called from
// Shutdown, before the wait on the serve goroutines those listeners feed.
func (s *Server) closeAuxListeners() {
	s.mu.Lock()
	attached := s.auxListeners
	s.auxListeners = nil
	s.mu.Unlock()
	for _, aux := range attached {
		if err := aux.Close(); err != nil {
			log.Printf("transport: close auxiliary listener: %v", err)
		}
	}
}
