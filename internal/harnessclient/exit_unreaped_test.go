//go:build linux || darwin

package harnessclient

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/procutil"
)

// awaitExitUnreaped returns once the child exits and leaves it a zombie,
// so the PID and group id it holds cannot be reused before the reap.
func TestExitIsObservedBeforeTheReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := awaitExitUnreaped(cmd.Process.Pid); err != nil {
		_ = cmd.Wait()
		t.Fatalf("awaitExitUnreaped: %v", err)
	}
	if !procutil.Exited(cmd.Process.Pid) {
		_ = cmd.Wait()
		t.Fatal("the child is not an unreaped zombie after its exit was observed")
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("reap after the observed exit = %v, want exit status 7", err)
	}
}

// The waiter records the backend's group while the exited backend is still
// unreaped, which is what makes the record the backend's own group.
func TestLaunchRecordsTheGroupBeforeReapingTheBackend(t *testing.T) {
	oldSnapshot := snapshotProcessGroup
	t.Cleanup(func() { snapshotProcessGroup = oldSnapshot })
	// Only the waiter snapshots a backend that is no longer running; the
	// teardown's own captures run while it is alive.
	recordedUnreaped := make(chan bool, 1)
	snapshotProcessGroup = func(pgid int) ([]ownedGroupMember, error) {
		if !instanceinfo.ProcessAlive(pgid) {
			select {
			case recordedUnreaped <- procutil.Exited(pgid):
			default:
			}
		}
		return oldSnapshot(pgid)
	}
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := launched.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case unreaped := <-recordedUnreaped:
		if !unreaped {
			t.Fatal("the group was recorded after the backend was reaped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never recorded the backend's group")
	}
}
