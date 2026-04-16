// Package terminal manages PTY-backed shell processes for thread-scoped
// terminals. The package exposes three types:
//
//   - Process wraps a single PTY + child process pair. It owns the pty
//     master fd, the *os.Process, and a goroutine that pumps output.
//   - Session owns a Process plus a bounded replay ring buffer and
//     fan-out to event subscribers.
//   - Manager maps terminalID -> *Session and is the public surface.
//
// Errors are surfaced explicitly: spawn failures return errors to the caller,
// read failures close the output channel and feed into an exit event.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// defaultRows/Cols are used when the caller does not specify a size.
const (
	defaultRows uint16 = 24
	defaultCols uint16 = 80

	// killGrace is how long we wait after SIGTERM before SIGKILL.
	killGrace = 500 * time.Millisecond
)

// ProcessConfig parametrises a PTY spawn.
type ProcessConfig struct {
	Shell string // absolute path; empty means /bin/sh
	Args  []string
	Cwd   string
	Env   []string // if nil, inherit os.Environ()
	Rows  uint16
	Cols  uint16
}

// ExitStatus captures the result of a process exit.
type ExitStatus struct {
	Code   int
	Signal syscall.Signal
	Reason string // human-readable, e.g. "exit" or "signal:SIGKILL"
}

// Process is one PTY-backed shell. It is not thread-safe for concurrent
// Start/Close calls; once started, Write/Resize/Kill are safe from different
// goroutines.
type Process struct {
	cmd *exec.Cmd
	pty *osFilePty

	output chan []byte   // closed when read loop exits
	done   chan struct{} // closed when Wait returns
	exit   ExitStatus
	exitMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// Start spawns the process. On success Process.Output() returns a channel of
// raw byte chunks from the PTY until the process exits.
func Start(cfg ProcessConfig) (*Process, error) {
	shell, args := resolveShell(cfg.Shell, cfg.Args)
	rows, cols := cfg.Rows, cfg.Cols
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}

	cmd := exec.Command(shell, args...)
	cmd.Dir = cfg.Cwd
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}

	ws := &pty.Winsize{Rows: rows, Cols: cols}
	f, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("terminal: pty spawn: %w", err)
	}

	p := &Process{
		cmd:    cmd,
		pty:    &osFilePty{File: f},
		output: make(chan []byte, 64),
		done:   make(chan struct{}),
	}

	go p.pumpOutput()
	go p.awaitExit()

	return p, nil
}

// Output returns the channel from which PTY output chunks are delivered.
// The channel is closed when the PTY read loop ends, which happens on process
// exit or fatal read error.
func (p *Process) Output() <-chan []byte {
	return p.output
}

// Done returns a channel that is closed when the process has exited.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// ExitStatus returns the exit result. Valid only after <-p.Done().
func (p *Process) ExitStatus() ExitStatus {
	p.exitMu.Lock()
	defer p.exitMu.Unlock()
	return p.exit
}

// PID returns the OS pid of the child process.
func (p *Process) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Write sends bytes to the PTY master (i.e. the shell's stdin).
func (p *Process) Write(data []byte) error {
	if _, err := p.pty.Write(data); err != nil {
		return fmt.Errorf("terminal: pty write: %w", err)
	}
	return nil
}

// Resize updates the PTY winsize.
func (p *Process) Resize(rows, cols uint16) error {
	if err := p.pty.resize(rows, cols); err != nil {
		return fmt.Errorf("terminal: pty resize: %w", err)
	}
	return nil
}

// Kill sends SIGKILL to the process group and waits for exit.
// pty.StartWithSize sets Setsid=true, so the PTY creates a new session whose
// session leader pid equals the child pid. Signalling the process group via
// -pid reaches any descendants spawned under the shell.
func (p *Process) Kill() error {
	return p.shutdown(syscall.SIGKILL, 0)
}

// Close performs graceful shutdown: SIGTERM the group, wait killGrace,
// then SIGKILL if still alive. Closing the PTY fd is always performed.
func (p *Process) Close() error {
	return p.shutdown(syscall.SIGTERM, killGrace)
}

func (p *Process) shutdown(initialSig syscall.Signal, grace time.Duration) error {
	var firstErr error
	p.closeOnce.Do(func() {
		pid := p.PID()
		if pid > 0 {
			// Negative pid signals the whole process group.
			if err := syscall.Kill(-pid, initialSig); err != nil && !isAlreadyDead(err) {
				firstErr = fmt.Errorf("terminal: signal group: %w", err)
			}
		}

		if grace > 0 {
			select {
			case <-p.done:
				// exited during grace window
			case <-time.After(grace):
				if pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
			}
		}

		// Closing the PTY master unblocks the read goroutine even if the
		// child already exited.
		if err := p.pty.Close(); err != nil && firstErr == nil && !errors.Is(err, io.EOF) {
			firstErr = fmt.Errorf("terminal: close pty: %w", err)
		}
		<-p.done
		p.closeErr = firstErr
	})
	return p.closeErr
}

// pumpOutput reads from the PTY in a loop and pushes chunks into the output
// channel. Exits on read error or EOF and closes the channel.
func (p *Process) pumpOutput() {
	defer close(p.output)
	buf := make([]byte, 4096)
	for {
		n, err := p.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			p.output <- chunk
		}
		if err != nil {
			// EOF and closed-pty both mean the child has exited (or the
			// fd was closed from our side). Either way we stop.
			return
		}
	}
}

// awaitExit waits for the child process to end and records the exit status.
func (p *Process) awaitExit() {
	defer close(p.done)
	err := p.cmd.Wait()
	status := ExitStatus{}
	if err == nil {
		status.Code = 0
		status.Reason = "exit"
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status.Code = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					status.Signal = ws.Signal()
					status.Reason = fmt.Sprintf("signal:%s", ws.Signal())
				} else {
					status.Reason = "exit"
				}
			} else {
				status.Reason = "exit"
			}
		} else {
			status.Code = -1
			status.Reason = fmt.Sprintf("wait-failed:%v", err)
		}
	}
	p.exitMu.Lock()
	p.exit = status
	p.exitMu.Unlock()
}

// isAlreadyDead reports whether the syscall.Kill error indicates the target
// process is already gone. These are safe to ignore.
func isAlreadyDead(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
