package harnessclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultLaunchTimeout is how long a boot gets to print its bootstrap
// line. Thirty seconds because a first boot on a fresh data root runs
// the whole migration chain — the same budget e2e/src/harness.ts uses.
const DefaultLaunchTimeout = 30 * time.Second

// stderrTailLines is how much of the child's stderr a launch failure
// quotes. Enough to carry a Go panic with its first frames; short enough
// that the actual error is still visible above it.
const stderrTailLines = 200

// LaunchOptions describes a backend to start. Binary and DataRoot are
// required; everything else has a working default.
type LaunchOptions struct {
	// Binary is the agent-overflow executable to run.
	Binary string
	// DataRoot is the --data-dir value: the isolated root whose
	// <DataRoot>/agent-overflow subdirectory holds the DB and settings.
	DataRoot string
	// MockProvider overrides where the boot finds ao-mockprovider
	// (default: alongside Binary, which is what the boot itself resolves).
	MockProvider string
	// Soak boots the soak shell instead of the harness shell. Both
	// isolate identically; only the autopilot and the bootstrap contract
	// differ, and a soak still prints the harness line when --window is
	// set... which it is not here, so Soak implies the caller reads the
	// registry rather than stdout.
	Soak bool
	// Window opens the real webview window instead of running headless.
	Window bool
	// DevAssetsURL points the boot's asset handler at a Vite dev server
	// (FRONTEND_DEVSERVER_URL) so a harness serves the working tree's
	// frontend instead of the embedded build.
	DevAssetsURL string
	// KeepHome sets AO_HARNESS_KEEP_HOME, which leaves $HOME real so the
	// instance can READ the developer's provider session files. It never
	// widens what the instance can write: credentials stay pinned under
	// the data root.
	KeepHome bool
	// ExtraArgs are appended verbatim after the mode flags.
	ExtraArgs []string
	// Env entries are appended to the parent environment.
	Env []string
	// Timeout bounds the wait for the bootstrap line. Zero means
	// DefaultLaunchTimeout.
	Timeout time.Duration
	// Detach puts the child in its own session and stops tracking it once
	// the bootstrap line arrives, so the instance outlives this process.
	// It requires StdoutPath and StderrPath: a detached child cannot keep
	// writing into pipes nobody will read.
	Detach bool
	// StdoutPath / StderrPath redirect the child's streams to files. The
	// bootstrap line is then read by tailing StdoutPath.
	StdoutPath string
	StderrPath string
}

// Launched is a started backend. For an attached launch it owns the
// process; for a detached one it carries the pid and nothing else.
type Launched struct {
	Bootstrap Bootstrap
	// PID is the backend process. Always set.
	PID int

	cmd *exec.Cmd
	// stderrDone closes when the stderr drain has read the pipe to EOF.
	// A boot that fails fast can finish before the drain goroutine has
	// been scheduled at all, and an error message that raced it would
	// quote an empty tail — which is the one thing the reader needs.
	stderrDone chan struct{}

	mu         sync.Mutex
	stderrTail []string
	stderrPath string
}

// Launch starts a backend and returns once it has printed its bootstrap
// line. Every failure path quotes the child's stderr tail, because the
// interesting half of a failed boot is always there and never here.
func Launch(ctx context.Context, opts LaunchOptions) (*Launched, error) {
	if strings.TrimSpace(opts.Binary) == "" {
		return nil, errors.New("harnessclient: no backend binary given")
	}
	if strings.TrimSpace(opts.DataRoot) == "" {
		return nil, errors.New("harnessclient: no data root given")
	}
	if opts.Detach && (opts.StdoutPath == "" || opts.StderrPath == "") {
		return nil, errors.New("harnessclient: a detached launch needs stdout and stderr files")
	}

	mode := "--harness"
	if opts.Soak {
		mode = "--soak"
	}
	args := []string{mode, "--data-dir", opts.DataRoot}
	if opts.Window {
		args = append(args, "--window")
	}
	if opts.MockProvider != "" {
		args = append(args, "--mock-provider", opts.MockProvider)
	}
	args = append(args, opts.ExtraArgs...)

	cmd := exec.Command(opts.Binary, args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	if opts.DevAssetsURL != "" {
		cmd.Env = append(cmd.Env, "FRONTEND_DEVSERVER_URL="+opts.DevAssetsURL)
	}
	if opts.KeepHome {
		cmd.Env = append(cmd.Env, "AO_HARNESS_KEEP_HOME=1")
	}
	if opts.Detach {
		applyDetachAttrs(cmd)
	}

	launched := &Launched{cmd: cmd, stderrPath: opts.StderrPath}

	var stdoutPipe io.ReadCloser
	var stderrPipe io.ReadCloser
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	if opts.StdoutPath != "" {
		out, err := os.OpenFile(opts.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", opts.StdoutPath, err)
		}
		closers = append(closers, out)
		cmd.Stdout = out
	} else {
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("stdout pipe: %w", err)
		}
		stdoutPipe = pipe
	}
	if opts.StderrPath != "" {
		// Append: a restart on the same data dir must not erase the
		// evidence of the boot that came before it.
		errFile, err := os.OpenFile(opts.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", opts.StderrPath, err)
		}
		closers = append(closers, errFile)
		cmd.Stderr = errFile
	} else {
		pipe, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("stderr pipe: %w", err)
		}
		stderrPipe = pipe
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", opts.Binary, err)
	}
	launched.PID = cmd.Process.Pid

	if stderrPipe != nil {
		launched.stderrDone = make(chan struct{})
		go launched.drainStderr(stderrPipe)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultLaunchTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bs Bootstrap
	var err error
	if stdoutPipe != nil {
		bs, err = launched.awaitFromPipe(waitCtx, stdoutPipe, exited)
	} else {
		bs, err = launched.awaitFromFile(waitCtx, opts.StdoutPath, exited)
	}
	if err != nil {
		_ = launched.kill()
		launched.awaitStderrDrain()
		return nil, fmt.Errorf("%w\nbinary: %s\nstderr:\n%s", err, opts.Binary, launched.StderrTail())
	}
	if bs.StartupError != "" {
		_ = launched.Terminate(context.Background())
		return nil, fmt.Errorf("backend started but App.Start failed: %s", bs.StartupError)
	}
	launched.Bootstrap = bs

	// Drain the wait result in the background. An attached caller reaps
	// its child this way; a detached one is about to exit and must never
	// block on a backend that runs for hours.
	go func() { <-exited }()
	return launched, nil
}

// awaitFromPipe reads the child's stdout pipe until the bootstrap line.
func (l *Launched) awaitFromPipe(ctx context.Context, pipe io.Reader, exited <-chan error) (Bootstrap, error) {
	type result struct {
		bs  Bootstrap
		err error
	}
	done := make(chan result, 1)
	go func() {
		bs, err := scanBootstrap(pipe, nil)
		done <- result{bs, err}
	}()
	select {
	case <-ctx.Done():
		return Bootstrap{}, fmt.Errorf("harness did not print its bootstrap line within the deadline: %w", ctx.Err())
	case err := <-exited:
		// Give the scanner the last of the buffered pipe before deciding.
		select {
		case res := <-done:
			if res.err == nil {
				return res.bs, nil
			}
		case <-time.After(time.Second):
		}
		return Bootstrap{}, fmt.Errorf("harness exited before printing its bootstrap line (%v)", err)
	case res := <-done:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				return Bootstrap{}, errors.New("harness closed stdout without printing its bootstrap line")
			}
			return Bootstrap{}, res.err
		}
		return res.bs, nil
	}
}

// awaitFromFile tails the stdout capture file. A detached launch cannot
// use a pipe — the CLI exits and the child would take SIGPIPE on its
// next write — so the file IS the channel, polled rather than streamed.
// Only the bootstrap line is ever written there (the boot re-points
// os.Stdout at stderr immediately after), so the file stays a few
// hundred bytes and re-reading it whole costs nothing.
func (l *Launched) awaitFromFile(ctx context.Context, path string, exited <-chan error) (Bootstrap, error) {
	const pollInterval = 50 * time.Millisecond
	var exitErr error
	childGone := false
	for {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Bootstrap{}, fmt.Errorf("read %s: %w", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			bs, ok, parseErr := ParseBootstrapLine(line)
			if parseErr != nil {
				return Bootstrap{}, parseErr
			}
			if ok {
				return bs, nil
			}
		}
		if childGone {
			return Bootstrap{}, fmt.Errorf("harness exited before printing its bootstrap line (%v)", exitErr)
		}
		select {
		case <-ctx.Done():
			return Bootstrap{}, fmt.Errorf("harness did not print its bootstrap line within the deadline: %w", ctx.Err())
		case exitErr = <-exited:
			// One more pass over the file: the line may have landed
			// between the read above and the exit.
			childGone = true
		case <-time.After(pollInterval):
		}
	}
}

func (l *Launched) drainStderr(pipe io.Reader) {
	defer close(l.stderrDone)
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		l.mu.Lock()
		l.stderrTail = append(l.stderrTail, scanner.Text())
		if len(l.stderrTail) > stderrTailLines {
			l.stderrTail = l.stderrTail[1:]
		}
		l.mu.Unlock()
	}
}

// awaitStderrDrain gives the stderr reader a bounded moment to catch up
// before an error quotes what it collected. Bounded because a child that
// leaked its stderr pipe to a grandchild would otherwise hold the drain
// open forever, and a late tail is worth less than a returned error.
func (l *Launched) awaitStderrDrain() {
	if l.stderrDone == nil {
		return
	}
	select {
	case <-l.stderrDone:
	case <-time.After(time.Second):
	}
}

// StderrTail returns the last lines of the child's stderr: the in-memory
// ring for an attached launch, the tail of the capture file for a
// detached one.
func (l *Launched) StderrTail() string {
	l.mu.Lock()
	lines := append([]string(nil), l.stderrTail...)
	path := l.stderrPath
	l.mu.Unlock()
	if len(lines) > 0 {
		return strings.Join(lines, "\n")
	}
	if path == "" {
		return "(no stderr captured)"
	}
	tail, err := TailFile(path, stderrTailLines)
	if err != nil {
		return fmt.Sprintf("(stderr at %s unreadable: %v)", path, err)
	}
	if len(tail) == 0 {
		return fmt.Sprintf("(stderr at %s is empty)", path)
	}
	return strings.Join(tail, "\n")
}

// Terminate asks the backend to stop and escalates to a kill after the
// grace period. The graceful path is what removes the instance's
// discovery files, so a kill leaves stale rows behind by design.
func (l *Launched) Terminate(ctx context.Context) error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	return TerminateProcess(ctx, l.cmd.Process.Pid, 5*time.Second)
}

func (l *Launched) kill() error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	return l.cmd.Process.Kill()
}
