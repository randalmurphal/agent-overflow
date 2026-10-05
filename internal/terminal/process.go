//go:build !windows

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
//
// A terminal lives as long as its shell. A job the shell left running in the
// background may hold the pty open long after, so the shell's exit, not the
// end of the pty's output, ends the terminal (Process.awaitExit).
//
// This file is POSIX-only. The Windows binary (cmd/agent-overflow-windows)
// never spawns terminals — it's a launcher around wsl.exe — so a stubbed
// Windows variant in process_windows.go satisfies the //... cross-compile
// without forcing a full Windows PTY port.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"agent-overflow/internal/procutil"
)

// defaultRows/Cols are used when the caller does not specify a size.
const (
	defaultRows uint16 = 24
	defaultCols uint16 = 80

	// killGrace is how long we wait after SIGTERM before SIGKILL.
	killGrace = 500 * time.Millisecond

	// exitDrainBytes bounds what the output pump reads once the shell has
	// exited. A pty holds far less between a writer and the master (about
	// 20 KiB on Linux, and its writers block past that), so everything the
	// shell wrote before it exited is delivered. A background job still
	// writing to the pty is cut off here.
	exitDrainBytes = 256 * 1024
)

// errOutputDrained ends the output pump once the shell has exited and the
// pty holds nothing more.
var errOutputDrained = errors.New("terminal: pty output drained after exit")

// ProcessConfig parametrises a PTY spawn.
type ProcessConfig struct {
	Shell string // absolute path; empty means /bin/sh
	Args  []string
	Cwd   string
	Env   []string // if nil, inherit os.Environ(); terminal capabilities are normalized
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
	// raw reads the master without blocking a thread (readOutput).
	raw syscall.RawConn

	output chan []byte   // closed when read loop exits
	done   chan struct{} // closed when Wait returns
	exit   ExitStatus
	exitMu sync.Mutex
	// exited is set once Wait has reaped the shell: the output pump then
	// reads only what the pty already holds.
	exited atomic.Bool

	closeOnce sync.Once
	closeErr  error

	ptyCloseOnce sync.Once
	ptyCloseErr  error

	// pauseNudge holds Refresh's nudged winsize for refreshNudgePause.
	// Tests replace it to hold the nudge until the child has reported it.
	pauseNudge func(time.Duration)
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
	cmd.Env = normalizeTerminalEnv(cfg.Env)

	ws := &pty.Winsize{Rows: rows, Cols: cols}
	f, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("terminal: pty spawn: %w", err)
	}
	master, err := pollableMaster(f)
	var raw syscall.RawConn
	if err == nil {
		raw, err = master.SyscallConn()
		if err != nil {
			_ = master.Close()
		}
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("terminal: pty master: %w", err)
	}

	p := &Process{
		cmd:        cmd,
		pty:        &osFilePty{File: master},
		raw:        raw,
		output:     make(chan []byte, 64),
		done:       make(chan struct{}),
		pauseNudge: time.Sleep,
	}

	go p.pumpOutput()
	go p.awaitExit()

	return p, nil
}

// Output returns the channel from which PTY output chunks are delivered.
// The channel is closed when the PTY read loop ends: once the shell has
// exited and the output it wrote before exiting is delivered, or on a fatal
// read error. The caller drains it; the pty closes after the last chunk.
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

// refreshNudgePause is how long Refresh holds the one-row-smaller winsize before
// restoring it. A back-to-back change+restore is coalesced by Node's tty layer
// (which Claude Code's Ink renderer sits on) into a net no-op, so the child
// never observes a change and never repaints. The pause must outlast one of the
// child's event-loop turns so it sees the shrink before the restore. 40ms sits
// comfortably above an idle Node turn (a spike measured ~25ms as sufficient)
// while staying imperceptible for a rare, user-initiated repaint.
const refreshNudgePause = 40 * time.Millisecond

// nudgeRows returns the transient row count Refresh shrinks (or grows) to before
// restoring rows, plus whether a nudge should run at all. A zero rows is not a
// valid winsize, so it reports ok=false. Shrinking by one row is the default; a
// single-row terminal can't shrink to a valid zero-row winsize, so it grows by
// one instead. Either result is a real winsize that differs from rows, which is
// what forces the child's SIGWINCH handler to observe a change and repaint.
func nudgeRows(rows uint16) (nudged uint16, ok bool) {
	switch rows {
	case 0:
		return 0, false
	case 1:
		return rows + 1, true
	default:
		return rows - 1, true
	}
}

// Refresh forces the child to repaint by briefly shrinking the PTY by one row
// and restoring it — the programmatic form of the manual terminal resize users
// do to clear a glitched TUI frame. A bare SIGWINCH is not enough: Node-based
// TUIs only re-render when the kernel reports a *changed* winsize, so we make a
// real change and undo it. Nudging rows (not cols) keeps the intermediate frame
// at the correct width, so the only visible transient is a one-row blip, never a
// horizontal reflow; shrinking (not growing) blanks the bottom row briefly
// instead of scrolling the whole screen.
//
// Refresh ends by restoring (rows, cols), so a child that ignores the nudge is a
// no-op. That fail-safe only holds because callers serialize size operations:
// Session.Refresh holds resizeMu across this whole call so a concurrent
// Session.Resize can't slip a different size in between the shrink and the
// restore (which would leave the PTY restored to the stale size). Don't call
// this on a PTY that another goroutine may resize concurrently.
func (p *Process) Refresh(rows, cols uint16) error {
	if cols == 0 {
		return nil
	}
	nudged, ok := nudgeRows(rows)
	if !ok {
		return nil
	}
	if err := p.pty.resize(nudged, cols); err != nil {
		return fmt.Errorf("terminal: refresh nudge: %w", err)
	}
	p.pauseNudge(refreshNudgePause)
	if err := p.pty.resize(rows, cols); err != nil {
		return fmt.Errorf("terminal: refresh restore: %w", err)
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
			if err := procutil.SignalGroup(pid, initialSig); err != nil && !errors.Is(err, os.ErrProcessDone) {
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

		// Closing the PTY master ends the output pump, which may still be
		// reading what the pty holds.
		if err := p.closePTY(); err != nil && firstErr == nil {
			firstErr = err
		}
		<-p.done
		p.closeErr = firstErr
	})
	return p.closeErr
}

// closePTY closes the master once, which hangs up the terminal for any
// process still holding it, as closing a terminal window does. Both the
// output pump's end and shutdown call it. A pty master's close fails only
// for a descriptor already closed, which the once rules out.
func (p *Process) closePTY() error {
	p.ptyCloseOnce.Do(func() {
		if err := p.pty.Close(); err != nil && !errors.Is(err, io.EOF) {
			p.ptyCloseErr = fmt.Errorf("terminal: close pty: %w", err)
		}
	})
	return p.ptyCloseErr
}

// pumpOutput reads from the PTY in a loop and pushes chunks into the output
// channel until the shell has exited and the pty holds nothing more, the
// pty's other end closes, or a read fails. Then it closes the channel and
// the pty. Once the shell has exited it reads at most exitDrainBytes.
func (p *Process) pumpOutput() {
	defer func() {
		close(p.output)
		// Shutdown returns this close's error; after a natural exit no
		// caller waits (closePTY says why it cannot fail).
		_ = p.closePTY()
	}()
	buf := make([]byte, 4096)
	afterExit := 0
	for {
		exited := p.exited.Load()
		n, err := p.readOutput(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			p.output <- chunk
			if exited {
				afterExit += n
				if afterExit >= exitDrainBytes {
					return
				}
			}
		}
		if err != nil {
			// EOF, EIO (every holder of the pty's other end closed it),
			// a drained pty after the shell's exit, or a closed master.
			return
		}
	}
}

// readOutput reads one chunk from the master. Before the shell exits it
// waits for output; after, it returns errOutputDrained when the pty holds
// nothing more. A master the poller does not take (darwin) blocks in the
// read instead, and the kernel ends that read when the shell exits.
func (p *Process) readOutput(buf []byte) (int, error) {
	for {
		var n int
		var readErr error
		err := p.raw.Read(func(fd uintptr) bool {
			for {
				n, readErr = syscall.Read(int(fd), buf)
				if readErr != syscall.EINTR {
					break
				}
			}
			if readErr == syscall.EAGAIN {
				if p.exited.Load() {
					n, readErr = 0, errOutputDrained
					return true
				}
				return false // wait until readable, or woken by the exit
			}
			return true
		})
		switch {
		case err == nil && readErr != nil:
			return 0, readErr
		case err == nil && n == 0:
			return 0, io.EOF
		case err == nil:
			return n, nil
		case errors.Is(err, os.ErrDeadlineExceeded) && p.exited.Load():
			// The shell's exit woke this read (awaitExit). Clear the
			// deadline and read what the pty still holds.
			if err := p.pty.SetReadDeadline(time.Time{}); err != nil {
				return 0, err
			}
		default:
			return 0, err
		}
	}
}

// awaitExit reaps the shell and records its exit status. The shell's exit
// ends the terminal even while a background job holds the pty open: the
// output pump delivers what the shell wrote and stops, and its close of the
// master hangs up whatever still holds the pty. The kernel has already sent
// SIGHUP to the pty's foreground job as the shell, its session leader,
// exited.
func (p *Process) awaitExit() {
	defer close(p.done)
	defer func() {
		p.exited.Store(true)
		// Wake a read waiting on an empty pty. The deadline fails only on a
		// master the poller does not take, whose read the kernel ends as the
		// shell exits, or on one shutdown already closed, whose read has
		// already ended.
		_ = p.pty.SetReadDeadline(time.Now())
	}()
	err := p.cmd.Wait()
	status := ExitStatus{}
	if err == nil {
		status.Code = 0
		status.Reason = "exit"
	} else {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
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
