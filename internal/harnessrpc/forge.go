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
// from the control server. Going offline closes it with http.Server.Close,
// which drops every pooled keep-alive connection, and listens on the same
// port again, so each request the app sends while offline arrives on a
// fresh connection, is recorded, and gets no reply: the same
// *TransientError a dead network gives, not a 5xx or a reply on a stale
// connection.
type ForgeAPIServer struct {
	handler http.Handler

	mu      sync.Mutex
	addr    string
	server  *http.Server
	offline bool
	done    bool
}

// StartForgeAPI starts the fake forge's HTTP listener for h and returns
// the endpoint the app's isolated forge API transport uses. Must run
// before App.Start, like StartControl.
func StartForgeAPI(h *Harness) (*ForgeAPIServer, ForgeAPIEndpoint, error) {
	if h == nil {
		return nil, ForgeAPIEndpoint{}, fmt.Errorf("harness receiver unavailable")
	}
	s := &ForgeAPIServer{handler: h.forge, addr: loopback.EphemeralIPv6}
	if err := s.listen(); err != nil {
		return nil, ForgeAPIEndpoint{}, err
	}
	h.mu.Lock()
	h.forgeAPI = s
	h.mu.Unlock()
	return s, ForgeAPIEndpoint{BaseURL: "http://" + s.addr, Token: h.forgeToken}, nil
}

// listen binds s.addr (an ephemeral port the first time, the same port
// after) and serves on it. Callers hold mu or own s exclusively.
func (s *ForgeAPIServer) listen() error {
	ln, err := net.Listen("tcp6", s.addr)
	if err != nil {
		return fmt.Errorf("forge API: listen on %s: %w", s.addr, err)
	}
	s.addr = ln.Addr().String()
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	s.server = server
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("harness forge API: serve: %v", err)
		}
	}()
	return nil
}

// setOffline drops every open connection and listens again on the same
// port (true); the engine, already offline, records and drops each
// request that arrives. Coming back (false) keeps the listener, or
// listens again if going offline could not. Repeating either state is a
// no-op.
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
		if s.server == nil {
			// Going offline closed the listener but could not listen again.
			return s.listen()
		}
		return nil
	}
	err := s.server.Close()
	s.server = nil
	if err != nil {
		return fmt.Errorf("forge API: close: %w", err)
	}
	return s.listen()
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
