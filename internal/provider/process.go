package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"agent-overflow/internal/logging"
	"agent-overflow/internal/procutil"
)

const (
	// maxLineSize is the max stdout line buffer: 32 MB.
	// Raised from 10 MB to tolerate large diffs and command outputs that
	// a single provider line may contain (particularly when Claude emits
	// a turn/diff/updated notification with the full cumulative patch).
	maxLineSize = 32 * 1024 * 1024
)

// The escalation graces are variables only so tests can shorten them; nothing
// else assigns them.
var (
	// shutdownGrace is how long to wait after closing stdin before sending SIGTERM.
	shutdownGrace = 3 * time.Second

	// killGrace is how long to wait after SIGTERM before sending SIGKILL.
	killGrace = 2 * time.Second
)

// stopLeftovers ends what the leader left in its group. A test replaces it
// with a stop that fails.
var stopLeftovers = procutil.StopConfiguredGroup

// ErrLineTooLong is returned when a single stdout line exceeds maxLineSize.
// Callers should treat this as session-fatal: the subprocess is killed by
// ReadLine before the error is surfaced so no orphan process remains.
var ErrLineTooLong = errors.New("provider: stdout line exceeded maximum size")

// Process manages a subprocess with stdin/stdout pipes.
type Process struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	stdoutFile *os.File
	done       chan struct{}
	err        error
	// groupErr is set before done closes when processes the leader left in
	// its group could not be ended; the group then stays watched.
	groupErr         error
	mu               sync.Mutex
	eventLogger      *logging.Logger
	eventLogRedactor EventLogRedactor
	threadID         string
	provider         string
	stderrTail       *stderrTee
	// stderrDrained closes when the stderr drain reached EOF.
	stderrDrained chan struct{}
	// killOnce guards the one-shot kill triggered on oversized lines so
	// concurrent ReadLine failures (should never happen but defense in
	// depth) do not double-signal the process group.
	killOnce sync.Once
	// stdoutCloseOnce closes the parent-side read pipe after ReadLine has
	// drained it. We intentionally do not close this from the Wait goroutine:
	// doing so races process exit against the last stdout bytes.
	stdoutCloseOnce sync.Once
}

// SpawnConfig configures subprocess creation.
type SpawnConfig struct {
	Binary           string
	Args             []string
	Dir              string
	Env              EnvInput
	EventLogger      *logging.Logger
	EventLogRedactor EventLogRedactor
	ThreadID         string
	Provider         string
	// GracefulCancel replaces exec's default "SIGKILL the instant the context
	// is done" with SIGTERM-the-group, escalating to SIGKILL only after
	// killGrace. Opt-in, and opt-in only for children that can be holding a
	// credential mid-write: an account probe drives the CLI's own OAuth
	// refresh, and Anthropic retires the previous refresh token the moment the
	// token endpoint answers — so a deadline that kills the CLI between that
	// answer and its write to disk destroys the login with no copy anywhere.
	// SIGTERM lets the CLI finish the write and exit.
	//
	// Both signals go to the process group (procutil.StopConfiguredGroup),
	// so a helper that ignores SIGTERM is killed with the leader; exec's own
	// WaitDelay escalation would kill only the leader.
	//
	// Session spawns deliberately do not set it: their Close() already runs
	// the graceful stdin/TERM/KILL ladder, and a context cancel there is a
	// teardown that must not wait.
	GracefulCancel bool
}

// EventLogRedactor rewrites provider stdin/stdout lines before they are
// written to the optional raw provider-event log. Returning nil skips that
// log entry.
type EventLogRedactor func(direction string, data []byte) []byte

// Spawn starts a subprocess with stdin/stdout pipes and process group isolation.
// The context is associated with the command — canceling it will kill the process.
// Prefer Close() for graceful shutdown. lifetime says what ends the process if
// the backend dies first; a launch without one is refused before it starts.
func Spawn(ctx context.Context, lifetime Lifetime, cfg SpawnConfig) (*Process, error) {
	if err := lifetime.check(ctx); err != nil {
		return nil, err
	}
	reaper := lifetime.reaper
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Dir = cfg.Dir
	// applySysProcAttr is platform-split: POSIX wires up Setpgid so we
	// can signal the whole tree later; the Windows stub leaves the
	// default attrs in place because the launcher never spawns provider
	// children on Windows (the WSL-side Linux backend does).
	applySysProcAttr(cmd)
	if cfg.GracefulCancel {
		// Returning nil once the group is gone keeps exec's own accounting: the
		// exit leaves Wait reporting the context error, exactly as the default
		// Kill-based cancel does. A group the stop could not empty is Wait's
		// error, and settleGroup keeps it watched. WaitDelay only bounds Wait
		// once the stop has returned.
		cmd.Cancel = func() error {
			if err := procutil.StopConfiguredGroup(cmd, killGrace); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
			return nil
		}
		cmd.WaitDelay = killGrace
	}

	cmd.Env = BuildEnvironment(cfg.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdin pipe: %w", err)
	}

	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutWrite

	// Tee stderr: forward to our own stderr so provider debug output is
	// visible during development, and retain a bounded tail so an
	// unexpected exit can surface the provider's last words to the user
	// (e.g. a CLI rejecting an unknown flag exits 1 with the reason only
	// on stderr — invisible in the UI without this capture).
	//
	// Like stdout above, this is a manual os.Pipe rather than handing
	// exec an io.Writer: a writer would make cmd.Wait block on exec's
	// internal copy goroutine until stderr EOF, and a grandchild that
	// inherited the fd (backgrounded helper, MCP server) can hold that
	// open long past the provider's own exit. With the *os.File the fd
	// passes straight through and Wait returns on process exit; our
	// drain goroutine below consumes the read end independently.
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		return nil, fmt.Errorf("provider: stderr pipe: %w", err)
	}
	cmd.Stderr = stderrWrite
	stderrTail := &stderrTee{}
	stderrDrained := make(chan struct{})

	if err := cmd.Start(); err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		stderrRead.Close()
		stderrWrite.Close()
		return nil, fmt.Errorf("provider: start %s: %w", cfg.Binary, err)
	}
	reaper.Watch(cmd.Process.Pid)
	// The child inherited stdoutWrite. The parent must close its copy so the
	// reader observes EOF after the child exits and its final bytes drain.
	if err := stdoutWrite.Close(); err != nil {
		stdoutRead.Close()
		stderrRead.Close()
		stderrWrite.Close()
		return nil, fmt.Errorf("provider: close parent stdout writer: %w", err)
	}
	// Same for the parent's stderr write end. Best-effort: a failed
	// close only delays the drain goroutine's EOF, it doesn't affect
	// the session.
	_ = stderrWrite.Close()

	// Drain stderr until EOF. Owns closing the read end. Exits when
	// every writer fd is gone (the provider and any inheriting
	// descendants), which may be after cmd.Wait returns — that's fine,
	// the goroutine holds no Process state beyond the tee.
	go func() {
		defer close(stderrDrained)
		defer stderrRead.Close()
		buf := make([]byte, 4096)
		for {
			n, err := stderrRead.Read(buf)
			if n > 0 {
				_, _ = stderrTail.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// We use bufio.Reader (not Scanner) so we control the per-line size
	// check ourselves. Scanner's ErrTooLong path aborts the scan but leaves
	// the subprocess alive; our readNDJSONLine below kills the process on
	// overflow so no orphan remains. 64 KB initial buffer matches the
	// previous Scanner sizing; the reader grows internally as needed.
	reader := bufio.NewReaderSize(stdoutRead, 64*1024)

	p := &Process{
		cmd:              cmd,
		stdin:            stdin,
		stdout:           reader,
		stdoutFile:       stdoutRead,
		stderrTail:       stderrTail,
		stderrDrained:    stderrDrained,
		done:             make(chan struct{}),
		eventLogger:      cfg.EventLogger,
		eventLogRedactor: cfg.EventLogRedactor,
		threadID:         cfg.ThreadID,
		provider:         cfg.Provider,
	}

	// Wait goroutine: detect process exit. Wait sees only the leader; what
	// it left in the group (an MCP server, a backgrounded helper) has nobody
	// to manage it, so it is ended here, and the group is released only once
	// it is empty. Both land before done closes, so a returned Close has
	// nothing left running or watched.
	go func() {
		p.err = cmd.Wait()
		p.groupErr = p.settleGroup(reaper)
		close(p.done)
	}()

	return p, nil
}

// settleGroup ends what the exited leader left in its group and releases the
// group once it is empty. A group it cannot empty stays watched, so the orphan
// reaper still ends it with the backend, and the failure is logged and
// returned from Close and Kill.
func (p *Process) settleGroup(reaper procutil.Reaper) error {
	if procutil.ConfiguredGroupAlive(p.cmd) {
		if err := stopLeftovers(p.cmd, killGrace); err != nil && !errors.Is(err, os.ErrProcessDone) {
			err = fmt.Errorf("provider: end processes left in group %d: %w", p.cmd.Process.Pid, err)
			log.Printf("provider: provider=%q thread=%q: %v", p.provider, p.threadID, err)
			return err
		}
	}
	reaper.Release(p.cmd.Process.Pid)
	return nil
}

// WriteLine writes a line to stdin (appends newline).
func (p *Process) WriteLine(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("provider: process already exited")
	default:
	}

	_, err := p.stdin.Write(slices.Concat(data, []byte{'\n'}))
	if err != nil {
		return fmt.Errorf("provider: write to stdin: %w", err)
	}
	p.logEvent("out", data)
	return nil
}

// ReadLine reads the next line from stdout. Returns io.EOF when process exits.
//
// A single line is capped at maxLineSize bytes. When that cap is exceeded
// ReadLine asynchronously kills the subprocess (so no orphan keeps writing
// into a buffer nobody reads from) and returns ErrLineTooLong.
func (p *Process) ReadLine() ([]byte, error) {
	line, err := p.readBoundedLine()
	if err != nil {
		if errors.Is(err, ErrLineTooLong) {
			// Kill asynchronously: signalGroup is fast, but Wait on the
			// done channel could block behind stdout drainage. Callers
			// get the error immediately and the process is reaped by the
			// Wait goroutine once SIGKILL takes effect.
			p.killOnce.Do(func() {
				p.signalGroup(syscall.SIGKILL)
			})
			return nil, err
		}
		// cmd.Wait() closes the stdout pipe, which can race with the read.
		// Treat a closed-pipe error as EOF — the process is gone either way.
		if err == io.EOF || isClosedPipeErr(err) {
			p.closeStdout()
			return nil, io.EOF
		}
		return nil, err
	}
	p.logEvent("in", line)
	return line, nil
}

func (p *Process) closeStdout() {
	p.stdoutCloseOnce.Do(func() {
		if p.stdoutFile != nil {
			_ = p.stdoutFile.Close()
		}
	})
}

// readBoundedLine returns the next newline-terminated line from stdout with
// the trailing '\n' stripped. A line that would exceed maxLineSize causes
// the function to return ErrLineTooLong immediately, without draining the
// remainder of the line — the caller is expected to kill the subprocess so
// the pipe closes and further reads return io.EOF. Draining would block
// indefinitely when the subprocess writes a partial line and then sleeps
// without emitting a newline.
func (p *Process) readBoundedLine() ([]byte, error) {
	// ReadSlice moves whole buffered chunks per call instead of a per-byte
	// loop. Fast-path the common case where the full line fits inside
	// bufio's buffer so a ~120-byte streaming delta allocates an exact-size
	// slice instead of a 4 KiB buffer. Chunks returned by ReadSlice alias
	// bufio's internal buffer and MUST be copied before returning.
	chunk, err := p.stdout.ReadSlice('\n')
	switch err {
	case nil:
		n := len(chunk) - 1
		if n > maxLineSize {
			return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
		}
		line := make([]byte, n)
		copy(line, chunk[:n])
		return line, nil
	case io.EOF:
		if len(chunk) == 0 {
			return nil, err
		}
		if len(chunk) > maxLineSize {
			return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
		}
		line := make([]byte, len(chunk))
		copy(line, chunk)
		return line, nil
	case bufio.ErrBufferFull:
		if len(chunk) > maxLineSize {
			return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
		}
	default:
		return nil, err
	}

	buf := make([]byte, len(chunk), max(4096, len(chunk)*2))
	copy(buf, chunk)
	for {
		chunk, err := p.stdout.ReadSlice('\n')
		switch err {
		case nil:
			buf = append(buf, chunk[:len(chunk)-1]...)
			if len(buf) > maxLineSize {
				return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
			}
			return buf, nil
		case bufio.ErrBufferFull:
			// Chunk without a newline yet: accumulate and keep reading.
			buf = append(buf, chunk...)
			if len(buf) > maxLineSize {
				return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
			}
		case io.EOF:
			buf = append(buf, chunk...)
			if len(buf) > 0 {
				if len(buf) > maxLineSize {
					return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
				}
				// Final line without trailing newline: preserve it, next
				// call returns io.EOF.
				return buf, nil
			}
			return nil, err
		default:
			// Non-EOF read error mid-line: discard the partial line, same
			// as the previous per-byte loop.
			return nil, err
		}
	}
}

// isClosedPipeErr returns true if err indicates a closed pipe or file
// descriptor. This happens when the stdout pipe is closed out from under a
// read in flight — Close/Kill teardown racing the last ReadLine.
//
// Identity, not message text: an *fs.PathError from an *os.File wraps the
// syscall sentinel, and matching on the rendered sentence broke on any
// wrapper that re-worded it (and would silently match a provider line that
// merely CONTAINED the words). What each sentinel covers here:
//
//   - os.ErrClosed — the reachable one. Reading a closed *os.File yields
//     `read |0: file already closed`.
//   - syscall.EPIPE — `broken pipe`. A write-side errno; kept because the
//     previous matcher covered it and a stdin-shaped caller would hit it.
//   - io.ErrClosedPipe — io.Pipe's own sentinel. Not reachable through
//     os.Pipe, listed so an in-process pipe substituted for the fd later
//     lands in the same branch instead of surfacing as a session error.
func isClosedPipeErr(err error) bool {
	return errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE)
}

// Done returns a channel that closes when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Err returns the process exit error. Only valid after Done() is closed.
func (p *Process) Err() error {
	return p.err
}

// Close performs graceful shutdown:
// 1. Close stdin
// 2. Wait shutdownGrace for natural exit
// 3. SIGTERM the process group
// 4. Wait killGrace
// 5. SIGKILL the process group
//
// Returns nil when no process of the group is left, regardless of the
// leader's exit code; an error when one could not be ended.
// A subprocess exiting non-zero is the goal of Close, not a failure of
// it — propagating `cmd.Wait`'s ExitError up the call chain caused
// "Revert failed: Exit status 1" toasts when callers (e.g. revert,
// thread delete) intentionally tore down a Claude session that had
// just been interrupted. The exit status stays available via Err()
// for callers that need it; the unexpected-exit banner is emitted by
// the read loop only when closing.Load() is false, so genuine crashes
// still surface.
func (p *Process) Close() error {
	// Close stdin to signal the process to exit. Deliberately WITHOUT p.mu:
	// WriteLine holds that lock across its stdin write, so a writer blocked on
	// a full pipe (a child that stopped reading) owns mu until it drains — and
	// the unlocked close is what unblocks it, so taking mu here would deadlock
	// Close against the very caller it is recovering.
	p.stdin.Close()

	select {
	case <-p.done:
		return p.closed()
	case <-time.After(shutdownGrace):
	}

	// SIGTERM the process group.
	p.signalGroup(syscall.SIGTERM)

	select {
	case <-p.done:
		return p.closed()
	case <-time.After(killGrace):
	}

	// SIGKILL the process group.
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return p.closed()
}

// Kill immediately kills the process group. It returns the leader's exit
// error joined with a failure to end what the leader left in its group.
func (p *Process) Kill() error {
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return errors.Join(p.err, p.groupErr)
}

// closed is Close's result once done has closed.
func (p *Process) closed() error {
	return errors.Join(p.closeResult(p.err), p.groupErr)
}

// closeResult treats a subprocess exit (any code or signal) as a
// successful close — the process is gone, which is what Close was
// trying to make happen. We log abnormal exits so a misbehaving
// provider CLI still leaves a trail in the dev log, but the caller
// doesn't see it as a teardown failure. Any non-exit error (e.g. a
// future genuine close-operation failure) propagates unchanged.
func (p *Process) closeResult(err error) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		log.Printf("provider: intentional close provider=%q thread=%q pid=%d exit=%v", p.provider, p.threadID, p.PID(), exitErr)
		return nil
	}
	return err
}

// signalGroup sends a signal to the process group. The actual syscall
// is in the platform-split file (signalGroupPlatform). Windows has no
// process-group concept here; the stub returns without acting because
// the Windows binary never spawns provider children.
func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	signalGroupPlatform(p.cmd.Process.Pid, sig)
}

// PID returns the spawned subprocess's OS process id, or 0 before the
// process has started (or after a failed spawn). Because applySysProcAttr
// sets Setpgid, the child is its own process-group leader, so this value
// doubles as the process-group id callers pass to a negative-PID group
// kill — see signalGroupPlatform and the orphan reaper.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// StderrTail returns the most recent stderr output the subprocess
// wrote, bounded by stderrTailCap. Raw bytes — callers that put this
// in user-facing text MUST route it through SanitizeChildStderr (see
// ProcessExitInfo's security note).
//
// It is read after the process exited, so it first waits for the drain to
// take what the process wrote, at most stderrDrainWait: a descendant that
// inherited the fd can hold stderr open past the exit.
func (p *Process) StderrTail() string {
	select {
	case <-p.stderrDrained:
	case <-time.After(stderrDrainWait):
	}
	return p.stderrTail.Tail()
}

// stderrDrainWait matches WaitProcessExitErr's reap wait.
const stderrDrainWait = 100 * time.Millisecond

// stderrTailCap bounds the retained stderr tail. 2 KB holds a typical
// CLI startup error (arg parse failure, missing module, ENOENT) with
// room for a few preceding warning lines, while keeping a runaway
// debug stream from accumulating in memory for the session's lifetime.
const stderrTailCap = 2048

// stderrTee forwards subprocess stderr to the parent's stderr while
// retaining the last stderrTailCap bytes for exit diagnostics. Fed by
// the drain goroutine in Spawn, read by StderrTail after exit.
//
// Write never returns an error: the os.Stderr forward is best-effort
// (a closed fd or full pipe must not stop the capture this type
// exists for).
type stderrTee struct {
	mu   sync.Mutex
	tail []byte
}

func (t *stderrTee) Write(p []byte) (int, error) {
	_, _ = os.Stderr.Write(p)
	t.mu.Lock()
	t.tail = append(t.tail, p...)
	if len(t.tail) > stderrTailCap {
		trimmed := make([]byte, stderrTailCap)
		copy(trimmed, t.tail[len(t.tail)-stderrTailCap:])
		t.tail = trimmed
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *stderrTee) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.tail)
}

func (p *Process) logEvent(direction string, data []byte) {
	if p.eventLogger == nil || p.threadID == "" || p.provider == "" {
		return
	}
	if p.eventLogRedactor != nil {
		data = p.eventLogRedactor(direction, data)
		if data == nil {
			return
		}
	}

	if err := p.eventLogger.LogProviderEvent(logging.ProviderEventEntry{
		ThreadID:  p.threadID,
		Direction: direction,
		Provider:  p.provider,
		// Raw bytes, no string copy: the logger embeds valid JSON as-is
		// and the line buffer is fresh per event, never reused.
		Data: data,
	}); err != nil {
		log.Printf("provider: raw event log failed: %v", err)
	}
}
