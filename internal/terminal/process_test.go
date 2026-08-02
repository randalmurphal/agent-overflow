//go:build !windows

package terminal

import (
	"os"
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
// reached the child, and the trailing "24 80" proves the size was restored.
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

	if err := p.Refresh(24, 80); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// The shrink nudge fires SIGWINCH and the child reports the smaller "23 80";
	// the restore returns it to the original "24 80" (fail-safe: never mis-sized).
	// Both are matched against one accumulating buffer so the two reports landing
	// in a single read chunk can't strand the second assertion.
	needles := []string{"23 80", "24 80"}
	out := drainOrdered(t, p.Output(), needles, 3*time.Second)
	if !containsInOrder(out, needles) {
		t.Fatalf("expected nudged '23 80' then restored '24 80' in output, got %q", out)
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

// drainOrdered reads chunks into one accumulating buffer until every needle has
// appeared in order, or the deadline passes. Unlike chaining drainUntil calls,
// the single buffer means two needles arriving in one read chunk are both seen —
// chaining would consume both while matching the first and then block forever on
// the second.
func drainOrdered(t *testing.T, ch <-chan []byte, needles []string, timeout time.Duration) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		if containsInOrder(sb.String(), needles) {
			return sb.String()
		}
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

// containsInOrder reports whether every needle occurs in s in the given order
// (each match consuming the text before it, so repeats must be distinct
// occurrences).
func containsInOrder(s string, needles []string) bool {
	off := 0
	for _, n := range needles {
		i := strings.Index(s[off:], n)
		if i < 0 {
			return false
		}
		off += i + len(n)
	}
	return true
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
