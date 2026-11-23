//go:build !windows

package harnessclient

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// These cases signal a real exited child and need the signal to answer
// os.ErrProcessDone. Unix answers that for a reaped pid. Windows refuses
// the kill of an exited process with ERROR_ACCESS_DENIED instead, and its
// pid probe reads the exit code, so the sentinel is not its proof of exit.

// A kill refused because the process already exited proves it gone, even
// when a probe of its pid, which another process may since hold, says
// alive. Once the backend exited from the polite stop and was reaped, the
// probe says alive and the identity check passes, as they would for a pid
// reused by a process that looks the same, so the kill that follows the
// grace finds it done.
func TestTerminateTreatsAKillOfAFinishedProcessAsStopped(t *testing.T) {
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	exited := launched.startWaiter()
	restoreProbe, restoreIdentity := processAlive, verifyProcessIdentity
	processAlive = func(pid int) bool {
		select {
		case <-exited:
			return true
		default:
			return restoreProbe(pid)
		}
	}
	verifyProcessIdentity = func(int, instanceinfo.ProcessIdentity) error { return nil }
	t.Cleanup(func() { processAlive, verifyProcessIdentity = restoreProbe, restoreIdentity })

	if err := TerminateProcessVerified(context.Background(), launched.Bootstrap.PID, launched.identity, time.Second); err != nil {
		t.Fatalf("terminate of a process that exited = %v, want success", err)
	}
}

// A polite stop refused because the process already exited is a stop.
func TestTerminateTreatsAStopOfAFinishedProcessAsStopped(t *testing.T) {
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid := launched.Bootstrap.PID
	if err := TerminateProcessVerified(context.Background(), pid, launched.identity, time.Second); err != nil {
		t.Fatalf("first terminate: %v", err)
	}
	<-launched.startWaiter()
	restoreProbe, restoreIdentity := processAlive, verifyProcessIdentity
	processAlive = func(int) bool { return true }
	verifyProcessIdentity = func(int, instanceinfo.ProcessIdentity) error { return nil }
	t.Cleanup(func() { processAlive, verifyProcessIdentity = restoreProbe, restoreIdentity })

	if err := TerminateProcessVerified(context.Background(), pid, launched.identity, time.Second); err != nil {
		t.Fatalf("terminate of a reaped process = %v, want success", err)
	}
}
