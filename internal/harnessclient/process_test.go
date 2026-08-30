package harnessclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

func TestTerminateOwnedRechecksIdentityBeforeGroupKill(t *testing.T) {
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", t.TempDir()))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	restoreProbe := processAlive
	restoreVerify := verifyProcessIdentity
	processAlive = func(int) bool { return true }
	checks := 0
	verifyProcessIdentity = func(int, instanceinfo.ProcessIdentity) error {
		checks++
		if checks > 1 {
			return errors.New("pid was recycled")
		}
		return nil
	}
	t.Cleanup(func() {
		processAlive = restoreProbe
		verifyProcessIdentity = restoreVerify
		_ = launched.Terminate(context.Background())
	})

	err = launched.Terminate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "before signal") {
		t.Fatalf("Terminate error = %v, want identity refusal before signal", err)
	}
	if checks != 2 {
		t.Fatalf("identity checks = %d, want initial and pre-kill checks", checks)
	}
}

func TestTerminateRefusesANonPID(t *testing.T) {
	if err := TerminateProcess(context.Background(), 0, time.Second); err == nil {
		t.Fatal("terminate accepted pid 0")
	}
}

// A delivered SIGKILL is a signal accepted, not a process gone: an
// uninterruptible sleep (a wedged 9p read under WSL, a stalled NFS mount)
// outlives it. Reporting "stopped" there is what puts two backends on one
// SQLite file, so the kill is confirmed and an unconfirmed one is an
// error. The liveness probe is stubbed because no cooperating child can be
// asked to survive SIGKILL on demand.
func TestTerminateReportsAProcessThatSurvivedSIGKILL(t *testing.T) {
	dataRoot := t.TempDir()
	launched, err := Launch(context.Background(), fakeBackendOpts(t, "linger", dataRoot))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid := launched.Bootstrap.PID

	restoreProbe := processAlive
	processAlive = func(int) bool { return true }
	restoreWindow := KillConfirmWindow
	KillConfirmWindow = 150 * time.Millisecond
	t.Cleanup(func() {
		processAlive = restoreProbe
		KillConfirmWindow = restoreWindow
		_ = TerminateProcess(context.Background(), pid, time.Second)
	})

	// Zero grace so the polite signal's wait is skipped and the kill path
	// is what this exercises.
	err = TerminateProcess(context.Background(), pid, 0)
	if err == nil {
		t.Fatal("terminate reported success for a process it never confirmed dead")
	}
	if !strings.Contains(err.Error(), "survived SIGKILL") {
		t.Fatalf("error does not name what happened: %v", err)
	}
}
