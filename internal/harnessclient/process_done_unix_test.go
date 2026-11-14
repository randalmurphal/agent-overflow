//go:build !windows

package harnessclient

import (
	"context"
	"testing"
	"time"
)

// These cases signal a real exited child and need the signal to answer
// os.ErrProcessDone. Unix answers that for a reaped pid. Windows refuses
// the kill of an exited process with ERROR_ACCESS_DENIED instead, and its
// pid probe reads the exit code, so the sentinel is not its proof of exit.

// A kill refused because the process already exited proves it gone, even
// when a probe of its pid, which another process may since hold, says
// alive. The probe blocks until the backend exited from the polite stop
// and was reaped, so the kill that follows the grace finds it done.
func TestTerminateTreatsAKillOfAFinishedProcessAsStopped(t *testing.T) {
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	exited := launched.startWaiter()
	restoreProbe := processAlive
	processAlive = func(int) bool {
		<-exited
		return true
	}
	t.Cleanup(func() { processAlive = restoreProbe })

	if err := TerminateProcess(context.Background(), launched.Bootstrap.PID, time.Second); err != nil {
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
	if err := TerminateProcess(context.Background(), pid, time.Second); err != nil {
		t.Fatalf("first terminate: %v", err)
	}
	<-launched.startWaiter()
	restoreProbe := processAlive
	processAlive = func(int) bool { return true }
	t.Cleanup(func() { processAlive = restoreProbe })

	if err := TerminateProcess(context.Background(), pid, time.Second); err != nil {
		t.Fatalf("terminate of a reaped process = %v, want success", err)
	}
}
