package app

import (
	"fmt"
	"log"
	"net"
	"sync"

	"agent-overflow/internal/loopback"
	"agent-overflow/internal/transport"
)

// localListenerState is the transport's ::1 door for clients on this side
// of a WSL boundary: provider sessions' `ao` CLI (AO_ENDPOINT) and the
// local control file read by `pair`, `service` and a desktop window
// adopting a running backend. The main bind stays IPv4 because Windows
// reaches it through WSL's localhost forwarding, which carries IPv4 only;
// in-distro clients use ::1 so they stay in the Linux kernel
// (loopback.EphemeralIPv6). ServeAuxiliary gives it the main bind's routes,
// credentials and session registry, and it outlives LAN rebinds. While it
// is down those clients use the main bind.
type localListenerState struct {
	mu   sync.Mutex
	ln   net.Listener
	aux  *transport.AuxListener
	addr string
}

// startLocalListener runs once the transport is serving, before the local
// control file is first published and before any session can start.
func (a *App) startLocalListener() {
	srv := a.transportServer.Load()
	if srv == nil {
		return
	}
	ln, err := net.Listen("tcp6", loopback.EphemeralIPv6)
	if err != nil {
		a.bootPhaseFailed(fmt.Errorf("local ::1 listener unavailable, local clients use %s: %w", srv.Addr(), err))
		return
	}
	s := &a.localListener
	s.mu.Lock()
	defer s.mu.Unlock()
	aux, err := srv.ServeAuxiliary(ln, func(cause error) { a.localListenerFailed(ln, cause) })
	if err != nil {
		if closeErr := ln.Close(); closeErr != nil {
			log.Printf("transport: close unused local listener: %v", closeErr)
		}
		a.bootPhaseFailed(fmt.Errorf("local ::1 listener unavailable, local clients use %s: %w", srv.Addr(), err))
		return
	}
	s.ln, s.aux, s.addr = ln, aux, ln.Addr().String()
	log.Printf("transport: local listener on %s", s.addr)
}

// localListenerFailed retires a listener whose accept loop ended, points the
// local control file back at the main bind, and leaves AO_ENDPOINT for new
// sessions on the main bind too. Sessions already holding the ::1 endpoint
// fail their CLI calls visibly until they restart.
func (a *App) localListenerFailed(ln net.Listener, cause error) {
	s := &a.localListener
	s.mu.Lock()
	if s.ln != ln {
		s.mu.Unlock()
		return
	}
	aux := s.aux
	s.ln, s.aux, s.addr = nil, nil, ""
	s.mu.Unlock()
	log.Printf("transport: local listener %s stopped, local clients now use the main bind: %v", ln.Addr(), cause)
	if aux != nil {
		if err := aux.Close(); err != nil {
			log.Printf("transport: detach failed local listener: %v", err)
		}
	}
	a.publishLocalControl()
}

// localListenerAddr is the ::1 host:port local clients dial, or "" while
// the listener is down and they use the main bind.
func (a *App) localListenerAddr() string {
	s := &a.localListener
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
