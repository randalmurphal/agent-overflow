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
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-overflow/internal/logging"
)

const (
	// maxLineSize is the max stdout line buffer: 32 MB.
	// Raised from 10 MB to tolerate large diffs and command outputs that
	// a single provider line may contain (particularly when Claude emits
	// a turn/diff/updated notification with the full cumulative patch).
	maxLineSize = 32 * 1024 * 1024

	// shutdownGrace is how long to wait after closing stdin before sending SIGTERM.
	shutdownGrace = 3 * time.Second

	// killGrace is how long to wait after SIGTERM before sending SIGKILL.
	killGrace = 2 * time.Second
)

// ErrLineTooLong is returned when a single stdout line exceeds maxLineSize.
// Callers should treat this as session-fatal: the subprocess is killed by
// ReadLine before the error is surfaced so no orphan process remains.
var ErrLineTooLong = errors.New("provider: stdout line exceeded maximum size")

// Process manages a subprocess with stdin/stdout pipes.
type Process struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	done        chan struct{}
	err         error
	mu          sync.Mutex
	eventLogger *logging.Logger
	threadID    string
	provider    string
	// killOnce guards the one-shot kill triggered on oversized lines so
	// concurrent ReadLine failures (should never happen but defense in
	// depth) do not double-signal the process group.
	killOnce sync.Once
}

// SpawnConfig configures subprocess creation.
type SpawnConfig struct {
	Binary      string
	Args        []string
	Dir         string
	Env         map[string]string
	EventLogger *logging.Logger
	ThreadID    string
	Provider    string
}

// Spawn starts a subprocess with stdin/stdout pipes and process group isolation.
// The context is associated with the command — canceling it will kill the process.
// Prefer Close() for graceful shutdown.
func Spawn(ctx context.Context, cfg SpawnConfig) (*Process, error) {
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Build env: inherit current env + overrides.
	// PATH is treated as additive (prepend to existing PATH) rather than replacing it.
	env := os.Environ()
	for k, v := range cfg.Env {
		if strings.ToUpper(k) == "PATH" {
			if existing := os.Getenv("PATH"); existing != "" {
				v = v + string(os.PathListSeparator) + existing
			}
		}
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdout pipe: %w", err)
	}

	// Forward stderr so provider debug output is visible during development.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("provider: start %s: %w", cfg.Binary, err)
	}

	// We use bufio.Reader (not Scanner) so we control the per-line size
	// check ourselves. Scanner's ErrTooLong path aborts the scan but leaves
	// the subprocess alive; our readNDJSONLine below kills the process on
	// overflow so no orphan remains. 64 KB initial buffer matches the
	// previous Scanner sizing; the reader grows internally as needed.
	reader := bufio.NewReaderSize(stdoutPipe, 64*1024)

	p := &Process{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      reader,
		done:        make(chan struct{}),
		eventLogger: cfg.EventLogger,
		threadID:    cfg.ThreadID,
		provider:    cfg.Provider,
	}

	// Wait goroutine: detect process exit.
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
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

	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	_, err := p.stdin.Write(buf)
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
			return nil, io.EOF
		}
		return nil, err
	}
	p.logEvent("in", line)
	return line, nil
}

// readBoundedLine returns the next newline-terminated line from stdout with
// the trailing '\n' stripped. A line that would exceed maxLineSize causes
// the function to return ErrLineTooLong immediately, without draining the
// remainder of the line — the caller is expected to kill the subprocess so
// the pipe closes and further reads return io.EOF. Draining would block
// indefinitely when the subprocess writes a partial line and then sleeps
// without emitting a newline.
func (p *Process) readBoundedLine() ([]byte, error) {
	buf := make([]byte, 0, 4096)
	for {
		b, err := p.stdout.ReadByte()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				// Final line without trailing newline: preserve it, next
				// call returns io.EOF.
				return buf, nil
			}
			return nil, err
		}
		if b == '\n' {
			return buf, nil
		}
		if len(buf) >= maxLineSize {
			return nil, fmt.Errorf("%w (cap=%d bytes)", ErrLineTooLong, maxLineSize)
		}
		buf = append(buf, b)
	}
}

// isClosedPipeErr returns true if err indicates a closed pipe or file descriptor.
// This happens when cmd.Wait() closes stdout before the scanner finishes reading.
func isClosedPipeErr(err error) bool {
	return strings.Contains(err.Error(), "file already closed") ||
		strings.Contains(err.Error(), "broken pipe")
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
func (p *Process) Close() error {
	// Close stdin to signal the process to exit.
	p.stdin.Close()

	select {
	case <-p.done:
		return p.err
	case <-time.After(shutdownGrace):
	}

	// SIGTERM the process group.
	p.signalGroup(syscall.SIGTERM)

	select {
	case <-p.done:
		return p.err
	case <-time.After(killGrace):
	}

	// SIGKILL the process group.
	return p.Kill()
}

// Kill immediately kills the process group.
func (p *Process) Kill() error {
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return p.err
}

// signalGroup sends a signal to the process group.
func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	// Negative PID sends to the process group.
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
}

func (p *Process) logEvent(direction string, data []byte) {
	if p.eventLogger == nil || p.threadID == "" || p.provider == "" {
		return
	}

	if err := p.eventLogger.LogProviderEvent(logging.ProviderEventEntry{
		ThreadID:  p.threadID,
		Direction: direction,
		Provider:  p.provider,
		Data:      string(data),
	}); err != nil {
		log.Printf("provider: raw event log failed: %v", err)
	}
}
