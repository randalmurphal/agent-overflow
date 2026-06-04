package terminal

import (
	"sync"
	"sync/atomic"
	"time"
)

// maxReplayBytes caps the ring buffer per session. 256 KiB holds a few
// thousand lines of typical shell output — enough to re-hydrate an xterm on
// reconnect without unbounded memory growth.
const maxReplayBytes = 256 * 1024

// SessionOptions describes the caller-supplied knobs for a new session.
type SessionOptions struct {
	Shell string
	Args  []string
	Cwd   string
	Env   []string
	Rows  uint16
	Cols  uint16
}

// SessionSummary is the flat description of a session for listings.
type SessionSummary struct {
	TerminalID string `json:"terminalID"`
	ThreadID   string `json:"threadID"`
	Shell      string `json:"shell"`
	Cwd        string `json:"cwd"`
	Rows       uint16 `json:"rows"`
	Cols       uint16 `json:"cols"`
	PID        int    `json:"pid"`
	StartedAt  int64  `json:"startedAt"`
	Running    bool   `json:"running"`
	ExitCode   int    `json:"exitCode"`
	ExitReason string `json:"exitReason"`
}

// ReplaySnapshot is the replay buffer plus the last output sequence included
// in that snapshot. Clients use ThroughSequence to discard live output events
// that arrived before replay hydration completed.
type ReplaySnapshot struct {
	Data            []byte
	FromSequence    uint64
	ThroughSequence uint64
}

type replayChunkRange struct {
	sequence uint64
	bytes    int
}

// outputEmitter is invoked for each PTY output chunk. sequence monotonically
// increases across a session so clients can re-order if needed.
type outputEmitter func(terminalID string, sequence uint64, data []byte)

// exitEmitter is invoked once the process has exited and the output pump has
// drained.
type exitEmitter func(terminalID string, status ExitStatus)

// Session wraps a Process plus a bounded replay ring buffer and metadata.
// Thread-safe for concurrent Write/Resize/Replay/Close from different
// goroutines.
type Session struct {
	id       string
	threadID string
	shell    string
	cwd      string
	rows     uint16
	cols     uint16
	opts     SessionOptions
	started  time.Time

	proc *Process

	ring     *ringBuffer
	sequence atomic.Uint64
	ranges   []replayChunkRange
	ringSize int

	mu       sync.Mutex
	exit     ExitStatus
	running  bool
	onOutput outputEmitter
	onExit   exitEmitter
}

// newSession allocates and starts a session. The caller must hold the
// Manager's lock only long enough to register the session; Session itself
// does not depend on the Manager for correctness.
func newSession(id, threadID string, opts SessionOptions, onOutput outputEmitter, onExit exitEmitter) (*Session, error) {
	cfg := ProcessConfig{
		Shell: opts.Shell,
		Args:  opts.Args,
		Cwd:   opts.Cwd,
		Env:   opts.Env,
		Rows:  opts.Rows,
		Cols:  opts.Cols,
	}
	proc, err := Start(cfg)
	if err != nil {
		return nil, err
	}
	shell, _ := resolveShell(opts.Shell, opts.Args)

	rows, cols := opts.Rows, opts.Cols
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}

	s := &Session{
		id:       id,
		threadID: threadID,
		shell:    shell,
		cwd:      opts.Cwd,
		rows:     rows,
		cols:     cols,
		opts:     opts,
		started:  time.Now(),
		proc:     proc,
		ring:     newRingBuffer(maxReplayBytes),
		running:  true,
		onOutput: onOutput,
		onExit:   onExit,
	}
	go s.pump()
	return s, nil
}

// ID returns the terminal ID.
func (s *Session) ID() string { return s.id }

// ThreadID returns the thread the session belongs to.
func (s *Session) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *Session) rebindThread(threadID string, onOutput outputEmitter, onExit exitEmitter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threadID = threadID
	s.onOutput = onOutput
	s.onExit = onExit
}

// Write proxies to the underlying PTY.
func (s *Session) Write(data []byte) error {
	return s.proc.Write(data)
}

// Resize proxies to the underlying PTY and updates the cached size.
func (s *Session) Resize(rows, cols uint16) error {
	if err := s.proc.Resize(rows, cols); err != nil {
		return err
	}
	s.mu.Lock()
	s.rows = rows
	s.cols = cols
	s.mu.Unlock()
	return nil
}

// Replay returns the entire current replay buffer as a single byte slice.
// Callers should treat this as an opaque blob to feed straight into xterm.
func (s *Session) Replay() []byte {
	return s.ring.snapshot()
}

// ReplaySnapshot returns replay bytes and a sequence watermark atomically.
func (s *Session) ReplaySnapshot() ReplaySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fromSequence uint64
	if len(s.ranges) > 0 {
		fromSequence = s.ranges[0].sequence
	}
	return ReplaySnapshot{
		Data:            s.ring.snapshot(),
		FromSequence:    fromSequence,
		ThroughSequence: s.sequence.Load(),
	}
}

// Summary returns a flat description of the session.
func (s *Session) Summary() SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionSummary{
		TerminalID: s.id,
		ThreadID:   s.threadID,
		Shell:      s.shell,
		Cwd:        s.cwd,
		Rows:       s.rows,
		Cols:       s.cols,
		PID:        s.proc.PID(),
		StartedAt:  s.started.UnixMilli(),
		Running:    s.running,
		ExitCode:   s.exit.Code,
		ExitReason: s.exit.Reason,
	}
}

// Close requests graceful shutdown of the underlying process. Returns once
// the PTY and wait goroutines have unwound.
func (s *Session) Close() error {
	return s.proc.Close()
}

// Kill forces immediate process-group termination.
func (s *Session) Kill() error {
	return s.proc.Kill()
}

// Done returns a channel that is closed after the session has fully exited
// and its onExit callback has fired.
func (s *Session) Done() <-chan struct{} {
	return s.proc.Done()
}

// pump runs until the Process output channel closes, pushing each chunk into
// the ring buffer and invoking the emitters.
func (s *Session) pump() {
	for chunk := range s.proc.Output() {
		s.mu.Lock()
		seq := s.sequence.Add(1)
		s.ring.append(chunk)
		s.rememberReplayRange(seq, len(chunk))
		onOutput := s.onOutput
		s.mu.Unlock()
		if onOutput != nil {
			onOutput(s.id, seq, chunk)
		}
	}
	// Wait for process exit so we have a final status.
	<-s.proc.Done()
	status := s.proc.ExitStatus()
	s.mu.Lock()
	s.exit = status
	s.running = false
	onExit := s.onExit
	s.mu.Unlock()
	if onExit != nil {
		onExit(s.id, status)
	}
}

func (s *Session) rememberReplayRange(sequence uint64, byteCount int) {
	if byteCount <= 0 {
		return
	}
	s.ranges = append(s.ranges, replayChunkRange{sequence: sequence, bytes: byteCount})
	s.ringSize += byteCount
	for s.ringSize > maxReplayBytes && len(s.ranges) > 0 {
		overflow := s.ringSize - maxReplayBytes
		first := &s.ranges[0]
		if overflow < first.bytes {
			first.bytes -= overflow
			s.ringSize = maxReplayBytes
			return
		}
		s.ringSize -= first.bytes
		s.ranges = s.ranges[1:]
	}
}
