package wsllauncher

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakePlatform stands in for the Job Object so a test can say whether the
// platform close succeeded without owning a real handle.
type fakePlatform struct{ closeErr error }

func (fakePlatform) adopt(*exec.Cmd) error { return nil }
func (f fakePlatform) close() error        { return f.closeErr }

// errKillRefused is the shape Windows reports when TerminateProcess is
// called on a child the Job Object close has already killed: a permission
// refusal about a process that is on its way out.
var errKillRefused = errors.New("TerminateProcess: Access is denied.")

// startStopTestChild spawns the lifetime helper child and returns its
// launcher. mode "exited" prints its readiness line and exits; "running"
// stays up until the test kills it.
func startStopTestChild(t *testing.T, mode string, platform platformLauncher, kill func() error) *Launcher {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLauncherLifetimeChild$")
	cmd.Env = append(os.Environ(), "AO_TEST_LAUNCHER_LIFETIME="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The injected kill never terminates anything, so the real one has
		// to run here or a "running" child outlives the test.
		_ = cmd.Process.Kill()
	})
	if ready, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("child readiness = %q, %v", ready, err)
	}
	return &Launcher{cmd: cmd, platform: platform, killChild: kill}
}

// TestLauncherStopIgnoresKillRefusalForAnExitedChild is the Windows
// teardown log fix: the Job Object close kills the child, the
// belt-and-braces Kill then reports "Access is denied" about a process
// that is already dead, and Stop must read the exit rather than the
// errno. Reported as a failure it made every ordinary window close look
// like a teardown that had gone wrong.
func TestLauncherStopIgnoresKillRefusalForAnExitedChild(t *testing.T) {
	l := startStopTestChild(t, "exited", fakePlatform{}, func() error { return errKillRefused })
	if err := l.Wait(); err != nil {
		t.Fatalf("wait for the exited child: %v", err)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("Stop reported %v for a child the platform close already killed", err)
	}
}

// TestLauncherStopReportsKillFailureForALiveChild is the other half: the
// refusal is only forgiven for a process that is actually gone. A child
// still running when Kill fails is a real teardown failure and has to
// reach the caller.
func TestLauncherStopReportsKillFailureForALiveChild(t *testing.T) {
	l := startStopTestChild(t, "running", fakePlatform{}, func() error { return errKillRefused })
	err := l.Stop()
	if err == nil {
		t.Fatal("Stop reported success while the child was still running")
	}
	if !strings.Contains(err.Error(), "kill child") {
		t.Fatalf("Stop error = %v, want the kill failure", err)
	}
}

// TestLauncherStopReportsKillFailureWhenPlatformCloseFailed pins the
// gate: nothing killed the child on the launcher's behalf, so a Kill
// failure is reported even though the process turns out to be gone.
func TestLauncherStopReportsKillFailureWhenPlatformCloseFailed(t *testing.T) {
	closeErr := errors.New("CloseHandle: refused")
	l := startStopTestChild(t, "exited", fakePlatform{closeErr: closeErr}, func() error { return errKillRefused })
	if err := l.Wait(); err != nil {
		t.Fatalf("wait for the exited child: %v", err)
	}
	err := l.Stop()
	if err == nil {
		t.Fatal("Stop reported success after the platform close failed")
	}
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "kill child") {
		t.Fatalf("Stop error = %v, want both the close and the kill failure", err)
	}
}

// TestLauncherStopKeepsProcessDoneQuiet pins the pre-existing sentinel
// path: an already-reaped child answers os.ErrProcessDone, which was
// never a failure and must not become one.
func TestLauncherStopKeepsProcessDoneQuiet(t *testing.T) {
	l := startStopTestChild(t, "exited", fakePlatform{}, func() error { return os.ErrProcessDone })
	if err := l.Stop(); err != nil {
		t.Fatalf("Stop reported %v for an already-done child", err)
	}
}
