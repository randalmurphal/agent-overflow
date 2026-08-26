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
	"slices"
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

// defaultReapGrace is how long a mock's registration outlives the
// evidence that its process is gone — an `exiting` report, or a failed
// liveness probe of its pid.
//
// It exists so the two things a test does after a mock dies still work:
// read the final reports off HarnessListMocks, and see the row itself
// marked dead rather than simply absent (an absent row and a row that
// never registered are indistinguishable). Past the grace the row is
// removed, because otherwise every session stop outside HarnessReset —
// thread stop/delete, session restart, provider crash — leaks a
// registration and its 64-slot command channel for the life of the
// process, which over a soak is thousands of them.
const defaultReapGrace = 30 * time.Second

// maxTrackedPendingAdvances bounds MockInfo.PendingAdvances. The mock's
// own buffer is capped at the same order of magnitude; this is only the
// projection of it, and a runaway driver must not grow the registry.
const maxTrackedPendingAdvances = 32

// MockInfo is the backend-side view of a registered mock, exposed
// through HarnessListMocks.
type MockInfo struct {
	MockID       string       `json:"mockId"`
	Registration Registration `json:"registration"`
	Scenario     string       `json:"scenario"`
	RegisteredAt time.Time    `json:"registeredAt"`
	// Exited is set when the mock posts its exiting report, AND when a
	// liveness probe of Registration.PID finds the process gone. Commands
	// to an exited mock are refused — nothing will ever drain them.
	//
	// The pid probe is what makes a SIGKILLed mock read dead: it reports
	// nothing on its way out, and a registry that only believed reports
	// answered "live" for a corpse while Command() happily enqueued into
	// a channel with no reader and returned success.
	Exited bool `json:"exited"`
	// SessionConfig is the permission/sandbox configuration this mock
	// observed the app launch it with, latched from its ReportSessionConfig.
	// nil until that report arrives (or for a mock that never posts one).
	SessionConfig *SessionConfig `json:"sessionConfig,omitempty"`
	// OpenGate names the waitSignal gate this mock is currently blocked
	// on ("stall" for an indefinite stall step), empty when it is not
	// blocked. Projected from the mock's own waiting_signal /
	// advance_released reports: the mock is the only process that knows,
	// and it is by definition parked when the answer is interesting.
	OpenGate string `json:"openGate"`
	// PendingAdvances lists the advance commands the mock buffered
	// against a gate that had not opened yet, in arrival order. The
	// buffer is per-turn and the mock discards it at every turn boundary,
	// so an entry still here on a settled mock is a driving command that
	// did nothing.
	PendingAdvances []string `json:"pendingAdvances"`
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

	// reapGrace and alive are the two reaping knobs, injectable so the
	// tests can drive the sweep without sleeping or forking a process.
	reapGrace time.Duration
	alive     func(pid int) bool

	// done releases active long-polls on Shutdown. http.Server.Shutdown
	// waits for handlers but does not cancel their request contexts, so
	// without this a connected /commands poll would pin shutdown for
	// the full 25s window.
	done chan struct{}

	listener net.Listener
	httpSrv  *http.Server
}

type mockConn struct {
	// seq is the registration ordinal, and the sort key Mocks() uses.
	// RegisteredAt cannot be it: two mocks spawned in the same
	// millisecond tie, and time is not monotonic across a suspend.
	seq      int
	info     MockInfo
	commands chan Command
	// front holds a batch that was dequeued for a long-poll whose
	// response write then failed. The mock never received those bytes,
	// so they are re-queued AHEAD of the channel rather than lost — the
	// CLI has already told its caller the command was accepted.
	front []Command
	// exitedAt is when the mock reported exiting; deadSince is when a pid
	// probe first found the process gone. Either one starts the reap
	// clock. Both zero means the mock is live as far as anyone knows.
	exitedAt  time.Time
	deadSince time.Time
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
		cfg:       cfg,
		token:     base64.RawURLEncoding.EncodeToString(buf[:]),
		mocks:     make(map[string]*mockConn),
		reapGrace: defaultReapGrace,
		alive:     processAlive,
		done:      make(chan struct{}),
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
// unknown, has exited or died, or its queue is full (not draining).
//
// The liveness probe is the load-bearing half: without it a SIGKILLed
// mock accepted commands forever, the enqueue succeeded, the RPC
// answered OK, and the driving test waited on a gate nothing would ever
// release.
func (s *Server) Command(mockID string, cmd Command) error {
	s.mu.Lock()
	s.sweepLocked(time.Now())
	conn, ok := s.mocks[mockID]
	var exited bool
	if ok {
		exited = conn.info.Exited
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownMock, mockID)
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
// new mock). A straggler poll from a dying process gets "unknown mock",
// which its client treats as terminal and stops polling on.
func (s *Server) ClearMocks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.mocks)
}

// Mocks lists registered mocks in registration order, after sweeping
// dead ones: a mock whose pid is gone reads Exited, and one that has
// been gone longer than the reap grace is removed entirely.
func (s *Server) Mocks() []MockInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(time.Now())
	out := make([]MockInfo, 0, len(s.mocks))
	seqs := make(map[string]int, len(s.mocks))
	for id, conn := range s.mocks {
		info := conn.info
		info.PendingAdvances = slices.Clone(conn.info.PendingAdvances)
		if info.PendingAdvances == nil {
			info.PendingAdvances = []string{}
		}
		seqs[id] = conn.seq
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b MockInfo) int { return seqs[a.MockID] - seqs[b.MockID] })
	return out
}

// sweepLocked folds pid liveness into every registration and removes the
// ones that have been dead longer than the reap grace. Callers hold mu.
//
// A registration with no usable pid (<= 0 — a hand-built test
// registration, or a mock that could not read its own) is never probed:
// no pid is an absence of evidence, and reaping on it would drop live
// mocks.
func (s *Server) sweepLocked(now time.Time) {
	for id, conn := range s.mocks {
		if pid := conn.info.Registration.PID; pid > 0 && s.alive != nil && !s.alive(pid) {
			if conn.deadSince.IsZero() {
				conn.deadSince = now
			}
			conn.info.Exited = true
		} else {
			conn.deadSince = time.Time{}
		}
		since := conn.exitedAt
		if since.IsZero() || (!conn.deadSince.IsZero() && conn.deadSince.Before(since)) {
			since = conn.deadSince
		}
		if !since.IsZero() && now.Sub(since) >= s.reapGrace {
			delete(s.mocks, id)
		}
	}
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
	seq := s.seq
	info := MockInfo{
		MockID:          fmt.Sprintf("mock-%d", seq),
		Registration:    reg,
		Scenario:        assignment.ScenarioName,
		RegisteredAt:    time.Now(),
		PendingAdvances: []string{},
	}
	s.mocks[info.MockID] = &mockConn{
		seq:      seq,
		info:     info,
		commands: make(chan Command, commandQueueCap),
	}
	s.mu.Unlock()

	s.report(info, Report{Kind: ReportRegistered, Detail: assignment.ScenarioName})

	if err := writeJSON(w, RegisterResponse{
		MockID:      info.MockID,
		Scenario:    assignment.ScenarioJSON,
		FixtureRoot: assignment.FixtureRoot,
	}); err != nil {
		log.Printf("harness control: %s never received its registration: %v", info.MockID, err)
	}
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	mockID := r.URL.Query().Get("mock")
	s.mu.Lock()
	conn, ok := s.mocks[mockID]
	var out []Command
	if ok {
		out, conn.front = conn.front, nil
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, unknownMockBody, http.StatusNotFound)
		return
	}

	// A batch requeued by a previous failed write goes out first, in its
	// original order, ahead of anything that arrived since.
	if len(out) > 0 {
		s.deliver(w, conn, out)
		return
	}

	// Long-poll: wait for the first command, then drain whatever else
	// is immediately available so a burst arrives as one response.
	select {
	case cmd := <-conn.commands:
		s.deliver(w, conn, append(out, cmd))
	case <-time.After(longPollWindow):
		_ = writeJSON(w, []Command{})
	case <-s.done:
		// Server shutting down; release the poll with an empty batch so
		// Shutdown isn't pinned for the rest of the window.
		_ = writeJSON(w, []Command{})
	case <-r.Context().Done():
		// Client went away; nothing to write.
	}
}

// deliver drains whatever else is immediately available onto out and
// writes the batch. A failed write means the mock did NOT receive the
// batch — the response never left — so the whole thing goes back on the
// front of the queue rather than evaporating while HarnessMockCommand
// has already reported success to its caller.
func (s *Server) deliver(w http.ResponseWriter, conn *mockConn, out []Command) {
	for drained := false; !drained; {
		select {
		case cmd := <-conn.commands:
			out = append(out, cmd)
		default:
			drained = true
		}
	}
	if err := writeJSON(w, out); err != nil {
		s.requeueFront(conn, out)
		s.mu.Lock()
		info := conn.info
		s.mu.Unlock()
		log.Printf("harness control: %s command delivery failed, %d command(s) requeued: %v", info.MockID, len(out), err)
		s.report(info, Report{
			Kind:   ReportFixtureError,
			Detail: fmt.Sprintf("command delivery failed; %d command(s) requeued: %v", len(out), err),
		})
	}
}

// requeueFront puts a batch back at the head of the mock's queue,
// preserving order across repeated failures.
func (s *Server) requeueFront(conn *mockConn, batch []Command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn.front = append(slices.Clone(batch), conn.front...)
}

// unknownMockBody is the 404 body a mock's client recognises as
// terminal (control.ErrUnknownMock). Kept as one constant because the
// two halves live in different processes.
const unknownMockBody = "unknown mock"

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	mockID := r.URL.Query().Get("mock")
	s.mu.Lock()
	conn, ok := s.mocks[mockID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, unknownMockBody, http.StatusNotFound)
		return
	}
	var rep Report
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.applyReportLocked(conn, rep)
	info := conn.info
	info.PendingAdvances = slices.Clone(conn.info.PendingAdvances)
	s.mu.Unlock()
	s.report(info, rep)
	w.WriteHeader(http.StatusNoContent)
}

// applyReportLocked folds a report into the registration's latched
// state. Callers hold mu.
//
// The gate projection is derived rather than queried because the mock
// is the only process that knows and is parked precisely when the
// answer matters. It resyncs at every turn boundary, which is exactly
// where the engine clears its own advance buffer, so a missed or
// out-of-order report cannot accumulate.
func (s *Server) applyReportLocked(conn *mockConn, rep Report) {
	switch rep.Kind {
	case ReportExiting:
		conn.info.Exited = true
		if conn.exitedAt.IsZero() {
			conn.exitedAt = time.Now()
		}
	case ReportSessionConfig:
		if rep.SessionConfig != nil {
			// Latched onto MockInfo as well as fanned out as an event so a test
			// can read it after the fact (HarnessListMocks) instead of racing the
			// harness:mock stream — the config is observable before the first
			// turn, long before a test knows to start listening.
			observed := *rep.SessionConfig
			conn.info.SessionConfig = &observed
		}
	case ReportWaitingSignal:
		conn.info.OpenGate = rep.Detail
	case ReportAdvanceReleased:
		conn.info.OpenGate = ""
		conn.info.PendingAdvances = dropFirstMatchingAdvance(conn.info.PendingAdvances, rep.Gate)
	case ReportAdvanceBuffered:
		conn.info.OpenGate = rep.OpenGate
		if rep.Detail == AdvanceDroppedDetail {
			return
		}
		if len(conn.info.PendingAdvances) < maxTrackedPendingAdvances {
			conn.info.PendingAdvances = append(conn.info.PendingAdvances, rep.Gate)
		}
	case ReportTurnStarted, ReportTurnInterrupted, ReportScenarioDone:
		// Turn boundary: the engine drops its advance buffer here, so the
		// projection resyncs rather than carrying a stale entry forward.
		conn.info.OpenGate = ""
		conn.info.PendingAdvances = nil
	}
}

// dropFirstMatchingAdvance removes the buffered advance that released
// gate, using the engine's own matching rule (an unnamed advance
// releases any gate). A release with nothing buffered — the ordinary
// case, where the advance arrived while the gate was already open —
// drops nothing.
func dropFirstMatchingAdvance(pending []string, gate string) []string {
	for i, name := range pending {
		if name == "" || gate == "" || name == gate {
			return append(pending[:i:i], pending[i+1:]...)
		}
	}
	return pending
}

func (s *Server) report(info MockInfo, rep Report) {
	if s.cfg.OnReport != nil {
		s.cfg.OnReport(info, rep)
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("control: encode response: %w", err)
	}
	return nil
}
