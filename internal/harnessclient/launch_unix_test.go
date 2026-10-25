//go:build !windows

package harnessclient

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// A member the backend starts after the group's SIGTERM never receives it.
// When the backend then exits, the member the launcher recorded at the
// backend's exit is the group's proof, and it gets a SIGTERM of its own
// instead of outliving the grace period.
func TestTerminateSignalsAMemberStartedAfterTheGroupSignal(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	opts := fakeBackendOpts(t, "fork-on-term", t.TempDir())
	opts.Env = append(opts.Env, fakeBackendChildPID+"="+childPIDFile)
	launched, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	started := time.Now()
	terminateErr := launched.Terminate(context.Background())
	elapsed := time.Since(started)
	childPIDBytes, readErr := os.ReadFile(childPIDFile)
	if readErr != nil {
		t.Fatalf("read child pid: %v (terminate: %v)", readErr, terminateErr)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	t.Cleanup(func() {
		if instanceinfo.ProcessAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	})
	if terminateErr != nil {
		t.Fatalf("Terminate: %v", terminateErr)
	}
	if instanceinfo.ProcessAlive(childPID) {
		t.Fatalf("child %d survived the teardown", childPID)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("teardown took %s: the child waited out the grace period instead of receiving SIGTERM", elapsed)
	}
}
