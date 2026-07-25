package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// longPollWindow bounds a /commands request. Mocks immediately re-poll,
// so the value only trades idle-connection lifetime against reconnect
// chatter.
const longPollWindow = 25 * time.Second

// commandQueueCap bounds buffered commands per mock. Live driving is
// human/agent paced; a full queue means the mock stopped draining and
// the enqueue error should surface to the RPC caller.
const commandQueueCap = 64

// MockInfo is the backend-side view of a registered mock, exposed
// through HarnessListMocks.
type MockInfo struct {
	MockID       string       `json:"mockId"`
	Registration Registration `json:"registration"`
	Scenario     string       `json:"scenario"`
	RegisteredAt time.Time    `json:"registeredAt"`
	// Exited is set when the mock posts its exiting report. Commands to
	// an exited mock are refused — nothing will ever drain them. A mock
	// killed without reporting (SIGKILL) stays Exited=false; liveness
	// tracking is report-based by design.
	Exited bool `json:"exited"`
	// SessionConfig is the permission/sandbox configuration this mock
	// observed the app launch it with, latched from its ReportSessionConfig.
	// nil until that report arrives (or for a mock that never posts one).
	SessionConfig *SessionConfig `json:"sessionConfig,omitempty"`
}

// Assignment is what the backend's resolver hands a registering mock.
type Assignment struct {
	// ScenarioName is recorded for MockInfo / harness:mock events.
	ScenarioName string
	// ScenarioJSON is the validated scenario document.
	ScenarioJSON json.RawMessage
	// FixtureRoot resolves relative fixture paths.
	FixtureRoot string
}

// ServerConfig wires the two backend callbacks.
type ServerConfig struct {
	// Resolve picks the scenario for a registering mock. Called on the
	// register request's goroutine; an error refuses the registration
	// (the mock falls back to its no-control behaviour).
	Resolve func(Registration) (Assignment, error)
	// OnReport receives every mock progress report (already tagged
	// with its MockID + Registration). The backend re-emits these as
	// harness:mock events.
	OnReport func(info MockInfo, rep Report)
}

// Server is the loopback control listener. One per harness process.
type Server struct {
	cfg   ServerConfig
	token string

	mu    sync.Mutex
	seq   int
	mocks map[string]*mockConn

	// done releases active long-polls on Shutdown. http.Server.Shutdown
	// waits for handlers but does not cancel their request contexts, so
	// without this a connected /commands poll would pin shutdown for
	// the full 25s window.
	done chan struct{}

	listener net.Listener
	httpSrv  *http.Server
}

type mockConn struct {
	info     MockInfo
	commands chan Command
}

// NewServer builds an unstarted server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Resolve == nil {
		return nil, errors.New("control: ServerConfig.Resolve is required")
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("control: generate token: %w", err)
	}
	return &Server{
		cfg:   cfg,
		token: base64.RawURLEncoding.EncodeToString(buf[:]),
		mocks: make(map[string]*mockConn),
		done:  make(chan struct{}),
	}, nil
}

// Start binds 127.0.0.1:0 and serves. Returns once the listener is
// bound; serve errors after that are logged (the harness dying with the
// control plane is visible enough through failed session starts).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("control: listen: %w", err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", s.auth(s.handleRegister))
	mux.HandleFunc("GET /commands", s.auth(s.handleCommands))
	mux.HandleFunc("POST /report", s.auth(s.handleReport))
	s.httpSrv = &http.Server{Handler: mux}

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("harness control: serve: %v", err)
		}
	}()
	return nil
}

// Addr returns the bound address ("127.0.0.1:<port>").
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Token returns the bearer token mocks must present.
func (s *Server) Token() string { return s.token }

// Shutdown stops the listener and releases every long-poll.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	s.mu.Lock()
	select {
	case <-s.done:
		// Already shut down once.
	default:
		close(s.done)
	}
	s.mu.Unlock()
	return s.httpSrv.Shutdown(ctx)
}

// Command queues a live command for a mock. Errors when the mock is
// unknown, has exited, or its queue is full (not draining).
func (s *Server) Command(mockID string, cmd Command) error {
	s.mu.Lock()
	conn, ok := s.mocks[mockID]
	var exited bool
	if ok {
		exited = conn.info.Exited
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("control: unknown mock %q", mockID)
	}
	if exited {
		return fmt.Errorf("control: mock %q has exited; nothing will consume the command", mockID)
	}
	select {
	case conn.commands <- cmd:
		return nil
	default:
		return fmt.Errorf("control: mock %q command queue full", mockID)
	}
}

// ClearMocks drops every registration — the harness-reset hook, called
// after all provider sessions are stopped so no live mock is orphaned.
// The id sequence keeps counting so mock ids never repeat across a
// reset (a stale id from a previous test can only miss, never alias a
// new mock). A straggler poll from a dying process gets "unknown mock"
// and the mock client falls back to standalone behaviour on its way
// out.
func (s *Server) ClearMocks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.mocks)
}

// Mocks lists registered mocks in registration order.
func (s *Server) Mocks() []MockInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MockInfo, 0, len(s.mocks))
	for _, conn := range s.mocks {
		out = append(out, conn.info)
	}
	// Registration order == numeric suffix order; a simple sort keeps
	// the list stable for tests.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].RegisteredAt.After(out[j].RegisteredAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// auth wraps a handler with bearer-token verification. Mismatches get
// 404 (matching the transport server's don't-fingerprint convention).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var reg Registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, "bad registration", http.StatusBadRequest)
		return
	}
	assignment, err := s.cfg.Resolve(reg)
	if err != nil {
		log.Printf("harness control: refuse registration from pid %d (%s in %s): %v", reg.PID, reg.Protocol, reg.Cwd, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	s.mu.Lock()
	s.seq++
	info := MockInfo{
		MockID:       fmt.Sprintf("mock-%d", s.seq),
		Registration: reg,
		Scenario:     assignment.ScenarioName,
		RegisteredAt: time.Now(),
	}
	s.mocks[info.MockID] = &mockConn{
		info:     info,
		commands: make(chan Command, commandQueueCap),
	}
	s.mu.Unlock()

	s.report(info, Report{Kind: ReportRegistered, Detail: assignment.ScenarioName})

	writeJSON(w, RegisterResponse{
		MockID:      info.MockID,
		Scenario:    assignment.ScenarioJSON,
		FixtureRoot: assignment.FixtureRoot,
	})
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	mockID := r.URL.Query().Get("mock")
	s.mu.Lock()
	conn, ok := s.mocks[mockID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown mock", http.StatusNotFound)
		return
	}

	// Long-poll: wait for the first command, then drain whatever else
	// is immediately available so a burst arrives as one response.
	var out []Command
	select {
	case cmd := <-conn.commands:
		out = append(out, cmd)
		for {
			select {
			case cmd := <-conn.commands:
				out = append(out, cmd)
			default:
				writeJSON(w, out)
				return
			}
		}
	case <-time.After(longPollWindow):
		writeJSON(w, []Command{})
	case <-s.done:
		// Server shutting down; release the poll with an empty batch so
		// Shutdown isn't pinned for the rest of the window.
		writeJSON(w, []Command{})
	case <-r.Context().Done():
		// Client went away; nothing to write.
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	mockID := r.URL.Query().Get("mock")
	s.mu.Lock()
	conn, ok := s.mocks[mockID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown mock", http.StatusNotFound)
		return
	}
	var rep Report
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	switch {
	case rep.Kind == ReportExiting:
		s.mu.Lock()
		conn.info.Exited = true
		s.mu.Unlock()
	case rep.Kind == ReportSessionConfig && rep.SessionConfig != nil:
		// Latched onto MockInfo as well as fanned out as an event so a test
		// can read it after the fact (HarnessListMocks) instead of racing the
		// harness:mock stream — the config is observable before the first
		// turn, long before a test knows to start listening.
		s.mu.Lock()
		observed := *rep.SessionConfig
		conn.info.SessionConfig = &observed
		s.mu.Unlock()
	}
	s.mu.Lock()
	info := conn.info
	s.mu.Unlock()
	s.report(info, rep)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) report(info MockInfo, rep Report) {
	if s.cfg.OnReport != nil {
		s.cfg.OnReport(info, rep)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("harness control: encode response: %v", err)
	}
}
