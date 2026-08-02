package wsllauncher

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Most of the launcher's heavy lifting (Job Object adopt + close, real
// wsl.exe spawning) only meaningfully exists on Windows. These tests
// run on macOS via the public surface plus targeted helpers — what we
// verify here is parsing, error shape, and the cross-platform
// interface contracts.

func TestBuildLaunchArgs(t *testing.T) {
	got := buildLaunchArgs("Ubuntu-24.04", "/home/u/.local/bin/agent-overflow", nil)
	want := []string{
		"--cd", "~",
		"-d", "Ubuntu-24.04",
		"--",
		"/home/u/.local/bin/agent-overflow", "--listen", "127.0.0.1:0", "--print-url-fd", "0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buildLaunchArgs() = %v, want %v", got, want)
	}

	// --cd must precede the `--` separator; after `--`, wsl.exe treats
	// every token as part of the Linux command line, so a misplaced --cd
	// would be passed to the backend binary instead of WSL.
	cd := slices.Index(got, "--cd")
	sep := slices.Index(got, "--")
	if cd < 0 || sep < 0 || cd > sep {
		t.Fatalf("--cd (index %d) must come before -- (index %d): %v", cd, sep, got)
	}

	// ExtraArgs (test fixtures) append after the fixed flag set.
	withExtra := buildLaunchArgs("D", "/bin/x", []string{"--fixture", "arg"})
	if n := len(withExtra); n < 2 || withExtra[n-2] != "--fixture" || withExtra[n-1] != "arg" {
		t.Fatalf("extraArgs not appended last: %v", withExtra)
	}
}

func TestLaunch_ErrorsOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verified on non-Windows hosts only")
	}
	_, _, err := Launch(context.Background(), LaunchOptions{
		Distro:     "Ubuntu",
		BinaryPath: "/usr/local/bin/agent-overflow",
	})
	if err == nil {
		t.Fatal("expected Launch to error on non-Windows hosts")
	}
	if !strings.Contains(err.Error(), "only available on Windows") {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
	// Sentinel check: callers must be able to differentiate
	// "Windows-only" from arbitrary launch errors via errors.Is.
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Launch error chain doesn't unwrap to ErrNotSupported: %v", err)
	}
}

func TestInstallPayload_ErrorsOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verified on non-Windows hosts only")
	}
	err := InstallPayload(context.Background(), "Ubuntu", "C:/host", "/wsl/path")
	if err == nil {
		t.Fatal("expected InstallPayload to error on non-Windows hosts")
	}
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("InstallPayload error chain doesn't unwrap to ErrNotSupported: %v", err)
	}
}

// lineSink collects the lines a drain forwards. The scanner goroutine
// outlives readBootstrapLine (it keeps draining stdout for the child's
// lifetime), so tests that inspect forwarded lines must synchronize with
// it rather than sharing a plain slice.
type lineSink struct{ lines chan string }

func newLineSink() *lineSink { return &lineSink{lines: make(chan string, 64)} }

func (s *lineSink) drop(line string) { s.lines <- line }

func (s *lineSink) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-s.lines:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a forwarded line")
		return ""
	}
}

func TestReadBootstrapLine_HappyPath(t *testing.T) {
	stdin := strings.NewReader("starting...\n__AO_BOOTSTRAP__: {\"port\":54321,\"token\":\"abc123\"}\n")

	sink := newLineSink()
	bs, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, sink.drop)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bs.Port != 54321 || bs.Token != "abc123" {
		t.Fatalf("unexpected bootstrap: %+v", bs)
	}
	if got := sink.next(t); got != "starting..." {
		t.Fatalf("dropped line = %q, want %q", got, "starting...")
	}
}

// TestReadBootstrapLine_KeepsDrainingAfterBootstrap — the sentinel ends
// the bootstrap wait, not the reading. Post-bootstrap stdout keeps
// flowing through the same dropFn, and the bytes the scanner had already
// buffered past the sentinel are not lost on the way.
func TestReadBootstrapLine_KeepsDrainingAfterBootstrap(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	// One write: the bootstrap line AND the line after it land in the
	// scanner's buffer together, so a drain that restarted from the raw
	// reader instead of the scanner would drop "buffered-with-sentinel".
	if _, err := pw.WriteString("__AO_BOOTSTRAP__: {\"port\":54321,\"token\":\"abc123\"}\nbuffered-with-sentinel\n"); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	sink := newLineSink()
	bs, err := readBootstrapLine(context.Background(), pr, DefaultBootstrapPrefix, sink.drop)
	if err != nil {
		t.Fatalf("readBootstrapLine: %v", err)
	}
	if bs.Port != 54321 {
		t.Fatalf("unexpected bootstrap: %+v", bs)
	}
	if got := sink.next(t); got != "buffered-with-sentinel" {
		t.Fatalf("first drained line = %q, want %q", got, "buffered-with-sentinel")
	}

	if _, err := pw.WriteString("after-handoff\n"); err != nil {
		t.Fatalf("write post-bootstrap: %v", err)
	}
	if got := sink.next(t); got != "after-handoff" {
		t.Fatalf("second drained line = %q, want %q", got, "after-handoff")
	}

	// EOF ends the drain; the goroutine must not outlive the stream.
	if err := pw.Close(); err != nil {
		t.Fatalf("close write half: %v", err)
	}
}

// TestReadBootstrapLine_PostBootstrapStdoutNeverBlocksChild — the bug this
// drain exists for: an unread stdout fills the OS pipe buffer (64 KiB on
// Linux) and the child blocks inside write(2) forever. Writing several
// times that much must complete.
func TestReadBootstrapLine_PostBootstrapStdoutNeverBlocksChild(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	if _, err := pw.WriteString("__AO_BOOTSTRAP__: {\"port\":1,\"token\":\"t\"}\n"); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	if _, err := readBootstrapLine(context.Background(), pr, DefaultBootstrapPrefix, func(string) {}); err != nil {
		t.Fatalf("readBootstrapLine: %v", err)
	}

	written := make(chan error, 1)
	go func() {
		chatter := strings.Repeat("gtk-message: a library wrote to stdout\n", 16*1024) // ~600 KiB
		_, err := pw.WriteString(chatter)
		written <- err
	}()

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("post-bootstrap stdout write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-bootstrap stdout write blocked: the backend would be wedged in write(2)")
	}
}

// TestDrainStream_FallsBackToDiscardOnOversizedLine — a line longer than
// the scanner's cap stops the line scan. Stopping there would re-create
// the wedged-pipe bug, so the drain downgrades to discarding bytes and
// says so in the log.
func TestDrainStream_FallsBackToDiscardOnOversizedLine(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	var logBuf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previous) })

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainStream(newStreamScanner(pr), pr, "stdout", func(string) {})
	}()

	written := make(chan error, 1)
	go func() {
		// One 256 KiB line with no newline (blows the 64 KiB cap),
		// followed by more bytes that must still be consumed.
		_, err := pw.WriteString(strings.Repeat("x", 256*1024) + strings.Repeat("y\n", 64*1024))
		written <- err
	}()

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write blocked: the drain gave up on the stream after the oversized line")
	}

	if err := pw.Close(); err != nil {
		t.Fatalf("close write half: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not end after the stream closed")
	}

	if out := logBuf.String(); !strings.Contains(out, "discarding the rest of the stream") {
		t.Fatalf("downgrade to discarding must be logged, got: %q", out)
	}
}

// TestDrainStream_ClosedStreamStopsQuietly — Wait/Stop closing the pipe is
// the clean end of a drain, not a failure worth a log line: nothing can
// block on a pipe that no longer exists.
func TestDrainStream_ClosedStreamStopsQuietly(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pw.Close()

	var logBuf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previous) })

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainStream(newStreamScanner(pr), pr, "stdout", func(string) {})
	}()

	time.Sleep(20 * time.Millisecond) // let the drain park inside Read
	if err := pr.Close(); err != nil {
		t.Fatalf("close read half: %v", err)
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not end after the reader was closed")
	}
	if out := logBuf.String(); out != "" {
		t.Fatalf("closed stream must end the drain quietly, logged: %q", out)
	}
}

// TestDrainStderr_ForwardsUntilEOF — the stderr drain keeps the same
// line-forwarding contract now that it shares the stdout implementation.
func TestDrainStderr_ForwardsUntilEOF(t *testing.T) {
	sink := newLineSink()
	done := make(chan struct{})
	go func() {
		defer close(done)
		drainStderr(strings.NewReader("first\nsecond\n"), sink.drop)
	}()

	if got := sink.next(t); got != "first" {
		t.Fatalf("stderr line 1 = %q", got)
	}
	if got := sink.next(t); got != "second" {
		t.Fatalf("stderr line 2 = %q", got)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainStderr did not return at EOF")
	}
}

func TestReadBootstrapLine_RejectsMalformedJSON(t *testing.T) {
	stdin := strings.NewReader("__AO_BOOTSTRAP__: not-json\n")
	_, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, nil)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode bootstrap") {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
}

func TestReadBootstrapLine_RejectsZeroPort(t *testing.T) {
	stdin := strings.NewReader("__AO_BOOTSTRAP__: {\"port\":0,\"token\":\"abc\"}\n")
	_, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, nil)
	if err == nil {
		t.Fatal("expected error on zero port")
	}
	if !strings.Contains(err.Error(), "invalid bootstrap") {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
}

func TestReadBootstrapLine_RejectsEmptyToken(t *testing.T) {
	stdin := strings.NewReader("__AO_BOOTSTRAP__: {\"port\":1234,\"token\":\"\"}\n")
	_, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, nil)
	if err == nil {
		t.Fatal("expected error on empty token")
	}
}

func TestReadBootstrapLine_TimeoutFires(t *testing.T) {
	// blockingReader simulates a child that never prints the bootstrap.
	// We expect the context cancellation to surface as the error.
	r := blockingReader{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := readBootstrapLine(ctx, r, DefaultBootstrapPrefix, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestReadBootstrapLine_TimeoutWithClose_GoroutineExits(t *testing.T) {
	// Production timeout path: the caller MUST close the reader so the
	// scanner goroutine unblocks. We pair an os.Pipe reader (which
	// blocks Read until either bytes arrive or the pipe is closed)
	// with a deliberate Close on the timeout path; the test asserts
	// that readBootstrapLine returns promptly. A regression that broke
	// the close-on-timeout contract would either hang indefinitely
	// (test timeout) or surface as a stuck goroutine in subsequent
	// runs.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Sentinel goroutine: close the read half once ctx fires — mirrors
	// the Windows launcher's stdout-pipe closure on bootstrap timeout.
	go func() {
		<-ctx.Done()
		_ = pr.Close()
	}()

	start := time.Now()
	_, err = readBootstrapLine(ctx, pr, DefaultBootstrapPrefix, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	// 250ms is generous: the ctx fires at 50ms, the sentinel closes
	// the pipe shortly after, and the scanner goroutine should exit
	// before its "child closed stdout" branch runs.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("readBootstrapLine took %v, want < 250ms (goroutine likely leaked)", elapsed)
	}
}

func TestReadBootstrapLine_StdoutCloseBeforeBootstrap(t *testing.T) {
	stdin := strings.NewReader("oops\n")
	_, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, nil)
	if err == nil {
		t.Fatal("expected error when stdout closes without bootstrap")
	}
	if !strings.Contains(err.Error(), "closed stdout") {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
}

func TestLauncherStop_Idempotent(t *testing.T) {
	// Launcher with only the platform field exercises the Stop code
	// path without spawning a real process. The stub close() returns
	// nil; we verify a second call is a no-op.
	platform, err := newPlatformLauncher()
	if err != nil {
		t.Fatalf("newPlatformLauncher: %v", err)
	}
	l := &Launcher{platform: platform}

	if err := l.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestLauncherWait_NilCommand(t *testing.T) {
	l := &Launcher{}
	if err := l.Wait(); err == nil {
		t.Fatal("expected error from Wait on Launcher with no command")
	}
}

// blockingReader.Read blocks until the caller cancels the underlying
// context — used to simulate a child that prints nothing.
type blockingReader struct{}

func (blockingReader) Read(p []byte) (int, error) {
	// Block long enough that any reasonable timeout fires first.
	time.Sleep(time.Hour)
	return 0, nil
}
