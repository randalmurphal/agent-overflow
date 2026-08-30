package harnessclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/instanceinfo"
)

// DefaultLaunchTimeout is how long a boot gets to print its bootstrap
// line. Thirty seconds because a first boot on a fresh data root runs
// the whole migration chain — the same budget e2e/src/harness.ts uses.
const DefaultLaunchTimeout = 30 * time.Second

// stderrTailLines is how much of the child's stderr a launch failure
// quotes. Enough to carry a Go panic with its first frames; short enough
// that the actual error is still visible above it.
const stderrTailLines = 200
const postStartCleanupTimeout = 5 * time.Second

var captureProcessIdentity = instanceinfo.CaptureProcessIdentity

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
	// Soak boots the soak backend mode consumed by a launcher or a windowed
	// shell instead of the harness backend mode. Both isolate identically.
	Soak bool
	// Autopilot arms the soak preset on a Soak boot (seeded threads plus a
	// turn that streams forever). It is what makes the instance a SOAK
	// rather than a harness behind the launcher bootstrap, and what the
	// instance stamps as its mode.
	Autopilot bool
	// Window opens the real webview window instead of running headless.
	Window bool
	// DevAssetsURL points the boot's asset handler at a Vite dev server
	// (FRONTEND_DEVSERVER_URL) so a harness serves the working tree's
	// frontend instead of the embedded build.
	DevAssetsURL string
	// KeepHome sets AO_HARNESS_KEEP_HOME, which leaves $HOME real for child
	// processes. The backend's provider state remains pinned under the data
	// root. Child processes can still read the developer's real home.
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
	// MemoryLimitBytes installs an OS-owned aggregate memory boundary around
	// the backend and every descendant. Zero leaves containment disabled for
	// library callers and unit tests. Harness entry points set it explicitly.
	MemoryLimitBytes uint64
}

// Launched is a started backend. For an attached launch it owns the
// process; for a detached one it carries the pid and nothing else.
type Launched struct {
	Bootstrap Bootstrap
	// PID is the backend process. Always set.
	PID int

	cmd      *exec.Cmd
	identity instanceinfo.ProcessIdentity
	// waitDone is started immediately after cmd.Start. It remains available
	// on every post-start failure, including identity capture failure, so a
	// caller never has to call cmd.Wait itself or wait without a bound.
	waitDone chan struct{}
	// containment is retained by the handle until the process and its
	// descendants are proven gone.
	containment       containment.Group
	containmentMu     sync.Mutex
	containmentClosed bool
	containmentErr    error
	unverified        bool
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
	opts.DataRoot = instanceinfo.NormalizeSystemPath(opts.DataRoot)
	if opts.StdoutPath != "" {
		opts.StdoutPath = instanceinfo.NormalizeSystemPath(opts.StdoutPath)
	}
	if opts.StderrPath != "" {
		opts.StderrPath = instanceinfo.NormalizeSystemPath(opts.StderrPath)
	}

	var group containment.Group
	if opts.MemoryLimitBytes > 0 {
		var err error
		var enforcement string
		group, enforcement, err = containment.PrepareWithFallback(opts.MemoryLimitBytes)
		if err != nil {
			return nil, fmt.Errorf("install harness memory containment: %w", err)
		}
		if enforcement != "cgroup-v2" && enforcement != "kernel" {
			if writeErr := writeContainmentEvidence(opts.DataRoot, enforcement); writeErr != nil {
				return nil, errors.Join(fmt.Errorf("harness memory containment fallback: %s", enforcement), writeErr)
			}
			_, _ = fmt.Fprintf(os.Stderr, "harnessclient: using %s\n", enforcement)
		}
		defer func() {
			if group != nil {
				_ = group.Close()
			}
		}()
	}

	mode := "--harness"
	if opts.Soak {
		mode = "--soak"
	}
	args := []string{mode, "--data-dir", opts.DataRoot}
	if opts.Autopilot {
		args = append(args, "--autopilot")
	}
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
		if err := validateCapturePath(opts.StdoutPath); err != nil {
			return nil, fmt.Errorf("stdout capture path: %w", err)
		}
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
		if err := validateCapturePath(opts.StderrPath); err != nil {
			return nil, fmt.Errorf("stderr capture path: %w", err)
		}
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
	// Configure containment only after stdio is wired. The Windows
	// detached-process decision depends on whether streams point at capture
	// files. Every harness child owns a process group/job, including attached
	// launches, so descendants cannot outlive a teardown.
	applyDetachAttrs(cmd)
	if group != nil {
		if err := group.Configure(cmd); err != nil {
			return nil, fmt.Errorf("configure harness memory containment: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", opts.Binary, err)
	}
	launched.PID = cmd.Process.Pid
	ownedGroup := group
	launched.containment = ownedGroup
	group = nil
	if ownedGroup != nil {
		if err := ownedGroup.Adopt(cmd); err != nil {
			closeLaunchPipes(stdoutPipe, stderrPipe)
			launched.unverified = true
			launched.startWaiter()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), postStartCleanupTimeout)
			cleanupErr := launched.cleanupUnverified(cleanupCtx)
			cancel()
			return launched, fmt.Errorf("adopt backend into memory containment: %w", errors.Join(err, cleanupErr))
		}
	}
	// A fast-failing backend can become a zombie between Start and the
	// identity read. Keep the pipes open long enough to collect its bootstrap
	// or startup error, then perform the unverified cleanup path. Returning
	// the raw /proc race here used to hide the backend's actual failure.
	identity, identityErr := captureProcessIdentity(launched.PID)
	if identityErr != nil {
		launched.unverified = true
	} else {
		launched.identity = identity
	}

	if stderrPipe != nil {
		launched.stderrDone = make(chan struct{})
		go launched.drainStderr(stderrPipe)
	}
	exited := launched.startWaiter()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultLaunchTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bs Bootstrap
	var bootstrapErr error
	if stdoutPipe != nil {
		bs, bootstrapErr = launched.awaitFromPipe(waitCtx, stdoutPipe, exited)
	} else {
		bs, bootstrapErr = launched.awaitFromFile(waitCtx, opts.StdoutPath, exited)
	}
	if bootstrapErr != nil {
		if killErr := launched.Kill(context.Background()); killErr != nil {
			bootstrapErr = errors.Join(bootstrapErr, fmt.Errorf("terminate backend after launch failure: %w", killErr))
		}
		launched.awaitStderrDrain()
		// Keep the handle when cleanup could not prove the process gone. The
		// caller owns the root and must be able to retry cleanup before it
		// quarantines or reuses that root.
		return launched, fmt.Errorf("%w\nbinary: %s\nstderr:\n%s", bootstrapErr, opts.Binary, launched.StderrTail())
	}
	if bs.PID != launched.PID {
		identityErr := fmt.Errorf("harness bootstrap pid %d does not match spawned pid %d", bs.PID, launched.PID)
		if killErr := launched.Kill(context.Background()); killErr != nil {
			identityErr = errors.Join(identityErr, fmt.Errorf("terminate backend after bootstrap identity mismatch: %w", killErr))
		}
		return launched, identityErr
	}
	if bs.StartupError != "" {
		startupErr := fmt.Errorf("backend started but App.Start failed: %s", bs.StartupError)
		if terminateErr := launched.Terminate(context.Background()); terminateErr != nil {
			startupErr = errors.Join(startupErr, fmt.Errorf("terminate backend after startup failure: %w", terminateErr))
		}
		return launched, startupErr
	}
	if identityErr != nil {
		if killErr := launched.Kill(context.Background()); killErr != nil {
			identityErr = errors.Join(identityErr, fmt.Errorf("terminate backend after identity capture failure: %w", killErr))
		}
		return launched, fmt.Errorf("capture backend process identity: %w", identityErr)
	}
	launched.Bootstrap = bs

	// Drain the wait result in the background. An attached caller reaps
	// its child this way; a detached one is about to exit and must never
	// block on a backend that runs for hours.
	go func() {
		<-exited
		if err := launched.closeContainment(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "harnessclient: close memory containment: %v\n", err)
		}
	}()
	return launched, nil
}

func (l *Launched) startWaiter() chan struct{} {
	if l.waitDone != nil {
		return l.waitDone
	}
	l.waitDone = make(chan struct{})
	go func() {
		_ = l.cmd.Wait()
		close(l.waitDone)
	}()
	return l.waitDone
}

func writeContainmentEvidence(dataRoot, enforcement string) error {
	path := filepath.Join(dataRoot, "agent-overflow", "logs", "harness-containment.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create containment evidence directory: %w", err)
	}
	document := fmt.Sprintf("{\"version\":1,\"enforcement\":%q}\n", enforcement)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		return fmt.Errorf("write containment evidence %s: %w", path, err)
	}
	return nil
}

func validateCapturePath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	abs = instanceinfo.NormalizeSystemPath(abs)
	for current := filepath.Clean(abs); ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlinked capture component %s", current)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect capture component %s: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

// awaitFromPipe reads the child's stdout pipe until the bootstrap line.
func (l *Launched) awaitFromPipe(ctx context.Context, pipe io.Reader, exited <-chan struct{}) (Bootstrap, error) {
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
	case <-exited:
		// Give the scanner the last of the buffered pipe before deciding.
		select {
		case res := <-done:
			if res.err == nil {
				return res.bs, nil
			}
			// cmd.Wait closes its pipe as part of reaping the child. The
			// scanner can therefore report the platform-specific
			// "file already closed" error instead of io.EOF. Normalize both
			// forms to the launch contract rather than leaking an
			// implementation detail that makes a failed boot look unrelated.
			return Bootstrap{}, fmt.Errorf("harness closed stdout without printing its bootstrap line: %w", res.err)
		case <-time.After(time.Second):
			return Bootstrap{}, errors.New("harness closed stdout without printing its bootstrap line")
		}
	case res := <-done:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				return Bootstrap{}, errors.New("harness closed stdout without printing its bootstrap line")
			}
			return Bootstrap{}, fmt.Errorf("harness closed stdout without printing its bootstrap line: %w", res.err)
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
func (l *Launched) awaitFromFile(ctx context.Context, path string, exited <-chan struct{}) (Bootstrap, error) {
	const pollInterval = 50 * time.Millisecond
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
			return Bootstrap{}, errors.New("harness exited before printing its bootstrap line")
		}
		select {
		case <-ctx.Done():
			return Bootstrap{}, fmt.Errorf("harness did not print its bootstrap line within the deadline: %w", ctx.Err())
		case <-exited:
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
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	if l.unverified {
		return l.cleanupUnverified(ctx)
	}
	if err := terminateOwnedProcess(ctx, l.cmd.Process.Pid, l.identity, 5*time.Second); err != nil {
		return err
	}
	return waitForCommandExit(ctx, l)
}

// Kill forcefully terminates the authenticated process group and waits for
// the owner to disappear. It is intentionally separate from Terminate so a
// caller whose grace period expired cannot accidentally repeat a polite stop.
func (l *Launched) Kill(ctx context.Context) error {
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	if l.unverified {
		return l.cleanupUnverified(ctx)
	}
	proof, err := killOwnedProcess(l.cmd.Process.Pid, l.identity)
	if err != nil {
		return err
	}
	return waitForCommandAndGroup(ctx, l, proof)
}

func (l *Launched) cleanupUnverified(ctx context.Context) error {
	if l == nil || l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, postStartCleanupTimeout)
		defer cancel()
	}
	var errs []error
	proof, proofErr := captureUnverifiedProof(l.PID)
	if proofErr != nil {
		errs = append(errs, fmt.Errorf("capture unverified backend process tree: %w", proofErr))
		proof = emptyOwnedGroupProof(l.PID)
	}
	if err := killUnverifiedProcessTree(l.cmd); err != nil && processAlive(l.PID) {
		errs = append(errs, fmt.Errorf("kill unverified backend process group: %w", err))
	}
	if err := waitForCommandAndGroup(ctx, l, proof); err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	if err := l.closeContainment(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func closeLaunchPipes(pipes ...io.ReadCloser) {
	for _, pipe := range pipes {
		if pipe != nil {
			_ = pipe.Close()
		}
	}
}

func waitForCommandAndGroup(ctx context.Context, l *Launched, proof ownedGroupProof) error {
	if err := waitForCommandExit(ctx, l); err != nil {
		return err
	}
	if err := waitForOwnedExit(ctx, l.PID, proof); err != nil {
		return err
	}
	return nil
}

func waitForCommandExit(ctx context.Context, l *Launched) error {
	if l.waitDone != nil {
		select {
		case <-l.waitDone:
		case <-ctx.Done():
			return fmt.Errorf("wait for backend process %d to exit: %w", l.PID, ctx.Err())
		}
	}
	return nil
}

func (l *Launched) closeContainment() error {
	if l == nil {
		return nil
	}
	l.containmentMu.Lock()
	defer l.containmentMu.Unlock()
	if l.containmentClosed {
		return l.containmentErr
	}
	l.containmentClosed = true
	if l.containment != nil {
		l.containmentErr = l.containment.Close()
	}
	return l.containmentErr
}
