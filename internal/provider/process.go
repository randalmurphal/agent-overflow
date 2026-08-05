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

	"agent-overflow/internal/appimage"
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
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	stdout           *bufio.Reader
	stdoutFile       *os.File
	done             chan struct{}
	err              error
	mu               sync.Mutex
	eventLogger      *logging.Logger
	eventLogRedactor EventLogRedactor
	threadID         string
	provider         string
	stderrTail       *stderrTee
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
	Env              map[string]string
	UnsetEnv         []string
	EventLogger      *logging.Logger
	EventLogRedactor EventLogRedactor
	ThreadID         string
	Provider         string
}

// EventLogRedactor rewrites provider stdin/stdout lines before they are
// written to the optional raw provider-event log. Returning nil skips that
// log entry.
type EventLogRedactor func(direction string, data []byte) []byte

// Spawn starts a subprocess with stdin/stdout pipes and process group isolation.
// The context is associated with the command — canceling it will kill the process.
// Prefer Close() for graceful shutdown.
func Spawn(ctx context.Context, cfg SpawnConfig) (*Process, error) {
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Dir = cfg.Dir
	// applySysProcAttr is platform-split: POSIX wires up Setpgid so we
	// can signal the whole tree later; the Windows stub leaves the
	// default attrs in place because the launcher never spawns provider
	// children on Windows (the WSL-side Linux backend does).
	applySysProcAttr(cmd)

	cmd.Env = BuildEnvironment(cfg.Env, cfg.UnsetEnv...)

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

	if err := cmd.Start(); err != nil {
		stdoutRead.Close()
		stdoutWrite.Close()
		stderrRead.Close()
		stderrWrite.Close()
		return nil, fmt.Errorf("provider: start %s: %w", cfg.Binary, err)
	}
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
		done:             make(chan struct{}),
		eventLogger:      cfg.EventLogger,
		eventLogRedactor: cfg.EventLogRedactor,
		threadID:         cfg.ThreadID,
		provider:         cfg.Provider,
	}

	// Wait goroutine: detect process exit.
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

// BuildEnvironment returns the current process environment, scrubbed of the
// AppImage launch artifacts, with the requested variables removed and explicit
// overrides applied. PATH overrides are additive so provider-specific binary
// directories do not hide the user's normal command search path — and the
// inherited half of that merge is read back off the scrubbed base, so the
// mount's bin directory cannot re-enter through the override path.
//
// This is the ONE env rule every provider process gets, and it is exported for
// that reason: providers whose Config carries an override map reach it through
// Spawn, and the ones whose Config carries a full []string environment
// (claudetui, which launches a real TUI) call it directly. Two different env
// rules across providers is exactly how an injected variable goes missing on
// one of them — which is why an override also *replaces* the inherited value
// rather than being appended after it. Appending happens to work under
// exec.Cmd's last-wins rule and does not under every consumer of a []string
// environment, so the duplicate never gets to exist.
func BuildEnvironment(overrides map[string]string, unset ...string) []string {
	base := appimage.Scrub(os.Environ())
	removed := make([]string, 0, len(unset)+len(overrides))
	removed = append(removed, unset...)
	for key := range overrides {
		removed = append(removed, key)
	}
	env := filterEnvironment(base, removed...)
	for key, value := range overrides {
		if strings.EqualFold(key, "PATH") {
			if existing := envValue(base, "PATH"); existing != "" {
				value += string(os.PathListSeparator) + existing
			}
		}
		env = append(env, key+"="+value)
	}
	return env
}

// envValue returns the value of key in env, last entry wins. Lookup is
// case-insensitive to match the removal rule below: a Windows-style
// environment that spells the variable `Path` must not have it removed as an
// override collision and then re-read as empty here.
func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		name, candidate, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			value = candidate
		}
	}
	return value
}

// FilterEnvironment removes named variables from env and scrubs the AppImage
// launch artifacts out of what remains. An empty env means inherit the current
// process environment, matching exec.Cmd's default and the previous fetcher
// behavior. Matching is case-insensitive so this helper is also safe for
// Windows-style environments.
//
// The scrub lives here rather than at each call site because every caller is
// assembling a child environment: a provider CLI must resolve its binaries and
// libraries against the user's real system, not against a squashfs mount that
// disappears when Agent Overflow exits. It is marker-gated and idempotent, so
// a non-AppImage launch is untouched and an already-scrubbed env passed back
// in stays as it is.
func FilterEnvironment(env []string, unset ...string) []string {
	if len(env) == 0 {
		env = os.Environ()
	}
	return filterEnvironment(appimage.Scrub(env), unset...)
}

// filterEnvironment is FilterEnvironment's removal half, over an environment
// the caller has already scrubbed. Splitting the two is what lets
// BuildEnvironment scrub exactly once, before it reads PATH back out for the
// additive merge.
func filterEnvironment(env []string, unset ...string) []string {
	if len(unset) == 0 {
		return append([]string(nil), env...)
	}
	removed := make(map[string]struct{}, len(unset))
	for _, key := range unset {
		removed[strings.ToUpper(strings.TrimSpace(key))] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if _, shouldRemove := removed[strings.ToUpper(key)]; found && shouldRemove {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
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
	// ReadSlice moves whole buffered chunks per call instead of the former
	// per-byte ReadByte loop — the ingest path is serialized per session,
	// so a 32 MB aggregated-output line used to stall every subsequent
	// event behind ~32M per-byte iterations. Chunks returned by ReadSlice
	// alias bufio's internal buffer and MUST be copied into buf: callers
	// retain the returned line past the next read.
	buf := make([]byte, 0, 4096)
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
//
// Returns nil when the process is gone, regardless of its exit code.
// A subprocess exiting non-zero is the goal of Close, not a failure of
// it — propagating `cmd.Wait`'s ExitError up the call chain caused
// "Revert failed: Exit status 1" toasts when callers (e.g. revert,
// thread delete) intentionally tore down a Claude session that had
// just been interrupted. The exit status stays available via Err()
// for callers that need it; the unexpected-exit banner is emitted by
// the read loop only when closing.Load() is false, so genuine crashes
// still surface.
func (p *Process) Close() error {
	// Close stdin to signal the process to exit.
	p.stdin.Close()

	select {
	case <-p.done:
		return closeResult(p.err)
	case <-time.After(shutdownGrace):
	}

	// SIGTERM the process group.
	p.signalGroup(syscall.SIGTERM)

	select {
	case <-p.done:
		return closeResult(p.err)
	case <-time.After(killGrace):
	}

	// SIGKILL the process group.
	return closeResult(p.Kill())
}

// Kill immediately kills the process group.
func (p *Process) Kill() error {
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return p.err
}

// closeResult treats a subprocess exit (any code or signal) as a
// successful close — the process is gone, which is what Close was
// trying to make happen. We log abnormal exits so a misbehaving
// provider CLI still leaves a trail in the dev log, but the caller
// doesn't see it as a teardown failure. Any non-exit error (e.g. a
// future genuine close-operation failure) propagates unchanged.
func closeResult(err error) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		log.Printf("provider: subprocess exited %v during intentional close", exitErr)
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
func (p *Process) StderrTail() string {
	return p.stderrTail.Tail()
}

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
		Data:      string(data),
	}); err != nil {
		log.Printf("provider: raw event log failed: %v", err)
	}
}
