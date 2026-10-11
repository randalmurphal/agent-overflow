// forge.go is the fake forge's harness surface: seeding the forge state
// ao-mockforge and the HTTP listener answer from, and reading back what
// the app asked it.
// Each invocation also fans out as a harness:forge event so a spec can
// await the call a UI action causes instead of polling.
package harnessrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/loopback"
)

func (h *Harness) onForgeInvocation(inv forgefake.Invocation) {
	if h.config.Host == nil {
		return
	}
	h.config.Host.Emit(eventchan.HarnessForge, inv)
}

// HarnessForgeSeed adds repositories, pull or merge requests, comments,
// review threads, CI and attachments to the fake forge (the fixture
// format is forgefake.Fixture). A repository already seeded under the
// same forge and project is replaced. Returns the fixture with every
// generated id filled in. HarnessReset clears it.
func (h *Harness) HarnessForgeSeed(raw json.RawMessage) (forgefake.Fixture, error) {
	var fixture forgefake.Fixture
	if err := decodeStrictJSON("forge fixture", raw, &fixture); err != nil {
		return forgefake.Fixture{}, err
	}
	seeded, err := h.forge.Seed(fixture)
	if err != nil {
		return forgefake.Fixture{}, fmt.Errorf("seed forge: %w", err)
	}
	return seeded, nil
}

// HarnessForgeInvocations returns every recorded gh or glab invocation
// with a sequence number above since: argv, cwd, stdin, the route that
// answered (or unhandled) and the exit status.
func (h *Harness) HarnessForgeInvocations(since int) forgefake.InvocationLog {
	return h.forge.Invocations(since)
}

// HarnessForgeOffline makes the fake forge unreachable (true) or reachable
// again (false): while offline every gh and glab call fails the way the
// real CLI does when the forge host does not resolve, every HTTP request
// is recorded as route "offline" and dropped without a reply, and the
// seeded state waits for the forge to come back. HarnessReset brings it
// back.
func (h *Harness) HarnessForgeOffline(offline bool) error {
	if offline {
		h.forge.SetOffline(true)
		return h.forgeAPIServer().setOffline(true)
	}
	if err := h.forgeAPIServer().setOffline(false); err != nil {
		return err
	}
	h.forge.SetOffline(false)
	return nil
}

// HarnessForgeRateLimit puts one quota pool of the fake forge under a
// rate limit until its reset (unix seconds): with remaining 0 the pool
// refuses every request as the forge does, otherwise it answers and
// reports the remaining quota (forgefake.RateLimit). The limit lifts at
// the reset; HarnessReset clears it.
func (h *Harness) HarnessForgeRateLimit(limit forgefake.RateLimit) error {
	return h.forge.SetRateLimit(limit)
}

// ForgeAPIEndpoint is where an isolated boot's forge API transport sends
// its requests (forgeapi.Options.Isolated): the fake's base URL and the
// fixed token it accepts.
type ForgeAPIEndpoint struct {
	BaseURL string
	Token   string
}

// ForgeAPIServer is the fake forge's own loopback HTTP listener, apart
// from the control server. Going offline closes every open connection,
// pooled keep-alive ones included, and keeps the listener, so each
// request the app sends while offline arrives on a fresh connection, is
// recorded, and gets no reply: the same *TransientError a dead network
// gives, not a 5xx or a reply on a stale connection. The port stays
// bound throughout, so nothing else on the machine can take it while
// the forge is offline.
type ForgeAPIServer struct {
	mu      sync.Mutex
	addr    string
	server  *http.Server
	offline bool
	done    bool

	// connsMu guards conns, the connections the server has accepted and
	// not yet closed, kept by its ConnState hook.
	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// StartForgeAPI starts the fake forge's HTTP listener for h and returns
// the endpoint the app's isolated forge API transport uses. Must run
// before App.Start, like StartControl.
func StartForgeAPI(h *Harness) (*ForgeAPIServer, ForgeAPIEndpoint, error) {
	if h == nil {
		return nil, ForgeAPIEndpoint{}, fmt.Errorf("harness receiver unavailable")
	}
	ln, err := net.Listen("tcp6", loopback.EphemeralIPv6)
	if err != nil {
		return nil, ForgeAPIEndpoint{}, fmt.Errorf("forge API: listen on %s: %w", loopback.EphemeralIPv6, err)
	}
	s := &ForgeAPIServer{addr: ln.Addr().String(), conns: make(map[net.Conn]struct{})}
	server := &http.Server{Handler: h.forge, ReadHeaderTimeout: 10 * time.Second, ConnState: s.trackConn}
	s.server = server
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("harness forge API: serve: %v", err)
		}
	}()
	h.mu.Lock()
	h.forgeAPI = s
	h.mu.Unlock()
	return s, ForgeAPIEndpoint{BaseURL: "http://" + s.addr, Token: h.forgeToken}, nil
}

// trackConn keeps the set of open connections setOffline closes. The
// server reports StateNew before it reads the first request.
func (s *ForgeAPIServer) trackConn(conn net.Conn, state http.ConnState) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	switch state {
	case http.StateNew:
		s.conns[conn] = struct{}{}
	case http.StateHijacked, http.StateClosed:
		delete(s.conns, conn)
	}
}

// setOffline closes every open connection (true); the engine, already
// offline, records and drops each request that arrives after. Coming back
// (false) changes nothing here. Repeating either state is a no-op.
func (s *ForgeAPIServer) setOffline(offline bool) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || offline == s.offline {
		return nil
	}
	s.offline = offline
	if !offline {
		return nil
	}
	s.connsMu.Lock()
	open := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		open = append(open, conn)
	}
	s.connsMu.Unlock()
	var errs []error
	for _, conn := range open {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("forge API: drop connections: %w", err)
	}
	return nil
}

// Shutdown closes the listener and every connection for good.
func (s *ForgeAPIServer) Shutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	if s.server == nil {
		return
	}
	if err := s.server.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: forge API shutdown: %v\n", err)
	}
	s.server = nil
}

// forgeAPIServer returns the started listener, nil before StartForgeAPI.
func (h *Harness) forgeAPIServer() *ForgeAPIServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forgeAPI
}
