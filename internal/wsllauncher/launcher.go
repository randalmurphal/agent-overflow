package wsllauncher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultBootstrapPrefix is the line prefix the WSL-side backend writes
// to stdout to hand its listen address back to the launcher.
//
// Why a stdout sentinel rather than fd 3: real fd-3 plumbing through
// wsl.exe is awkward (Windows pipes don't propagate cleanly through
// the WSL boundary). A magic-prefixed stdout line is portable, easy to
// match, and tolerant to other startup chatter on stdout.
const DefaultBootstrapPrefix = "__AO_BOOTSTRAP__:"

// ErrNotSupported is the sentinel returned by every Windows-only entry
// point on macOS and Linux hosts. The wsllauncher package compiles on
// non-Windows so the Wails build for the desktop binary can import
// shared types (Distro, Bootstrap), but anything that actually spawns
// wsl.exe is a no-go off Windows.
//
// On Windows hosts ErrNotSupported is unused (the real Launch /
// InstallPayload implementations succeed or surface their own concrete
// errors); declaring the sentinel cross-platform keeps callers' error
// chain checks compilable on both hosts.
//
// Callers should pair errors.Is(err, ErrNotSupported) checks with their
// platform-specific fallbacks rather than substring-matching the
// message.
var ErrNotSupported = errors.New("wsllauncher: only available on Windows")

// Bootstrap is what the WSL backend writes back to the launcher.
// Mirrors the JSON the Linux main.go emits when --print-url-fd is set.
type Bootstrap struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// LaunchOptions configures Launch.
type LaunchOptions struct {
	// Distro is the WSL distribution name (`-d <distro>` argument to
	// wsl.exe). Required.
	Distro string

	// BinaryPath is the absolute path inside the distro to the Linux
	// agent-overflow binary (e.g. /home/user/.local/bin/agent-overflow).
	// Required.
	BinaryPath string

	// ExtraArgs are appended after the bootstrap-mode flags. Mostly
	// useful for tests injecting a known-good fixture script.
	ExtraArgs []string

	// StdoutPrefix overrides DefaultBootstrapPrefix. Empty falls back
	// to DefaultBootstrapPrefix.
	StdoutPrefix string

	// BootstrapTimeout overrides the default bootstrap-line wait. Zero
	// falls back to DefaultBootstrapTimeout (8s) — generous for cold-
	// boot SQLite + WSL2 9P startup, short enough that a hung child
	// surfaces an error to the picker UI.
	BootstrapTimeout time.Duration

	// CommandRunner overrides exec.CommandContext for testing. Empty
	// uses the real wsl.exe via the package's resolveCommand helper.
	CommandRunner CommandRunner
}

// DefaultBootstrapTimeout is the default value for
// LaunchOptions.BootstrapTimeout.
const DefaultBootstrapTimeout = 8 * time.Second

// CommandRunner is the seam tests use to substitute a stub script for
// wsl.exe. Production callers leave it nil and the launcher uses
// exec.CommandContext("wsl.exe", ...).
type CommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// Launcher controls the lifetime of a wsl.exe child process. The
// concrete cleanup behaviour differs by host: on Windows the child is
// pinned to a Job Object so that closing the parent's handle terminates
// every descendant; on non-Windows hosts (where Launch errors out)
// Launcher exposes a no-op Stop so test code can construct the type.
type Launcher struct {
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	platform  platformLauncher
	bootstrap Bootstrap

	stopOnce sync.Once
	stopErr  error
}

// platformLauncher is the host-specific cleanup hook. Windows installs
// a Job Object handle here; other hosts get an empty struct.
type platformLauncher interface {
	// adopt assigns the running child to the platform's containment
	// primitive. Called once after cmd.Start succeeds.
	adopt(cmd *exec.Cmd) error

	// close tears down the containment primitive, killing the child as
	// a side effect on Windows.
	close() error
}

// Bootstrap returns the {port, token} the child handed back at launch.
func (l *Launcher) Bootstrap() Bootstrap { return l.bootstrap }

// Wait blocks until the child process exits and returns its run error.
// Wait is a thin wrapper around (*exec.Cmd).Wait — it does NOT close
// the platform primitive; callers should defer Stop separately.
func (l *Launcher) Wait() error {
	if l.cmd == nil {
		return errors.New("wsllauncher: Launcher has no underlying command")
	}
	return l.cmd.Wait()
}

// Stop terminates the child. On Windows this closes the Job Object
// handle, which the kernel translates into a kill signal for every
// process assigned to it. Stop is idempotent — a second call returns
// the original error (or nil).
func (l *Launcher) Stop() error {
	l.stopOnce.Do(func() {
		var errs []error
		if l.platform != nil {
			if err := l.platform.close(); err != nil {
				errs = append(errs, fmt.Errorf("close platform handle: %w", err))
			}
		}
		// On non-Windows hosts (where the Job Object doesn't exist), or
		// as a belt-and-braces follow-up on Windows, send a kill signal
		// directly. os.ErrProcessDone is the documented sentinel
		// returned by Process.Kill when the child has already exited;
		// errors.Is unwraps the platform-specific error chain so we
		// don't have to substring-match the message.
		if l.cmd != nil && l.cmd.Process != nil {
			if err := l.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("kill child: %w", err))
			}
		}
		if len(errs) > 0 {
			l.stopErr = errors.Join(errs...)
		}
	})
	return l.stopErr
}

// resolveCommand wraps exec.CommandContext so tests can inject a stub.
// Production callers pass nil for opts.CommandRunner, which selects
// the real wsl.exe.
func resolveCommand(opts LaunchOptions) CommandRunner {
	if opts.CommandRunner != nil {
		return opts.CommandRunner
	}
	return exec.CommandContext
}

// readBootstrapLine drains the child's stdout until either the
// bootstrap-prefixed line shows up (success) or ctx fires (timeout /
// child died). The non-bootstrap lines are forwarded to the dropFn
// callback so the caller can log them — useful for diagnosing a child
// that died before printing the bootstrap.
//
// We read by line rather than buffering the whole stream because the
// child keeps running after handing off the bootstrap, so we must not
// block on stdout EOF.
//
// Goroutine ownership contract: when ctx fires while scanner.Scan() is
// blocked, this function returns immediately but the scanner goroutine
// is still parked inside the underlying Read. The CALLER is responsible
// for closing the underlying reader (e.g. cmd.StdoutPipe()) on the
// timeout path so the parked Read returns and the goroutine exits.
// Without that close, the goroutine leaks for the lifetime of the
// process — which on a stuck-child boot can be the entire launcher
// session.
func readBootstrapLine(ctx context.Context, r io.Reader, prefix string, dropFn func(string)) (Bootstrap, error) {
	type result struct {
		bs  Bootstrap
		err error
	}
	resCh := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(r)
		// Default 64 KiB line buffer is enough for the bootstrap line
		// (port + token < 200 bytes) but cap it explicitly so a child
		// that floods stdout without a newline doesn't OOM the launcher.
		scanner.Buffer(make([]byte, 4096), 64*1024)
		for {
			// Cheap pre-check before each Scan iteration so a steady
			// stream of non-bootstrap lines (think log spew) yields to
			// ctx cancellation between lines. Scan itself is blocking;
			// caller-side Close is what unparks an in-flight Read.
			select {
			case <-ctx.Done():
				resCh <- result{err: fmt.Errorf("read child stdout: %w", ctx.Err())}
				return
			default:
			}

			if !scanner.Scan() {
				break
			}
			line := scanner.Text()
			if !strings.HasPrefix(line, prefix) {
				if dropFn != nil {
					dropFn(line)
				}
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			var bs Bootstrap
			if err := json.Unmarshal([]byte(payload), &bs); err != nil {
				resCh <- result{err: fmt.Errorf("decode bootstrap %q: %w", payload, err)}
				return
			}
			if bs.Port <= 0 || bs.Token == "" {
				resCh <- result{err: fmt.Errorf("invalid bootstrap: %+v", bs)}
				return
			}
			resCh <- result{bs: bs}
			return
		}
		if err := scanner.Err(); err != nil {
			resCh <- result{err: fmt.Errorf("read child stdout: %w", err)}
			return
		}
		resCh <- result{err: errors.New("child closed stdout before printing bootstrap")}
	}()

	select {
	case <-ctx.Done():
		return Bootstrap{}, fmt.Errorf("waiting for bootstrap: %w", ctx.Err())
	case r := <-resCh:
		return r.bs, r.err
	}
}

// ReadBootstrapForTest is the package-external entry point into the
// bootstrap-line parser. It exists so tests in package main (which
// owns writeBootstrap) can drive the round-trip without re-importing
// the launcher's private helper. Production callers go through Launch.
//
// The contract matches readBootstrapLine: caller must close the
// reader to unblock the scanner if ctx fires.
func ReadBootstrapForTest(ctx context.Context, r io.Reader, prefix string) (Bootstrap, error) {
	return readBootstrapLine(ctx, r, prefix, nil)
}

// drainStderr forwards every stderr line through dropFn until EOF.
// Runs in its own goroutine for the lifetime of the child.
func drainStderr(r io.Reader, dropFn func(string)) {
	if r == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		if dropFn != nil {
			dropFn(scanner.Text())
		}
	}
}
