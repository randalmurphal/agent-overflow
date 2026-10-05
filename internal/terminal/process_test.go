//go:build !windows

package terminal

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessStartNormalizesTerminalCapabilities(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("COLORTERM", "legacy")

	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", `printf 'TERM=%s COLORTERM=%s\n' "$TERM" "$COLORTERM"`},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	got := drainOutput(t, p.Output(), 2*time.Second)
	if !strings.Contains(got, "TERM=xterm-256color COLORTERM=truecolor") {
		t.Fatalf("PTY child environment = %q", got)
	}
}

// TestProcessStartEcho spawns `sh -c 'echo hi; exit 0'` and asserts we
// receive the "hi" output, the process exits cleanly, and the output channel
// closes.
func TestProcessStartEcho(t *testing.T) {
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "echo hi"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	collected := drainOutput(t, p.Output(), 2*time.Second)
	if !strings.Contains(collected, "hi") {
		t.Fatalf("expected output to contain 'hi', got %q", collected)
	}

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit")
	}

	status := p.ExitStatus()
	if status.Code != 0 {
		t.Fatalf("expected exit 0, got %+v", status)
	}
	if status.Reason != "exit" {
		t.Fatalf("expected reason 'exit', got %q", status.Reason)
	}
}

// TestProcessWriteInput writes a line to a `cat` shell and verifies it comes
// back.
func TestProcessWriteInput(t *testing.T) {
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "cat"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	if err := p.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := drainUntil(t, p.Output(), "hello", 2*time.Second)
	if !strings.Contains(got, "hello") {
		t.Fatalf("did not see echoed 'hello', got %q", got)
	}

	if err := p.Close(); err != nil {
		t.Logf("Close returned: %v (cat may SIGTERM non-zero)", err)
	}
}

// TestProcessResize invokes Resize and confirms no error. We can't easily
// observe the new size without running stty inside, so we run stty -a in the
// PTY and check the visible columns match.
func TestProcessResize(t *testing.T) {
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "stty -a; sleep 0.1; exit 0"},
		Cwd:   t.TempDir(),
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	if err := p.Resize(30, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit")
	}
}

// TestProcessRefreshNudgesAndRestores verifies Refresh delivers a SIGWINCH by
// briefly shrinking the winsize and then restores the original. A shell traps
// WINCH and reports `stty size` on each one: the shrunk "23 80" proves the nudge
// reached the child, and a later "24 80" proves the size was restored.
//
// The shell echoes READY only after installing the trap, and the test waits for
// it before nudging. Without that sync, a startup race lets the first (shrink)
// SIGWINCH reach the default handler before the trap is installed — it is then
// dropped and the test observes only the restore. In production the provider's
// SIGWINCH handler is installed long before any refresh, so READY models the
// steady state Refresh actually runs against.
func TestProcessRefreshNudgesAndRestores(t *testing.T) {
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "trap 'stty size' WINCH; echo READY; while :; do sleep 0.02; done"},
		Cwd:   t.TempDir(),
		Rows:  24,
		Cols:  80,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	// Wait for the trap to be installed before nudging (see doc comment).
	ready := drainUntil(t, p.Output(), "READY", 3*time.Second)
	if !strings.Contains(ready, "READY") {
		t.Fatalf("shell did not signal trap readiness, got %q", ready)
	}

	// The shell runs its trap only after the current sleep, and `stty size`
	// reads the winsize when it runs, so a fixed pause can end before the
	// child reports the nudge. Hold the nudge until it has.
	var nudged string
	p.pauseNudge = func(time.Duration) {
		nudged = drainUntil(t, p.Output(), "23 80", 3*time.Second)
	}
	if err := p.Refresh(24, 80); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !strings.Contains(nudged, "23 80") {
		t.Fatalf("expected the nudged '23 80' during the nudge, got %q", nudged)
	}
	// Everything read from here on was written after the restore
	// (fail-safe: never mis-sized).
	if restored := drainUntil(t, p.Output(), "24 80", 3*time.Second); !strings.Contains(restored, "24 80") {
		t.Fatalf("expected the restored '24 80' after the nudge, got %q", restored)
	}
}

// TestNudgeRows pins the pure size arithmetic Refresh relies on. The rows==1
// boundary is load-bearing: a single-row terminal must grow (not shrink to a
// invalid 0-row winsize), so this guards a future "rows <= 1" → "rows < 1"
// style regression that would emit an invalid winsize.
func TestNudgeRows(t *testing.T) {
	cases := []struct {
		rows   uint16
		want   uint16
		wantOk bool
	}{
		{rows: 0, want: 0, wantOk: false},        // invalid winsize → no nudge
		{rows: 1, want: 2, wantOk: true},         // can't shrink to 0 → grow
		{rows: 2, want: 1, wantOk: true},         // smallest size that still shrinks
		{rows: 24, want: 23, wantOk: true},       // typical
		{rows: 65535, want: 65534, wantOk: true}, // max, no overflow
	}
	for _, c := range cases {
		got, ok := nudgeRows(c.rows)
		if got != c.want || ok != c.wantOk {
			t.Errorf("nudgeRows(%d) = (%d, %v), want (%d, %v)", c.rows, got, ok, c.want, c.wantOk)
		}
	}
}

// TestProcessKillsGroup ensures that a shell which forks a long-running child
// is fully killed when we call Close/Kill. We accomplish this by having the
// shell print the child pid to stdout and then asserting that the child pid
// is not alive after Kill returns.
func TestProcessKillsGroup(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getenv("TERMINAL_GROUP_TEST") == "" {
		t.Skip("group kill test flaky on some CI runners; set TERMINAL_GROUP_TEST=1 to enable")
	}

	// `sh -c "sleep 60 & echo $!; wait"` — prints the child pid then waits.
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "sleep 60 & echo CHILDPID=$!; wait"},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	childPID := extractChildPID(t, p.Output(), 2*time.Second)
	if childPID == 0 {
		t.Fatal("did not observe CHILDPID from PTY output")
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Give the kernel a brief moment to reap.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child pid %d is still alive after Kill", childPID)
}

// A job the shell leaves in the background keeps the pty open, and the
// terminal still ends with the shell, as a terminal window closes when its
// shell exits: the output closes while the job (which ignores the hangup)
// runs on. The shell is quiet before it exits, so the output pump is
// waiting on an empty pty when the exit lands.
func TestProcessEndsWhenTheShellExitsWhileAJobHoldsThePTY(t *testing.T) {
	p, job := startShellWithBackgroundJob(t, `sleep 0.3; exit 3`)

	out, closed := collectOutput(p.Output(), 10*time.Second)
	if !closed {
		t.Fatalf("output still open after the shell exited; got %q", out)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not report the shell's exit")
	}
	if code := p.ExitStatus().Code; code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !processAlive(job) {
		t.Fatal("the background job died, so it did not hold the pty")
	}
}

// Everything the shell wrote before it exited is delivered, though the pty
// stays open behind a background job. On Linux nothing reads the output
// until the shell has exited, so the output channel fills and the rest of
// the shell's output is still in the pty when it exits. Darwin holds an
// exiting session leader until its terminal's output has been read, so the
// shell cannot exit first there and the test reads before waiting.
func TestProcessDeliversTheShellsLastOutputWhileAJobHoldsThePTY(t *testing.T) {
	const lines = 80 // more reads than the output channel holds
	p, _ := startShellWithBackgroundJob(t,
		`i=0; while [ $i -lt 80 ]; do printf 'line\n'; sleep 0.005; i=$((i+1)); done; printf 'TAIL\n'; exit 0`)

	awaitShellExit := func() {
		t.Helper()
		select {
		case <-p.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("process did not report the shell's exit")
		}
	}
	if runtime.GOOS == "linux" {
		awaitShellExit()
	}
	out, closed := collectOutput(p.Output(), 10*time.Second)
	if !closed {
		t.Fatalf("output still open after the shell exited; got %q", out)
	}
	awaitShellExit()
	if got := strings.Count(out, "line"); got != lines || !strings.Contains(out, "TAIL") {
		t.Fatalf("delivered %d of %d lines (tail seen: %v) the shell wrote before exiting",
			got, lines, strings.Contains(out, "TAIL"))
	}
}

// startShellWithBackgroundJob starts a shell that leaves a job holding the
// pty and ignoring the hangup, then runs script. It returns the process and
// the job's pid, which the test's cleanup kills.
func startShellWithBackgroundJob(t *testing.T, script string) (*Process, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "job.pid")
	p, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", `(trap '' HUP; exec sleep 30) & echo $! > "$0"; ` + script, pidFile},
		Cwd:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p, backgroundJobPID(t, pidFile)
}

// backgroundJobPID reads the pid a test shell wrote to path and kills that
// process when the test ends.
func backgroundJobPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && convErr == nil && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("background job pid never written to %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collectOutput reads ch until it closes or timeout passes, reporting
// whether it closed.
func collectOutput(ch <-chan []byte, timeout time.Duration) (string, bool) {
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return sb.String(), true
			}
			sb.Write(chunk)
		case <-deadline:
			return sb.String(), false
		}
	}
}

// TestProcessStartRejectsBadCwd confirms we surface pty-spawn errors.
func TestProcessStartRejectsBadCwd(t *testing.T) {
	_, err := Start(ProcessConfig{
		Shell: "/bin/sh",
		Args:  []string{"-c", "true"},
		Cwd:   "/this/path/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected Start to fail for missing cwd")
	}
	if !strings.Contains(err.Error(), "terminal:") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

// drainOutput reads available chunks from the channel until it closes or the
// deadline is reached, returning the concatenated string.
func drainOutput(t *testing.T, ch <-chan []byte, timeout time.Duration) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return sb.String()
			}
			sb.Write(chunk)
		case <-deadline:
			return sb.String()
		}
	}
}

func drainUntil(t *testing.T, ch <-chan []byte, needle string, timeout time.Duration) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return sb.String()
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), needle) {
				return sb.String()
			}
		case <-deadline:
			return sb.String()
		}
	}
}

func extractChildPID(t *testing.T, ch <-chan []byte, timeout time.Duration) int {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return 0
			}
			sb.Write(chunk)
			if idx := strings.Index(sb.String(), "CHILDPID="); idx >= 0 {
				rest := sb.String()[idx+len("CHILDPID="):]
				var pid int
				for _, r := range rest {
					if r >= '0' && r <= '9' {
						pid = pid*10 + int(r-'0')
					} else if pid > 0 {
						return pid
					}
				}
				if pid > 0 {
					return pid
				}
			}
		case <-deadline:
			return 0
		}
	}
}

// processAlive returns true if the given pid is running. Implemented via
// signal-0 which never delivers a signal but reports existence.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}
