package wsllauncher

import (
	"context"
	"errors"
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

func TestReadBootstrapLine_HappyPath(t *testing.T) {
	stdin := strings.NewReader("starting...\n__AO_BOOTSTRAP__: {\"port\":54321,\"token\":\"abc123\"}\n")

	var dropped []string
	bs, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, func(s string) {
		dropped = append(dropped, s)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bs.Port != 54321 || bs.Token != "abc123" {
		t.Fatalf("unexpected bootstrap: %+v", bs)
	}
	if len(dropped) != 1 || dropped[0] != "starting..." {
		t.Fatalf("expected one dropped line, got %v", dropped)
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
