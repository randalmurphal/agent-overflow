package terminal

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// ErrTerminalNotFound is returned when a terminal ID does not match an active
// session.
var ErrTerminalNotFound = errors.New("terminal not found")

// OutputCallback is invoked for each output chunk emitted by any session.
type OutputCallback func(threadID, terminalID string, sequence uint64, data []byte)

// ExitCallback is invoked once a session's process has exited.
type ExitCallback func(threadID, terminalID string, status ExitStatus)

// Manager owns the collection of active terminal sessions. It is the sole
// public surface for terminal operations; all app-level bindings route
// through it.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session // terminalID -> session

	onOutput OutputCallback
	onExit   ExitCallback
}

// NewManager constructs a Manager with the supplied output and exit
// callbacks. The callbacks are invoked from per-session goroutines and must
// be safe for concurrent use.
func NewManager(onOutput OutputCallback, onExit ExitCallback) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		onOutput: onOutput,
		onExit:   onExit,
	}
}

// Open starts a new terminal session for the given thread. The caller
// receives a summary describing the freshly spawned session.
func (m *Manager) Open(threadID string, opts SessionOptions) (SessionSummary, error) {
	if threadID == "" {
		return SessionSummary{}, fmt.Errorf("terminal: thread ID required")
	}
	if opts.Cwd == "" {
		return SessionSummary{}, fmt.Errorf("terminal: cwd required")
	}

	terminalID := uuid.NewString()
	outputCB := m.sessionOutputCB(threadID)
	exitCB := m.sessionExitCB(threadID, terminalID)

	sess, err := newSession(terminalID, threadID, opts, outputCB, exitCB)
	if err != nil {
		return SessionSummary{}, err
	}

	m.mu.Lock()
	m.sessions[terminalID] = sess
	m.mu.Unlock()

	return sess.Summary(), nil
}

// Write sends data to the terminal with the given ID.
func (m *Manager) Write(terminalID string, data []byte) error {
	sess, err := m.get(terminalID)
	if err != nil {
		return err
	}
	return sess.Write(data)
}

// Resize changes the winsize of the given terminal.
func (m *Manager) Resize(terminalID string, rows, cols uint16) error {
	sess, err := m.get(terminalID)
	if err != nil {
		return err
	}
	return sess.Resize(rows, cols)
}

// Refresh forces the given terminal's child to repaint (a winsize nudge that
// leaves the visible size unchanged). Backs the UI "refresh" affordance that
// clears a glitched provider TUI frame without the user resizing by hand.
func (m *Manager) Refresh(terminalID string) error {
	sess, err := m.get(terminalID)
	if err != nil {
		return err
	}
	return sess.Refresh()
}

// Close terminates the terminal. Returns without error if the terminal does
// not exist (idempotent close).
func (m *Manager) Close(terminalID string) error {
	m.mu.Lock()
	sess, ok := m.sessions[terminalID]
	if ok {
		delete(m.sessions, terminalID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return sess.Close()
}

// List returns summaries of all active sessions belonging to the given
// thread.
func (m *Manager) List(threadID string) []SessionSummary {
	m.mu.Lock()
	summaries := make([]SessionSummary, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if sess.ThreadID() == threadID {
			summaries = append(summaries, sess.Summary())
		}
	}
	m.mu.Unlock()
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].StartedAt == summaries[j].StartedAt {
			return summaries[i].TerminalID < summaries[j].TerminalID
		}
		return summaries[i].StartedAt < summaries[j].StartedAt
	})
	return summaries
}

// ThreadProcess is one running PTY, named by the thread that owns it.
// The dev-server scan (internal/devscan) traces a listening socket back
// to a thread through these, so a dev server the person started by hand
// in a thread's terminal is attributed to that thread.
type ThreadProcess struct {
	ThreadID string
	// PID is the shell. Every PTY spawn sets Setpgid, so it is also the
	// group id everything typed into that terminal inherits.
	PID int
}

// ThreadProcesses returns the running PTY of every thread, in one pass.
// List answers for one thread; this is the whole-manager view the
// dev-server scan needs, and it is deliberately not List in a loop over
// thread ids the caller would have to know first.
func (m *Manager) ThreadProcesses() []ThreadProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	processes := make([]ThreadProcess, 0, len(m.sessions))
	for _, sess := range m.sessions {
		summary := sess.Summary()
		if !summary.Running || summary.PID <= 0 {
			continue
		}
		processes = append(processes, ThreadProcess{ThreadID: summary.ThreadID, PID: summary.PID})
	}
	return processes
}

// MoveThread reassigns all active sessions from one thread key to another.
// The PTYs keep running; only their owner key and future output/exit events
// change. Returns updated summaries for the moved sessions.
func (m *Manager) MoveThread(fromThreadID, toThreadID string) ([]SessionSummary, error) {
	if fromThreadID == "" {
		return nil, fmt.Errorf("terminal: source thread ID required")
	}
	if toThreadID == "" {
		return nil, fmt.Errorf("terminal: target thread ID required")
	}
	if fromThreadID == toThreadID {
		return m.List(toThreadID), nil
	}

	m.mu.Lock()
	allSessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		allSessions = append(allSessions, sess)
	}
	m.mu.Unlock()

	sessions := make([]*Session, 0)
	for _, sess := range allSessions {
		if sess.ThreadID() == fromThreadID {
			sessions = append(sessions, sess)
		}
	}

	for _, sess := range sessions {
		terminalID := sess.ID()
		sess.rebindThread(
			toThreadID,
			m.sessionOutputCB(toThreadID),
			m.sessionExitCB(toThreadID, terminalID),
		)
	}

	summaries := make([]SessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		summaries = append(summaries, sess.Summary())
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].StartedAt == summaries[j].StartedAt {
			return summaries[i].TerminalID < summaries[j].TerminalID
		}
		return summaries[i].StartedAt < summaries[j].StartedAt
	})
	return summaries, nil
}

// Replay returns the replay buffer for the given terminal.
func (m *Manager) Replay(terminalID string) ([]byte, error) {
	sess, err := m.get(terminalID)
	if err != nil {
		return nil, err
	}
	return sess.Replay(), nil
}

// ReplaySnapshot returns replay bytes plus the output sequence watermark for
// the given terminal.
func (m *Manager) ReplaySnapshot(terminalID string) (ReplaySnapshot, error) {
	sess, err := m.get(terminalID)
	if err != nil {
		return ReplaySnapshot{}, err
	}
	return sess.ReplaySnapshot(), nil
}

// Summary returns the current summary (winsize, pid, timing) for one terminal.
// Backs the take-control attach path, which sizes a freshly mounted xterm to the
// PTY's last-drawn dimensions before writing the replay.
func (m *Manager) Summary(terminalID string) (SessionSummary, error) {
	sess, err := m.get(terminalID)
	if err != nil {
		return SessionSummary{}, err
	}
	return sess.Summary(), nil
}

// Restart closes the current session and starts a fresh one with the same
// options. The new session's ID replaces the old one.
func (m *Manager) Restart(terminalID string) (SessionSummary, error) {
	m.mu.Lock()
	sess, ok := m.sessions[terminalID]
	if ok {
		delete(m.sessions, terminalID)
	}
	m.mu.Unlock()
	if !ok {
		return SessionSummary{}, ErrTerminalNotFound
	}

	threadID := sess.ThreadID()
	opts := sess.opts
	// Close the old session before spawning the replacement.
	_ = sess.Close()

	return m.Open(threadID, opts)
}

// CloseThread closes every active session belonging to the given thread.
// Errors are collected and returned as a joined error (best-effort cleanup).
func (m *Manager) CloseThread(threadID string) error {
	m.mu.Lock()
	var toClose []*Session
	for id, sess := range m.sessions {
		if sess.ThreadID() == threadID {
			toClose = append(toClose, sess)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, sess := range toClose {
		if err := sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Shutdown kills every active session. Called during app shutdown.
func (m *Manager) Shutdown() error {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		all = append(all, sess)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	var errs []error
	for _, sess := range all {
		if err := sess.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) get(terminalID string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[terminalID]
	if !ok {
		return nil, ErrTerminalNotFound
	}
	return sess, nil
}

func (m *Manager) sessionOutputCB(threadID string) outputEmitter {
	if m.onOutput == nil {
		return nil
	}
	return func(terminalID string, seq uint64, data []byte) {
		m.onOutput(threadID, terminalID, seq, data)
	}
}

func (m *Manager) sessionExitCB(threadID, terminalID string) exitEmitter {
	return func(id string, status ExitStatus) {
		// Drop the session from the map on exit so List/Get reflect reality.
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		if m.onExit != nil {
			m.onExit(threadID, id, status)
		}
	}
}
