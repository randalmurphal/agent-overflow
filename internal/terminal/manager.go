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
