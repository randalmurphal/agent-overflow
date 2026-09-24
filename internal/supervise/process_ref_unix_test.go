//go:build !windows

package supervise

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// startSleeper starts a child that runs until it is killed.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestCurrentProcessRefNamesThisProcess(t *testing.T) {
	self, err := CurrentProcessRef()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ProcessRefOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self.PID != os.Getpid() || self.Start == "" || self != again {
		t.Fatalf("CurrentProcessRef = %+v, then %+v", self, again)
	}
	if running, err := self.Running(); err != nil || !running {
		t.Fatalf("Running = %v, %v", running, err)
	}
}

// A ProcessRef's start is the birth marker instanceinfo records for the
// process, read by the one reader both use.
func TestProcessRefStartIsTheInstanceIdentityStartTime(t *testing.T) {
	cmd := startSleeper(t)
	for _, pid := range []int{os.Getpid(), cmd.Process.Pid} {
		ref, err := ProcessRefOf(pid)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := instanceinfo.CaptureProcessIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if ref.Start != identity.StartTime {
			t.Fatalf("pid %d: ProcessRef start %q, identity start %q", pid, ref.Start, identity.StartTime)
		}
	}
}

// TestWaitForExitWaitsForTheProcessItNames: a wait times out on a running
// process and returns once it exits, before its parent reaps it.
func TestWaitForExitWaitsForTheProcessItNames(t *testing.T) {
	cmd := startSleeper(t)
	ref, err := ProcessRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := WaitForExit(t.Context(), ref, 150*time.Millisecond); !errors.Is(err, ErrProcessRunning) {
		t.Fatalf("WaitForExit on a running process = %v, want ErrProcessRunning", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Not reaped yet: the exited child is a zombie, which has exited.
	if err := WaitForExit(t.Context(), ref, 5*time.Second); err != nil {
		t.Fatalf("WaitForExit after the kill = %v", err)
	}
	_ = cmd.Wait()
	if running, err := ref.Running(); err != nil || running {
		t.Fatalf("Running after the process was reaped = %v, %v", running, err)
	}
	if err := WaitForExit(t.Context(), ref, time.Second); err != nil {
		t.Fatalf("WaitForExit on a reaped process = %v", err)
	}
}

// TestAReusedProcessIDIsNotTheProcess: an id that names a process started
// at another time is the exit of the one the reference named, so a wait
// never follows a reused id to an unrelated process.
func TestAReusedProcessIDIsNotTheProcess(t *testing.T) {
	cmd := startSleeper(t)
	live, err := ProcessRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	reused := ProcessRef{PID: live.PID, Start: live.Start + "0"}
	if running, err := reused.Running(); err != nil || running {
		t.Fatalf("Running on a reused id = %v, %v", running, err)
	}
	start := time.Now()
	if err := WaitForExit(t.Context(), reused, 5*time.Second); err != nil {
		t.Fatalf("WaitForExit on a reused id = %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("WaitForExit on a reused id waited %s for an unrelated process", took)
	}
}

func TestWaitForExitStopsWithItsContext(t *testing.T) {
	cmd := startSleeper(t)
	ref, err := ProcessRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := WaitForExit(ctx, ref, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForExit after cancel = %v", err)
	}
}

func TestProcessRefRequiresAnIDAndAStart(t *testing.T) {
	for _, ref := range []ProcessRef{{}, {PID: os.Getpid()}, {Start: "1"}, {PID: -1, Start: "1"}} {
		if _, err := ref.Running(); err == nil {
			t.Errorf("Running(%+v) accepted an incomplete reference", ref)
		}
		if err := WaitForExit(t.Context(), ref, time.Second); err == nil {
			t.Errorf("WaitForExit(%+v) accepted an incomplete reference", ref)
		}
	}
	if _, err := ProcessRefOf(0); err == nil {
		t.Error("ProcessRefOf(0) named a process")
	}
}
